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

The join window admits devices until it closes. A device that tries while it is closed shows "Not registered" with its MAC and tries again every 5 minutes. A device that lost its credentials, after a Wi-Fi reset for example, needs the window open again to re-pair. It keeps its friendly ID. `./trmnl-byos device list -data data` lists registered devices with their battery, signal and wake time. `serve -debug` logs each device request's headers, without its token.

`device delete ABC123` (a friendly ID or a MAC) removes a device with its state and renders. Its next request resets it into setup, where it needs the join window to re-pair. `device block ABC123` blocks a MAC, registered or not, and keeps the device's row. A blocked device shows a "Blocked" screen and sleeps a day between requests. `device unblock` lifts the block, and `device blocked` lists the blocked MACs. The admin API has `DELETE /api/v1/devices/{id}`, `GET /api/v1/blocklist`, and `PUT` and `DELETE /api/v1/blocklist/{id}`, where `{id}` is a MAC or a friendly ID. `GET /api/v1/devices` shows `blocked` for each device.

`device set ABC123 -name Kitchen` names a device. `-profile og_bwry` tells the server what panel a device has when its firmware reports another, as a 4-color OG on firmware 1.8.10 or older reports `og`, and `-palette gray-4` picks the palette it renders with when a plugin instance asks for none. `-zone America/Los_Angeles` sets the time zone a device's schedules are read in and its renders are made for, and `-locale de` the locale its renders are made for. Without them a device uses the server's `TZ` and its `locale` setting, which is `en` until `setting set locale CODE` changes it. `-sleep 23:00-06:30` sets a window, read in the device's time zone, in which the device is shown a "Sleeping" screen and no playlist item, and wakes at the window's end. `none` clears any of them. The same settings are in `PATCH /api/v1/devices/{id}`.

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
trmnl-byos instance add -recipe 1 -field city=Zurich -field api_key=...
trmnl-byos playlist add -device ABC123 -instance 1 -duration 15m
```

In Compose, run these with `docker compose -f deploy/compose.yaml exec server trmnl-byos ...`, after copying the recipe in with `docker compose ... cp`. Each `playlist add` appends an item to the device's playlist. A timer wake moves to the next item once the current one's time is up, and a button press moves on at once. `-duration 0` keeps an item until a press. `instance add -palette bw` renders in black and white on a panel that also takes grays, which keeps its refreshes partial. `recipe list`, `instance list` and `playlist list` show what is set up. `playlist remove` takes an item out, `playlist move ITEM INDEX` reorders, `playlist set ITEM -hidden` hides an item, and `playlist set ITEM` or `playlist schedule NAME` with `-days mon,tue`, `-window 09:00-17:00`, `-from` and `-until` limit when an item or playlist is shown (a device shows its first playlist that is scheduled now, so an unscheduled one at the end is the default). `instance add -field k=v` sets a custom field of the recipe (repeat the key for each choice of a multi-select, and the recipe's `password` fields are stored encrypted), and `instance set ID -field k=v -clear k` changes them later (a password is better given with `-field-file k=path`, or `-` for stdin, since `-field` shows it in `ps` and the shell history), which renders the instance again. `instance remove` deletes an instance and its renders once no item shows it.

To run the renderer alone, build its image from the repository root and run it with an init process:

```
docker build -f deploy/renderer/Dockerfile -t trmnl-byos-render .
docker run --rm --init -p 8081:8081 trmnl-byos-render
```

With one running, `TRMNL_RENDER_URL=http://localhost:8081 go test -tags docker ./internal/render/` builds a small recipe in it. `serve -render-url` points the server at a renderer, and the renderer needs `HTTP_PROXY` and `HTTPS_PROXY` set to the server's `-proxy-listen` address.

## Firmware updates

A device installs firmware only when you ask. Only add a build made for the device's exact firmware model, the `Model` header it sends (`firmware list` and `device list` show it). The firmware does not check the board it installs onto, so a build for another board on the same chip installs and can leave the device needing a USB cable. The server checks the chip and the file's structure, not the board.

```
trmnl-byos firmware add -model xiao_epaper_display -version 1.8.19 FW1.8.19.bin
trmnl-byos firmware device -updates on ABC123
trmnl-byos firmware rollout 1 -canary ABC123
trmnl-byos firmware list
```

`firmware add` takes the app image TRMNL publishes for OTA (the `firmware.bin` of a build, not the merged flash image). It accepts only models in the table in `docs/design.md`, and `-version` is the dotted version from the release's file name. Nothing is offered after `add`. `firmware rollout` offers the build to the canary device, and to every device of that model after the canary reports the new version. `firmware cancel 1` stops offering it. A build is offered to a device at most 3 times. If the canary is offered it 3 times and still does not report the version, `firmware list` shows the rollout as `stalled` and nothing is offered to anyone. The `LAST OFFERED` column is when the canary was last told to install the build. Run `firmware rollout` again, with the same canary to retry or another one, or cancel. `firmware device -updates on|off` and `-pin VERSION` or `-pin none` set which devices may be offered firmware, which is none until you turn it on. A device on firmware older than 1.8.17 is never offered an update, since its `Model` may not name its board. The same operations are in the admin API, under `/api/v1/firmware` and `PATCH /api/v1/devices/{id}`:

```
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/octet-stream" --data-binary @FW1.8.19.bin "http://localhost:8080/api/v1/firmware?model=xiao_epaper_display&version=1.8.19"
curl -H "Authorization: Bearer $TOKEN" -d '{"canary": "ABC123"}' http://localhost:8080/api/v1/firmware/1/rollout
```

## Admin API

Everything the commands above do to a running server's data, a script can do over HTTP at `/api/v1`. It is on the same port as the device API, so keep that port on a trusted network. Create a token, which is shown once:

```
trmnl-byos token add -name my-script
```

`token list` shows each token's name and when it was last used, and `token remove my-script` revokes one. Send the token as a bearer token:

```
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/devices
curl -H "Authorization: Bearer $TOKEN" -X PUT -d '{"for_seconds": 600}' http://localhost:8080/api/v1/join
curl -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/zip" --data-binary @recipe.zip http://localhost:8080/api/v1/recipes
curl -H "Authorization: Bearer $TOKEN" -d '{"recipe_id": 1, "fields": {"city": "Zurich", "api_key": "..."}}' http://localhost:8080/api/v1/instances
curl -H "Authorization: Bearer $TOKEN" -d '{"name": "Kitchen"}' http://localhost:8080/api/v1/playlists
curl -H "Authorization: Bearer $TOKEN" -d '{"playlist": "Kitchen", "device": "ABC123", "instance_id": 1, "duration_seconds": 900}' http://localhost:8080/api/v1/playlists/items
```

Bodies and responses are JSON, with times in Unix seconds and durations in seconds. Errors are `{"error": "..."}` with the HTTP status that fits, and a bad custom field value adds `"fields"`, the message for each bad field by name. `PATCH /api/v1/instances/ID` changes `name`, `palette`, `fields` and `mark_secret`, where `""` or `[]` clears a field and `mark_secret` maps a keyname to `true` to store its value encrypted or `false` to unmark it, which needs a new value or a `clear` in the same request, since a secret is never shown back. The routes are listed in `docs/design.md`. Secret fields are never returned, only the names of the ones that are set, in `secret_fields`, and the names marked secret, set or not, in `secret_keys`. `secret_fields` is `null`, not a list, when the server has no `TRMNL_SECRET_KEY` and the instance has secrets, because the names cannot be read without the key.

### Admin password

The web UI signs in with one admin password. Set it, or change it, on the server:

```
trmnl-byos password
```

It asks twice without echo. From a script, pipe the password in as one line: `printf '%s\n' "$PW" | trmnl-byos password`. It must be `min_password_length` to 1024 bytes, and that setting is 12 unless you change it (see Settings). The minimum applies only when a password is set, so a shorter one set earlier still works. If the server can be reached from the internet, use a long one, such as four or more random words: nothing but its length slows someone guessing it. Setting it signs out every browser. A signed-in session also works for `/api/v1`.

### Settings

A few values can change while the server runs, with no restart:

- `refresh_seconds`: how long devices sleep between wakes. Default 900, from 1 to 31536000. A device picks up a change at its next wake.
- `min_password_length`: the shortest new admin password. Default 12, from 1 to 1024.

```
trmnl-byos setting list
trmnl-byos setting set refresh_seconds 600
trmnl-byos setting reset refresh_seconds
```

`list` shows each value, its default, and whether it is the default. A stored value that is not usable shows as `bad`, with a note, and the default is used until you set or reset it. `reset` goes back to the default. The API has `GET` and `PATCH /api/v1/settings`: `curl -H "Authorization: Bearer $TOKEN" -X PATCH -d '{"refresh_seconds": 600}' http://localhost:8080/api/v1/settings`. A field left out of a `PATCH` is kept, and a bad one changes nothing.

### Pushing data to a recipe

A script can push data to a plugin instance, at the path TRMNL uses. The ID is the instance ID, and the token is the secret. The instance's recipe sees the data as top-level variables, and a push makes the instance render again at the next 30 second check.

```
curl -H "Authorization: Bearer $TOKEN" -d '{"merge_variables": {"stories": ["a", "b"]}}' http://localhost:8080/api/custom_plugins/1
curl -H "Authorization: Bearer $TOKEN" -d '{"merge_strategy": "stream", "stream_limit": 20, "merge_variables": {"stories": ["c"]}}' http://localhost:8080/api/custom_plugins/1
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/custom_plugins/1
```

`merge_strategy` is `replace` (the default), `deep_merge` or `stream`, as on TRMNL. A body and the stored data are each limited to 64 KB, a `trmnl` key is refused, and an instance takes a burst of 10 pushes and then 60 an hour.

## License

Copyright 2026 Aaron Hopkins and contributors. All rights reserved.

Licensed under the [Apache License, Version 2.0](LICENSE).
