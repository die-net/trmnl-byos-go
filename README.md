# trmnl-byos-go

A self-hosted server for TRMNL e-ink displays, in Go with SQLite. The design is in [docs/design.md](docs/design.md).

## Build

Go 1.26 or newer. CGO builds use `github.com/mattn/go-sqlite3`. `CGO_ENABLED=0` uses `modernc.org/sqlite`, which needs no C toolchain and makes a static binary. CI tests both.

```
go test ./...
CGO_ENABLED=0 go test ./...
golangci-lint run
```

SQL is in `internal/db/schema.sql` and `internal/db/queries/`. Regenerate with sqlc 1.30.0:

```
go generate ./internal/db/
```

## License

Copyright 2026 Aaron Hopkins and contributors. All rights reserved.

Licensed under the [Apache License, Version 2.0](LICENSE).
