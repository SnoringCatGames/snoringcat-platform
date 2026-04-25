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

## What is NOT in this repo

- Game logic — stays in each game's repo.
- Per-game GameLift fleet config and server build — each game owns
  its own `gamelift-deploy/` directory and Dockerfile.
- The patched WebRTC GDExtension build — lives in
  godot-gamelift-session-manager.
