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

## Architecture (target)

### Backend (`backend/`)

Single AWS SAM stack `snoringcat-platform-backend`. Handlers split
between **global** (no `game_id` needed: auth, accounts, friends,
presence, settings/global) and **game-scoped** (`/v1/games/{game_id}/...`:
parties, matchmaking, match results, leaderboards, profiles, fleet,
sessions).

JWT claims include `{player_id, game_id, exp, ...}`. A
`game_id_resolver` middleware extracts and validates `game_id` and
loads the corresponding `games[game_id]` config row (cached in
warm Lambda memory for ~5 minutes).

DynamoDB tables fall into two groups:

**Global (no game_id in key):**
- `accounts` — player_id PK
- `identities` — provider_external_id PK (e.g. `google#117234...`)
- `friends` — player_id PK + friend_id SK
- `presence` — player_id PK, single row with current game_id and TTL
- `consent_audit` — player_id PK
- `settings` — player_id PK + scope SK (`global` or `game#{game_id}`)

**Per-game (composite key with game_id):**
- `game_profiles` — player_id PK + game_id SK
- `match_history` — player_id PK + `game_id#timestamp` SK
- `leaderboards` — leaderboard_id PK (id encodes game)
- `parties` — party_id PK, party row holds game_id attribute
- `active_sessions` — player_id PK, holds game_id attribute
- `fleet_state` — state_key PK with key `game#{game_id}`

**Config:**
- `games` — game_id PK. Per-game runtime config: display_name,
  icon_url, fleet_id, matchmaker_name, ruleset, physics_fps, etc.

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
  godot-gamelift-session-manager)
- `Platform.screens.*` — auth screen, consent screen, etc.

## Versioning

- **Platform API**: semver tags (`v1.4.0`). MAJOR ⇒ breaking,
  ships as new URL prefix `/v2/`. MINOR ⇒ additive. PATCH ⇒ fix.
- **Client SDK**: same semver, version-locked with backend.
- **Game `protocol_version`**: per-game, governs realtime
  client/server protocol. Independent of platform version.

## Sibling repos

- `godot-rollback-netcode` — generic rollback netcode framework.
- `godot-gamelift-session-manager` — GameLift session provider +
  patched WebRTC GDExtension build.

These are consumed by each game alongside the platform SDK. They
are version-independent from the platform.

## Code Style

Follows the same conventions as `hopnbop_private`:
- Backend Python: PEP 8, type hints required on public APIs,
  Pydantic for HTTP request/response models.
- GDScript: Godot style guide + 80-char lines, tabs, `not` over `!`,
  parens for line wrapping, period-terminated comments. See the
  hopnbop_private CLAUDE.md for full conventions.

## Testing

- **Backend**: pytest + moto (already-established conventions).
  `oasdiff` runs in CI to block breaking `/v1/` schema changes.
- **Client SDK**: GUT, headless. The compliance suite at
  `addons/snoringcat_platform_client/test/compliance/` is the
  contract every consuming game runs in its own CI on submodule bump.

## Migration from hopnbop

The `backend/scripts/migrate-from-hopnbop.py` script (TODO, Phase 1)
performs an in-place rename of the existing `hopnbop-*` DynamoDB
tables and backfills `game_id="hopnbop"` on existing rows. It is
idempotent and supports `--dry-run`.

## Deployment

TODO Phase 1: `backend/scripts/deploy.ps1` ports the existing
`scripts/deploy-backend.ps1` from hopnbop_private but parameterizes
the stack name and table prefixes.

## Infra (`infra/`)

Platform-shared infrastructure for the Hetzner host(s) running
Nakama, Postgres, and observability.

- `infra/pulumi/snoringcat-platform/` — Pulumi IaC for Hetzner
  Cloud (private network, servers, firewalls) + Cloudflare DNS.
  Stack name `prod`, state in S3 `hopnbop-pulumi-state`. Several
  values are still hardcoded in `main.go` (zone
  `snoringcat.games`, IPs `10.0.1.10` / `10.0.1.20`, server
  type `cpx21`); extracting them into `Pulumi.<stack>.yaml` is
  a follow-up.
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
- `migrate_ddb_to_nakama.py` — one-shot migration script
  (Phase E) that copies DynamoDB rows into Nakama Storage.
  Idempotent (`if_not_exists` semantics).
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
