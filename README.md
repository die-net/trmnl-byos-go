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

For now every device shows a "Connected" screen with its friendly ID and a row of gray swatches. A panel in its gray mode shows each swatch as a different shade.

```
./trmnl-byos calibrate -data data -for 30m ABC123
```

`calibrate` shows a device a gray calibration screen from its next wake, for 30 minutes by default; press the device's button to wake it now. Each row is one of the panel's grays over strips of black-and-white dither. The box in each row whose halves match from arm's length says how dark that gray really looks, in sixteenths. `-dot 2` draws the dither with 2x2-pixel dots, which checks whether the panel blurs single pixels. Measured levels go in the firmware model table in `internal/profile`.

## License

Copyright 2026 Aaron Hopkins and contributors. All rights reserved.

Licensed under the [Apache License, Version 2.0](LICENSE).
