# trmnl-byos-go

A self-hosted server for [TRMNL](https://trmnl.com) e-ink displays, in Go with SQLite. It runs TRMNL recipes through trmnlp, TRMNL's own plugin tool, and renders each screen ahead of the device's wake. The server is one binary and one database file. Recipes run in a sandbox on the same host, or in a separate renderer.

## Why I built it

I liked the TRMNL idea from the start: a quiet e-ink screen on the wall that shows what you care about and lasts months on a charge. I wanted to build my own, though. I bought a couple of Seeed Studio's 7.5" OG DIY kits and 3D-printed cases for them.

TRMNL makes that easy. Its firmware is open source, and any device can point at a server you run yourself. I tried several of the community's self-hosted servers, and learned a lot from each. Then I wanted to try a different set of trade-offs:

- **Recipes should look the way they do on TRMNL.** This server never parses Liquid. It hands each recipe to [trmnlp](https://github.com/usetrmnl/trmnlp), TRMNL's own plugin development tool, which uses TRMNL's Liquid gem, its page wrapper and its framework versions, and runs transforms in Ruby, Python, JavaScript and PHP.
- **Recipes are untrusted code.** They come from the internet and run code of their own. So each build runs in a sandbox, or in a renderer run apart from the server, with no access to the server's database or secrets. Its only way out is a proxy that refuses private addresses.
- **A device's wake should be short.** Recipe screens are rendered ahead of each wake, so a screen that hasn't changed costs one small request and no download or repaint.
- **Firmware updates should never brick a board.** A build is offered only to devices of the exact firmware model it was made for, one canary device first.
- **Running it should be boring.** The server is one Go binary and one SQLite file, with no database or queue server.

## TRMNL's own service

TRMNL sells ready-made devices, starting at $139 for the OG, with a cloud service that has over 1,000 plugins and needs no setup. If you want a screen that just works, buy one. It is a good product, and its makers support the open-source side generously: the firmware is GPLv3, the design framework is open, and TRMNL lists the self-hosted servers in its own docs.

A self-hosted server is for people who want to run everything themselves, keep their data on their own network, or build their own hardware. With this one, you give up TRMNL's native plugins and anything that needs its cloud, such as OAuth plugins. You keep TRMNL recipes, the community plugins written in Liquid.

## Other self-hosted servers

TRMNL's [BYOS page](https://docs.trmnl.com/go/diy/byos) lists the community's servers. Each one is a good fit for someone, and this one borrows ideas from several of them:

- Terminus: per-device button commands and sleep windows.
- Inker: drift-free playlist timing, encrypted plugin secrets and the device blocklist.
- LaraPaper: the time-limited join window and scheduled playlists.

The trade-offs are in [Alternatives considered](docs/architecture.md#alternatives-considered).

This is how they compare, as of November 2026, from each project's own docs:

| Server | Built with | How it draws recipes and screens | What it runs | Stands out for |
| --- | --- | --- | --- | --- |
| trmnl-byos-go | Go, SQLite | trmnlp, in a sandbox on the host or in an isolated renderer | One binary plus trmnlp's packages, or 3 containers in Compose | Recipe fidelity, build isolation, per-model firmware rollouts |
| [Terminus](https://github.com/usetrmnl/terminus) | Ruby, Hanami, PostgreSQL | Its own Liquid in a headless browser | Web, worker, PostgreSQL and Valkey | The flagship: multi-user accounts, sensors, official firmware and model syncing |
| [LaraPaper](https://github.com/usetrmnl/larapaper) | PHP, Laravel, SQLite | Liquid or Blade in headless Chrome | One container | Recipe catalogs, a proxy to TRMNL's cloud, an MCP server, wide device support |
| [Kuroshiro](https://github.com/usetrmnl/kuroshiro) | TypeScript, NestJS, PostgreSQL | TRMNL's framework in headless Chromium | App and PostgreSQL | Recipe import with transforms, mashups, firmware tied to compatible models, a guard against private addresses |
| [Inker](https://github.com/usetrmnl/inker) | TypeScript, NestJS, React, SQLite | Headless Chromium | One container | A drag-and-drop screen designer for homelab dashboards |
| [BYOS Next.js](https://github.com/usetrmnl/byos_next) | Next.js, React, PostgreSQL | Satori-style components, or a headless browser | App and PostgreSQL, or serverless | Easy deployment, including on Vercel |
| [BYOS FastAPI](https://github.com/usetrmnl/byos_fastapi) | Python, FastAPI, SQLite | Python plugins drawn with Pillow | One process | Careful dithering and tone curves for e-ink |
| [BYOS Phoenix](https://github.com/usetrmnl/byos_phoenix) | Elixir, Phoenix, SQLite | Phoenix pages in a headless browser | One app | A small, readable reference implementation |

If you want multi-user accounts, Terminus is a mature choice. For a large recipe catalog, look at LaraPaper. For a screen designer, look at Inker.

## Devices

TRMNL's [DIY device page](https://docs.trmnl.com/go/diy/byod) lists the hardware that runs TRMNL's firmware or a TRMNL client. This server knows these firmware models by name, with their panel size, palettes and image size limit:

- TRMNL OG, OG 4-color and OG second generation (`og`, `og_4clr`, `og_gen2`, `og_gen2_4clr`)
- TRMNL X (`x`)
- Seeed Studio's 7.5" OG DIY kit (`xiao_epaper_display`), with its larger image limit and its grays measured
- Seeed reTerminal E1001, E1002, E1003 and E1004
- M5PaperS3 (`m5_papers3`)
- Waveshare 7.5" black and white (`waveshare`)

A device whose firmware reports a model name from TRMNL's registry, such as `xteink_x3` or `seeed_sticky`, needs no mapping. Other clients that speak the same protocol, such as the KOReader, Kindle and Kobo clients, work at the size they report, in black and white, unless they report a registry model. Any device can be told what panel it has.

Firmware updates are offered for 19 firmware models, each checked against its chip. The list is in [architecture](docs/architecture.md#firmware-updates).

## Status

The server runs devices, recipes, playlists with schedules, firmware rollouts, a web UI and a JSON API today. The first release, v0.1.0, is coming, with prebuilt binaries on the [Releases page](https://github.com/die-net/trmnl-byos-go/releases). Mashups, an MCP server and more are on the [roadmap](docs/roadmap.md).

## Docs

- [Setup](docs/setup.md): install the server and its recipe tools, choose how builds are isolated, and set up a first device.
- [Admin guide](docs/admin.md): the web UI, the command line, playlists and recipes, firmware updates, settings and the JSON API.
- [Architecture](docs/architecture.md): how it works.
- [Roadmap](docs/roadmap.md): what is designed and not built yet.
- [Development](docs/development.md): building, testing and linting.

## AI Disclosure

Though the design, architecture, some of the code, reviews, and testing were
done by a human, most of the code was written by Opus 5.5 and Fable 5.2.

## License

Copyright 2026 Aaron Hopkins and contributors. All rights reserved.

Licensed under the [Apache License, Version 2.0](LICENSE).
