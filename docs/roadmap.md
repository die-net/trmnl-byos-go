# trmnl-byos-go roadmap

This is what is designed and not built yet, the work deferred past the first release, and the open questions. How the server works today is in [architecture](architecture.md). The sections below follow its sections, so a missing piece is under the same heading in both. When a roadmap item is built, its text moves into the architecture doc in the same change.

## Device protocol and profiles

- **Sensor readings.** `SENSORS` is not stored. The `sensor_reading` table (see [Data model](#data-model)) would keep the latest reading per kind, plus history.
- **Registry refresh at runtime.** The model and palette registry is embedded in the binary and refreshed with `go generate`. Refreshing it daily at runtime is deferred.
- **The "too large" screen names the recipe.** Today it shows the image's size, the device's limit and a hint, but not which recipe made the image.
- **Image size warnings before a device fails.** A recipe page that shows its renders' sizes against the smallest limit among the devices that show it, and warns past 80%, before a device ever fails. Today `render_image.bytes` holds the sizes and the check runs at `/api/display`.

## Scheduling and pre-rendering

- **A dashboard of wake times.** The firmware reports the previous wake's duration in `Wake-Time`, which `trmnl-byos device list` and the JSON API show. A dashboard that plots it is not built.
- **Aligned wakes.** Wakes a plugin asks for on the minute or on the hour. Today a wake time is only what the server sent.
- **Ready screens per device.** Each device always has three screens ready: current (for button and power-on wakes), next (for skip and touch-bar next) and a short history, kept in a `device_screen` table. Rewind does not need it, since the device redraws its own previous image. After a render, the server would update each affected device's ready screens. Today there is no `device_screen` table, and a device gets the newest render when it wakes.
- **Staleness options.** A configurable age before "Out of date", and skipping the item instead of showing that screen.

## Playlists, mashups and input

**Mashups.** A playlist item would be either one plugin instance at full layout, or a mashup: a layout (left/right, top/bottom, quadrants) with a plugin instance per slot. Only the `playlist_slot` table and `playlist_item.layout` exist, and `AddItem` writes slot 0 and layout `full`. The plan: the framework renders a mashup as one page with mashup classes. trmnlp cannot put several recipes on one page: it renders each of a recipe's half and quadrant layouts on a full-size page, inside a one-slot mashup in the slot's position (the half-horizontal layout fills the top half). So the server composes a mashup by cropping each slot from its recipe's render and placing it, which keeps the framework's slot frame and spacing.

## Plugins and rendering

- **Recipe sources.** Installing from the TRMNL gallery, the community catalog (`bnussbau/trmnl-recipe-catalog`) or a git URL. Today a recipe comes from a local directory or zip.
- **Per-instance render settings.** Dark mode, icons and framework version pins.
- **Renderer interface.** There is no `Render(job) -> PNG` interface and no static-image backend. The scheduler calls the shim client (`*render.Client`) directly, and `internal/screen` draws system screens in process. A pluggable interface is the plan, so other backends can be added. An image-URL backend (fetch a PNG such as a Grafana panel, quantize in Go) and a native Ogre backend are deferred.

## Firmware updates

**Sources.** Official releases synced from TRMNL on a schedule (as Terminus does), going through the same checks as an upload, are not built yet. Taking `update_firmware`/`firmware_url` from the TRMNL cloud via the deferred cloud proxy is another option, since the cloud already picks per device.

## Data model

These tables and columns are designed and do not exist:

| Table or column | What it is for |
| --- | --- |
| `device_state`: battery_pct | Battery as a percentage. |
| `sensor_reading` | device_id, at, kind, value, unit (latest kept per kind, plus history). |
| `model` / `palette` | Vendored registry rows. The registry is embedded in the binary as JSON and is not in the database. |
| `recipe`: source_ref | Updates from a source, such as the TRMNL catalog or a git repository, as a replace fetched by the server. |
| `outbound_host` | recipe_id, host, first_seen, last_seen, refused, from the renderer proxy's log. |
| `plugin_instance`: framework_pin, dark_mode, refresh_override, align, stale_after | Per-instance render settings. |
| `instance_data`: callback state | Async polling: answering a recipe's 202 and taking the callback that brings its data, which the server does not do. A recipe that uses it fails to build, saying so. A failed fetch of a polling recipe is a failed build, recorded on its render (see the [render path](architecture.md#plugins-and-rendering)). |
| `device_screen` | device_id, role (current, next, history n), png_hash. |

## Admin UI, API and MCP

**Web UI.** The design adds live updates over server-sent events, and these pages and parts:

- Dashboard: devices, last seen, battery and wake-time trends, current screen thumbnails.
- Devices: logs. The firmware's log entries are stored in `device_log`, and no command or route reads them yet.
- Recipes and instances: install from the gallery, a catalog or git; the hosts each recipe contacts and any refused requests; preview at any device profile; delete a recipe.
- Playlists: build a mashup.
- Firmware: sync official releases.
- Tokens: create API tokens in the UI.

**Routes.**

- `POST /api/screen`, which would accept a full markup or PNG screen from a script.
- `GET /api/alias/<uuid>?model=<name>`, which would return a plugin instance's current image for any model, for clients outside a playlist (a Home Assistant card, a web page).

**MCP server** (`/mcp` over HTTP, plus stdio), tools:

- `list_recipes`, `get_recipe`, `create_recipe`, `update_recipe` (markup, settings, transform)
- `render_preview` (instance, layout, model) returning the PNG
- `list_devices`, `get_device`, `list_playlists`, `update_playlist`
- `get_logs` (device, since)

This lets an assistant write a recipe, render it, look at the result and iterate against the real server.

## Security and privacy

- **Reverse-proxy identity.** Trusting an identity header from a configured reverse proxy, in place of the admin password.
- **A separate device port.** Device endpoints on a separate port bound to the LAN. Today the server has one `-listen` address.
- **A systemd renderer unit.** A renderer unit that gives the renderer what Compose does:
  - `User=` an account that nothing else uses.
  - `PrivateNetwork=yes`, reaching the proxy through `systemd-socket-proxyd` on a Unix socket. The proxy command would need to listen on a Unix socket, which `listenOn` is where to add.
  - `InaccessiblePaths=` for the data directory, and `TasksMax=` and `MemoryMax=`.
  - `ProtectSystem=strict`, which makes the whole file system read-only.
  - `PrivateTmp=yes`, which gives the unit its own `/tmp` and `/var/tmp`, where builds run.
  - `TemporaryFileSystem=/dev/shm`, which gives it its own `/dev/shm`.
  - `ProtectHome=read-only`, which keeps the account's home read-only.
  - `PrivateIPC=yes`, which gives it its own message queues and other IPC objects.
  - `SystemCallFilter=~add_key keyctl request_key`, which keeps builds from the user's kernel keyring. That keyring outlives every process of the user while the shim runs. Docker's default seccomp profile already denies those calls.
  - No `ReadWritePaths=`. That leaves the user as little to write as Compose does.
  - `PrivatePIDs=yes` (systemd 257 and later), which makes the shim process 1 of its own PID namespace, where a build's `SIGSTOP` cannot reach it, as in Compose.
  - `Restart=always`, which starts it again however it stopped.
  - Not `WatchdogSec=`, which would need the shim to send keepalives, which it does not.
- **A sandbox for each build, with no containers.** A Go sandbox library, not yet published, would let the server run each build in a sandbox of its own. Recipes would then need only the binary and packages from the platform's package manager (`apt`, `dnf` or `brew`), with no container stack. The library has a Linux backend, built on bubblewrap, namespaces and a seccomp filter, and a macOS one, built on Seatbelt through `sandbox-exec`. The plan:
  - The shim becomes a subcommand of `trmnl-byos` that runs inside the sandbox, one sandbox per build. The sandbox shows the host's files read-only, hides the data directory, the server's configuration and people's homes, and lets a build write only its own directories.
  - The proxy runs inside `serve`, with one listener per build. On Linux the build's network namespace has only a loopback, with that listener in it, so the proxy is its only way out. Since each listener belongs to one build, the proxy knows which build made each request, which the per-build host record below needs.
  - Firefox runs headless, without Xvfb.
  - Limits come from a delegated cgroup where the host gives one, and otherwise from resource limits and a watchdog on the build's memory. On Linux everything a build started ends with it, since the sandbox's process 1 ending kills the rest.
  - At startup the server finds trmnlp and its runtimes, and works out what the sandbox must show, so a Homebrew, rbenv or apt install needs no configuration.
  - Compose and a systemd unit stay supported. There the operator's container or unit is the boundary, and the library's checks report at startup what it enforces.
  - On macOS the sandbox can't keep a loopback port private. Until trmnlp lets WebDriver use fixed ports, a build there can reach every local service on the Mac, and the server says so at startup.
  - Renders would depend on the host's versions of trmnlp, Firefox and its fonts, rather than on a pinned image. A supported version range and a render self-test are the ways considered to limit drift.
- **Per-build credentials and a host record.** Credentials unique to each build, and a record of the hosts a build contacts and the requests refused, kept against its recipe and shown in the UI (the `outbound_host` table). Today the proxy writes refused or failed tunnels to its log, and nothing says which recipe made them. The record would show, for example, Paperboy's third-party "battery" endpoint, which receives the device ID, battery level and a timestamp.

## Deferred work

Each of these has a place in the design so it can land later without reshaping the data model or the device API.

| Feature | Where it plugs in | Notes |
| --- | --- | --- |
| Native rendering (Ogre or SVG + resvg) | Renderer interface, new backend | For our own small plugins; never for recipes, which need the framework's JavaScript. Ogre needs upstream fixes first. |
| Image-URL plugins (Grafana panels, camera snapshots, radar) | Renderer interface, Go quantizer | No browser needed. Likely the first native backend. |
| TRMNL cloud proxy (hybrid) | A screen source per device | Needs TRMNL Developer edition. Gives native cloud plugins and model-correct firmware offers. |
| OAuth plugins | Plugin instance fields `oauth_*` | trmnlp has OAuth helpers; the server needs a callback flow. |
| Async polling and Plugin Merge strategies | `instance_data` | Async: answer 202, accept a callback. Merge: one instance reads another's data. |
| Multi-user accounts | `user` table, ownership columns | Keep the single-admin path as the default. The UI shows times in the browser's zone, and in the server's `TZ` without its script (see the [web UI](architecture.md#admin-ui-api-and-mcp)). With accounts, the time the UI shows becomes a setting of the user, and a device already has its own time zone. The `min_password_length` setting exists and applies when a password is set. What is deferred is the forced reset at login. Only a login can see a password's length, since the hash does not keep it, so a login with a password shorter than the setting would succeed, and its session could do nothing but set a new password. |
| Firmware hosting from TRMNL releases | `firmware` sync job | Same validation as uploads. The release file names carry the version the descriptor lacks. |
| Admin UI palette choice | A `setting` row naming the palette, set as an attribute on `<html>` | Each palette is one more block of the same CSS custom properties, so adding one changes no page. Candidates: oxblood on warm paper, teal on slate, indigo on plum. A light, dark or follow-the-device choice fits the same way. |
| Server-side sensors | `sensor_reading` with source = server | Device sensors via the `SENSORS` header are not built either: nothing reads or stores the header. |
| Cut off the network, and the writes outside its scratch directory, for code the PR reviewer runs | The review job in `.github/workflows/claude-code-review.yml` | Claude's `go build`, `go vet` and `go test` run the pull request's code inside Claude Code's bubblewrap sandbox, in a job that holds `CLAUDE_CODE_OAUTH_TOKEN`. A probe of Claude Code 2.1.325 on ubuntu-24.04 (image 20261111.370.1) checked what that code can reach. It can't get the OAuth token: the token isn't in its environment, its PID namespace shows only its own 4 processes, no file it can read holds the token, and `sudo` fails because of the no-new-privileges flag. It also can't write the action's code, the runner's directory and command files, `~/.claude`, `~/.claude.json`, `.git/config` or `.git/hooks`. Claude's own Read tool is denied outside the checkout and the review directory. Still open: (1) The network is unrestricted (DNS, direct TCP, and HTTPS to any host), so the code can send out anything it can read. That includes the job's read-only `GITHUB_TOKEN`, which can read this repository, and the runner's `.credentials` file, which holds a JWT whose power wasn't checked. (2) It can write outside the scratch directory: `$HOME`, including `~/.local/share/claude` where the Claude Code binary lives (busy while it runs), `~/.bun`, the runner's `_temp`, and the checkout, including the root `CLAUDE.md` that later reviewer agents read. Nothing found runs code from those paths with the token after Claude exits, but that wasn't tested. (3) The result holds only for that Claude Code version and image, and a new action pin needs the probe again. Options: a separate job or runner user with no token and no network except the module proxy, or Claude Code's sandbox with a network allowlist and writes limited to the scratch directory and the Go caches. The prompt's rule that the reviewer runs code only when it has read it, judges it safe, and needs a test stays until then, since a pull request can try to talk its way past it. |

## Open questions and risks

The biggest risk is coupling to trmnlp, a 0.x development tool whose CLI and directory layout are its only interface.

- [x] How long does `trmnlp build --png` take, and what is its peak memory? About 7.5 s and 1 GB for all four layouts on a Mac (see Runtime in [architecture](architecture.md#deployment-and-footprint)). Re-measure on the Proxmox host.
- [x] Can trmnlp render a mashup of several recipes as one page? No. Each layout renders in its slot on a full-size page, so the server composes mashups from slot crops.
- [x] Do `.trmnlp.yml` `variables` reach a webhook recipe's templates as merge variables? Yes.
- [x] Do concurrent `trmnlp build` runs in different directories interfere? No: two in one container both finished, at about 1.1x the time of one, and memory adds up per build.
- [x] Can framework assets avoid the internet on every build? Yes, through the proxy's cache (see Renderer network in [architecture](architecture.md#security-and-privacy)).
- [ ] Does the firmware accept a plain `http://` server URL? The code says yes: `http_client.h` picks TLS by the URL's scheme. Confirm on a device.
- [x] Which firmware `Model` values map to which registry models? For firmware, which needs the chip, not the registry model, the table is built (see [Firmware updates](architecture.md#firmware-updates)). `internal/profile` still has no registry model for `xiao_epaper_3clr`, `xiao_epaper_6clr`, `xiao_epaper_mini`, `lilygo_t5pro`, `m5_paper_color` and `waveshare_397`, so those get the fallback profile.
- [x] Should the renderer get an egress allowlist built from approved hosts? No. It gets no route out except a proxy that refuses non-public addresses (see Renderer network in [architecture](architecture.md#security-and-privacy)).
- [x] Does Firefox in the trmnlp image use `HTTP_PROXY` and `HTTPS_PROXY`? Yes, with `NO_PROXY` covering localhost.
- [ ] Are measured gray levels firmware-specific? The OG was measured on 1.8.10. Firmware 1.8.19's bb_epaper reworks 4-gray on 7.5" panels and sets VCOM to −1.60 V, which can shift contrast. Re-measure after updating, and key the measured levels in `internal/profile` by firmware version, or by the `Panel-Vcom` header 1.8.19 sends, if the levels move.
- [ ] Admin auth: the built-in password login is built. Is a reverse-proxy identity header also wanted?

**Risks.**

- trmnlp CLI or layout changes break rendering. Mitigation: pin the image, golden-image tests over the community catalog, upgrade deliberately.
- Some recipes fetch from APIs that answer only TRMNL's servers. The Hourly Weather Forecast recipe got HTTP 401 from its data source. That blank screen is caught now: a fetch failure is a failed build, which keeps the last good screen, shows on the device by the rules of Staleness, and is in the API's `last_error` (see the [render path](architecture.md#plugins-and-rendering)) and on the web UI's recipes list and instance page.
- Framework 4.0 removes 3.x classes. Recipes pinned to 3.x are fine with the asset cache; recipes on `latest` may break. Mitigation: show the resolved version and allow pinning per instance.
- TRMNL changes the device protocol. Mitigation: the protocol surface is small and covered by tests built from real firmware request captures.
