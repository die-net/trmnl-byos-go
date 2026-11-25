# Development

This guide covers building and testing trmnl-byos-go. Read [architecture](architecture.md) before changing how something works, and `CLAUDE.md` at the repository root for the project's rules.

## Building

Go 1.26 or newer. There are two binaries:

```bash
go build -o trmnl-byos ./cmd/trmnl-byos
go build -o trmnl-render ./cmd/trmnl-render
```

The server builds with either SQLite driver:

- With CGO on, the default, it uses `github.com/mattn/go-sqlite3`, which needs a C toolchain.
- With `CGO_ENABLED=0` it uses `modernc.org/sqlite`, which needs none and makes a static binary. The Docker images build this way.

Code must work with both. To run what you built, see [setup](setup.md).

## Testing

```bash
go test ./...
CGO_ENABLED=0 go test ./...
```

CI runs both, on the oldest supported Go (1.26) and the newest (1.27), and runs `go test -race ./...` with CGO on. It also runs `govulncheck`.

The web UI's tests run its script in Node.js, so `node` must be on the path. Without it they fail rather than skip, so the script never goes unchecked.

Some of the renderer's tests run only on Linux, since the shim lists processes and mounts from `/proc`, and sandboxed builds exist only there.

On Linux, `go test ./...` runs sandboxed builds with a fake trmnlp (`internal/render/sandbox_linux_test.go`). They cover a build, each failure (trmnlp fails, the fetch fails, a malformed project, a timeout, over memory), a canceled build, a sandbox that cannot start, two builds at once, the proxy listener, and the sandbox library's check. They skip, saying why, where no sandbox can start: there is no bubblewrap, or the host is Ubuntu 24.04 or later and the test binary has no AppArmor profile with a `userns` rule. Run `trmnl-byos doctor` to get a profile and the commands to load it, and adapt its path to the test binary. CI's Ubuntu runners have bubblewrap, and Ubuntu's own AppArmor profile for it, which lets it make a user namespace but takes the capabilities a sandbox's first stage needs. The library's probe passes there, so the tests also start a sandbox with a trivial command, see it fail, and skip, saying so. They run only where a developer runs them.

### Tests against a real renderer

Tests with the `trmnlp` build tag build small recipes with the real trmnlp (the file is `internal/render/trmnlp_test.go`). They need the internet. Without `TRMNL_RENDER_URL`, they run real builds in a sandbox on this host, so trmnlp and its tools must be on the `PATH` (see [setup](setup.md#builds-in-a-sandbox-on-the-host)). A host that cannot run them fails them, since they run only when asked for:

```bash
go test -tags trmnlp ./internal/render/
```

With `TRMNL_RENDER_URL`, they build in that renderer. Start one (see [Running the renderer](#running-the-renderer)), then:

```bash
TRMNL_RENDER_URL=http://localhost:8081 go test -tags trmnlp ./internal/render/
```

The shim refuses a build while another runs, so these tests do not run in parallel. `TestRendererWebGL` checks that a page that draws with WebGL is not blank. `TestRendererRefusesOwnBuilds` runs only against the shim.

## Lint

```bash
golangci-lint run
```

The linters are set in `.golangci.yml`. Lint is clean before work is done.

## Generated code

SQL is in `internal/db/schema.sql` and `internal/db/queries/`. The `*.sql.go` files are generated from them by sqlc 1.30.0. Change the SQL, never the generated files, then regenerate:

```bash
go generate ./internal/db/
```

`internal/profile/registry/` is a copy of TRMNL's model and palette registry, from `trmnl.com/api/models` and `trmnl.com/api/palettes`. Refresh it with:

```bash
go generate ./internal/profile/
```

It fetches with `curl`.

## Running the renderer

`serve` with no `-render-url` builds recipes in a sandbox on the host. That needs bubblewrap and the tools on `serve`'s `PATH` (see [setup](setup.md#builds-in-a-sandbox-on-the-host)), and `trmnl-byos doctor` checks them. The rest of this section is about the renderer run apart with `-render-url`.

The renderer runs only on Linux. To run it on its own, build its image from the repository root and run it without an init process, since the shim is process 1 and reaps its own children:

```bash
docker build -f deploy/renderer/Dockerfile -t trmnl-byos-render .
docker run --rm --read-only --tmpfs /tmp:mode=1777,size=1g -p 8081:8081 trmnl-byos-render
```

The shim refuses to start if its user can write anywhere but its scratch directories. The image leaves directories writable that a build could plant files in for later builds to run, so the root file system must be read-only. This command leaves the renderer on Docker's default network, with a direct path to the internet and the LAN. That is fine for development, and not for real use (see [setup](setup.md#what-every-deployment-must-give-it)).

To build recipes through it, give the renderer a proxy and point a server at it:

```bash
trmnl-byos serve -data data -render-url http://localhost:8081 -proxy-listen :3128
```

The renderer needs `HTTP_PROXY` and `HTTPS_PROXY` set to that proxy. On `docker run`, add `-e HTTP_PROXY=http://HOST:3128 -e HTTPS_PROXY=http://HOST:3128`, where `HOST` is an address of your machine the container can reach. Or run `trmnl-byos proxy` on its own.

The full Compose setup, with the renderer isolated, is `docker compose -f deploy/compose.yaml up -d --build` (see [setup](setup.md#docker-compose)).

### What the shim needs outside a container

Outside a container, the shim needs trmnlp and its dependencies, and `Xvfb` on its `PATH` (see [What it needs](setup.md#what-it-needs)). It also needs a user account that runs nothing else. After each build it kills every other process of its user, and removes every file its user owns in `/tmp`, `/var/tmp`, `/dev/shm`, `/dev/mqueue` and the account's home.

It refuses to start:

- as root;
- while any other process of its user runs;
- if its user can write anywhere else;
- if another user owns anything in those scratch directories.

So it needs its own `/tmp`, `/var/tmp`, `/dev/shm` and `/dev/mqueue`. [Setup](setup.md#renderer-under-its-own-user-account) lists the systemd settings that give that.

## Debugging devices

`serve -debug` logs each device request's headers, without its token. A request captured from a real device can become a test fixture, with the test's own MAC and token in place of the device's.
