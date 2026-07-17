# snoringcat-platform

Shared infrastructure for Snoring Cat Games multiplayer titles.

This repo contains:

- **`runtime/`** — Go plugin loaded by Nakama at startup, mounted
  at `/nakama/data/modules/snoringcat.so`. Registers RPCs (auth,
  presence, match lifecycle, version check, GDPR export, etc.)
  and a `MatchmakerMatched` hook that allocates Edgegap
  deployments for matched players.
- **`infra/`** — Pulumi IaC for the Hetzner platform tier
  (`pulumi/`) plus the docker-compose + config bundles that
  ship to the Hetzner hosts (`remote/nakama/`,
  `remote/postgres/`, `remote/cost-monitor/`).
- **`scripts/`** — Phase A/B provisioning orchestrators
  (`phase-a.ps1`, `phase-b.ps1`) and live smoke helpers
  (`probe-runtime-status.ps1`, `platform_smoke_test.gd`,
  `test-google-auth.py`).
- **`addons/snoringcat_platform_client/`** — Godot 4.7 addon
  that wraps the Nakama HTTP/realtime API, plus reusable UI
  building blocks (auth screen, friends panel, party UI,
  side-panel framework, screen state machine, focus navigator,
  settings persistence). Game projects consume this via git
  submodule.
- **`docs/`** — API spec, client SDK guide, per-game
  configuration reference.

## Repo layout

```
runtime/                            # Nakama Go plugin
  main.go                             # InitModule entry point
  fleet_allocator.go                  # MatchmakerMatched → Edgegap
  match_lifecycle.go                  # register_server, match_end
  auth.go, presence.go, version.go,   # supporting RPCs
  player_data.go, bulk_import.go,
  client_ip.go
  runtime_status.go                   # build/config probe
infra/
  pulumi/snoringcat-platform/         # Hetzner + Cloudflare DNS IaC
  remote/nakama/                      # Nakama+Caddy+Prom/Loki compose
  remote/postgres/                    # Postgres compose + pg_hba
  remote/cost-monitor/                # systemd timer + script
scripts/
  phase-a.ps1, phase-b.ps1            # provisioning orchestrators
  probe-runtime-status.ps1            # live runtime status RPC probe
  platform_smoke_test.gd              # client SDK smoke
  test-google-auth.py                 # Google OAuth flow smoke
addons/snoringcat_platform_client/
  core/                               # Auth, API clients, settings
  ui/                                 # Screens, panels, overlays
  input/                              # Device manager, focus navigator
  util/                               # Viewport, fonts, localization
  translations/                       # Platform-only translation strings
  test/compliance/                    # GUT tests games run on submodule bump
  plugin.cfg
docs/
  api-spec.md
  client-sdk-guide.md
  legacy-hopnbop-api.md               # historical AWS SAM API reference
  archive/                            # Phase E/F migration archeology
    per-game-config.md                # pre-Phase-F DynamoDB schema; superseded
                                      #   by the Postgres `games` table
                                      #   (runtime/per_game_config.go)
```

## Architecture

The platform sits between game clients and game-server
containers, on three vendors:

- **Hetzner** — Nakama + Postgres (co-tenanted) + Caddy/TLS
  on a single CPX21 box in Hillsboro (upsized from CPX11
  on 2026-06-01). The 2026-05-06
  consolidation collapsed the original two-box layout (with
  full Prometheus/Grafana/Loki/Promtail stack) into one box
  with the obs stack stripped; visibility is now ad-hoc via
  the daily Claude prod-health-check job + UptimeRobot +
  cost-monitor Discord. Static spend ~$8/mo.
- **Edgegap** — game-server containers (the consuming game's
  Linux export, packaged via the game's `Dockerfile.edgegap`),
  allocated on demand by the runtime's matchmaker hook.
  Pay-per-active-session.
- **Cloudflare** — DNS, Pages (web client), R2 (heavy web
  assets). Free tier for our scale.

The previous design (AWS SAM Lambda + API Gateway + DynamoDB)
was retired 2026-05-03 (Phase F of the migration documented in
the consuming game's `MIGRATION_PLAN.md`). Git history of
`backend/` prior to its deletion captures the original schema.

## Versioning

The Go runtime plugin is built per-commit; the `runtime_status`
RPC reports the build SHA so a deployed plugin is always
traceable to source. Client SDK semver tags on this repo
(`v1.4.0`); each consuming game pins via the submodule pointer.
Game `protocol_version` is per-game and unrelated to the
platform SDK version.

## Consumed by

- [hopnbop](https://github.com/SnoringCatGames/hopnbop)

## Sibling repos

- [godot-rollback-netcode](https://github.com/SnoringCatGames/godot-rollback-netcode)
- [godot-gamelift-session-manager](https://github.com/SnoringCatGames/godot-gamelift-session-manager)
  (historical; scope being re-evaluated post-migration)
