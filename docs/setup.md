# Setup

This guide installs a trmnl-byos server and sets up a first device. Day-to-day use is in the [admin guide](admin.md). How the parts fit together is in [architecture](architecture.md).

## The parts

- **The server**, `trmnl-byos serve`. One binary with an embedded SQLite database. It answers devices, schedules renders, and serves the web UI and the JSON API. It needs nothing else to show its own screens and update firmware.
- **The build tools**: trmnlp, TRMNL's own plugin tool, which drives Firefox, and what trmnlp runs. They render recipes. By default `serve` runs each build in a sandbox of its own on the same host, so they must be installed there. Builds in a sandbox run only on Linux so far.
- **The renderer**, `trmnl-render`. The other way to run builds: the tools behind a small HTTP shim, in its own container or under its own user account. `serve -render-url` sends builds to it. It runs only on Linux.
- **The proxy**, `trmnl-byos proxy`. A renderer's only way to the internet. It refuses private addresses, and caches the TRMNL framework's assets, Google Fonts and Highcharts. It is the same binary as the server. Sandboxed builds get the same proxy inside `serve`, so they need no separate one.

Recipes are untrusted code from the internet, so builds are kept apart from everything else. A build never gets the database, the secrets key or the data directory, and it can reach nothing but the proxy. A sandbox, Compose and a separate user account each give it those properties, and differ in how strongly (see [Choosing how builds run](#choosing-how-builds-run)). Any other way must give them too (see [What every deployment must give it](#what-every-deployment-must-give-it)). Docker is one way, not the only one. The standalone `docker run` below does not give them, and is for development only.

## Getting the server

Prebuilt server binaries will be on the project's GitHub Releases page, starting with v0.1.0. Until then, build it from source with Go (see [development](development.md)), or use the Compose setup below, which builds everything from the repository.

The server binary has no runtime dependencies. Put it anywhere on the machine that will run it, and give it a data directory.

Recipes need trmnlp and everything trmnlp runs. That is not part of the binary: install it from the host's packages for sandboxed builds (see [Builds in a sandbox on the host](#builds-in-a-sandbox-on-the-host)), or use the renderer's image (see [The renderer](#the-renderer)).

## Ways to run it

| Shape | Recipes | What it takes |
| --- | --- | --- |
| [Sandbox on the host](#builds-in-a-sandbox-on-the-host) | Yes | A Linux host, the server binary, bubblewrap, and trmnlp with its tools from the host's packages. The simplest way. |
| [Server only](#server-only) | No | The server binary. |
| [Docker Compose](#docker-compose) | Yes | Docker with Compose, and a clone of the repository. Ships today in `deploy/`. A stronger boundary. |
| [Renderer under its own user account](#renderer-under-its-own-user-account) | Yes | A Linux host, trmnlp and its dependencies, and a service manager such as systemd. No unit file ships yet. A stronger boundary. |

### Choosing how builds run

All of these keep the secrets key, the database and the data directory from a build, and let it reach the internet only through the filtering proxy. They differ in what else they enforce:

| | Sandbox on the host | Compose | Renderer under its own user |
| --- | --- | --- | --- |
| Separate user from the server | No. Builds run as the server's user. | Yes, the image's user, in its own container | Yes, an account that runs nothing else |
| Files a build sees | The host's files read-only, minus the data directory, the cache, people's homes, `/run`, `/tmp` and `/var`. Writes go to its own home and its private `/tmp`, `/var/tmp` and `/dev/shm`. | Its image, read-only, and a 1 GB `/tmp` | What the operator leaves readable to the user. Writes only to scratch places the shim empties. |
| Network | A network namespace with only a loopback and the build's proxy listener | An internal network whose only other member is the proxy | The operator's network setup. Not enforced by the shim. |
| Memory limit | `-render-memory`, set by the kernel with `-render-cgroup`, otherwise by a soft watchdog | 2 GB, set by Docker | `MemoryMax=` or similar, set by the operator |
| Process limit | 1024 with `-render-cgroup`, otherwise none until the 2-minute timeout | 1024, set by Docker | `TasksMax=` or similar, set by the operator |
| What outlives a build | Nothing. Its processes die with its PID namespace and its files go with its tmpfs and home. | Nothing the shim's sweep can find | Nothing the shim's sweep can find |
| Runs where | Linux only | Anywhere Docker runs Linux containers | Linux only |

Pick Compose or a separate user when the server holds anything you would not want a kernel or sandbox flaw to reach. A sandbox has no user boundary (see [What the sandbox does not give](#what-the-sandbox-does-not-give)).

## Settings and secrets

`serve` takes these flags:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-data DIR` | `data` | The directory holding the database, images, recipes and firmware. |
| `-listen ADDR` | `:8080` | The address for devices, the web UI and the API. |
| `-public-url URL` | from each request | The base URL devices reach the server at, such as `http://192.168.1.10:8080`. |
| `-render-url URL` | none | The URL of a renderer run apart, such as `http://renderer:8081`. Without it, each build runs in a sandbox on this host. |
| `-render-memory MiB` | 2048 | The most memory one sandboxed build may use. At least 256, or `serve` refuses to start. |
| `-render-cgroup DIR` | none | A cgroup v2 directory delegated to the server, as an absolute path. Each sandboxed build gets a child cgroup of its own in it (see [Limits with a cgroup](#limits-with-a-cgroup)). |
| `-proxy-listen ADDR` | none | Run a proxy for a renderer run apart in the server's process, on this address, such as `:3128`. Never publish it. Sandboxed builds do not use it. |
| `-cache DIR` | `cache` in the data directory | The asset cache. Sandboxed builds' proxy uses it, and so does `-proxy-listen`. |
| `-debug` | off | Log at debug level, including each device request's headers, without its token. |

A sandboxed build gets at most 2 minutes, the same as `trmnl-render`'s default `-timeout`. There is no flag for it. [`trmnl-byos doctor`](admin.md#checking-the-server-with-doctor) takes the same flags.

And these environment variables:

| Variable | Meaning |
| --- | --- |
| `TZ` | The server's time zone, such as `Europe/Zurich`. Recipes render in it and schedules are read in it, unless a device sets its own. UTC when unset. The server refuses to start on a name it does not know. |
| `TRMNL_SECRET_KEY` | The key that encrypts secret recipe fields: 32 random bytes in base64, from `openssl rand -base64 32`. The server runs without it, but refuses to store a secret value until it is set. If you lose or change it, the stored secrets cannot be read, and each instance that has them must be created again. Keep it out of any file a sandboxed build could read: a file that holds it, such as a systemd `EnvironmentFile=`, must be readable only by root, which is how systemd reads it. |

Back up the data directory. Its `cache` directory, the asset cache, may be left out of a backup, and can be deleted at any time: it costs only fetching the assets again. It can grow to 2 GB.

The device API, the web UI and the JSON API share one port, over plain HTTP. Keep that port on a trusted network. HTTPS through a reverse proxy is an option. If the server can be reached from the internet, give it a long admin password (see the [admin guide](admin.md#admin-password-and-signing-in)).

## Builds in a sandbox on the host

This is the simplest way to run recipes. `serve` with no `-render-url` runs each build in a sandbox of its own, on the host that runs the server. It uses [bubblewrap](https://github.com/containers/bubblewrap), Linux namespaces and a seccomp filter, through the `github.com/die-net/sandbox` library. It runs only on Linux so far. Elsewhere, including macOS, every build fails as an outage that says so and says to use `-render-url`.

1. Get the server binary (see [Getting the server](#getting-the-server)).
2. Install these from the system's packages, or from a version manager (see below):
   - bubblewrap.
   - trmnlp v0.34.1, the pinned version that is tested. Its own Dockerfile at that tag, in the usetrmnl/trmnlp repository, lists what it needs.
   - Ruby, Firefox (or Firefox ESR) and geckodriver, which trmnlp drives to draw each page.
   - ImageMagick 7, which turns the page into a PNG.
   - Xvfb. Each build starts its own display, since a page that draws with WebGL, as map recipes do, came out blank without one.
   - Node, Python and PHP, only for recipes whose transforms are in those languages.
   - Fonts, such as DejaVu, Inter and Noto.
3. Check the host: `trmnl-byos doctor -data /var/lib/trmnl-byos`. It names each missing tool or restriction and how to fix it.
4. Run it: `trmnl-byos serve -data /var/lib/trmnl-byos`, as a user of its own, under the service manager you use. Then set the admin password: `trmnl-byos password -data /var/lib/trmnl-byos`.

At startup `serve` logs `render: each build runs in a sandbox` with the backend, or a warning that builds cannot start until the host is fixed, which says to run `trmnl-byos doctor`.

**Ubuntu 24.04 and later.** They restrict unprivileged user namespaces with AppArmor, and Ubuntu 26.04's own profile for bubblewrap takes the capabilities a sandbox needs from what bubblewrap starts, so builds cannot start until the `trmnl-byos` binary has an AppArmor profile of its own. `trmnl-byos doctor` prints the profile for the binary's own path, and the commands that load it. The profile lets the binary, and every program it starts, make user namespaces and use capabilities in them, and keeps them out of stricter profiles of their own. It grants nothing to any other program.

**Where the tools can be.** `serve` looks for each tool on its own `PATH`, at its first build, and again at each build until it finds all the required ones, so installing a tool later needs no restart. A tool under `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc`, `/opt` or `/nix` is already visible to a build. For one anywhere else, the sandbox shows it read-only:

- A version manager's install (rbenv, pyenv, nodenv, nvm, asdf and mise): that version's directory.
- Linuxbrew: the whole `/home/linuxbrew/.linuxbrew`, but for its `var`, where its services keep their data.
- Anything else: only the tool's own file, never the directory around it, since that could be a person's home with their credentials.

Ruby's gem path is shown too, and `/var/cache/fontconfig`, unless a directory of it could show a person's credentials: a home, or a directory inside one that is not named by a Ruby version, such as `~/.gem`, where RubyGems keeps its push key. Builds then cannot start, and `trmnl-byos doctor` says which. Install gems system-wide, or where RubyGems puts them by default, `~/.local/share/gem/ruby/VERSION`. Prefer system-wide installs. rbenv's shims do not work, since they run rbenv from `~/.rbenv`: put the version's `bin` directory on `PATH` instead. A snap-packaged Firefox cannot run in the sandbox.

**What can go wrong.** If the sandbox cannot start, because bubblewrap is missing, the AppArmor restriction above is in the way, user namespaces are off, a required tool is missing or `-render-cgroup` is unusable, `serve` still starts and keeps serving the last good screens. Each build fails as a renderer outage, retried after 2 seconds doubling to 30, and nothing is recorded against the instance. The error and the instance's page in the web UI say "Builds cannot start", with the reason. A host that gets fixed needs no restart.

The sandbox cannot run inside default Docker or Podman, or inside a hardened systemd unit (`RestrictNamespaces=`, or `SystemCallFilter=` without `@mount`), since they refuse the namespace and mount calls bubblewrap makes. `trmnl-byos doctor` names the fix.

Builds in a sandbox share nothing, so two at once are safe. The scheduler still sends one at a time.

### What the sandbox does not give

Compose and a separate user account enforce more than the sandbox does:

- Builds run as the server's own user. Namespaces and the seccomp filter keep them from the server's files and processes, but there is no user boundary. A flaw in the kernel, bubblewrap or the library goes straight to the server's data. Compose and a separate user add a different uid.
- The secrets key lives only in the server's environment, which a build never gets, and the build's PID namespace hides the server's `/proc`. But a build sees the host's files read-only outside the paths the sandbox covers, so a file that holds the key must be readable only by root.
- Writes to the build's home, on the data directory's disk, are bounded per file (256 MiB) but not in total, until the build ends and the home is removed.
- Without `-render-cgroup`, memory is bounded only by a soft watchdog that reads the build's memory every 250 ms. A spike between reads is missed, and the tmpfs a build writes is not counted. Nothing bounds how many processes a build starts until its 2-minute timeout ends it. They count against the same limits as the server's, so a build that forks without end can stop the server, until the service manager starts it again. Use `-render-cgroup` where that matters.

### Limits with a cgroup

With `-render-cgroup DIR`, each build gets a child cgroup with a hard memory limit of `-render-memory` and no swap, 1024 processes and a kill of the whole group on an out-of-memory. After the build the server kills whatever is left in it and removes it. A build the kernel kills for memory fails with "the build used over N MiB of memory".

`DIR` must be a cgroup v2 directory the server may write to, as an absolute path. Under systemd, give the unit `Delegate=yes`, and `DelegateSubgroup=` too, such as `DelegateSubgroup=server`, so the server runs in a subgroup and not in the delegated cgroup itself. The kernel refuses to enable controllers in a cgroup that has processes. Then `-render-cgroup` is the unit's own cgroup path under `/sys/fs/cgroup`, such as `/sys/fs/cgroup/system.slice/trmnl-byos.service`. No unit file ships yet. `trmnl-byos doctor` makes and removes a child cgroup to show that it works.

## Server only

```bash
trmnl-byos serve -data /var/lib/trmnl-byos -listen :8080
trmnl-byos password -data /var/lib/trmnl-byos
```

Run `serve` under the service manager you use, as a user of its own. It creates the data directory and the database when it first starts, and every other command needs it to have started once, or fails with "no database". Then set the admin password, and go on to [First device](#first-device).

Without bubblewrap and the build tools, `serve` still runs, and every recipe build fails as an outage that the web UI shows ("Builds cannot start"). Devices keep the screens they have. Install the tools to build recipes in a sandbox (see above), or give `serve` a `-render-url`.

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

The renderer is the `trmnl-render` shim, from `cmd/trmnl-render`, next to trmnlp. It is the stronger option, with a deployment that gives its isolation. `serve -render-url` sends builds to it instead of to a sandbox. `deploy/renderer/Dockerfile` builds it on the `trmnl/trmnlp:v0.34.1` image, and that image's Dockerfile, in the usetrmnl/trmnlp repository at tag v0.34.1, is the reference list of what trmnlp needs:

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

Or run it inside the server's process with `serve -proxy-listen :3128`. Sandboxed builds need neither. A server with `-render-url` and no `-proxy-listen` says at startup that the proxy must run apart. It never starts one on its own.

`proxy -forward-listen :8081 -forward-to renderer:8081` also forwards each TCP connection on port 8081 to the renderer, for a server that shares no network with it, as in Compose. The target is fixed by the flag.

### Connecting the server

`serve -render-url` points the server at the renderer, directly or through the proxy's forward. The renderer answers `102 Processing` when it starts a build. Anything in between that drops 1xx responses, such as an HTTP reverse proxy, makes every build fail with an error that says so. Use a direct connection or a TCP forward.

### What every deployment must give it

These properties keep a recipe from reaching anything it should not. Compose gives all of them. Any other way of running the renderer must give them too:

- It runs as a user of its own, which runs nothing but the shim, with no access to the database, the secrets key, the data directory or the server's environment.
- Nothing a build writes or starts outlives it. Its user can write nowhere but scratch places that are the renderer's own, which the shim empties after each build.
- Its only network path is the proxy's address. It cannot reach the internet, the LAN, services on the host's loopback, or the server's listeners. The server's `/api/setup` gives a key to any MAC while the join window is open, so the renderer must not be able to reach it.
- Its process count, memory and privileges are limited.

The sandbox on the host gives the first three, except that it has no user of its own. It limits memory and privileges, but limits processes only with `-render-cgroup` (see [What the sandbox does not give](#what-the-sandbox-does-not-give)).

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
