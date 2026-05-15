# CLAUDE.md

Project-specific guidance for Claude Code.

## Project Overview

`snoringcat-platform` is the shared backend + Godot client SDK for
Snoring Cat Games multiplayer titles. One AWS stack, one Godot
addon, consumed by N game projects via git submodule.

This repo is the result of extracting reusable infrastructure out
of `hopnbop_private`. Hop 'n Bop is currently the only consumer
but the system is built to host multiple games.

The active restructure plan lives at
`~/.claude/plans/in-general-i-ve-been-snoopy-pearl.md` (user's
machine). Check there for the current phase and next steps.

## Architecture

### Runtime (`runtime/`)

Server-side platform code is a Go plugin loaded by Nakama at
startup, mounted at `/nakama/data/modules/snoringcat.so`. Source
in `runtime/`. The plugin registers RPCs (auth, presence,
match lifecycle, version check, etc.) and a `MatchmakerMatched`
hook that allocates Edgegap deployments for matched players.

Required runtime env vars when the matchmaker hook is enabled
(i.e. `EDGEGAP_TOKEN` is set):
- `EDGEGAP_APP_NAME` — game-server app name in Edgegap (e.g.
  `hopnbop-server`).
- `EDGEGAP_APP_VERSION` — version tag (e.g. `v8`).
- `NAKAMA_GAME_VERSION` — display version surfaced via
  `version_check` RPC; matched against client `config/version`.

Persistent state lives in Postgres (Nakama's standard schema:
users, friends, leaderboards, storage, parties, etc.). No
custom tables; the runtime plugin operates on Nakama's
built-in models via `nk.StorageWrite` and friends.

The previous design (AWS SAM + DynamoDB) was retired in
Phase F (2026-05-03). For archeology of the original
per-game DynamoDB schema, see git history of `backend/`
prior to the deletion commit.

### Client SDK (`addons/snoringcat_platform_client/`)

Single autoload `Platform`. Initialized by the consuming game with
`Platform.initialize(game_id, api_base_url, sdk_version)`.
Game code never reaches into addon internals; calls go through:

- `Platform.auth.*` — sign-in, sign-out, token refresh
- `Platform.account.*` — profile, link/unlink identity, delete
- `Platform.friends.*` — list, add by code, accept/decline
- `Platform.party.*` — create, invite, leave
- `Platform.presence.*` — set rich presence, read friend presence
- `Platform.settings.*` — local + cloud sync, scoped global/game
- `Platform.matchmaking.*` — start, poll, cancel
- `Platform.session.*` — connect, disconnect (delegates to
  the session-provider classes in the consuming game's `src/core/`)
- `Platform.screens.*` — auth screen, consent screen, etc.

## Versioning

- **Runtime plugin** is built per-commit; the runtime_status
  RPC reports `build_id` (git SHA) so a deployed plugin is
  always traceable to the source commit.
- **Client SDK**: semver tags on this repo (`v1.4.0`). MAJOR
  ⇒ breaking. MINOR ⇒ additive. PATCH ⇒ fix. Each game pins
  the SDK at a SHA via the submodule pointer.
- **Game `protocol_version`**: per-game, governs realtime
  client/server protocol. Independent of platform version.

## Sibling repos

- `godot-rollback-netcode` — generic rollback netcode framework.
- `godot-gamelift-session-manager` — historical (named for the
  AWS GameLift integration that's now retired). Most of the
  addon was deleted from `hopnbop_private` during the migration.
  The repo's role going forward is undecided; either archive or
  re-scope as a generic "session provider" abstraction
  (Edgegap-targeted or vendor-neutral).

These are consumed by each game alongside the platform SDK. They
are version-independent from the platform.

## Code Style

Follows the same conventions as `hopnbop_private`:
- Runtime Go: standard `gofmt`, `staticcheck` clean (the
  `nakama-runtime` workflow runs both).
- GDScript: Godot style guide + 80-char lines, tabs, `not` over `!`,
  parens for line wrapping, period-terminated comments. See the
  hopnbop_private CLAUDE.md for full conventions.

## Testing

- **Runtime**: `go test ./runtime/...` (when tests exist —
  most of the runtime is currently smoke-tested via the live
  `runtime_status` probe and the consuming game's nightly smoke
  workflow rather than per-package unit tests).
- **Client SDK**: GUT, headless. The compliance suite at
  `addons/snoringcat_platform_client/test/compliance/` is the
  contract every consuming game runs in its own CI on submodule
  bump.

## Infra (`infra/`)

Platform-shared infrastructure for the Hetzner host(s) running
Nakama, Postgres, and observability.

- `infra/pulumi/snoringcat-platform/` — Pulumi IaC for Hetzner
  Cloud (private network, servers, firewalls) + Cloudflare DNS.
  Stack name `prod`, state in Cloudflare R2 bucket
  `hopnbop-pulumi-state-r2` (S3-compat backend). Pulumi reads
  `R2_ACCESS_KEY_ID`/`R2_SECRET_ACCESS_KEY`/`R2_ENDPOINT` from
  `~/.hopnbop-migration/credentials.env`; `phase-a.ps1`
  routes those into `AWS_*` env vars before invoking Pulumi.
  Stack-level values (`zoneName`, `location`, `networkZone`,
  `serverType`, `image`) live in `Pulumi.<stack>.yaml`;
  defaults in `main.go` are fallbacks for stacks that don't
  override. Private IPs (`10.0.1.10`/`10.0.1.20`) and the
  `/16` network range stay hardcoded — changing them needs a
  state migration, not just a config swap.
- `infra/remote/nakama/` — Nakama + Caddy + Prometheus/Grafana/
  Loki/Promtail. `docker-compose.yml`, `Caddyfile`, scrape
  configs, dashboards. Deployed to `/opt/nakama/` on the
  Nakama host.
- `infra/remote/postgres/` — Postgres docker-compose +
  `pg_hba.conf`. Deployed to `/opt/postgres/` on the Postgres
  host.
- `infra/remote/cost-monitor/` — hourly systemd timer for
  Hetzner + Edgegap + R2 + GitHub Actions MTD spend. Deployed
  to `/opt/snoringcat/cost-monitor/`. Discord summary daily
  at 09:00 UTC; threshold crossings ping immediately;
  emergency cap PATCHes Edgegap to `capacity_max=0`. Full
  details in `PLATFORM_ARCHITECTURE.md` → "Cost monitor".

## Scripts (`scripts/`)

Platform-shared automation scripts. Run from the snoringcat-platform
repo root (or the consuming game's submodule path).

- `phase-a.ps1`, `phase-b.ps1` — provisioning orchestrators.
  `phase-a` brings up Hetzner servers + private network +
  Postgres + Nakama + Caddy via Pulumi and `scp`. `phase-b`
  layers on observability (Prometheus/Grafana/Loki/Promtail)
  + UptimeRobot + cost-monitor. Both idempotent with
  `-StartAt`/`-StopAt` step gates.
- `test-google-auth.py`, `platform_smoke_test.gd` — live
  smoke tests for the auth flow and the addon's API client.
- `probe-runtime-status.ps1` — calls the `runtime_status`
  RPC on the live Nakama runtime and pretty-prints the
  build_id, registered RPCs, and Edgegap config.

Operator-local state for the migration scripts lives at
`~/.hopnbop-migration/` (state.json, credentials.env, SSH
keys); the path is hopnbop-named for historical reasons but
each game can point its own scripts at a different
`$MigDir` if needed.

## Runtime modules (`runtime/`)

Go modules loaded by Nakama at startup. Built into a `.so`
plugin via the `heroiclabs/nakama-pluginbuilder` Docker image
and mounted at `/nakama/data/modules/snoringcat.so` on the
Hetzner host.

Layout:
- `main.go` — `InitModule` entry point.
- `fleet_allocator.go` — `MatchmakerMatched` hook that
  allocates Edgegap deployments for matched players.
- `match_lifecycle.go` — `register_server` / `match_end` RPCs.
- `auth.go`, `presence.go`, `version.go`, `player_data.go`,
  `bulk_import.go`, `client_ip.go` — supporting RPCs.
- `runtime_status.go` — read-only build/config probe.

**Required env vars when matchmaker hook is enabled
(`EDGEGAP_TOKEN` set):**
- `EDGEGAP_APP_NAME` — game-server app name in Edgegap (e.g.
  `hopnbop-server`). No default — the runtime fails fast if
  unset, since it's platform-shared and has no game default.
- `EDGEGAP_APP_VERSION` — version tag (e.g. `v3`). Same.

When the hook is disabled (`EDGEGAP_TOKEN` unset), the runtime
still loads and registers RPCs, just without fleet allocation.

**Build (locally):**
```bash
cd runtime
docker run --rm -v "$(pwd):/backend" -w /backend \
  heroiclabs/nakama-pluginbuilder:3.25.0 \
  build -buildmode=plugin -trimpath \
  -o ./build/snoringcat.so .
```

**CI:** `hopnbop_private/.github/workflows/nakama-runtime.yml`
and the `nakama-runtime` job in `release.yml` build and SCP
this plugin to the Nakama host. Both check out the platform
repo as a submodule via `SUBMODULE_PAT`.

**Naming:** the directory is `runtime/` (not
`nakama-runtime/`) so future non-Nakama runtime modules can
live alongside it without an awkward dir rename.

## What is NOT in this repo

- Game logic — stays in each game's repo.
- Per-game GameLift fleet config and server build — each game owns
  its own `gamelift-deploy/` directory and Dockerfile.
- The patched WebRTC GDExtension build — lives in
  godot-gamelift-session-manager.
