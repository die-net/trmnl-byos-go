# Admin guide

This guide covers running a trmnl-byos server day to day: the web UI, the command line, devices, recipes and playlists, firmware updates, settings, and the JSON API. To install the server and set up your first device, see [setup](setup.md). How each part works inside is in [architecture](architecture.md).

The examples use `ABC123` for a device's friendly ID. Use your own device's.

## Three ways in

Everything here can be done three ways, and all three apply the same checks:

- **The web UI**, on the server's port, after you sign in with the admin password.
- **The command line**, `trmnl-byos <command>`, run on the server's machine against its data directory.
- **The JSON API** at `/api/v1`, with an API token, for scripts.

Setting the admin password and managing API tokens are only on the command line. Calibration is on the command line and in the API, not in the web UI. Pushing data to a recipe is only in the API.

### Running commands

Every command but `proxy` and `doctor` takes `-data DIR`, the server's data directory, which is `data` unless you give another. Give the same directory the server uses. The examples here use `/var/lib/trmnl-byos`, as [setup](setup.md#server-only) does. A command given a directory with no database fails with "no database at PATH: pass the -data directory the server uses", and creates nothing. Only `serve` creates the database, so start it once before you run any other command but `doctor`. In Compose, wait for the server container to start first.

In the Compose setup, run each command inside the server's container, where the default data directory is the server's:

```bash
docker compose -f deploy/compose.yaml exec server trmnl-byos device list
```

To install a recipe from a file, copy the file into the container first with `docker compose -f deploy/compose.yaml cp`.

The command line is a separate process from the server. A change it makes that needs a new render, such as an instance edit, is found at the server's next scheduler check, at most 30 seconds later, and a device shows it at its next wake after the render. A change made in the web UI or the API wakes the scheduler at once. Blocks, calibration, and a device's sleep window, button and double-click settings need no render, and take effect at the device's next wake.

Run `trmnl-byos -h` for the list of commands, and `trmnl-byos <command> <subcommand> -h` for a command's flags. The [command reference](#command-reference) at the end lists every command.

## Admin password and signing in

The web UI signs in with one admin password. Set it, or change it, on the server:

```bash
trmnl-byos password -data /var/lib/trmnl-byos
```

It asks twice without echo. From a script, pipe the password in as one line: `printf '%s\n' "$PW" | trmnl-byos password -data /var/lib/trmnl-byos`.

- It must be from `min_password_length` to 1024 bytes. That setting is 12 unless you change it (see [Settings](#settings)). The minimum applies only when a password is set, so a shorter one set earlier still works.
- If the server can be reached from the internet, use a long password, such as four or more random words. The server doesn't lock out or rate-limit sign-in attempts, so a short password can be guessed.
- Setting the password signs out every browser.
- A signed-in session lasts 30 days from its last use.
- A signed-in session also works for `/api/v1`, so the web UI's browser can call the API.

Until a password is set, a sign-in attempt fails with a message that says to run `trmnl-byos password`.

### Checking the server with doctor

`trmnl-byos doctor` checks whether the host can run recipe builds. It takes `serve`'s flags (see [setup](setup.md#settings-and-secrets)), so run it with the same command line as `serve`. It never starts the server, and makes nothing in the data directory, which need not exist yet. It writes one line per check:

```
pass|fail|warn|n/a|skip|note  <name>: <detail>
```

The detail of a failing check has the fix. It exits non-zero when a check that builds need fails. A `warn` does not change the exit status.

With `-render-url`, it checks that the renderer answers a GET at that URL (any HTTP status means it is there). It says that `trmnl-render` checks its own user, writable places and processes when it starts and refuses to start otherwise, and that the deployment gives the network, the limits and what it can read. It checks none of those.

Without `-render-url`, it checks the sandbox on this host:

- The sandbox library's probe: the backend, whether it runs in a container, and each prerequisite with its fix. On Ubuntu 24.04 and later it prints the AppArmor profile.
- Each tool, with its path and version. A missing required tool fails, and a missing optional one (Node, Python, PHP) warns.
- Each path the sandbox shows read-only.
- That the `-data` directory exists. It warns if not, since `serve` makes it when it first starts.
- The library's check, run in a sandbox with the build's real settings. It expects the data directory and the cache to be unreadable, and the server's listen address to be unreachable.
- `build.memory` and `build.tasks`. With `-render-cgroup`, it makes and removes a child cgroup to prove it works.
- `build.oom`. Without `-render-cgroup` on a host that runs systemd, it warns that a build that uses up memory can get a process killed in the server's unit, and systemd's default then stops the server (see [setup](setup.md#running-under-systemd)).
- A self-test, run in a sandbox that is a build's in all but its command, with a real proxy. It sends `CONNECT` through the build's listener. 169.254.169.254:80, 127.0.0.1, `::1` and each of the host's interface addresses at the server's listen port must be refused, and `trmnl.com:443` must be allowed, since builds need the internet.

A failure of any check sets the exit status, but for the library's `limits.memory` and `limits.tasks`. They look at the sandbox the check makes, which has neither a build's cgroup nor its watchdog, so they are listed without counting, and a `note` line says so. `build.memory` and `build.tasks` say what bounds a build.

## The web UI

Open the server's address in a browser, such as `http://192.168.1.10:8080`, and sign in. The pages are:

- **Devices.** Every device with its current screen and whether it is online. Open or close the join window. Block a MAC, and see the blocked MACs.
- **A device's page.** Its screen, its facts, and the playlist it would show now. A form for its name, firmware updates and pin, panel profile and palette, time zone and locale, sleep window, and what a button press and a double-click do. The order of its playlists. Block, unblock or delete it.
- **Recipes.** Every recipe with its instances, the playlists that show them, when each last rendered and why a build failed. Upload a recipe zip. Remove an instance, once no playlist shows it.
- **A recipe's page.** Add an instance, with a control for each of the recipe's custom fields. Replace the recipe's files with a new zip.
- **An instance's page.** Edit its name, palette and fields, and see how its builds stand. When the host cannot start a sandbox, it says "Builds cannot start: REASON; run trmnl-byos doctor on the server to see why" until a sandbox starts again. A build failed with "the server stopped during this build" was running when the server stopped. After a planned restart that is expected, and the next build clears it.
- **Playlists.** Every playlist with its schedule, devices and items. Create a playlist. Move, hide, show and remove items.
- **A playlist's page.** Its name and schedule, its items, adding an item, and linking or unlinking devices.
- **Firmware.** Every build with its rollout stage. Upload a build.
- **A build's page.** Start or restart its rollout with a canary, cancel it, and see each device of its model with whether it is offered the build and why not.
- **Settings.** Each setting with its range and default, and a reset button.

Times show in your browser's time zone. Every delete goes through a confirm page first. A device, instance or playlist form, or a delete, block or unblock, saved from a page that is out of date, because something changed since it loaded, is refused and shown again with what is stored now, so it never undoes another change.

## Devices

### Adding devices

A new device registers only while the join window is open. Open it for 10 minutes from the Devices page, or:

```bash
trmnl-byos join -data /var/lib/trmnl-byos -for 10m
```

`-for 0` closes it. The API has `GET` and `PUT /api/v1/join` with `{"for_seconds": 600}`. [Setup](setup.md#first-device) walks through a first device.

A device that tries while the window is closed shows "Not registered" with its MAC, and tries again every 5 minutes. A device that lost its credentials, after a Wi-Fi reset for example, needs the window open again to re-pair. It keeps its friendly ID and settings.

### Listing devices

```bash
trmnl-byos device list -data /var/lib/trmnl-byos
```

It lists registered devices with their battery, signal and wake time. `serve -debug` logs each device request's headers, without its token.

### Device settings

`device set` changes only the settings you give it. It takes a friendly ID or a MAC:

```bash
trmnl-byos device set -data /var/lib/trmnl-byos -name Kitchen -zone America/Los_Angeles ABC123
```

| Flag | What it sets |
| --- | --- |
| `-name NAME` | The device's name. Empty for none. |
| `-profile MODEL` | The registry model the panel is, when its firmware reports another. A 4-color OG on firmware 1.8.10 or older reports `og`, so it needs `-profile og_bwry`. `none` goes back to what the firmware reports. |
| `-palette ID` | The palette to render with when a plugin instance asks for none, one of the model's, such as `gray-4`. `none` for the model's default. |
| `-zone NAME` | The IANA time zone its schedules are read in and its renders are made for. `none` for the server's `TZ`. |
| `-locale CODE` | The locale its renders are made for, such as `de` or `pt-BR`. `none` for the server's `locale` setting. |
| `-sleep HH:MM-HH:MM` | A window, in the device's time zone, in which it shows a "Sleeping" screen and no playlist item, and wakes at the window's end. `none` for always awake. |
| `-button next\|previous\|first\|refresh\|default` | What a single press does. The default is `next`. |
| `-double-click none\|identify\|restart_playlist\|rewind\|sleep\|add_wifi\|default` | What a double-click does. The default is `rewind`. `none` turns it off, so `default` is how to clear the setting. |

A button press in the sleep window shows the device's usual screen until its next timer wake. A double-click set to `sleep` sleeps until the next end of the sleep window, or 8 hours with no window.

The device's page in the web UI has the same settings, and so does `PATCH /api/v1/devices/{id}` (see [JSON API](#json-api)).

### Removing and blocking devices

```bash
trmnl-byos device delete -data /var/lib/trmnl-byos ABC123
trmnl-byos device block -data /var/lib/trmnl-byos ABC123
trmnl-byos device unblock -data /var/lib/trmnl-byos ABC123
trmnl-byos device blocked -data /var/lib/trmnl-byos
```

- `delete` (a friendly ID or a MAC) removes a device with its state and renders. Its next request resets it: it erases its key, Wi-Fi settings and server URL, and starts its setup portal. To re-pair it, open the join window and set it up again as in [First device](setup.md#first-device). It then registers as a new device. If it was a firmware rollout's canary, that rollout is deleted too.
- `block` blocks a MAC, registered or not, and keeps the device's row. A blocked registered device shows a "Blocked" screen and checks in once a day. A blocked MAC that has not paired shows it at setup and tries again every 5 minutes.
- `unblock` lifts the block. A registered device is back as it was.
- `blocked` lists the blocked MACs.

To keep a device out for good, delete it and block its MAC. The API has `DELETE /api/v1/devices/{id}`, `GET /api/v1/blocklist`, and `PUT` and `DELETE /api/v1/blocklist/{id}`, where `{id}` is a MAC or a friendly ID.

### Gray calibration

A device with no playlist items shows a "Connected" screen with its friendly ID and a row of gray swatches. One whose playlists have nothing scheduled now shows "Nothing scheduled now" instead. A panel in its gray mode shows each swatch as a different shade.

```bash
trmnl-byos calibrate -data /var/lib/trmnl-byos -for 30m ABC123
```

`calibrate` shows a device a gray calibration screen from its next wake, for 30 minutes by default. Press the device's button to wake it now. The API has `POST /api/v1/devices/{id}/calibrate` with `dot` and `for_seconds`.

Each row of the screen is one of the panel's grays over strips of black-and-white dither. In each row, find the box whose halves match from arm's length. It says how dark that gray really looks, in sixteenths. `-dot 2` draws the dither with 2x2-pixel dots, which checks whether the panel blurs single pixels. Measured levels go in the firmware model table in `internal/profile`.

## Recipes and instances

A recipe is a TRMNL plugin: a `settings.yml`, Liquid templates and optional transforms. An instance is a recipe with its settings filled in. A device shows instances through its playlists. Recipes render only when the server has a renderer (see [setup](setup.md#the-renderer)).

### Installing a recipe

Install a recipe from a directory or a zip, such as one from TRMNL's recipe pages or GitHub:

```bash
trmnl-byos recipe add -data /var/lib/trmnl-byos path/to/recipe.zip
trmnl-byos recipe list -data /var/lib/trmnl-byos
```

The Recipes page uploads a zip too, and so does `POST /api/v1/recipes`. A zip may be up to 8 MB. An upload that is not a valid recipe is refused with the reason.

To replace a recipe's files and keep its ID and instances:

```bash
trmnl-byos recipe replace -data /var/lib/trmnl-byos 1 path/to/new.zip
```

With no path, `replace` uses the path the recipe was installed from. An uploaded recipe has none, so it needs a path. The recipe's page in the web UI and `PUT /api/v1/recipes/{id}` replace from an upload. Each instance keeps its values, and the answer lists any value the new files no longer accept.

### Adding an instance

```bash
trmnl-byos instance add -data /var/lib/trmnl-byos -recipe 1 -field city=Zurich -field-file api_key=key.txt
```

| Flag | What it does |
| --- | --- |
| `-recipe ID` | The recipe, from `recipe list`. |
| `-name NAME` | The instance's name. The recipe's by default. |
| `-palette ID` | The palette to render with when the device takes it, such as `bw`. The device's by default. |
| `-field k=v` | A custom field's value. Repeat for each field, and for each choice of a multi-select. |
| `-field-file k=path` | A field's value read from a file, or from stdin for `-`. One trailing newline is dropped. |
| `-secret k` | Mark a text field secret, so its value is stored encrypted and never shown back. |

`-field` puts the value in the command line, which `ps` and the shell history show. Give a password or a key with `-field-file`.

`-palette bw` renders in black and white on a panel that also takes grays. That keeps its refreshes partial, without the full flashing refresh a gray image needs.

### Changing and removing an instance

```bash
trmnl-byos instance set -data /var/lib/trmnl-byos 1 -field city=Bern -clear units
trmnl-byos instance list -data /var/lib/trmnl-byos
trmnl-byos instance remove -data /var/lib/trmnl-byos 1
```

`instance set` takes the same `-name`, `-palette`, `-field`, `-field-file` and `-secret` flags, and changes only what it is given. `-clear k` clears a field, a secret one too. `-plain k` unmarks a secret field, which needs a new value or `-clear` in the same command, since a secret is never shown back. A change of a field renders the instance again. `instance remove` deletes an instance and its renders, and is refused while a playlist shows it.

### Secret fields

A recipe's `password` fields, and the fields you mark secret, are stored encrypted with `TRMNL_SECRET_KEY`. Set that key before you add one (see [setup](setup.md#settings-and-secrets)). A secret must be at least 6 characters. It is never shown back, in the web UI, the API or a build log.

Without the key, an instance with no secrets works as usual, but giving it a secret is refused. One that has secrets can still be renamed or given another palette, but a change of any of its values is refused. If the key changes, the old secrets cannot be read. Create the instance again with its secrets.

## Playlists

A device shows the first of its playlists that is scheduled now and has an item to show. A playlist has items, each an instance with a duration. A timer wake moves to the next item once the current one's time is up. A button press moves at once, as the device's button command says, which is to the next item unless you change it.

### Adding items

```bash
trmnl-byos playlist add -data /var/lib/trmnl-byos -device ABC123 -instance 1 -duration 15m
```

Each `playlist add` appends an item. The playlist is named by `-playlist`, or the device's friendly ID by default, and is created if it is new. `-device` links the playlist to that device, last in its order, if it is not linked already. `-duration 0` keeps an item until a button press.

### Changing items

```bash
trmnl-byos playlist list -data /var/lib/trmnl-byos
trmnl-byos playlist move -data /var/lib/trmnl-byos 3 0
trmnl-byos playlist set -data /var/lib/trmnl-byos 3 -hidden
trmnl-byos playlist set -data /var/lib/trmnl-byos 3 -days mon,tue,wed,thu,fri -window 09:00-17:00
trmnl-byos playlist remove -data /var/lib/trmnl-byos 3
```

- `list` shows every playlist's schedule and each item's ID, index, schedule and hidden flag.
- `move ITEM INDEX` moves an item to an index in its playlist, counting from 0.
- `set ITEM` changes only the flags it is given: `-duration`, `-hidden` (or `-hidden=false` to show it again), and the schedule flags below.
- `remove ITEM` takes an item out.

### Schedules

`playlist set ITEM` schedules an item, and `playlist schedule NAME` schedules a whole playlist. Both take:

| Flag | Meaning |
| --- | --- |
| `-days mon,tue` | The weekdays it is active, or `all`. |
| `-window HH:MM-HH:MM` | The hours it is active, or `all` for all day. An end before the start wraps past midnight. |
| `-from YYYY-MM-DD` | The first day it is active, in the server's time zone. Empty for no start. |
| `-until YYYY-MM-DD` | The last day it is active, in the server's time zone. Empty for no end. |

Days and hours are read in the device's time zone. A device shows its first playlist that is scheduled now, so an unscheduled playlist at the end of its order is the default. A device whose playlists are all off schedule shows "Nothing scheduled now".

### Whole playlists and their devices

```bash
trmnl-byos playlist create -data /var/lib/trmnl-byos Evening
trmnl-byos playlist rename -data /var/lib/trmnl-byos Evening Night
trmnl-byos playlist link -data /var/lib/trmnl-byos Night ABC123
trmnl-byos playlist order -data /var/lib/trmnl-byos Night ABC123 1 0
trmnl-byos playlist unlink -data /var/lib/trmnl-byos Night ABC123
trmnl-byos playlist delete -data /var/lib/trmnl-byos Night
```

- `create NAME` makes an empty playlist. A name is 1 to 64 bytes and unique.
- `rename NAME NEW` renames it.
- `link NAME DEVICE` links a device, last in the device's order. An existing link stays where it is.
- `order NAME DEVICE FROM TO` moves an existing link from index FROM to index TO in the device's order. It is refused if the link is no longer at FROM.
- `unlink NAME DEVICE` removes the link.
- `delete NAME` removes the playlist with its items and links.

The web UI's Playlists page does all of this, and a device's page reorders its playlists.

## Firmware updates

A device installs firmware only when you ask. Only add a build made for the device's exact firmware model, the `Model` header it sends. `firmware list` and `device list` show it. The firmware does not check the board it installs onto, so a build for another board on the same chip installs and can leave the device needing a USB cable. The server checks the chip and the file's structure, not the board.

```bash
trmnl-byos firmware add -data /var/lib/trmnl-byos -model xiao_epaper_display -version 1.8.19 FW1.8.19.bin
trmnl-byos firmware device -data /var/lib/trmnl-byos -updates on ABC123
trmnl-byos firmware rollout 1 -data /var/lib/trmnl-byos -canary ABC123
trmnl-byos firmware list -data /var/lib/trmnl-byos
```

- `firmware add` takes the app image TRMNL publishes for OTA, the `firmware.bin` of a build, not the merged flash image. It accepts only the models in the chip table in [architecture](architecture.md#firmware-updates). `-version` is the dotted version from the release's file name, and can be left out only when the file itself holds a plain version. Nothing is offered after `add`.
- `firmware device -updates on|off` and `-pin VERSION` or `-pin none` set which devices may be offered firmware. Every device starts with updates off.
- `firmware rollout ID -canary DEVICE` offers the build to the canary, which must be a device that could be offered it. Once the canary reports the new version or a newer one, the build is offered to every other device of that model that could be: one with updates on and no pin, not blocked, and on a version from 1.8.17 up that is older than the build.
- `firmware cancel ID` stops offering it.

A build is offered to a device at most 3 times. If the canary is offered it 3 times and still does not report the version, `firmware list` shows the rollout as `stalled` and nothing is offered to anyone. The `LAST OFFERED` column is when the canary was last told to install the build. Run `firmware rollout` again, with the same canary to retry or another one, or cancel. A device on firmware older than 1.8.17 is never offered an update, since its `Model` may not name its board. Update it by USB or through TRMNL once.

The Firmware page in the web UI uploads builds and runs rollouts. A build's page lists each device of its model and says why it is or is not offered the build. The same operations are in the API, under `/api/v1/firmware` and `PATCH /api/v1/devices/{id}`:

```bash
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/octet-stream" --data-binary @FW1.8.19.bin "http://localhost:8080/api/v1/firmware?model=xiao_epaper_display&version=1.8.19"
curl -H "Authorization: Bearer $TOKEN" -d '{"canary": "ABC123"}' http://localhost:8080/api/v1/firmware/1/rollout
```

## Settings

A few values can change while the server runs, with no restart:

| Setting | Default | Range | Meaning |
| --- | --- | --- | --- |
| `refresh_seconds` | 900 | 1 to 31536000 | The longest devices sleep between wakes. A device wakes sooner when an item ends, a schedule changes, or a render is pending. It picks up a change at its next wake. |
| `min_password_length` | 12 | 1 to 1024 | The shortest new admin password, in bytes. |
| `locale` | `en` | one of trmnlp's locales | The locale renders are made for, unless a device sets its own. |

The locales are cs, da, de, de-AT, en, en-AU, en-GB, es-ES, fr, he, hu, id, is, it, ja, ko, lt, nl, no, pl, pt-BR, ro, ru, sk, sv, uk, zh-CN and zh-HK.

```bash
trmnl-byos setting list -data /var/lib/trmnl-byos
trmnl-byos setting set -data /var/lib/trmnl-byos refresh_seconds 600
trmnl-byos setting reset -data /var/lib/trmnl-byos refresh_seconds
```

`list` shows each value, its default, and whether it is the default. A stored value that is not usable shows as `bad`, with a note, and the default is used until you set or reset it. `reset` goes back to the default. The Settings page has the same. The API reads and changes settings with `GET` and `PATCH /api/v1/settings`, but cannot reset one. A field left out of a `PATCH` is kept, and a bad one changes nothing.

Server settings that are not stored in the database, such as the listen address and the time zone, are flags and environment variables of `serve` (see [setup](setup.md#settings-and-secrets)).

## JSON API

Most of what the web UI and the command line do to a running server's data, a script can do over HTTP at `/api/v1`. Setting the admin password, managing tokens and resetting a setting are not in the API. It is on the same port as the device API, so keep that port on a trusted network.

### Tokens

Create a token on the server. It is shown once:

```bash
trmnl-byos token add -data /var/lib/trmnl-byos -name my-script
```

`token list` shows each token's name and when it was last used, and `token remove my-script` revokes one. Only a token's SHA-256 is stored. Send the token as a bearer token:

```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/devices
```

A request without a known token gets 401. A signed-in web UI session works in place of a token.

### Conventions

- Bodies and responses are JSON, with times in Unix seconds and durations in seconds.
- A JSON body may be up to 64 KB, and an unknown field is refused.
- Errors are `{"error": "..."}` with the HTTP status that fits: 400 bad input, 401, 404 unknown ID, 403 for a cross-site request on a signed-in session, 409 conflict, 413 too large, 415. A bad custom field value adds `"fields"`, the message for each bad field by name.
- A `PATCH` changes only the fields it is given, and a refused one changes nothing. A `schedule` in it replaces the whole schedule, so a part left out goes back to every day, all day, with no dates.
- A device's `{id}` is its friendly ID or MAC. Other IDs are numbers.

### Routes

| Route | What it does |
| --- | --- |
| `GET /devices`, `GET /devices/{id}` | List devices, or one. A device's JSON has its settings, what is in effect, and `blocked`. |
| `PATCH /devices/{id}` | Change `name`, `updates_enabled`, `fw_pin`, `profile`, `palette`, `time_zone`, `locale`, `sleep`, `button_command` and `special_function`. `""` clears one. To refuse the change (409) if the settings changed since you read them, send back `settings_state` from a `GET`. |
| `DELETE /devices/{id}` | Delete a device (204). |
| `POST /devices/{id}/calibrate` | Show the calibration screen, with `dot` and `for_seconds`. |
| `GET /blocklist`, `PUT /blocklist/{id}`, `DELETE /blocklist/{id}` | List, block and unblock MACs. |
| `GET /join`, `PUT /join` | Read the join window, or open it with `for_seconds` (0 closes it). |
| `GET /settings`, `PATCH /settings` | Read or change `refresh_seconds`, `min_password_length` and `locale`. |
| `GET /firmware`, `POST /firmware?model=M&version=V` | List builds, or add one with the raw file as the body (`application/octet-stream`). |
| `GET /firmware/{id}/devices` | Each device of the build's model, and whether it is offered the build now, and why not. |
| `POST /firmware/{id}/rollout`, `DELETE /firmware/{id}/rollout` | Start a rollout with `canary`, or cancel it. |
| `GET /recipes`, `GET /recipes/{id}` | List recipes, or one with its `custom_fields`. |
| `POST /recipes`, `PUT /recipes/{id}` | Install a recipe, or replace one's files, with a zip as the body (`application/zip`, up to 8 MB). |
| `GET /instances`, `GET /instances/{id}` | List instances, or one, with `last_rendered_at` and `last_error`. |
| `POST /instances` | Create one with `recipe_id`, `name`, `palette`, `fields` and `mark_secret`. |
| `PATCH /instances/{id}` | Change `name`, `palette`, `fields` and `mark_secret`, and `clear` a list of fields. |
| `DELETE /instances/{id}` | Remove one. Refused (409) while a playlist shows it. |
| `GET /playlists`, `POST /playlists` | List playlists, or create one with `name`. |
| `PATCH /playlists/{id}`, `DELETE /playlists/{id}` | Change a playlist's `name` and `schedule`, or delete it with its items and links. |
| `PUT /playlists/{id}/devices/{device}` | Link a device, last in its order. |
| `PATCH /playlists/{id}/devices/{device}` | Move the link, with `order` (the device's playlist IDs as you saw them) and `priority` (the new index). |
| `DELETE /playlists/{id}/devices/{device}` | Unlink a device. |
| `POST /playlists/items` | Add an item with `playlist_id` or `playlist` (a name), `instance_id`, `duration_seconds`, and an optional `device` to link. |
| `PATCH /playlists/items/{id}`, `DELETE /playlists/items/{id}` | Change an item's `position`, `duration_seconds`, `hidden` and `schedule`, or remove it. |
| `GET /renders`, `GET /renders/{id}` | Each render target's status, or one with its build log. |

A schedule is `{"days": ["mon", "tue"], "window_start": "HH:MM", "window_end": "HH:MM", "starts_at": unix, "ends_at": unix}`. Every route, with each field's rules, is described in [architecture](architecture.md#admin-ui-api-and-mcp).

### Examples

```bash
curl -H "Authorization: Bearer $TOKEN" -X PUT -d '{"for_seconds": 600}' http://localhost:8080/api/v1/join
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/zip" --data-binary @recipe.zip http://localhost:8080/api/v1/recipes
curl -H "Authorization: Bearer $TOKEN" -d '{"recipe_id": 1, "fields": {"city": "Zurich"}}' http://localhost:8080/api/v1/instances
curl -H "Authorization: Bearer $TOKEN" -d '{"name": "Kitchen"}' http://localhost:8080/api/v1/playlists
curl -H "Authorization: Bearer $TOKEN" -d '{"playlist": "Kitchen", "device": "ABC123", "instance_id": 1, "duration_seconds": 900}' http://localhost:8080/api/v1/playlists/items
curl -H "Authorization: Bearer $TOKEN" -X PATCH -d '{"refresh_seconds": 600}' http://localhost:8080/api/v1/settings
```

### Secret fields in the API

`mark_secret` maps a keyname to `true` to store its value encrypted, or `false` to unmark it. Unmarking a field that has a value needs a new value or a `clear` in the same request, since a secret is never shown back. In a `PATCH`, `""` or `[]` clears a plain field.

Secret values are never returned. An instance's JSON has `secret_fields`, the names of the secret fields that are set, and `secret_keys`, the names marked secret, set or not. `secret_fields` is `null`, not a list, when the instance's sealed secrets cannot be opened. That happens when the server has no `TRMNL_SECRET_KEY`, or when the key changed, since the names cannot be read without the right key.

## Pushing data to a recipe

A script can push data to a plugin instance, at the path TRMNL uses. A script written for TRMNL changes its host and ID, and adds a token. The ID is the instance ID, and the API token is the secret. The instance's recipe sees the data as top-level variables. A push wakes the scheduler. If a playlist shows the instance and a renderer is set up, it renders the instance again, and the device shows the new image at its next wake. After a failed render, the new data waits for the retry.

```bash
curl -H "Authorization: Bearer $TOKEN" -d '{"merge_variables": {"stories": ["a", "b"]}}' http://localhost:8080/api/custom_plugins/1
curl -H "Authorization: Bearer $TOKEN" -d '{"merge_strategy": "stream", "stream_limit": 20, "merge_variables": {"stories": ["c"]}}' http://localhost:8080/api/custom_plugins/1
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/custom_plugins/1
```

- `merge_strategy` is `replace` (the default), `deep_merge` or `stream`, as on TRMNL.
- `stream` appends each pushed array to the stored one, and `stream_limit` keeps that many of the newest values.
- A body and the stored data are each limited to 64 KB, and a `trmnl` key is refused.
- An instance takes a burst of 10 pushes and then 60 an hour. Over that is 429 with `Retry-After`.
- `GET` returns the stored data.

## Command reference

Every command but `proxy` also takes `-data DIR`. A device is a friendly ID or a MAC.

| Command | Usage |
| --- | --- |
| `serve` | Run the server. Its flags are in [setup](setup.md#settings-and-secrets). |
| `proxy` | Run the renderer's proxy apart from the server (see [setup](setup.md#the-renderer)). |
| `doctor` | Check that builds can run, with serve's flags (see [below](#checking-the-server-with-doctor)). |
| `join` | `join [-for duration]` |
| `password` | `password` |
| `calibrate` | `calibrate [-for duration] [-dot 1\|2] <device>` |
| `device list` | `device list` |
| `device set` | `device set [-name name] [-profile model\|none] [-palette id\|none] [-zone name\|none] [-locale code\|none] [-sleep HH:MM-HH:MM\|none] [-button ...] [-double-click ...] <device>` |
| `device delete`, `block`, `unblock` | `device delete <device>` |
| `device blocked` | `device blocked` |
| `recipe add` | `recipe add <recipe directory or zip>` |
| `recipe list` | `recipe list` |
| `recipe replace` | `recipe replace <id> [<recipe directory or zip>]` |
| `instance add` | `instance add -recipe ID [-name name] [-palette id] [-field k=v ...] [-field-file k=path ...] [-secret k ...]` |
| `instance list` | `instance list` |
| `instance set` | `instance set <instance ID> [-name name] [-palette id] [-field k=v ...] [-field-file k=path ...] [-clear k ...] [-secret k ...] [-plain k ...]` |
| `instance remove` | `instance remove <instance ID>` |
| `playlist add` | `playlist add -instance ID [-device ID] [-playlist name] [-duration d]` |
| `playlist list` | `playlist list` |
| `playlist remove` | `playlist remove <item ID>` |
| `playlist move` | `playlist move <item ID> <index>` |
| `playlist set` | `playlist set <item ID> [-duration d] [-hidden=bool] [-days d,d] [-window HH:MM-HH:MM\|all] [-from date] [-until date]` |
| `playlist schedule` | `playlist schedule <playlist name> [-days d,d] [-window HH:MM-HH:MM\|all] [-from date] [-until date]` |
| `playlist create`, `delete` | `playlist create <playlist name>` |
| `playlist rename` | `playlist rename <playlist name> <new name>` |
| `playlist link`, `unlink` | `playlist link <playlist name> <device>` |
| `playlist order` | `playlist order <playlist name> <device> <from> <to>` |
| `token add` | `token add -name name` |
| `token list` | `token list` |
| `token remove` | `token remove <name>` |
| `setting list` | `setting list` |
| `setting set` | `setting set <key> <value>` |
| `setting reset` | `setting reset <key>` |
| `firmware add` | `firmware add -model model [-version version] <file>` |
| `firmware list` | `firmware list` |
| `firmware device` | `firmware device [-updates on\|off] [-pin version\|none] <device>` |
| `firmware rollout` | `firmware rollout <id> -canary <device>` |
| `firmware cancel` | `firmware cancel <id>` |
