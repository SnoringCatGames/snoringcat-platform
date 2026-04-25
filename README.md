# snoringcat-platform

Shared infrastructure for Snoring Cat Games multiplayer titles.

This repo contains:

- **`backend/`** — AWS SAM stack (Lambda + DynamoDB + API Gateway)
  hosting auth, accounts, friends, presence, parties, matchmaking,
  fleet warmup, leaderboards, and the per-game configuration table.
  Single shared deployment serves every Snoring Cat game; per-game
  scoping is via `game_id` in JWT claims and request paths.
- **`addons/snoringcat_platform_client/`** — Godot 4.5 addon that
  wraps the backend's HTTP API, plus reusable UI building blocks
  (auth screen, friends panel, party UI, side-panel framework,
  screen state machine, focus navigator, settings persistence).
  Game projects consume this via git submodule.
- **`docs/`** — API spec, client SDK guide, per-game configuration
  reference.

## Repo layout

```
backend/
  template.yaml                     # SAM template
  src/
    handlers/                       # API Gateway entry points
    services/                       # Business logic
    utils/                          # Shared helpers
  tests/                            # pytest + moto
  scripts/
    deploy.ps1
    migrate-from-hopnbop.py         # one-shot migration
addons/snoringcat_platform_client/
  core/                             # Auth, API clients, settings
  ui/                               # Screens, panels, overlays
  input/                            # Device manager, focus navigator
  util/                             # Viewport, fonts, localization
  translations/                     # Platform-only translation strings
  test/compliance/                  # GUT tests games run on submodule bump
  plugin.cfg
docs/
  api-spec.md
  client-sdk-guide.md
  per-game-config.md
```

## Versioning

Backend and client SDK ship together as one semver release. URL
versioning (`/v1/`, `/v2/`) ensures old game clients keep working
after platform updates. The realtime game-server protocol
(`protocol_version`) is per-game and unrelated to the platform
SDK version.

## Status

Active extraction from `hopnbop_private`. See the
restructure plan referenced in CLAUDE.md for current phase.

## Consumed by

- [hopnbop_private](https://github.com/SnoringCatGames/hopnbop_private)

## Sibling repos

- [godot-rollback-netcode](https://github.com/SnoringCatGames/godot-rollback-netcode)
- [godot-gamelift-session-manager](https://github.com/SnoringCatGames/godot-gamelift-session-manager)
