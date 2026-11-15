# TRMNL BYOS Server in Go: Design

## Summary

We will build a small Go + SQLite server that speaks the TRMNL device protocol, pre-renders every screen before the device wakes, and renders TRMNL recipes unmodified by driving trmnlp in an optional, isolated renderer process. The first deployment is two 7.5" OG DIY kits on a Proxmox container, but nothing is hardcoded to that panel.

**Goals**

- Support every model in TRMNL's registry: 1-bit, 4-gray, 16-gray and the limited color palettes, at any size and scale factor.
- Run TRMNL recipes (settings.yml + Liquid + transforms) with the same output as TRMNL's own renderer.
- Keep the device awake as briefly as possible: one fast request per wake, no download or repaint when nothing changed.
- Small, boring operations: one static Go binary, one SQLite file, no database or queue server.
- Easy for an AI assistant to extend: recipes as the plugin format, an MCP server for authoring and previews.
- Safe firmware updates: never send a build to a board it was not built for.

**Non-goals for v1**

- Multi-user accounts. One admin, or auth delegated to a reverse proxy.
- A drag-and-drop screen designer.
- Proxying to the TRMNL cloud, OAuth plugins, and native (non-browser) rendering. These are designed for and deferred (see [Deferred work](#deferred-work)).

## Alternatives considered

No existing server combines faithful recipe rendering with a small footprint, so we build one and borrow their best ideas.

| Option | Stack | Why not | What we borrow |
| --- | --- | --- | --- |
| Terminus | Ruby/Hanami, Postgres, Valkey, Sidekiq, Chromium (4 containers) | Heaviest stack. Own Liquid with 15 of ~25 TRMNL filters, always uses the latest framework, no transforms. Playlist cursor is shared across devices and advances on every wake. | Per-device button command, sleep windows, extension refresh schedules, detailed device logs, model and firmware sync jobs |
| byos_next | Next.js, Postgres, optional Chrome | Last commit Jun 30. Own Satori-based screens looked weak. | Little |
| Inker | NestJS, SQLite, Chromium | Moving to an Inker-native ecosystem. Imitation TRMNL CSS (~300 lines). Sends 8-bit gray PNGs, so the OG shows black and white only. | Per-item durations with drift-free timing, clock-aligned wakes, shared data sources, encrypted plugin secrets, device blocklist, live UI events |
| LaraPaper | PHP/Laravel, SQLite, Chromium | PHP. Approximate Liquid by default, no transforms, user Blade templates run as PHP. | MCP server, time-limited join window, scheduled playlists, alias endpoint, staleness flags, cloud proxy (deferred) |
| Ogre (pure-Go HTML to PNG) | Go library | Bugs in basics (borders, SVG strokes, truncation, min-width), one maintainer, 13 outside PRs unreviewed. Cannot run the framework's JavaScript, so it can never render recipes. | Kept as a possible native renderer later |

**Why trmnlp.** trmnlp is TRMNL's own plugin development tool (MIT). It uses the official `trmnl-liquid` gem, the real page wrapper with pinned framework versions, runs transforms in Python, JS, Ruby and PHP, and copies TRMNL's quantization per palette. It is the closest open reference to how the cloud renders a recipe.

## Architecture

A Go server owns all state and answers devices from cached PNGs; an optional renderer, trmnlp behind a small shim, does the rendering. The server streams each job to it, and the two share no files.

```mermaid
flowchart LR
  devices["TRMNL devices"] -->|"/api/setup, /api/display, /api/log"| api

  subgraph server["Server (Go)"]
    api["Device API"]
    sched["Scheduler"]
    store[("SQLite + PNG store")]
    admin["JSON API (built)<br/>Admin UI, MCP (not built)"]
    api --> store
    sched --> store
    admin --> store
  end

  subgraph renderer["Renderer (optional, isolated)"]
    shim["HTTP shim"] --> build["trmnlp build"]
  end

  you["You and assistants"] --> admin
  sched -->|"render job"| shim
  build -->|"PNG"| store
  build -->|"polling URLs, assets"| internet(("Internet"))
```

Device requests never wait on a render: the scheduler renders ahead, the shim runs one `trmnlp build` per job, and the PNG lands in the store by content hash.

- **Server**: device API, scheduler, store, the bearer-token JSON API, webhook ingest, the web UI's login page, shell and Devices list, and the CLI in one binary. The rest of the web UI and the MCP server are not built. The scheduler and the asset cache sweep run only when `-render-url` is set.
- **Renderer**: the `trmnl/trmnlp` image plus `trmnl-render`, a small Go shim (`deploy/renderer/Dockerfile`, `cmd/trmnl-render`, `internal/render`). The server POSTs a build job as one tar stream: the first member is `job.json` (size, palette, secret values) and the rest are the recipe project. The shim reads the job, then extracts the other members into a fresh directory, confined with `os.Root`, runs `trmnlp build` as an unprivileged user with its temporary and cache directories inside that directory, and streams back a tar of the build log and the four PNGs. If Xvfb exits, `trmnl-render` exits with an error, so the container restarts. A build gets at most 5 minutes from the server (the shim's own limit is 2). The renderer holds nothing between builds and shares no files with the server. How it is isolated depends on how it is deployed, but in every deployment its only way to the internet is the server's filtering proxy (see Renderer network).
- **Shutdown.** `serve` runs both HTTP servers and the loops (image expiry, the scheduler, the asset cache sweep) in one `errgroup`. When the process is told to stop, or any of them fails, such as a server that cannot listen, the rest are stopped, and the database is closed only after all of them have returned. A render in flight is canceled, not finished.
- **Asset cache**: the renderer's proxy caches the framework's CSS, JavaScript, fonts and images, Highcharts, and Google Fonts on the server's disk, so each is fetched from the internet once, and recipes pinned to old framework versions keep rendering.

## Device protocol and profiles

The server implements the three endpoints TRMNL firmware calls, and never renders inside a device request.

| Endpoint | Request | Response |
| --- | --- | --- |
| `GET /api/setup` | `ID` (MAC), `Model`, `FW-Version`, `Width`, `Height` | A new `api_key`, `friendly_id`, and an empty `image_url`, so the firmware draws its own screen with the friendly ID. Accepted only while the join window is open. |
| `GET /api/display` | `Access-Token`, battery, RSSI, `Refresh-Rate`, `Update-Source`, `special_function`, `Wake-Time`, `Image-Cached`, `SENSORS`, `Width`/`Height` | `image_url`, `filename`, `refresh_rate` (a number), `special_function`, `update_firmware`/`firmware_url`, `reset_firmware`. The firmware also reads `temperature_profile` and `maximum_compatibility`, which the server does not send. |
| `POST /api/log` | Firmware log entries | Stored per device with wake reason, heap and Wi-Fi state |

**What is built.** The three endpoints above, the status codes below, profile resolution from step 2 on, the image size limit, gray calibration, the join window, and `reset_firmware` for a device whose key a later setup replaced. The rest of this section says what is not built where it comes up, and the list at the end of it gathers them.

**Status codes.** The firmware reads the JSON `status` field, not the HTTP status, so `/api/setup` and `/api/display` answer HTTP 200 with the outcome in the body.

- `/api/setup` sends 200 for a registered device. For an unknown MAC it sends 404 with an `image_url`, which the device shows before trying again every 5 minutes.
- `/api/display` sends 0 for a normal response. The firmware ignores a response whose status is 200.
- An unknown device or a wrong token gets 202. The device keeps its credentials, shows its own setup screen, and polls again soon.
- Status 500 and `reset_firmware: true` both make the device erase its key, friendly ID, Wi-Fi settings and server URL, then start its setup portal. No response clears only the key. The server never sends 500. Today it sends `reset_firmware` to a registered MAC presenting a key that a later setup replaced; device delete, which would also trigger it, is not built. That device would otherwise keep its old key, never run setup again, and sit on its setup screen.
- An internal failure in `/api/display` (a lookup, drawing a screen, saving state) answers HTTP 200, status 0, no `image_url` and a `refresh_rate` of 5 minutes. In the firmware (`handleApiDisplayResponse` in `display_session.cpp`) that keeps the screen the device has and sleeps for `refresh_rate`. An HTTP error would instead be five quick retries in `HttpRetryRequest::execute`, then a 5 minute sleep, and a Wi-Fi error screen after an hour of failures (`requestErrorScreen`).
- An unknown MAC, or a request with no `ID`, is never reset. It may mean the server lost its database or is reading the wrong data directory, and a reset then would wipe every display at once.

**Device profile.** Each device resolves to a profile: width, height, rotation, offsets, scale factor, palette, plus the framework's CSS parameters (CSS screen size, pixel ratio, dither pixel ratio, density class). Resolution order:

1. Per-device override set in the UI (not built yet).
2. The firmware `Model` header mapped to a registry model. Firmware names differ from registry names (`xiao_epaper_display` and `og` map to `og_plus`; `x` maps to `v2`), so `internal/profile` keeps a mapping table, which also holds per-board size limits and measured gray levels. A `Model` that is itself a registry name, such as `xteink_x4`, needs no entry. Firmware 1.8.10 and older reports `og` for the 4-color OG too, which only the override can tell apart; newer firmware reports `og_4clr`.
3. For an unknown `Model`: the `Width`/`Height` headers, with black and white only and the smallest size limit. Matching size alone against the registry would be a guess, since a dozen models are 800x480.
4. With no usable headers either: 800x480, black and white.

The registry is a copy of `trmnl.com/api/models` and `/api/palettes` embedded in the binary and refreshed with `go generate`, never fetched per request. Refreshing it daily at runtime is deferred.

**Image size limit.** The firmware refuses an image body over `MAX_IMAGE_SIZE`: 90,000 bytes on a build without PSRAM, such as the original OG's ESP32-C3, and 750,000 bytes with it. The OG DIY kit's build gets PSRAM from the XIAO ESP32-S3 board definition, so it has the larger limit. The registry carries the limit as `image_size_limit` (90,000 for most TRMNL models, 750,000 for the TRMNL X, none for most third-party clients). The firmware table overrides it where a board's build is known to differ, as the DIY kit's is. A device whose model is unknown gets 90,000.

- Every stored image's size is known: the image store records it when the image is written, and each render row keeps it.
- `/api/display` checks the size against the requesting device's limit every time it hands out an image, not only when the image is rendered. One render can be shared by devices with different limits, and a device's model can change after a firmware update.
- An image over the limit is never handed out. The device gets a small "too large" screen with the image's size, the device's limit and a hint to try a palette with fewer grays, and a short `refresh_rate`. A device would otherwise fail the download on every wake and keep showing what it had. Naming the recipe on that screen is not built.
- Not built: a recipe page that shows its renders' sizes against the smallest limit among the devices that show it, and warns past 80%, before a device ever fails. Today `render_image.bytes` holds the sizes and the check runs at `/api/display`.

Typical screens are far below either limit: 2 to 12 KB for the recipes tested. Size grows with dithering, which defeats PNG's compression, and with more levels per pixel, so dithered photos at 16 grays or in color on a device without PSRAM are the case to watch. A plugin instance set to a smaller palette is the fix a person chooses; the server does not quietly fall back to one.

**Gray calibration.** The registry's gray palettes assume evenly spaced levels, and no measured brightness for any panel's levels has turned up. `trmnl-byos calibrate` shows a device a screen where each middle gray sits over black-and-white ordered dither patches with 1 to 15 of every 16 pixels black. The patch that matches from arm's length measures the level to about 1/16. `-dot 2` draws the dither with 2x2-pixel dots, to check whether the panel blurs isolated single pixels and skews the result.

The OG DIY kit measured 2/16 and 6/16 of white's light for its dark and light grays. Evenly spaced light would be 0.33 and 0.67, so the panel's grays are not evenly spaced in light. They are within measurement error of evenly spaced sRGB values (`#555555` and `#aaaaaa` give off 0.09 and 0.40), so TRMNL's palette is right if read as sRGB.

The server draws its own screens as ink coverage and maps each pixel to the level whose measured light is nearest the light the pixel should give off. Measured levels are kept per firmware model; a model not yet measured is assumed to be evenly spaced in sRGB. For the OG this puts glyph edges with 94% coverage or more at black, 75 to 94% at dark gray, 31 to 75% at light gray, and less at white. Blending in sRGB values and rounding, as before, made edges darker and text bolder than the font. Recipes are quantized by trmnlp and are not affected.

**Non-TRMNL clients.** KOReader, Kindle, Kobo, Nook, Android and Raspberry Pi clients speak the same `/api/display`. They may omit firmware headers such as `Update-Source` or the battery fields, so every header is optional.

**Joining and leaving.**

- A time-limited join window (default 10 minutes) admits devices; otherwise setup gets a "not registered" response. Until the admin UI has a join control, `trmnl-byos join` and `PUT /api/v1/join` open it.
- A known MAC needs the window too, since anything on the LAN can see a MAC. A device that lost its credentials re-pairs while the window is open and keeps its friendly ID and settings. Each setup issues a new key and retires the old one.

**Not built.** These are designed and nothing implements them yet:

- Deleting a device. There is no command or route, and no tombstone. The design is that deleting a device makes its next request return `reset_firmware: true`, so it forgets its credentials, its Wi-Fi settings and its server URL and needs the setup portal again. Since an unknown MAC is never reset, delete would keep the device's MAC as a tombstone until then. Until it exists, the rule that a deleted device is reset has no case it applies to, and only a replaced key gets `reset_firmware`.
- A MAC blocklist that rejects specific devices outright.
- A per-device profile override (step 1 above), and with it the 4-color OG on firmware 1.8.10 and older.
- Per-device settings: time zone, locale, button command, sleep window and `special_function` handling.
- Sensor readings. `SENSORS` is not stored.

## Scheduling and pre-rendering

Every screen a device might ask for is rendered before it wakes, and an unchanged screen costs the device one short request and no repaint.

**What the firmware gives us.** If `filename` matches a file in its flash cache, the firmware skips the download. If that image is already on the panel, it also skips the repaint (`displayed_image.h`). It reports the previous wake's duration in `Wake-Time`, which `trmnl-byos devices` and the JSON API show. A dashboard that plots it is not built.

**Rules.**

1. `filename` is the first 16 hex digits of the PNG's SHA-256. Same pixels, same name. The firmware keeps at most 31 characters of a filename as-is.
2. `/api/display` is a SQLite lookup plus a static file. If nothing is ready it shows a system screen: "Rendering" with a 60 second `refresh_rate` before an instance's first render, "Could not render" with the error and the retry rate after a failed first build, and "Out of date" once the last good render is past its stale limit (see Staleness). A device with no playlist items gets a "Connected" screen. A device that has items but none scheduled now gets "Nothing scheduled now" (see Playlists).
3. `refresh_rate` starts at the `refresh_seconds` setting (default 15 minutes) and is shortened to the seconds until the current playlist item ends, to the next schedule change (the next time a playlist or item schedule opens or closes in a way that changes what the device shows, searched up to the rate it would otherwise send), to 60 seconds while a first render is pending, to the retry rate after a failure or "Out of date", to the end of a calibration override, or to the short rate of a "too large" screen. The device chooses at the wake plus the 30 seconds a wake may come early, so the search for a change starts there, and the device is woken at the change itself, as at the end of an item. The rate is rounded up to a whole second. Not built: aligned wakes a plugin asks for (on the minute, on the hour) and sleep-window edges.
4. Next wake = last request + the `refresh_rate` we sent. A render is built ahead of the wake that would show it, with a lead time that covers two build durations (the target's last one, 30 seconds if none, and the same again for a build already running when it becomes due), the 30 seconds a wake may come early, and the scheduler's 30-second check interval.
5. Not built: each device always has three screens ready, current (for button and power-on wakes), next (for skip and touch-bar next) and a short history (for rewind). There is no `device_screen` table. A device gets the newest render when it wakes.

**Renders follow data, not wakes.** A plugin instance re-renders only when its `refresh_interval` comes due or its data changes. Wakes in between get the same filename. This matters because 8 of the top 25 recipes print the time or use randomness, so their output differs on every render.

**Render cache key.** (plugin instance, device, width, height, palette), and one build makes all four layouts. For most recipes the device is 0, so two OGs showing the same weather share one render. A recipe with any file that contains `trmnl.device` (markup, a polling URL in `settings.yml`, or a transform) gets one render per device instead, built with that device's values. The scheduler scans an installed recipe's files once, since they never change, and remembers the answer in memory by recipe ID. The device API looks up the device's own render first and falls back to the shared one. The `render` table has no foreign key on `device_id`, because 0 is no device, so a trigger deletes a deleted device's renders and their images. An older database whose `render` table has no `device_id` has both render tables dropped at startup. The scheduler builds them again.

**What is built.** The scheduler (`internal/scheduler`) scans for due targets, and sleeps 30 seconds only when a scan builds nothing. It looks at each instance in a playlist linked to a device, at the size and palette that device needs. A device's next wake is its `last_seen` plus the `refresh_rate` we sent. The playlist it shows then is the one chosen at that wake plus 30 seconds (see Playlists), and the item is what the playlist cursor gives for that time with no button press, with the items that are hidden or off schedule then skipped. That item's targets are wanted at that wake. A per-device target is wanted only by its own device, and a shared one by every device that will show it. A target is due when:

- it has never been rendered;
- its last build failed and its backoff has passed: 5 minutes after the first failure, doubling with each failure in a row, to at most an hour. `render.failures` counts them and a success resets it. A failure to reach the renderer (`render.ErrUnavailable`: a refused or reset connection, a timeout, a busy shim, a 5xx) is recorded but not counted, because it says nothing about the recipe. Otherwise every target would sit at the one hour wait after a long outage, and devices would stay stale for up to an hour after the renderer returned. While a failure is the latest result, nothing else makes it due;
- it is wanted at a wake at which its last good render would be older than the recipe's `refresh_interval`, and that wake is less than the lead time away (or already past);
- its last good render is two refresh intervals old, wanted or not. This bounds the age a button press can reach, and keeps every render younger than the age at which a device shows "Out of date", so scheduling never causes that screen;
- no device showing it has woken yet and the refresh interval has passed;
- its instance's data is newer than what the last build read. Each render records the `updated_at` of the `instance_data` its build read, as `render.data_at`, and the data is newer when its `updated_at` is greater. A push always raises `updated_at`: one in the same second as the stored value is stored one second later, so a push after a build read the data is never mistaken for it. A failed build is retried after its backoff whatever the data, so a push waits for that. The 30 second check and the push rate limit coalesce bursts of pushes, so there is no other delay.

Each scan builds one target, the one wanted at the earliest wake, and then scans again at once if it recorded an outcome, so a target that becomes urgent while others are queued is built before them. Targets due only by the last two rules go after those. A scan reads every `render` row in one query (without the logs). The delay before a due target's build starts is the build in progress plus a scan, which is what the lead time allows for. If a scan or the write of an outcome fails, the scheduler waits for the next check instead of spinning. A failed build records its error and log and keeps the last good images. A target that cannot be built at all, such as one whose recipe directory is missing or unreadable, is recorded the same way without calling the renderer, with the reason as its error. Its devices show "Could not render", it is retried with the same backoff as any failure, and the other targets still build. A build that takes over 5 minutes is canceled and is recorded as a failure that does not count toward the backoff, like any timeout. Pruning keeps every render of an instance whose recipe could not be scanned in that pass, since it is not known which of them are per device.

Before it builds, the scheduler deletes every `render` row that matches no target: an instance that left every playlist, or a size or palette that no device has now (a device of an unknown model that reported a new size, say). The images in `render_image` go with the row by cascade, so image expiry can delete them. A target is every item of every playlist a device is linked to, except the hidden ones, at the size the device last reported, so a device never loses a render it could show at its next wake. A hidden item's renders are deleted, and built again when it is shown again. An item that is only off schedule keeps its renders, so they are ready when its window opens. A removed instance that is added back is built again. Rule 5 (a current, next and history screen per device in `device_screen`) is not built: a device gets the newest render when it wakes. Aligned wakes and sleep windows are not built either, so a wake time is only what we sent.

**Staleness.** A device shows an item's last good render until it is older than two refresh intervals plus an hour. After that it shows "Out of date" with the render's age in whole hours ("over 3 hours") and its last error, and wakes again in 5 minutes. The age is coarse so the screen, and its filename, stay the same from wake to wake and the device does not repaint. The server remembers the image name of at most 256 system screens, and forgets them all past that, which costs one redraw each. Before an instance's first render it shows "Rendering", and wakes in a minute, or "Could not render" with the error. A render whose image is missing from the image store is treated the same as one that does not exist yet: the device API deletes the row, if it still names that image, shows "Rendering", and the scheduler builds the target again at its next check. A configurable age, and skipping the item instead, are later work.

## Playlists, mashups and input

Rotation is time-based and tracked per device: each item shows for its own duration, and two devices on one playlist never steal each other's turns.

**Model.**

- A device has one or more playlists, tried in priority order. A link's priority is the playlist's index in the device's order, 0 to n-1 with no gaps and unique per device, so 0 is tried first; linking, moving, unlinking and deleting a playlist keep it that way. A playlist has an optional schedule. The first playlist that is scheduled now and has at least one item to show wins, so an unscheduled playlist at the lowest priority is the default, and a scheduled playlist whose items are all hidden or off schedule falls through instead of blanking the device. If none qualifies, the device shows the "Nothing scheduled now" screen.
- A playlist item is either one plugin instance at full layout, or a mashup: a layout (left/right, top/bottom, quadrants) with a plugin instance per slot.
- Each item has a duration, or none. An item with no duration stays until a button or touch-bar press, with a long `refresh_rate`.
- Items carry their own schedule and a hidden flag. A hidden item, or one whose schedule is not active, is skipped. Items can be moved to another index in their playlist.
- **Schedule.** One shape serves playlists and items (the original design gave playlists only days and a window). `days` is a bitmask, bit 0 Sunday to bit 6 Saturday, 127 for every day, and 0 is invalid. `window_start` and `window_end` are minutes after local midnight, 0 to 1439. Equal means all day. An end before the start wraps past midnight and belongs to the day it starts on, so a Friday 22:00 to 06:00 window covers Friday 22:00 to Saturday 06:00 when Friday is in `days`. `starts_at` and `ends_at` are Unix seconds, 0 for unbounded, and the schedule is active in [`starts_at`, `ends_at`), with `starts_at` before `ends_at` when both are set. The default schedule is always active. Days and windows are read on the wall clock in the server's time zone (`TZ`), so a window follows a daylight saving change. The server's time zone is `TZ`, or UTC when `TZ` is unset, and the server refuses to start on a name it does not know. Recipes render in it, schedules are read in it, and the web UI and the CLI show and take dates in it, so all of them agree.
- **Choosing.** `playlist.Choose` evaluates schedules at the wake plus 30 seconds, the same slack the cursor uses for the end of an item, so a timer wake a little early does not choose the old playlist and sleep for a few seconds. The scheduler evaluates at its predicted wake plus the same slack. `playlist.NextChange` finds the first time after that at which the chosen playlist, or which of its items are shown, changes. It tries each schedule's window starts and ends for the local days involved, local midnights, `starts_at` and `ends_at`, and the times the UTC offset changes, and returns the first that gives a different choice, so a midnight between two listed days or a window that opens in a playlist that is not chosen is not a change.
- **Nothing scheduled.** A device that has playlist items but no playlist that is scheduled and has an item to show gets a screen with "Nothing scheduled now", its friendly ID and "None of this display's playlists is scheduled for now." The text is constant, so the image name is too, and its `refresh_rate` is the usual one shortened to the next schedule change. A device with no items still gets "Connected".

**Cursor.** Stored per device as (current item, started at), in `device_cursor`, a table of its own. A device that has no cursor has no row. `device_state` holds only what the device reported, so a row there means the device has woken, and a cursor never creates one. When a wake finds the duration elapsed, the cursor moves to the next item and the new start time is the old start plus the duration, so timing does not drift. A timer wake up to 30 seconds early counts as the end of the item, since deep-sleep clocks drift, so items last at least a minute. After a long sleep, whole turns through the playlist are skipped at once. A cursor on an item that was deleted, or is no longer in the device's playlist, starts the playlist over at the first item that is shown. A cursor on an item that is now hidden or off schedule moves to the next item that is shown, starting now, as does a press. Whole turns sum only the durations of the items that are shown. `refresh_rate` is the device's usual rate, or the time left in the item or until the next schedule change if that is shorter.

**What is built.** Playlists (`internal/playlist`): a device shows the first of its linked playlists that is scheduled now and has an item to show, items are full-layout instances with a duration or none, and a press (`button`, `EXT0` or `EXT1`) moves to the next item that is shown. Playlist and item schedules, hidden items and reordering are built. Mashups, the sleep window, `special_function`, and per-device button commands are not built yet. Playlists are edited with `trmnl-byos playlist add|remove|list|move|set|schedule|create|rename|delete|link|unlink`. `create NAME`, `rename NAME NEW` and `delete NAME` manage a whole playlist (names are trimmed, 1 to 64 bytes, unique, and `delete` removes its items and links), where `remove ITEM` removes one item. `link NAME DEVICE [-priority N]` links a device last, or moves an existing link to index N, and `unlink NAME DEVICE` removes the link. A link and an unlink renumber the device's priorities. Adding an item for a device links it last only if it is not linked, and never changes the priority of an existing link. `move ITEM INDEX` moves an item. `set ITEM` changes only the flags given of `-duration`, `-hidden`, `-days mon,tue`, `-window 09:00-17:00` (or `all`), `-from YYYY-MM-DD` and `-until YYYY-MM-DD`, where both dates are inclusive days in the server's zone and an empty value clears the bound. `schedule NAME` takes the schedule flags for a playlist. `list` shows every playlist's schedule and each item's index, schedule and hidden flag. Positions are an item's index, 0 to n-1 with no gaps, which a move and a delete keep and migration step 4 made true of existing rows. Migration step 5 did the same for priorities and added the unique index.

**Input (planned, mostly not built).** The headers say what woke the device; we never guess from timing. Built today: `playlist.Advance` treats any press (`button`, `EXT0`, `EXT1`) as next, and a timer wake advances when the duration has elapsed. The rest of this table and the paragraphs after it are design only.

| Signal | Source | Action |
| --- | --- | --- |
| `Update-Source: timer` | Scheduled wake | Advance if the item's duration elapsed |
| `Update-Source: button`, `EXT0` or `EXT1` | Single press. The value is the ESP32 wake source, so it depends on the board: the OG DIY kit sends `EXT0`. | The device's configured command: next (default), previous, first, or refresh |
| `special_function` header | Double click, or the TRMNL X touch bar | `rewind` shows the previous screen from history, `restart_playlist` jumps to the first item; others as configured |
| Touch bar right | TRMNL X | Next |

Planned: the `special_function` value in our responses is a per-device setting, sent on every response as the docs require. Today it is always `none`, and the request header is not read.

**Sleep window (not built).** Per device, for example 23:00 to 06:30: show a sleep screen and set `refresh_rate` to the window's end.

**Mashups (not built).** Only the `playlist_slot` table and `playlist_item.layout` exist, and `AddItem` writes slot 0 and layout `full`. The plan: the framework renders a mashup as one page with mashup classes. trmnlp cannot put several recipes on one page: it renders each of a recipe's half and quadrant layouts on a full-size page, inside a one-slot mashup in the slot's position (the half-horizontal layout fills the top half). So the server composes a mashup by cropping each slot from its recipe's render and placing it, which keeps the framework's slot frame and spacing.

## Plugins and rendering

The plugin format is the TRMNL recipe, unmodified, and the renderer is trmnlp driven through a small shim; the Go server never parses Liquid.

**Recipe format.** A directory or the zip from `trmnl.com/api/plugin_settings/<id>/archive`: `settings.yml`, `full.liquid`, `half_horizontal.liquid`, `half_vertical.liquid`, `quadrant.liquid`, optional `shared.liquid` and `transform.{js,py,rb,php}`. Built: a local directory or zip, installed with `trmnl-byos recipe add` or `POST /api/v1/recipes`, which can be written locally and previewed with `trmnlp serve`. Not built: installing from the TRMNL gallery, the community catalog (`bnussbau/trmnl-recipe-catalog`) or a git URL.

**What the top 25 recipes need** (survey of trmnl.com by popularity):

| Need | Count | Handled by |
| --- | --- | --- |
| Polling strategy | 23 | trmnlp polls during `build` |
| `polling_url` is Liquid, sometimes several URLs (`IDX_0`, `IDX_1`) | 3 multi-URL, many templated | trmnlp |
| Transforms (4 JS, 1 Ruby, 1 Python) | 6 | trmnlp image ships Node, Python, Ruby, PHP |
| JavaScript in the page (3 load Highcharts) | 6 | Browser in the trmnlp image |
| Pinned framework version (7 distinct versions) | 24 | Asset cache |
| `trmnl.device.*` in markup or URLs | 3 | Per-device build with `variables.trmnl.device` |
| Webhook strategy | 1 | Our webhook endpoint writes the data file |
| OAuth | 0 | Deferred |

**Render path.**

1. A recipe is installed into `recipes/<id>/` under the data directory (its row and the rename of its directory are one transaction, so a failed install leaves no row without a directory). A crash between the rename and the commit leaves a directory with no row, and row IDs are reused, so `serve` sweeps at startup, before the scheduler: it removes each `recipes/<id>` directory with no row, inside a write transaction so it cannot run between another process's rename and commit, and removes `.install-*` and `.upload-*.zip` in the recipes directory and `.upload-*.bin` in the firmware directory that are over an hour old, since a CLI install may be running. A zip member that fails its checksum is refused as invalid input (400). Each refusal says why in a plain sentence, without a Go error text in it: that the file is not a zip archive, is empty, has no `settings.yml` (or no `full.liquid`) where one is looked for, has a file name that leaves its directory, has too many files or is too large once unpacked, has a damaged member (named), or has a `settings.yml` that is empty or not YAML. A recipe whose `settings.yml` has no `name` is called by the name of the uploaded file without its directory (either kind of slash) or extension, which is only text and never reaches the filesystem, and by `upload` when the file has no usable name, as the API's upload does, since it has no file name. The source is a directory or zip that holds `settings.yml` at its top, in `src/`, or inside one top-level directory. Hidden files are skipped, names that would leave the directory are refused, and the server reads only `name` and `refresh_interval` from `settings.yml`; trmnlp fills in field defaults itself. For each build the server streams a project: the recipe's files under `src/`, and a `.trmnlp.yml` with the time zone (from `TZ`), the asset overrides, and each custom field set to `{{ env.FIELD_n }}`. The values travel in the build job. trmnlp runs every `custom_fields` value through Liquid, so a value written into the file would have any `{{` in it run as Liquid. For a recipe that reads `trmnl.device`, the `.trmnlp.yml` also has `variables.trmnl.device` with the device's `friendly_id`, `percent_charged`, `wifi_strength`, `firmware_version` and `refresh_interval_seconds`, from the device row and its state at its last wake. trmnlp does not run `variables` through Liquid, so these values go in the file as typed YAML and are only data. The MAC and api key are never passed. A field the device has not reported is left out, so trmnlp's placeholder shows. `percent_charged` and `wifi_strength` are estimates from the battery voltage (3.0 to 4.2 V) and RSSI (-100 to -50 dBm), not TRMNL's own conversion. The width, height and model come from the build's size, as before. Locale is not passed yet.
2. A script pushes data to an instance with `POST /api/custom_plugins/{id}` (see Inbound data). The server stores it in `instance_data`, and each build writes it into the project's `.trmnlp.yml` under `variables`, at the top level next to `trmnl`, which is trmnlp's documented override for user data. It is written as typed YAML, so a string with `{{ }}` or a newline stays a literal string, and a number stays a number. A pushed `trmnl` key is refused, so the server's own `variables.trmnl.device` cannot be overwritten. trmnlp's own webhook store is a cache file keyed by a hash of the project path, which is an internal detail to stay away from. Tested with Top Hacker News Story: a `stories` list under `variables` reached the templates as a top-level merge variable, next to the usual `trmnl` variables. An instance of any recipe may have data, not only one with `strategy: webhook`. trmnlp polls nothing for a webhook recipe, and its `refresh_interval` rules apply as for any other.
3. When a render is due, the server streams the project to the shim, which runs `trmnlp build --png --width W --height H --palette P` on it and streams the PNGs back. One build polls once and renders all four layouts; there is no option for one layout. A build runs in a process group of its own, and the whole group is killed when it ends or times out, since trmnlp starts Firefox and geckodriver. The container runs with an init process, which reaps what Firefox leaves behind.
4. The server stores the PNG under its content hash. Updating each affected device's ready screens (`device_screen`) is not built.

**Settings.** An instance has a name, field values, secret values and a palette. Not built: forms generated from `custom_fields` (`select` incl. multi-select, `string`, `number`, `lat_lon`; `author_bio` is metadata only), per-device time zone or locale overrides, dark mode, icons and framework version pins. The time zone is the server's `TZ`, and locale is not passed.

**Renderer interface (not built).** There is no `Render(job) -> PNG` interface and no static-image backend. The scheduler calls the shim client (`*render.Client`) directly, and `internal/screen` draws system screens in process. A pluggable interface is the plan, so other backends can be added. An image-URL backend (fetch a PNG such as a Grafana panel, quantize in Go) and a native Ogre backend are deferred.

## Firmware updates

The server is the only thing preventing a wrong-board update, so it offers a firmware build only to devices whose exact firmware model it was built for, and only when asked to.

**Why it has to be the server.** The firmware's OTA path (`firmware_update.cpp`) is `esp_ota_begin` / `esp_ota_write` / `esp_ota_end` with no board check. ESP-IDF's image verification rejects a different chip family (C3 vs S3) but not a different board on the same chip, and app rollback is disabled in the TRMNL X config we checked. A TRMNL X build sent to a XIAO S3 kit would install, boot with the wrong pins, and might need USB to recover.

**Rules.**

1. Each catalog entry names exactly one firmware model (the `Model` header value, e.g. `xiao_epaper_display`), a version, and a SHA-256. No wildcards, no "latest" URL. A model is in the catalog only if it is in the chip table below.
2. On upload, parse the ESP image header (chip ID) and the app description. Reject the file if the chip does not match the model's chip, the image structure or checksums do not hold, or the version disagrees with the entry.
3. Offer an update only if the catalog version is newer than the device's `FW-Version`, the device has updates enabled, and the device is not pinned. Never downgrade or offer the same version. A device version that is not dotted numbers gets no offer.
4. Roll out to one device first. Offer to the others only after that device checks in reporting the new `FW-Version`, or a newer one if it was flashed past the build.
5. Serve only the exact stored file; `firmware_url` points at our server with the hash in the path.

**What is built.** `internal/firmware` holds the catalog, the image checks and the offer rule. The device API offers and serves, and the commands and the admin API manage.

- **Catalog.** `trmnl-byos firmware add -model M [-version V] FILE` streams the file to a hidden temporary file in `firmware/` under the data directory while hashing it, checks it there, and renames it to `firmware/<sha256>.bin` only when every check passes. A refused upload leaves no file and no row. The upload is bounded by the model's app partition. The `firmware` table has one row per file, and a model has one build per version, so two different files cannot claim the same version. `firmware list` shows each build and its rollout, with the canary's version and the time the canary was last offered the build (`LAST OFFERED`, `-` for never), which shows a canary that was told to update and did not.
- **Image checks.** The file must be an ESP app image as `esp_image_format.h` lays it out: magic byte 0xE9 and at most 16 segments, every segment inside the file, the 8-bit checksum, the appended SHA-256 when the header says there is one, and the app descriptor (magic 0xABCD5432) at the start of the first segment. The header's `chip_id` must be the model's chip. This catches a truncated or corrupt upload and a merged flash image (which starts with the bootloader, not an app descriptor). It does not catch the case that matters most, a build for another board on the same chip, since nothing in the image names the board. Only the model the uploader gives does.
- **Version.** The app descriptor's version string is the ESP-IDF project version, which the TRMNL build does not set, so it is a git description, a commit hash or `1`, not the `FW_MAJOR.FW_MINOR.FW_PATCH` the firmware reports in `FW-Version` (that is compiled in from `include/config.h`). So the uploader gives the version, from the release's file name, and it must be dotted numbers with up to four parts. When the descriptor does hold a plain `X.Y.Z`, with or without a `v`, the given version must equal it, and it can be left out. A wrong version is not caught otherwise. A file labelled newer than it is would be installed, report its real version, and be offered again until the offer cap stalls the rollout.
- **Trusted Model header.** Before firmware 1.8.17 the firmware took `Model` from a board macro that two builds shared: `og` was the OG and the 4-color OG, `xiao_epaper_display` was the DIY kit and its 3-color variant, and `x` was two TRMNL X boards (checked at the 1.8.16 tag). A device on firmware older than 1.8.17 may be a different board than its `Model` says, so it is never offered firmware and cannot be a canary. Update such a device by USB or through TRMNL once.
- **Offer.** `/api/display` sets `update_firmware: true` and `firmware_url` (`<base>/firmware/<sha256>.bin`, built like image URLs) only when the build is for exactly the `Model` and `FW-Version` the device sent on this request (a missing header is never filled in from the stored value, though the stored copy keeps its old value when a header is missing); its rollout is active; its version is newer; the device has `updates_enabled`, no `fw_pin`, a firmware version of 1.8.17 or newer, and a model in the chip table; the file is on disk at the size recorded; and either the device is the rollout's canary or the canary now reports a matching model and the build's version or a newer one. Of several builds that qualify the newest is offered. The image and `filename` are still sent, so the device shows a screen if the update fails.
- **Offer cap.** The version label cannot be verified from the binary, and an OTA can fail for other reasons, so a device that does not take a build would be told to update on every wake. `firmware_offer` counts the offers of each build to each device, incremented in the same transaction that decides the offer. A build is offered to a device at most `MaxOffers` (3) times: one try and two retries for a dropped download. After that that device is not offered that build again. A device that reports the build's version is simply no longer older.
- **Stalled rollout.** When the canary has been offered the build 3 times and still does not report its version, the rollout is `stalled`: nothing is offered to anyone. `firmware list` and the API show it with the canary and the version it reported. Restart it with another canary, or with the same canary, which resets that device's count (the admin's explicit retry), or cancel it. Device delete is not built. Once it exists, the `firmware_offer` foreign key to `device` cascades, so a deleted device's counts go with it.
- **Canary.** A rollout starts with `firmware rollout <id> -canary DEVICE`, and refuses a canary that could not take the build now (another model, updates off, pinned, already on that version or newer, firmware too old). Until the canary reports the build's version, only it is offered the build. A canary that does not come back on that version leaves the rollout at the canary stage, which `firmware list` shows as `canary` (or `stalled`, below). The other devices keep running their firmware. Pick another canary with `firmware rollout` again, or `firmware cancel <id>`. Choosing another canary closes a rollout that was open until that device reports. A canceled rollout is offered to no one and its file is no longer served. Device delete is not built. Once it exists, `firmware_rollout.canary_device_id` is `ON DELETE CASCADE`, so deleting the canary would delete its rollout row, not cancel it.
- **Device policy.** `updates_enabled` is off for every device until `firmware device -updates on DEVICE`. `-pin VERSION` keeps a device where it is and `-pin none` clears the pin. The same two settings are in `PATCH /api/v1/devices/{id}`.
- **Per-device standing.** `firmware.DeviceOffers` lists the devices of a build's model by friendly ID, each with whether `/api/display` would offer it the build now and, if not, a `Reason` code and a sentence: `updates_off`, `pinned`, `wrong_model`, `bad_version`, `old_firmware`, `not_newer`, `bad_build`, `no_rollout`, `canceled`, `stalled`, `waiting_for_canary`, `cap_reached` or `file_missing`. It also says which device is the canary and which could be the canary now (`can_be_canary`: the device half of the rule passes). It shares the offer decision with `/api/display`: both call the same device half (`notEligible`) and rollout half. It decides on the model and version the device last reported, which are what its wakes send. A wake that leaves out `Model` or `FW-Version` is offered nothing (see Offer), even when the listing says the device would be offered the build. It takes one query for the build, one for the devices with their offer counts, and one stat of the file, however many devices there are. `GET /api/v1/firmware/{id}/devices` returns it.
- **Download is not authenticated.** The firmware's OTA code (`firmware_update.cpp`) fetches `firmware_url` with a bare `HTTPClient::GET()`, or `g_modem->httpGet` with no headers on the TRMNL X's modem path. It sends no `Access-Token` and no `ID`, so the route cannot ask for either, and `GET /firmware/{sha256}.bin` is open, like `/images`. It is limited to what a rollout names: the hash must be 64 lowercase hex digits (checked before the filesystem), the build must be in an active rollout, and the file must be the size recorded. It is served with `http.ServeContent`. The file is not secret, but a LAN client that knows a hash can fetch it while the rollout lasts. The firmware's `_firmwareUrl` buffer is 1024 bytes, which our URLs are well inside.
- **Failure behavior.** A failed OTA is recorded by the firmware, which will not try again for 24 hours and keeps running what it has. A successful one reboots. The firmware has no rollback and no board check, so everything above is about not sending a bad file.

**Chip table.** It is `Models` in `internal/firmware/models.go`, built from usetrmnl/trmnl-firmware at commit 744a955 (`platformio.ini` and `boards/`). Each model's PlatformIO environment gives its `DEVICE_MODEL` string, its board (hence the chip) and its partition table (hence the largest image the model can take). Chip IDs are from ESP-IDF's `esp_chip_id_t`: ESP32-C3 0x0005, ESP32-S3 0x0009, ESP32-C5 0x0017.

| Chip | Models | App partition |
| --- | --- | --- |
| ESP32-C3 | `og`, `og_4clr`, `xteink_x3` | 1,900,544 bytes (0x1D0000, `min_spiffs.csv`) |
| ESP32-C5 | `og_gen2` | 2,621,440 bytes (0x280000, `trmnl_gen2_partitions.csv`) |
| ESP32-C5 | `og_gen2_4clr` | 3,145,728 bytes (0x300000, `trmnl_x_partitions.csv`) |
| ESP32-S3 | `x`, `lilygo_t5pro`, `m5_papers3`, `m5_paper_color`, `seeed_sticky`, `reterminal_e1003`, `reterminal_e1004` | 3,145,728 bytes |
| ESP32-S3 | `reterminal_e1001`, `reterminal_e1002`, `waveshare_397`, `xiao_epaper_display`, `xiao_epaper_3clr`, `xiao_epaper_6clr`, `xiao_epaper_mini` | 1,900,544 bytes |

Left out, so they cannot have firmware added: `xteink_x4` (two builds share the name, one for a push-to-refresh power button), `trmnl_steam` (a development build), `crowpanel42` and `m5_paper_mono` (no partition table is set in the source, so the board default applies, which it does not state), `sensoria_s3` and `sensoria_c5` (not confirmed against a current environment), and the generic `seeed_esp32c3`, `seeed_esp32s3` and `waveshare` names. A model that is in the source under a different name than an older device reports gets no offer, since the name has to match.

**Sources.** Builds you upload. Official releases synced from TRMNL on a schedule (as Terminus does), going through the same checks, are not built yet. Taking `update_firmware`/`firmware_url` from the TRMNL cloud via the deferred cloud proxy is another option, since the cloud already picks per device.

## Data model

One SQLite file (WAL mode) holds all state; rendered PNGs and recipe sources live on disk next to it, addressed by hash.

**What is built.** The tables in `internal/db/schema.sql`:

| Table | Key fields |
| --- | --- |
| `device` | mac, api_key_hash, friendly_id, name, fw_model, fw_version, created_at, updates_enabled, fw_pin, screen_override, screen_override_until |
| `device_state` | device_id, last_seen, last_refresh_rate_sent, last_image, battery_v, rssi, wake_time_ms, update_source, width, height. A row exists once the device has called `/api/display` |
| `device_cursor` | device_id, item_id, item_started_at (the playlist cursor; deleted with the device) |
| `device_log` | device_id, received_at, device_at, level, message, detail (the entry as sent, with wake reason, heap and Wi-Fi state); the newest 1000 per device are kept. Written by `POST /api/log`, and no command or route reads it yet; read it with `sqlite3` |
| `recipe` | id, name, source (the path installed from, or `upload`; the web UI shows `Uploaded` or the path's last element, with the whole path as a tooltip), refresh_interval, installed_at |
| `plugin_instance` | recipe_id, name, field_values, secret_values (sealed), palette (empty for the device's own), created_at |
| `instance_data` | instance_id, data (the merged webhook JSON object), updated_at (rises with every push) |
| `playlist` | name, and its schedule: days, window_start, window_end, starts_at, ends_at (see Playlists) |
| `playlist_device` | playlist_id, device_id, priority (the playlist's index in the device's order, 0 to n-1 with no gaps, unique per device) |
| `playlist_item` | playlist_id, position (the item's index in its playlist, kept dense), layout, duration, hidden, and the same schedule columns as `playlist`. `layout` is always `full` and nothing reads it, since mashups are not built |
| `playlist_slot` | item_id, slot, instance_id. `slot` is always 0 until mashups, which put several instances in one item |
| `render` | instance_id, device_id, width, height, palette, rendered_at (last good build), attempted_at, duration_ms, error, log (latest build), data_at (the instance data's `updated_at` the latest build read), failures (builds failed in a row) |
| `render_image` | render_id, layout, image, bytes |
| `firmware` | fw_model, version, sha256 (lowercase hex, also the file name), chip, idf_version, bytes, source, uploaded_at; one build per model and version |
| `firmware_rollout` | firmware_id, canary_device_id, state (active or canceled), started_at |
| `firmware_offer` | device_id, firmware_id, offers, last_offered_at. `last_offered_at` is shown as the canary's last offer time in `firmware list` and the API |
| `api_token` | name, token_hash (SHA-256), created_at, last_used_at (0 until used) |
| `setting` | key, value, updated_at. The join window (`join_until`), the admin password hash (`admin_password_hash`, a PHC string), and the settings an admin can change (`refresh_seconds`, `min_password_length`; see Admin settings) are kept here |
| `admin_session` | token_hash (SHA-256), created_at, last_seen_at, expires_at. One row per signed-in browser |

**Not built.** These tables and columns are designed and do not exist:

| Table or column | What it is for |
| --- | --- |
| `device`: profile_override, palette_override, time_zone, locale, button_command, special_function, sleep_start, sleep_end, blocked | Per-device overrides and settings, and the MAC blocklist. Device delete would add a tombstone. |
| `device_state`: battery_pct | Battery as a percentage. |
| `sensor_reading` | device_id, at, kind, value, unit (latest kept per kind, plus history). |
| `model` / `palette` | Vendored registry rows. The registry is embedded in the binary as JSON and is not in the database. |
| `recipe`: source_ref, source_hash | Updates from a source. |
| `outbound_host` | recipe_id, host, first_seen, last_seen, refused, from the renderer proxy's log. |
| `plugin_instance`: framework_pin, dark_mode, refresh_override, align, stale_after | Per-instance render settings. |
| `instance_data`: last_error, last_success_at | Async polling. |
| `device_screen` | device_id, role (current, next, history n), png_hash. |

Migrations are embedded in the binary and run at startup, in one transaction, so a step that fails leaves the database as it was. The schema version is `PRAGMA user_version`, and `steps` in `internal/db/migrate.go` is the ordered list that takes a database from version n-1 to n. A new database gets `schema.sql` and is stamped with the latest version. A database with a newer version than the server knows is refused. A change to the schema is a new step plus the same change in `schema.sql`, and `TestMigrateMatchesSchema` fails if an upgraded database and a new one differ in tables, columns, indexes or triggers.

Step 1 adopts a database made before versions were numbered, which has version 0 and tables. Those servers ran `schema.sql` at every start and added columns on demand, so step 1 probes what exists: it drops the render tables if `render` has no `device_id`, creates missing tables from a frozen copy of the schema (`baselineSchema`), and adds each missing column in `addedColumns`. It is the only step that probes, and once a database is stamped it never runs again.

**Image store.** A PNG is stored as `images/<first 2 hex digits>/<next 14>.png`. Storing bytes that already exist never rewrites the file. It refreshes the file's modification time, at most hourly, which keeps snapshot churn low on filesystems such as ZFS. An hourly sweep deletes images that no row names and that were last stored more than 24 hours ago. The rows that name images are `device_state.last_image` and `render_image`, and `device_screen` once it exists. Scheduling deletes `render` rows that no device needs, which frees their images. A name handed out from a row is refreshed when it is handed out, since a render can replace the row before the device fetches the image. Before a new file is renamed into place its bytes are synced to disk, because a stored file is trusted by its name and never rewritten. After the rename the directory is synced too (and the store directory, when the rename made a new one), so the name is on disk before a row names it. A filesystem that cannot sync a directory is tolerated. If the directory sync fails, the file is removed and the write fails. The grace period covers an image written before the row naming it commits, and a device that fetches an image a moment after it is told the name.

## Admin UI, API and MCP

One JSON API backs the web UI, scripts and the MCP server, so anything you can click you can also script or ask an assistant to do.

**Web UI** (server-rendered HTML with small progressive enhancements, live updates over server-sent events):

- Dashboard: devices, last seen, battery and wake-time trends, current screen thumbnails.
- Devices: profile, playlist, button command, sleep window, firmware, logs. The list shows each device's current screen, and a join window control opens and closes the window.
- Recipes and instances: install from gallery, catalog, git or upload; settings form from `custom_fields`; the hosts each recipe contacts and any refused requests; preview at any device profile. Built: a page that lists each recipe with its source, refresh interval, install date and instances, uploads a recipe zip, adds an instance with a name and palette, and removes an instance. The page does not install from the gallery, a catalog or git, edit settings from `custom_fields`, delete a recipe, or preview.
- Playlists: items, durations, order, hidden flags, schedules, mashups. Built: the JSON API and the CLI for all but mashups, and a page that lists every playlist with its schedule, devices and items, moves, hides, shows and removes items, edits an item's duration, hidden flag and schedule, edits a playlist's schedule, and adds an item for an instance. The functions and the API to create, rename and delete a playlist, and to link and unlink a device and set its priority, are built, but the page does not use them yet. It does not build a mashup either.
- Firmware: catalog, upload, rollout state.

Pages are `html/template` with hand-written CSS and a small script, embedded in the binary. There is no build step and no framework. The layout is designed for a phone first and widens for a desktop. Times are shown in the server's time zone (`TZ`). It follows the device's light or dark preference. The font stack is the system's own: `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, system-ui, sans-serif`, and `ui-monospace, monospace` for code.

Every color is a CSS custom property, and a palette is one set of them for light and one for dark. The grays are tinted toward the accent, so the screen previews are the only true black, white and gray on a page. The one palette is moss on sage: green-gray surfaces with a moss green accent. A choice of palettes is deferred (see Deferred work).

**JSON API** (`/api/v1`, bearer token). Narrow on purpose; no attempt at parity with Terminus.

**What is built.** The JSON API and its tokens (`internal/adminapi`), the webhook ingest route, and admin password sessions (`internal/adminauth`) with the web UI shell (`internal/adminui`): the login page, a top bar and phone tab bar for Devices, Recipes, Playlists and Firmware, a Devices list with each device's current screen (the image it was last sent, from `/images/`, on a light paper background in both color schemes) and a join window control, and a page per device at `/devices/{id}`. That page shows the screen large, the device's facts and the playlist it would show now (its items that are shown), read-only, and a form for its name, the firmware update switch and its firmware pin, which uses the same checks as `PATCH /devices/{id}`. A device is Online if it woke within twice the refresh interval the server last sent it, Late after that, and Never seen before its first wake. The Playlists page at `/playlists` shows a card per playlist: its schedule in words (such as "Weekdays, 06:00 to 09:00"), the devices linked to it, and its items in order with duration, schedule and a Hidden badge. Each item has small forms to move it up or down, hide or show it, and remove it (a link to a confirm page at `/playlists/items/{id}/delete`, which changes nothing until its form is posted), and a page at `/playlists/items/{id}` for its duration (in seconds), hidden flag and schedule, saved in one `playlist.UpdateItem` call. A playlist's page at `/playlists/{id}` edits its schedule and adds an item for an instance. A schedule form has seven day checkboxes, a window start and end (both empty for all day), and a first and last day, both included and read in the server's zone. Validation is `playlist`'s, as for the API, and a refused form is shown again with the message and the values typed. The Recipes page at `/recipes` has an upload form and a card per recipe: its source (`Uploaded`, or the last element of the path it was installed from, with the whole path as a tooltip), how often it refreshes, the day it was installed, and its instances with their palette (the device's own when none is asked for) and the playlists that show them. The upload is `POST /recipes` as `multipart/form-data` with the zip in the `recipe` field. It is read as a stream and handed to `recipe.InstallZip`, so it has the same checks and the same 8 MB limit as `POST /api/v1/recipes`. The form posts to `/recipes#upload`, so the page that comes back scrolls to the panel. A page whose request has ended, because the client hung up or its deadline passed, writes nothing and logs the failed read at debug level, not as a server error. A refused upload shows the list again with the reason in the upload panel, just above the file field: 400 for no file, an invalid recipe (the reasons above) or an upload that was cut off, 413 with "The file is over 8 MB." for a file over its limit, and 413 with "The upload is too large." when the body is over its limit before the file part, such as a huge field. The body and the file part are read through `upload.Reader`, so a client that drops the connection gets a 400 and is not logged as a server error. A recipe whose `settings.yml` has no name gets the uploaded file's name. A recipe's page at `/recipes/{id}` adds an instance with a name (empty is the recipe's) and a palette from the registry, through `recipe.CreateInstance`. An instance has no settings form, so it gets no fields or secrets. Remove on an instance goes to a confirm page at `/recipes/instances/{id}/delete`, which changes nothing until its form is posted. An instance that a playlist shows has no Remove link on the list, only links to its playlists. Its confirm page, if visited directly, has no Remove button, and a post for it is refused with 409. Firmware is a placeholder page. The other pages of the web UI, the MCP server, `POST /api/screen` and `GET /api/alias` are not built (see below). The API is a thin layer over the packages the commands use (`recipe`, `playlist`, `device`, `apitoken`, `firmware`), so a command and its route share their validation. Their errors wrap one of four kinds from `internal/errkind` (invalid input, not found, conflict, too big), and the API maps each kind to a status in one place, so a new error that wraps a kind needs no change there.

- **Tokens.** `trmnl-byos token add -name NAME` makes 32 random bytes, base64url, and shows them once. Only the SHA-256 is stored, in `api_token`, and a request is looked up by that hash. `token list` never shows a token, and `token remove NAME` revokes one. A request without a known `Authorization: Bearer` token, on any `/api/v1` path, gets 401 with `WWW-Authenticate: Bearer`. `last_used_at` is updated at most once a minute per token. Device routes still take only a device `api_key`.
- **Deadlines.** The server's read and write timeouts (30 and 60 seconds) cover everything but the routes that move a large body. A firmware or recipe upload, and a firmware download, set their own deadlines with `http.NewResponseController`: 10 minutes, so a slow link does not cut them off.
- **Format.** JSON bodies are limited to 64 KB and refuse unknown fields. Responses use real HTTP statuses: 400 bad input, 401, 404 unknown ID, 409 conflict (an instance still in a playlist), 413, 415, and 500 with a generic message and the detail in the log. Times are Unix seconds and durations are seconds.
- **Routes.**
  - `GET /devices`, `GET /devices/{id}` (friendly ID or MAC). Never the `api_key` or its hash.
  - `POST /devices/{id}/calibrate` with `dot` and `for_seconds`.
  - `PATCH /devices/{id}` with `name` (at most 64 bytes, or `""` for none), `updates_enabled` and `fw_pin` (a version, or `""` for none). A field left out is kept. A bad field changes nothing. A device's JSON has all three.
  - `GET /join`, `PUT /join` with `for_seconds` (0 closes it).
  - `GET /settings`, `PATCH /settings` with `refresh_seconds` and `min_password_length`, both numbers. A field left out is kept. Every field is checked before any is written, and the writes are one transaction, so a bad field changes nothing (400). The response is the settings in effect. A stored value that is out of range is a 500, and `PATCH` can repair it.
  - `GET /firmware`, `POST /firmware?model=M[&version=V]` with the raw file as the body, `Content-Type: application/octet-stream`, up to the largest app partition (3 MiB). It is streamed to a temporary file, checked and moved into place like `firmware add`. 400 for a model not in the chip table, a chip that is not the model's, a bad image or version, 409 for a file or version already in the catalog, 413 for a file over the model's partition. `GET /firmware/{id}/devices` (404 for an unknown build) lists each device of the build's model with `device`, `name`, `model`, `version`, `canary`, `offers`, `last_offered_at`, `reason` (a code, "" when it would be offered the build now), `why` and `can_be_canary`. `POST /firmware/{id}/rollout` with `canary`, `DELETE /firmware/{id}/rollout`. A build's `rollout` is null, or has a `state` of `canary`, `open`, `stalled` or `canceled`, the `canary` device, its `canary_version`, `started_at` and `last_offered_at` (when the canary was last offered the build, 0 for never).
  - `GET /recipes`, `GET /recipes/{id}`, `POST /recipes` with a zip as the body, `Content-Type: application/zip`, up to 8 MB. The body is streamed to a temporary file in the recipes directory, installed, and removed. A body that is cut off, because the client hung up or stalled past the deadline, is 400 (`upload.Reader` marks a read error of the request as invalid input), not a 500 and not logged as an error. A failure to write the file to disk is still a 500. A firmware upload is read the same way.
  - `GET /instances`, `GET /instances/{id}`, `POST /instances` with `recipe_id`, `name`, `palette`, `fields` and `secrets`, `DELETE /instances/{id}`. A response has the plain field values and only the names of the secret fields that are set, as `secret_fields`. That is null when the names cannot be read: the server has no secrets key, or the sealed secrets do not open (see Secrets at rest). The list and one instance agree, and one unreadable instance does not fail the list.
  - `GET /playlists`, `POST /playlists/items` with `device`, `playlist`, `instance_id` and `duration_seconds`, `DELETE /playlists/items/{id}`. A playlist has its `id`, `name`, `devices` (each with a `priority`, the playlist's index in that device's order, 0 first) and `schedule`, and an item its `position` (its index), `hidden` and `schedule`. A schedule is `{"days": ["sun", "mon", ...], "window_start": "HH:MM", "window_end": "HH:MM", "starts_at": unix, "ends_at": unix}`, with the days in week order.
  - `POST /playlists` with `name` makes an empty playlist (201, the playlist as `GET` shows it; 400 for an empty, over-long or control-character name, 409 if the name is taken). `DELETE /playlists/{id}` removes a playlist with its items and links (204) and renumbers the priorities of the devices that were linked. `PUT /playlists/{id}/devices/{device}` (a friendly ID or MAC) links a device last, or with a body `{"priority": N}` moves its link to index N (200, the playlist; 400 if N is outside 0 to n-1, which leaves no link made; the body may be empty). `DELETE /playlists/{id}/devices/{device}` unlinks it (204; 404 if it is not linked).
  - `PATCH /playlists/{id}` with `name` and `schedule`. A schedule replaces the playlist's schedule (inside it, a field left out has its default: every day, all day, no dates). `PATCH /playlists/items/{id}` with any of `position`, `duration_seconds`, `hidden` and `schedule`. A field left out is kept. Everything is checked and applied in one transaction, so a request that is refused (400) changes nothing. Both answer with the playlist or item as `GET` shows it, and 404 for an unknown ID.
  - `GET /renders`, `GET /renders/{id}`. The list has each render target's status without its build log. One render's response adds the log.

**Inbound data.**

- **Webhook data.** `POST /api/custom_plugins/{id}` keeps TRMNL's path, so a script written for TRMNL changes its host and ID and adds an `Authorization: Bearer` header. `{id}` is the plugin instance ID. There is no UUID: the API token is the secret, not the URL. `GET` on the same path returns the stored data. The body is `{"merge_variables": {...}, "merge_strategy": "replace" | "deep_merge" | "stream", "stream_limit": N}`, and the response is `{"merge_variables": {...}}` with what is stored. The strategies follow TRMNL's webhook docs (docs.trmnl.com, Private Plugins, Webhooks):
  - `replace` (the default) makes the push the data.
  - `deep_merge` merges objects key by key. Anything else, arrays included, is replaced.
  - `stream` appends each pushed array to the stored array of the same key, and any other value replaces the old one. `stream_limit` is the longest an array may be; the oldest values, at the front, are dropped. TRMNL's docs do not say which end is kept, so this appends and keeps the newest. Zero or no limit means the size limit only.
  - `merge_variables` must be a JSON object, a `trmnl` key is refused, and an unknown strategy is refused, all with 400. TRMNL uses 422.
  - The body is limited to 64 KB (413) and so is the merged result (413), not TRMNL's 5 KB. An unknown instance is 404.
  - Pushes to one instance are limited in memory to a burst of 10 and 60 an hour. Over that is 429 with `Retry-After`. TRMNL's limit is 12 an hour.
  - The stored data is merged and written in one transaction. This is the one place the server decodes JSON into `map[string]any`, since pushed data has no fixed shape. Deleting the instance deletes its data.

**Not built.** Everything below, and the Web UI above, is design only:

- `POST /api/screen`, which would accept a full markup or PNG screen from a script.
- `GET /api/alias/<uuid>?model=<name>`, which would return a plugin instance's current image for any model, for clients outside a playlist (a Home Assistant card, a web page).
- The MCP server.
- Device delete and the MAC blocklist (see Device protocol).

**MCP server** (not built; `/mcp` over HTTP, plus stdio), tools:

- `list_recipes`, `get_recipe`, `create_recipe`, `update_recipe` (markup, settings, transform)
- `render_preview` (instance, layout, model) returning the PNG
- `list_devices`, `get_device`, `list_playlists`, `update_playlist`
- `get_logs` (device, since)

This lets an assistant write a recipe, render it, look at the result and iterate against the real server.

## Security and privacy

The device API is LAN-only and token-based; the admin side needs a login; and recipes are treated as untrusted code that talks to the internet.

- **Admin auth.** Built: bearer tokens, created with `trmnl-byos token add`, and one admin password with session cookies. `trmnl-byos password` sets the password (read twice without echo at a terminal, or one line from stdin) and signs out every session. It must be `min_password_length` to 1024 bytes, and that setting is 12 unless an admin changed it (see Admin settings). It is hashed with argon2id (19 MiB, 2 passes, 1 thread, a 16-byte salt, a 32-byte key) and stored as a PHC string in the `setting` table under `admin_password_hash`. Verification reads the settings from the stored string, so they can change later, and refuses a string that asks for over 64 MiB, 10 passes or 4 threads. One argon2 computation runs at a time, so concurrent logins cannot multiply memory use, and a request that is canceled stops waiting for its turn. `POST /login` opens a session: a 32-byte random token, base64url, in the `trmnl_session` cookie (`HttpOnly`, `SameSite=Lax`, `Path=/`, `Max-Age` 30 days, and `Secure` when the request came over TLS or `-public-url` is https). Only its SHA-256 is stored in `admin_session`. A new token is made at every login, and login always redirects to `/`, with no `next` parameter. A session lasts 30 days from its last use: a lookup that finds it last seen over a minute ago moves `last_seen_at` and `expires_at` forward. Login deletes expired rows, logout deletes its row, and setting the password deletes every row. When no password is set, a failed login says to run `trmnl-byos password`. Otherwise a wrong password gets a generic message. Guessing is limited by the cost of argon2id and one verification running at a time. The default minimum of 12 bytes suits a server only the LAN can reach. A server reachable from the internet should have a much longer password, which the README says and the server does not enforce, since it cannot tell which kind it is. There is no per-IP limit, because addresses are cheap to get. `/api/v1` accepts a valid session cookie in place of a bearer token. A request that rests on the cookie, and `POST /login` and `POST /logout`, go through `http.CrossOriginProtection`, which refuses an unsafe request that `Sec-Fetch-Site` or `Origin` shows came from another site. A request with a bearer token is not checked, since a browser does not add that header to a request another site makes. The UI's pages carry `Cache-Control: no-store`, `Content-Security-Policy: default-src 'self'; frame-ancestors 'none'` and `X-Content-Type-Options: nosniff`, and use no inline script or style. Not built: trusting an identity header from a configured reverse proxy. The UI will create tokens later.
- **Admin settings.** `internal/settings` holds the values an admin can change while the server runs, in the `setting` table, each with a default used while it has no row, and bounds. `refresh_seconds` (default 900, 1 to 31536000) is how long a device sleeps between wakes, and `min_password_length` (default 12, 1 to 1024) is the shortest new admin password. They are changed with `trmnl-byos setting list|set|reset` and `GET`/`PATCH /api/v1/settings`. A value that is not a whole number in bounds is refused as invalid input (400), and an unknown key as not found. `join_until` and `admin_password_hash` are not settings in this sense: this package cannot list or set them. `/api/display` reads `refresh_seconds` at each wake, one primary-key lookup, so a change applies at the next wake without a restart. If the stored value cannot be used, it logs the error and answers with the default, so the device is never left without a `refresh_rate`. `SetPassword` reads `min_password_length` when a password is set, and it fails with an error naming the key if the stored value is unusable. The minimum applies only then, so a shorter password set earlier still logs in. `trmnl-byos setting reset KEY` removes a bad row. Each setting is one entry in a table in the package.
- **Device auth.** Per-device `api_key` from setup, sent as `Access-Token`. Only its SHA-256 is stored, and the device is looked up by that hash, so neither a copy of the database nor request timing gives away a key. The key is 128 random bits, so a fast hash is enough. `ID` is optional for clients that leave it out, but must match the device when sent. Device endpoints on a separate port bound to the LAN are not built: the server has one `-listen` address.
- **Secrets at rest.** Custom fields marked secret (API keys, passwords) are sealed together with AES-256-GCM under `TRMNL_SECRET_KEY`, 32 random bytes in base64 from the environment (`internal/secrets`); the UI never shows them back. The instance ID is bound in as associated data, so a sealed value copied to another instance does not open. Recipes do not mark fields secret, so the person adding an instance chooses which are. A secret must be at least 6 characters. A shorter value is easy to guess, so sealing it protects nothing, and redacting it would hide its characters all over a build log. Such a value can be a plain field. Values sealed before the instance ID was bound in, or under another key, cannot be opened. There is no fallback decoder. The API shows `secret_fields` as null for such an instance and logs a warning with its ID, never the error. A render of it fails with a reason. The fix is to create the instance again with its secrets.
- **Secrets at render time.** No field value goes into a project's files. The project's `.trmnlp.yml` sets each field to `{{ env.FIELD_n }}`, which trmnlp expands, and the value, decrypted if secret, travels in the build job, which the shim passes to that one build as an environment variable. A transform or a failed fetch can print a value, and the build log and the error line made from it are stored and shown, so the server replaces each secret field's value with `[redacted]` in the log before it saves either. It matches the value as written and in its two URL-encoded forms, `url.QueryEscape` (a space is `+`) and `url.PathEscape` (a space is `%20`), longest first. trmnlp 0.34.1 prints the full polling URL when a poll gets a 3xx or 4xx answer other than 429, or a body that is not JSON. A recipe that writes `{{ api_key }}` into the URL shows the raw value there, and one that uses `url_encode` shows the query-encoded form. Other encodings, such as base64 or JSON escaping, are not matched, and neither is a value split by the log's size cut. There is no minimum length, so a very short secret, such as `1`, hides every such character in the log. Job variables must be named `FIELD_...`, so a job can never set one the build tools read, such as `LD_PRELOAD` or a proxy setting.
- **Renderer network.** Recipes may reach any public host but nothing private: not the LAN, the host, services running beside the server, the server's own listeners, or cloud metadata. Nothing in trmnlp enforces that. Its poller only warns about a private address, and its Firefox fetched images from loopback and the renderer's own private address in a test. Transforms in four languages can open any connection. So it is enforced around trmnlp, in two parts: the renderer can reach nothing but the server's proxy, and the proxy refuses non-public addresses.
  - Isolation is something each deployment provides, not something only a container gives. Any way of running the renderer must give it these properties:
    - It runs as its own user or in its own sandbox, with no access to the database, the secrets key, the data directory or the server's environment.
    - Its only network path is the proxy's address. It cannot reach the internet, the LAN, services on the host's loopback, or the server's other listeners.
    - Its process count, memory and privileges are limited, so a recipe cannot exhaust the host or gain privileges.
  - Ways to provide them:
    - Docker Compose, which ships today: the renderer is on an `internal: true` network (below). It does not yet meet every property (see the next bullet).
    - systemd, not built: a renderer unit with its own `User=`, `IPAddressDeny=any` with `IPAddressAllow=` for the proxy's address, `ProtectSystem=strict`, `InaccessiblePaths=` for the data directory, and `TasksMax=` and `MemoryMax=`.
    - A network namespace or another container runtime, such as Podman, set up the same way.
    - A renderer run as the server's own user with no sandbox meets none of these. It is for development only.
  - Not yet true for the server itself. In the shipped Compose file the server joins the render network and listens there on 8080 as well as the proxy's 3128, so a recipe's transform or page can connect to it directly. That exposes the device routes (behind a device key), the admin API (behind a bearer token), and `/api/setup`, which gives a key to any MAC while the join window is open. Closing this, and limiting the renderer's processes, memory and privileges, wait on the deployment options design.
  - In the shipped Compose file the renderer runs on a Docker `--internal` network, which has no gateway. Nothing inside it reaches the internet or the LAN directly.
  - The server runs an HTTP forward proxy, with `CONNECT` for HTTPS, on an address the renderer can reach. In Compose that is on the internal network, which the server joins too. The proxy resolves each host itself, refuses any loopback, private, link-local, CGNAT, unique-local or otherwise non-public address, and connects to the address it checked, so a DNS answer that changes between check and connect gets nothing through. It uses the same check as the server's own outbound client.
  - Each build gets `HTTP_PROXY` and `HTTPS_PROXY` pointing at the proxy, from the renderer's environment. Not built: credentials unique to each build, and a record of the hosts a build contacts and the requests refused, kept against its recipe and shown in the UI (the `outbound_host` table). Today the proxy writes refused or failed tunnels to the server log, and nothing says which recipe made them. The record would show, for example, Paperboy's third-party "battery" endpoint, which receives the device ID, battery level and a timestamp.
  - trmnlp's HTTP client, Python and the trmnlp image's Firefox honor the proxy variables; a test showed Firefox's page and image requests at the proxy. `NO_PROXY` must cover `localhost` and `127.0.0.1`, since trmnlp drives Firefox through a local WebDriver connection. Node's built-in `fetch` does not use a proxy, so a JavaScript transform that fetches on its own fails rather than leaking.
  - The proxy caches assets. A `CONNECT` tunnel hides what passes through it, so each project's `.trmnlp.yml` sets `framework_asset_host: http://trmnl.com` and `asset_overrides` that turn `https://` into `http://` for `trmnl.com`, `fonts.googleapis.com`, `fonts.gstatic.com` and `code.highcharts.com`. Firefox then sends those requests to the proxy in the clear, with their full URLs. The proxy serves them from the cache, or fetches them over HTTPS and stores them. Root-relative URLs in the framework's CSS resolve to `http://trmnl.com/...` and are cached the same way. Versioned paths (`/css/<version>/`, `/js/<version>/`, `/js/highcharts/<version>/`, Google's font files) are kept forever; anything else follows its `Cache-Control`, and a stale copy is served if the upstream fails. The proxy rewrites `https://fonts.gstatic.com/` to `http://` inside Google Fonts stylesheets, and keys those stylesheets by User-Agent, since Google picks the font format by it. It upgrades to HTTPS only for the cached hosts.
  - The proxy is built from the standard library and die-net code, not written from scratch:
    - The server is adapted from conduit's `internal/proxy.HTTPProxyServer` into `internal/renderproxy`. Plain-HTTP requests go through `net/http/httputil.ReverseProxy`, which streams bodies and handles hop-by-hop headers; its director turns a cached host's URL to `https://`, and its `Transport` is the caching round tripper below. `ReverseProxy` refuses `CONNECT`, and neither the standard library nor `golang.org/x/net` serves it (`x/net/proxy` and `x/net/http/httpproxy` are client side), so the server hijacks the connection, dials, answers 200, and copies both ways with a copy adapted from conduit's `internal/conn.CopyBidirectional`, which half-closes each direction and sets no deadlines so `io.Copy` can splice on Linux.
    - The dialer is copied from conduit's direct dialer (fully qualified lookups, TCP keepalive, one address family when the host has only one) into `internal/guard`, which also holds the address check copied from legwork's `guard`, in the dialer's `Control` hook. The check runs on the address each connection is about to use, so neither a redirect nor a DNS answer that changes between lookups gets around it. Recipes poll plain HTTP and other ports, so unlike legwork's client, any port on a public address is allowed.
    - The cache is copied from plainweb's `internal/cache` into `internal/cache`. An entry is one file: a JSON line with the request fields that matter and the response's status and headers, then the body. The file is named by the SHA-256 of those request fields, under a subdirectory of its first two hex digits. Entries are written to a temporary file and renamed into place. The modification time is when the entry was last known fresh, or for a versioned entry when it was last used, refreshed at most hourly. A janitor sweeps entries unused for 30 days, then the oldest past 2 GB. Reads and writes stream: a hit's body is the open file, and a miss's body is copied to the client and the cache entry at once, committed only if it was read to the end.
  - A test with trmnl-meteoswiss sent every framework asset through the cache and rendered the same image. `plugins.css` for framework 3.4.6 is 19 MB, and Firefox loads it once per layout, so the cache saves about 76 MB of downloads per build. What still goes out uncached: the recipe's own data, trmnlp's framework version list on `raw.githubusercontent.com`, two small `trmnl.com` API calls from the poller, and Firefox's own update and settings checks at Mozilla.
  - There is no per-host allowlist. Some hosts come from data, not markup: Top Hacker News Story shows each story's preview image, fetched from whatever site the story links to, so no install-time review could list them.
- **Transforms and page JavaScript** run inside the renderer, which gets only its project directories and must have no access to the server's database, secrets key or data directory (see Renderer network for how a deployment provides that). trmnlp itself is not a sandbox. The isolation around it is the boundary.
- **Plain HTTP on the LAN** saves the ESP32 a TLS handshake per request. HTTPS via a reverse proxy remains an option.

## Deployment and footprint

The deployment that ships is Docker Compose: two containers and two volumes. The server alone is small, and the renderer's size is the price of recipe fidelity. Other ways to run it, such as two systemd services, must give the renderer the isolation described in Renderer network. None ships yet.

| Component | Contents | Size | Required |
| --- | --- | --- | --- |
| Server | Static Go binary on distroless, embedded migrations, system-screen fonts (`deploy/server/Dockerfile`) | 36 MB uncompressed | Yes |
| Renderer | `trmnl/trmnlp` (Ruby, Firefox ESR, Node, Python, PHP, ImageMagick) + the shim | ~580 MB compressed (Docker Hub, v0.34.1) | Only for recipes |

**Volumes.**

- `data`: SQLite file, rendered PNGs, firmware files. Back up this one.
- `cache`: the asset cache. Only the server uses it, and deleting it costs only refetching.

**Runtime.** Measured with trmnlp 0.34.1 (arm64) in Colima with 4 CPUs and 4 GB, on an Apple Silicon Mac, so slower hardware will take longer:

| Build | Time |
| --- | --- |
| trmnl-meteoswiss, HTML only | 1.2 s |
| trmnl-meteoswiss, PNGs for all four layouts | 7.3 to 7.7 s |
| Two meteoswiss builds at once in one container | 8.3 s each |
| "Is this weather normal" (five polling URLs, Python transform, framework 3.1.1) | 16 s, mostly fetching |

- A build peaks at about 1 GB, Firefox included, and the container drops back to about 40 MiB after it. Two builds at once peaked at 2.1 GB.
- Starting the container and its virtual display adds about 0.2 s, so a container per build costs little; a long-running shim container idles at about 11 MiB with the display up.
- PNGs come out 1-bit for `--palette bw`, 2-bit for `gray-4` and 4-bit for `gray-16`, all grayscale (color type 0), at the requested size.
- At 2 bits the framework draws text in a bitmap font without anti-aliasing and draws grays as dither patterns. BBC World News at `gray-4` came out with 68 gray pixels out of 384,000, from its logo; the rest was black and white. Any gray pixel puts the OG in gray mode, which means a full, flashing refresh, while a 1-bit image gets a partial one. Whether that is worth it depends on the recipe, not the device: BBC World News gains nothing from gray, while meteoswiss draws real gray areas. So the device profile holds only what the panel can show, and a plugin instance can ask for less: `bw` to keep partial refreshes. The render uses the instance's palette when the device can show it, and the device's otherwise. A recipe could carry a suggested default, set when it is installed. The server does not guess by inspecting or altering images.
- At 16 grays (TRMNL X) the framework switches to an anti-aliased font and solid grays, and fits more items, so a recipe's layout differs by device.

**Networks.** Compose defines two: the server's normal network, and an `internal: true` network shared by the server and the renderer. The renderer is on the internal one only.

**Proxmox.** An LXC with Docker, or a small VM, works. The Compose file ships with the repo. No env file ships: you create `deploy/.env`, which git ignores (see the README). Both services are built from the repo with `build:` and no `image:`, so an update is `git pull` then `docker compose up -d --build`. Schema migrations run at startup.

## Deferred work

Each of these has a place in the design so it can land later without reshaping the data model or the device API.

| Feature | Where it plugs in | Notes |
| --- | --- | --- |
| Native rendering (Ogre or SVG + resvg) | Renderer interface, new backend | For our own small plugins; never for recipes, which need the framework's JavaScript. Ogre needs upstream fixes first. |
| Image-URL plugins (Grafana panels, camera snapshots, radar) | Renderer interface, Go quantizer | No browser needed. Likely the first native backend. |
| TRMNL cloud proxy (hybrid) | A screen source per device | Needs TRMNL Developer edition. Gives native cloud plugins and model-correct firmware offers. |
| OAuth plugins | Plugin instance fields `oauth_*` | trmnlp has OAuth helpers; the server needs a callback flow. |
| Async polling and Plugin Merge strategies | `instance_data` | Async: answer 202, accept a callback. Merge: one instance reads another's data. |
| Multi-user accounts | `user` table, ownership columns | Keep the single-admin path as the default. Times in the UI are shown in the server's `TZ` until then. With accounts, the time zone becomes a setting of the user or of the device. The `min_password_length` setting exists and applies when a password is set. What is deferred is the forced reset at login. Only a login can see a password's length, since the hash does not keep it, so a login with a password shorter than the setting would succeed, and its session could do nothing but set a new password. |
| Firmware hosting from TRMNL releases | `firmware` sync job | Same validation as uploads. The release file names carry the version the descriptor lacks. |
| Admin UI palette choice | A `setting` row naming the palette, set as an attribute on `<html>` | Each palette is one more block of the same CSS custom properties, so adding one changes no page. Candidates: oxblood on warm paper, teal on slate, indigo on plum. A light, dark or follow-the-device choice fits the same way. |
| Server-side sensors | `sensor_reading` with source = server | Device sensors via the `SENSORS` header are not built either: nothing reads or stores the header. |

## Open questions and risks

The biggest risk is coupling to trmnlp, a 0.x development tool whose CLI and directory layout are its only interface.

- [x] How long does `trmnlp build --png` take, and what is its peak memory? About 7.5 s and 1 GB for all four layouts on a Mac (see Runtime). Re-measure on the Proxmox host.
- [x] Can trmnlp render a mashup of several recipes as one page? No. Each layout renders in its slot on a full-size page, so the server composes mashups from slot crops.
- [x] Do `.trmnlp.yml` `variables` reach a webhook recipe's templates as merge variables? Yes.
- [x] Do concurrent `trmnlp build` runs in different directories interfere? No: two in one container both finished, at about 1.1x the time of one, and memory adds up per build.
- [x] Can framework assets avoid the internet on every build? Yes, through the proxy's cache (see Renderer network).
- [ ] Does the firmware accept a plain `http://` server URL? The code says yes: `http_client.h` picks TLS by the URL's scheme. Confirm on a device.
- [x] Which firmware `Model` values map to which registry models? For firmware, which needs the chip, not the registry model, the table is built (see Firmware updates). `internal/profile` still has no registry model for `xiao_epaper_3clr`, `xiao_epaper_6clr`, `xiao_epaper_mini`, `lilygo_t5pro`, `m5_paper_color` and `waveshare_397`, so those get the fallback profile.
- [x] Should the renderer get an egress allowlist built from approved hosts? No. It gets no route out except a proxy that refuses non-public addresses (see Renderer network).
- [x] Does Firefox in the trmnlp image use `HTTP_PROXY` and `HTTPS_PROXY`? Yes, with `NO_PROXY` covering localhost.
- [ ] Are measured gray levels firmware-specific? The OG was measured on 1.8.10. Firmware 1.8.19's bb_epaper reworks 4-gray on 7.5" panels and sets VCOM to −1.60 V, which can shift contrast. Re-measure after updating, and key the measured levels in `internal/profile` by firmware version, or by the `Panel-Vcom` header 1.8.19 sends, if the levels move.
- [ ] Admin auth: the built-in password login is built. Is a reverse-proxy identity header also wanted?

**Risks.**

- trmnlp CLI or layout changes break rendering. Mitigation: pin the image, golden-image tests over the community catalog, upgrade deliberately.
- Some recipes fetch from APIs that answer only TRMNL's servers. The Hourly Weather Forecast recipe got HTTP 401 from its data source. The server should show such a fetch failure on the recipe's page and in its staleness flag, not a blank screen.
- Framework 4.0 removes 3.x classes. Recipes pinned to 3.x are fine with the asset cache; recipes on `latest` may break. Mitigation: show the resolved version and allow pinning per instance.
- TRMNL changes the device protocol. Mitigation: the protocol surface is small and covered by tests built from real firmware request captures.
