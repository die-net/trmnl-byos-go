# trmnl-byos-go

A self-hosted server for TRMNL e-ink displays, in Go with SQLite. The design is in [docs/design.md](docs/design.md).

## Build

Go 1.26 or newer. CGO builds use `github.com/mattn/go-sqlite3`. `CGO_ENABLED=0` uses `modernc.org/sqlite`, which needs no C toolchain and makes a static binary. CI tests both.

```
go test ./...
CGO_ENABLED=0 go test ./...
golangci-lint run
```

`internal/profile/registry/` is a copy of TRMNL's model and palette registry. Refresh it with:

```
go generate ./internal/profile/
```

SQL is in `internal/db/schema.sql` and `internal/db/queries/`. Regenerate with sqlc 1.30.0:

```
go generate ./internal/db/
```

## Run

```
go build -o trmnl-byos ./cmd/trmnl-byos
./trmnl-byos serve -data data -listen :8080
```

`serve` keeps its database and images under `-data`. Point a device at `http://<server>:8080` in its Wi-Fi setup portal, then let it register:

```
./trmnl-byos join -data data -for 10m
```

The join window admits devices until it closes. A device that tries while it is closed shows "Not registered" with its MAC and tries again every 5 minutes. A device that lost its credentials, after a Wi-Fi reset for example, needs the window open again to re-pair. It keeps its friendly ID. `./trmnl-byos devices -data data` lists registered devices with their battery, signal and wake time. `serve -debug` logs each device request's headers, without its token.

A device with nothing scheduled shows a "Connected" screen with its friendly ID and a row of gray swatches. A panel in its gray mode shows each swatch as a different shade.

```
./trmnl-byos calibrate -data data -for 30m ABC123
```

`calibrate` shows a device a gray calibration screen from its next wake, for 30 minutes by default; press the device's button to wake it now. Each row is one of the panel's grays over strips of black-and-white dither. The box in each row whose halves match from arm's length says how dark that gray really looks, in sixteenths. `-dot 2` draws the dither with 2x2-pixel dots, which checks whether the panel blurs single pixels. Measured levels go in the firmware model table in `internal/profile`.

## Recipes and playlists

Recipes are rendered by trmnlp in a separate, isolated renderer, which reaches the internet only through the server's proxy. Compose is the way that ships today:

```
docker compose -f deploy/compose.yaml up -d --build
```

`deploy/compose.yaml` lists its settings: `PUBLIC_URL`, `TZ`, and `TRMNL_SECRET_KEY` for secret recipe fields. Put them in `deploy/.env`, which git ignores. Never publish the proxy's port, 3128.

Install a recipe from a directory or a zip, such as one from TRMNL's recipe pages or GitHub, then give it settings and put it on a device's playlist:

```
trmnl-byos recipe add path/to/recipe.zip
trmnl-byos instance add -recipe 1 -field city=Zurich -secret api_key=...
trmnl-byos playlist add -device ABC123 -instance 1 -duration 15m
```

In Compose, run these with `docker compose -f deploy/compose.yaml exec server trmnl-byos ...`, after copying the recipe in with `docker compose ... cp`. Each `playlist add` appends an item to the device's playlist. A timer wake moves to the next item once the current one's time is up, and a button press moves on at once. `-duration 0` keeps an item until a press. `instance add -palette bw` renders in black and white on a panel that also takes grays, which keeps its refreshes partial. `recipe list`, `instance list` and `playlist list` show what is set up. `playlist remove` takes an item out, and `instance remove` deletes an instance and its renders once no item shows it.

To run the renderer alone, build its image from the repository root and run it with an init process:

```
docker build -f deploy/renderer/Dockerfile -t trmnl-byos-render .
docker run --rm --init -p 8081:8081 trmnl-byos-render
```

With one running, `TRMNL_RENDER_URL=http://localhost:8081 go test -tags docker ./internal/render/` builds a small recipe in it. `serve -render-url` points the server at a renderer, and the renderer needs `HTTP_PROXY` and `HTTPS_PROXY` set to the server's `-proxy-listen` address.

## License

Copyright 2026 Aaron Hopkins and contributors. All rights reserved.

Licensed under the [Apache License, Version 2.0](LICENSE).
