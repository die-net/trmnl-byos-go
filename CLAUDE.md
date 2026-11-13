# trmnl-byos-go

trmnl-byos-go is a self-hosted server for TRMNL e-ink displays, in Go with SQLite. It speaks the TRMNL firmware protocol, schedules and pre-renders each device's screens around its wake times, and renders TRMNL recipes through a trmnlp sidecar. The design is `docs/design.md`. Read the sections a change touches before making it. A change to behavior it describes updates it in the same PR. The README has the build and test commands.

## Checks

- Go: `go test ./...`. CI also runs it with `CGO_ENABLED=0`, which uses the other SQLite driver, so code must work with both.
- Lint: `golangci-lint run`. It is clean before work is done, including issues you did not cause.

## Devices

- A device is known by the `api_key` it got from `/api/setup`, sent as `Access-Token`. Only the key's SHA-256 is stored, and a device is looked up by that hash. The `ID` header is optional, but when present it must be the device's MAC.
- `/api/setup` works only while the join window is open, for a known MAC too, since anything on the LAN can see a MAC. Each setup issues a new key, which retires the old one.
- A firmware build is offered only to a device whose `Model` header names the exact firmware model the build is for, never by wildcard, family, or "latest". The ESP image header's chip and the app description's version are checked on upload. The firmware does not check the board, so this check is all that stops a wrong-board update.
- Firmware is never offered as a downgrade, to a device with updates off or pinned, or to more than the canary device until it reports the new version.
- `/api/setup` and `/api/display` answer HTTP 200 with the outcome in the JSON `status` field, which is what the firmware reads. `/api/display` sends 0 for a normal response, never 200, which the firmware ignores.
- Status 500 and `reset_firmware: true` erase a device's key, Wi-Fi settings and server URL, and start its setup portal. `reset_firmware` is sent only to a deleted device, or to a registered MAC whose key was replaced by a later setup, which would otherwise stay stuck on its old key. A request from an unknown MAC or with no `ID` gets 202, never a reset: it may mean the server lost its database. Status 500 is never sent.
- No state may leave a device showing stale data indefinitely, or stuck where it cannot recover on its own. For a known device, a reset that sends it back through setup is better than leaving it there. Only an unknown device, where a reset could wipe every display after a server mistake, is left to poll.
- A response field the firmware reads as a number, such as `refresh_rate`, is encoded as a JSON number.
- A rendered image's `filename` is its content hash, so the firmware skips a download or repaint it has already done. It stays within the 31 characters the firmware keeps.
- Whatever hands out an image name stores the image through `imagestore.Store.Put` first, and a row that keeps an image in use is listed in `ListReferencedImages`. Image expiry depends on both.

## Security

- Recipes are untrusted code from the internet. Their transforms and page scripts run only in the renderer container, which never gets the database, the secrets key, or the data volume.
- Recipes may reach public hosts only. The renderer has no route out except the server's proxy, which refuses non-public addresses. A URL the server itself fetches that came from a recipe, its settings, or a webhook goes through the outbound client that makes the same check, never through `http.DefaultClient` or a client built for the call.
- SQL lives in `internal/db/queries/` and runs through sqlc. A query sqlc cannot handle, such as a migration, may bypass it: it lives in `internal/db` as a constant string with `?` parameters, and a comment says why sqlc cannot take it. No SQL is built from strings anywhere.
- A command runs through `exec.CommandContext` with an argument list, never through a shell such as `sh -c`.
- A file name or path from a recipe, an upload, or a request is cleaned and checked to stay inside its directory before it reaches the filesystem.
- A secret custom field is stored encrypted and never shown back or logged. Neither is a device `api_key`, an admin token, or a password hash.
- Every admin, API, and MCP route requires a signed-in session or a bearer token. Device routes accept only a device `api_key`.
- Nothing from a real deployment goes into the repo: no MAC, friendly ID, API key, Wi-Fi name, IP address, or hostname. A request captured from a real device becomes a fixture with the test's own MAC and token, and docs use placeholders such as `ABC123`.

## Database

- The schema is `internal/db/schema.sql` and queries are in `internal/db/queries/`. Do not edit the generated `*.sql.go` files. Change the SQL and run `go generate ./internal/db/` with sqlc 1.30.0.
- Times are stored as Unix seconds in `INTEGER` columns.
- A read followed by a write that depends on it runs in `Client.InTx`. Inside `fn`, read through `q`, never through `Read`, and never call `Write`.

## Go

- Wrap errors with a short lowercase context: `fmt.Errorf("save device: %w", err)`.
- Log with `log/slog`.
- Tests use the standard `testing` package, not testify. Several cases go in a table: `for _, tc := range []struct{...}{...}`.
- Every exported identifier has a doc comment. A field whose meaning or zero value is not obvious has one too. Comments say what a thing is for and why, in full sentences.
- Encode JSON from a struct with `json` tags, not from `map[string]any`. An anonymous struct is fine for a one-off payload. Decoding into `map[string]any` to inspect unknown JSON is fine.
- Test-only helpers, flags, and fields live in `_test.go` files, not in production code.
- A rename or move updates every caller. No wrapper, `type X = other.X` alias, or shim is kept for the old name.
- A new dependency comes from the standard library first, then `golang.org/x`, before anything else.

## Fixes

- Fix the general cause, in the parser, the model, or the shared helper, rather than special-casing the one input that broke.
- An `if`, flag, retry, or fallback added only to make one failure go away is not a fix. Find why it fails and fix that. If the root fix is out of reach, say so in the PR rather than patching around it.
- A failing check is never made to pass by weakening it: no new `t.Skip`, deleted or loosened assertion, raised timeout or threshold, `t.Log` in place of `t.Fatal`, `_ =` on an error, or `//nolint`, unless the owner agreed to that specific change.
- A `//nolint` is a last resort for code that is correct as written. It names the linter and says why the code is correct. For a gosec G115 integer conversion, give the value a type that already fits rather than silencing the cast.
- A new field, flag, config option, or endpoint is added only when no existing one can carry it. Prefer deleting or generalizing code to adding it.
- A behavior change comes with a test that exercises the failure mode or the integration path, not only the success path.

## Writing

- A commit subject is one imperative sentence about the change in behavior, ending with a period, for example "Refuse a firmware build whose chip does not match the device's model." Do not use Conventional Commits prefixes.
- Docs, comments, and messages people see use short, plain sentences.
