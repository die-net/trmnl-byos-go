# Setup

This guide installs a trmnl-byos server and sets up a first device. Day-to-day use is in the [admin guide](admin.md). How the parts fit together is in [architecture](architecture.md).

## The parts

- **The server**, `trmnl-byos serve`. One binary with an embedded SQLite database. It answers devices, schedules renders, and serves the web UI and the JSON API. It needs nothing else to run.
- **The renderer**, `trmnl-render`. It runs TRMNL recipes through trmnlp, TRMNL's own plugin tool, which drives Firefox. It is optional: without it the server still registers devices, shows its own screens and updates firmware, but renders no recipes. It runs only on Linux.
- **The proxy**, `trmnl-byos proxy`. The renderer's only way to the internet. It refuses private addresses, and caches the TRMNL framework's assets, Google Fonts and Highcharts. It is the same binary as the server.

Recipes are untrusted code from the internet, so the renderer is kept apart from everything else. It never gets the database, the secrets key or the data directory, and it can reach nothing but the proxy. Compose gives it those properties. Any other way must give them too (see [What every deployment must give it](#what-every-deployment-must-give-it)). Docker is one way, not the only one. The standalone `docker run` below does not give them, and is for development only.

## Getting the server

Prebuilt server binaries will be on the project's GitHub Releases page, starting with v0.1.0. Until then, build it from source with Go (see [development](development.md)), or use the Compose setup below, which builds everything from the repository.

The server binary has no runtime dependencies. Put it anywhere on the machine that will run it, and give it a data directory.

The renderer cannot be a single binary. It needs trmnlp and everything trmnlp runs (see [The renderer](#the-renderer)).

## Ways to run it

| Shape | Recipes | What it takes |
| --- | --- | --- |
| [Server only](#server-only) | No | The server binary. |
| [Docker Compose](#docker-compose) | Yes | Docker with Compose, and a clone of the repository. Ships today in `deploy/`. |
| [Renderer under its own user account](#renderer-under-its-own-user-account) | Yes | A Linux host, trmnlp and its dependencies, and a service manager such as systemd. No unit file ships yet. |

## Settings and secrets

`serve` takes these flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data DIR` | `data` | The directory holding the database, images, recipes and firmware. |
| `-listen ADDR` | `:8080` | The address for devices, the web UI and the API. |
| `-public-url URL` | from each request | The base URL devices reach the server at, such as `http://192.168.1.10:8080`. |
| `-render-url URL` | none | The renderer's URL, such as `http://renderer:8081`. Without it, recipes are not rendered. |
| `-proxy-listen ADDR` | none | Run the renderer's proxy in the server's process, on this address, such as `:3128`. Never publish it. |
| `-cache DIR` | `cache` | The proxy's asset cache, with `-proxy-listen`. |
| `-debug` | off | Log at debug level, including each device request's headers, without its token. |

And these environment variables:

| Variable | Meaning |
| --- | --- |
| `TZ` | The server's time zone, such as `Europe/Zurich`. Recipes render in it and schedules are read in it, unless a device sets its own. UTC when unset. The server refuses to start on a name it does not know. |
| `TRMNL_SECRET_KEY` | The key that encrypts secret recipe fields: 32 random bytes in base64, from `openssl rand -base64 32`. The server runs without it, but refuses to store a secret value until it is set. If you lose or change it, the stored secrets cannot be read, and each instance that has them must be created again. |

Back up the data directory. The cache directory can be deleted at any time, and costs only fetching the assets again.

The device API, the web UI and the JSON API share one port, over plain HTTP. Keep that port on a trusted network. HTTPS through a reverse proxy is an option. If the server can be reached from the internet, give it a long admin password (see the [admin guide](admin.md#admin-password-and-signing-in)).

## Server only

```bash
trmnl-byos serve -data /var/lib/trmnl-byos -listen :8080
trmnl-byos password -data /var/lib/trmnl-byos
```

Run `serve` under the service manager you use, as a user of its own. It creates the data directory and the database when it first starts, and every other command needs it to have started once, or fails with "no database". Then set the admin password, and go on to [First device](#first-device). You can add a renderer later by giving `serve` a `-render-url`.

## Docker Compose

`deploy/compose.yaml` runs the server, the proxy and the renderer in three containers, built from the repository:

```bash
git clone https://github.com/die-net/trmnl-byos-go.git
cd trmnl-byos-go
docker compose -f deploy/compose.yaml up -d --build
```

Its settings come from the environment, or from `deploy/.env`, which git ignores and which no file in the repository provides:

```
PUBLIC_URL=http://192.168.1.10:8080
TZ=Europe/Zurich
TRMNL_SECRET_KEY=...
```

Once the server container has started, set the admin password:

```bash
docker compose -f deploy/compose.yaml exec server trmnl-byos password
```

The web UI and devices use port 8080. Run any other command the same way, with `docker compose -f deploy/compose.yaml exec server trmnl-byos ...`.

**What it gives the renderer.**

- Two networks. The server is on the normal network only. The renderer is only on `render`, an internal network with no gateway, whose only other member is the proxy. The proxy is on both, and has IP forwarding off, so it never routes packets between them.
- The renderer reaches the internet only through the proxy, on port 3128, and the server sends it builds through the proxy's forward on port 8081. Never publish either port.
- The renderer runs as its image's unprivileged user, with every capability dropped and `no-new-privileges`.
- Its root file system is read-only, with a 1 GB tmpfs on `/tmp`. It runs with no init process, so the shim is process 1 and reaps what builds leave.
- Its memory is capped at 2 GB and its processes and threads at 1024.

**Checking the isolation.** The renderer's image has Python, and its requests go through the proxy:

```bash
docker compose -f deploy/compose.yaml exec renderer python3 -c 'import socket; socket.getaddrinfo("server", 8080)'
docker compose -f deploy/compose.yaml exec renderer python3 -c 'import urllib.request as u; u.urlopen("http://server:8080/")'
docker compose -f deploy/compose.yaml exec renderer python3 -c 'import urllib.request as u; print(u.urlopen("https://example.com/").status)'
```

The first fails to resolve `server`, since the renderer isn't on the server's network. The second fails with a 502 from the proxy, which refuses `server` because it isn't a public address. The third prints 200. After each build the renderer kills every other process of its user, so a command run in its container may be killed if a build ends while it runs.

**Updating.** Run `git pull`, then `docker compose -f deploy/compose.yaml up -d --build`. Schema migrations run when the server starts. The `data` volume holds the database, images, recipes and firmware: back it up. The `cache` volume is the proxy's asset cache.

## The renderer

### What it needs

The renderer is the `trmnl-render` shim, from `cmd/trmnl-render`, next to trmnlp. `deploy/renderer/Dockerfile` builds it on the `trmnl/trmnlp:v0.34.1` image, and that image's Dockerfile, in the usetrmnl/trmnlp repository at tag v0.34.1, is the reference list of what trmnlp needs:

- Ruby 4.0, with trmnlp v0.34.1 and its gems.
- Firefox ESR and geckodriver, which trmnlp drives to draw each page.
- ImageMagick 7, which turns the page into a PNG.
- Xvfb. The shim starts one display per build, and finds `Xvfb` on its `PATH`.
- Node, Python and PHP, for recipes whose transforms are in those languages.
- Fonts. The image installs DejaVu, Inter, Noto (with CJK) and a few others.

The trmnlp version is pinned, since its command line and project layout are the only interface the shim has. Use v0.34.1.

`trmnl-render` takes these flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-listen ADDR` | `:8081` | The address the server sends builds to. |
| `-trmnlp PATH` | `/app/bin/trmnlp` | The trmnlp command. The default is its path in the trmnlp image. |
| `-timeout D` | `2m` | The longest one build may take. |
| `-concurrency N` | 1 | Builds to run at once. Keep it at 1: each build takes about 1 GB, and builds that overlap can read each other's values, secrets included. |

It needs `HTTP_PROXY` and `HTTPS_PROXY` set to the proxy's address. It passes them to each build, and adds `localhost`, `127.0.0.1` and `::1` to `NO_PROXY` itself, since trmnlp talks to Firefox over a local connection.

### The proxy

Run the proxy on its own:

```bash
trmnl-byos proxy -listen :3128 -cache /var/cache/trmnl-byos
```

Or run it inside the server's process with `serve -proxy-listen :3128`. A server with `-render-url` and no `-proxy-listen` says at startup that the proxy must run apart. It never starts one on its own.

`proxy -forward-listen :8081 -forward-to renderer:8081` also forwards each TCP connection on port 8081 to the renderer, for a server that shares no network with it, as in Compose. The target is fixed by the flag.

### Connecting the server

`serve -render-url` points the server at the renderer, directly or through the proxy's forward. The renderer answers `102 Processing` when it starts a build. Anything in between that drops 1xx responses, such as an HTTP reverse proxy, makes every build fail with an error that says so. Use a direct connection or a TCP forward.

### What every deployment must give it

These properties keep a recipe from reaching anything it should not. Compose gives all of them. Any other way of running the renderer must give them too:

- It runs as a user of its own, which runs nothing but the shim, with no access to the database, the secrets key, the data directory or the server's environment.
- Nothing a build writes or starts outlives it. Its user can write nowhere but scratch places that are the renderer's own, which the shim empties after each build.
- Its only network path is the proxy's address. It cannot reach the internet, the LAN, services on the host's loopback, or the server's listeners. The server's `/api/setup` gives a key to any MAC while the join window is open, so the renderer must not be able to reach it.
- Its process count, memory and privileges are limited.

The shim checks its own user and where it can write, and refuses to start if they are wrong. It cannot check the network, its limits, or what it can read, so those are up to the deployment.

### Renderer under its own user account

The renderer can run on a Linux host, without containers, as a user account of its own. The shim's checks decide what that account needs:

- It refuses to start as root, or while any other process of its user runs, an init process or a login shell of that user included.
- After each build it kills every other process of its user, and removes every file its user owns in `/tmp`, `/var/tmp`, `/dev/shm`, `/dev/mqueue`, `TMPDIR` and the account's home, and every System V IPC object its user owns. So these must be the renderer's own. It refuses to start if another user owns any entry in them on a writable file system, and a sweep that finds one later stops the shim.
- It refuses to start if its user can write anywhere else, so the root file system must be read-only to it.

Under systemd, these settings in the renderer's unit give that:

- `User=` an account that nothing else uses.
- `ProtectSystem=strict`, which makes the whole file system read-only.
- `PrivateTmp=yes`, which gives the unit its own `/tmp` and `/var/tmp`, where builds run.
- `TemporaryFileSystem=/dev/shm`, its own `/dev/shm`.
- `ProtectHome=read-only`, which keeps the account's home read-only.
- `PrivateIPC=yes`, its own message queues and other IPC objects.
- `SystemCallFilter=~add_key keyctl request_key`, which keeps builds from the user's kernel keyring.
- `InaccessiblePaths=` for the server's data directory.
- `TasksMax=` and `MemoryMax=` for its processes and memory. A build peaks at about 1 GB.
- `PrivatePIDs=yes` (systemd 257 and later), which makes the shim process 1 of its own PID namespace, and `Restart=always`, which starts it again however it stopped.
- `Environment=` with `HTTP_PROXY` and `HTTPS_PROXY` set to the proxy.

The network is the part that takes the most care. The renderer's only path must be the proxy, and the server's listeners must not be reachable from it. A network namespace, or a container runtime such as Podman, can give that. `PrivateNetwork=yes` with the proxy on a Unix socket is the planned systemd way, but the proxy does not listen on a Unix socket yet. No unit file or tested network setup ships yet. Both are on the [roadmap](roadmap.md#security-and-privacy).

Run the proxy and the server as users other than the renderer's.

### Running the renderer alone in Docker

To run the renderer's image by itself, build it from the repository root and run it without an init process, since the shim is process 1 and reaps its own children:

```bash
docker build -f deploy/renderer/Dockerfile -t trmnl-byos-render .
docker run --rm --read-only --tmpfs /tmp:mode=1777,size=1g -p 8081:8081 trmnl-byos-render
```

The shim refuses to start if its user can write anywhere but its scratch directories. The image leaves directories writable that a build could plant files in for later builds to run, so the root file system must be read-only. This command gives the renderer Docker's default network, which reaches the internet and the LAN directly, so it is for development only (see [development](development.md#running-the-renderer)). For real use, give it a network whose only other member is the proxy, as Compose does.

## First device

A device registers only while the server's join window is open, so open it first, for 10 minutes by default. Give each command here the same `-data` directory as `serve`. A command given another one fails with "no database" rather than making an empty one:

```bash
trmnl-byos join -data /var/lib/trmnl-byos -for 10m
```

With Compose, run commands inside the server's container, where the default directory is the server's:

```bash
docker compose -f deploy/compose.yaml exec server trmnl-byos join -for 10m
```

The Devices page of the web UI has the same control. Then point the device at the server from its own setup portal. These steps are from TRMNL's firmware source, and match TRMNL's help article on connecting a device to a self-hosted server:

1. **Start the portal.** A device with no Wi-Fi saved starts it on its own, and its screen says which Wi-Fi network to join, named `TRMNL-` and a few characters. On an OG that is already set up, hold the button for 5 to 15 seconds while it starts. Holding it longer resets the device.
2. **Join that network** from a phone or a computer. It has no password. The portal usually opens by itself. If it doesn't, browse to `http://4.3.2.1`.
3. **Set the server.** Open Advanced Settings, then Server, and enter the server's URL in the API server field, such as `http://192.168.1.10:8080`, with no trailing slash. Save it. Plain `http://` works. Changing the server clears the device's old key and friendly ID, so it pairs again.
4. **Set Wi-Fi.** Go back, choose your network, enter its password, and save and connect. The OG uses 2.4 GHz Wi-Fi.

The device then calls the server's `/api/setup`. While the join window is open, it gets a new key and a friendly ID. It then shows the friendly ID on a "Connected" screen, until it has a playlist item to show.

If the join window was closed, the device shows "Not registered" with its MAC, and tries again every 5 minutes. Open the window, and it registers at its next try.

`trmnl-byos device list -data /var/lib/trmnl-byos` shows the new device, and so does the Devices page. From here, the [admin guide](admin.md) covers naming the device, installing recipes and building its playlist.

A device that later loses its credentials, after a Wi-Fi reset for example, needs the join window open again to re-pair. It keeps its friendly ID and settings.
