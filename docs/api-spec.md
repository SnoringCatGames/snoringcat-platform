# Platform API spec

This document is the **target** spec for the new shared platform.
The current `hopnbop` backend's endpoints are documented in
`docs/legacy-hopnbop-api.md` (Phase 0i) so we can compare the old
and new shapes during migration.

The authoritative machine-readable spec is `backend/openapi.json`,
generated from Pydantic models in `backend/src/models/`.

## Versioning

Every endpoint is namespaced with `/v1/` (or `/v2/`, etc.).
Once `/v1/` ships, we treat it as immutable except for additive
changes (new optional fields, new endpoints). Breaking changes go
out as `/v2/` while `/v1/` keeps serving until telemetry shows
zero traffic.

## Auth

JWT-based. JWT payload: `{player_id, game_id, exp, iat, anon}`.
The `game_id` claim is bound at sign-in time; clients sign in
once per game launch and the token is good for that game only.

## Endpoint groups

### Global (no `game_id` in path)

| Method | Path                              | Purpose                         |
|--------|-----------------------------------|---------------------------------|
| POST   | `/v1/auth/anon`                   | Anonymous sign-in               |
| POST   | `/v1/auth/login`                  | OAuth sign-in                   |
| POST   | `/v1/auth/refresh`                | Refresh JWT                    |
| POST   | `/v1/auth/logout`                 | Sign out                        |
| GET    | `/v1/account/profile`             | Read account profile            |
| PUT    | `/v1/account/profile`             | Update display name, etc.       |
| POST   | `/v1/account/link`                | Link OAuth identity             |
| DELETE | `/v1/account/link/{provider}`     | Unlink identity                 |
| POST   | `/v1/account/delete`              | Delete account                  |
| GET    | `/v1/account/export`              | Export player data (GDPR)       |
| GET    | `/v1/friends`                     | List friends                    |
| POST   | `/v1/friends/request`             | Send friend request             |
| POST   | `/v1/friends/accept`              | Accept request                  |
| POST   | `/v1/friends/decline`             | Decline request                 |
| DELETE | `/v1/friends/{friend_id}`         | Remove friend                   |
| GET    | `/v1/friends/notifications`       | Pending requests + presence     |
| PUT    | `/v1/presence`                    | Update own presence             |
| POST   | `/v1/presence/batch`              | Batch read friend presence      |
| GET    | `/v1/settings/{scope}`            | Read settings (`global` or `game#X`) |
| PUT    | `/v1/settings/{scope}`            | Update settings                 |
| GET    | `/v1/version`                     | Server version + protocol_version per game |
| POST   | `/v1/telemetry/crash`             | Submit crash report             |
| POST   | `/v1/telemetry/log`               | Submit log batch                |
| GET    | `/v1/games`                       | List registered games (display name + icon) |
| POST   | `/v1/consent`                     | Record legal consent            |

### Per-game (under `/v1/games/{game_id}/...`)

| Method | Path                                              | Purpose                |
|--------|---------------------------------------------------|------------------------|
| GET    | `/v1/games/{game_id}/profile/{player_id}`         | Per-game profile       |
| PUT    | `/v1/games/{game_id}/profile`                     | Update per-game name   |
| GET    | `/v1/games/{game_id}/leaderboard/{board_id}`      | Leaderboard page       |
| POST   | `/v1/games/{game_id}/matchmaking/start`           | Start matchmaking      |
| GET    | `/v1/games/{game_id}/matchmaking/status/{ticket}` | Poll ticket            |
| POST   | `/v1/games/{game_id}/matchmaking/cancel`          | Cancel ticket          |
| POST   | `/v1/games/{game_id}/match/result`                | Server-only: report match |
| GET    | `/v1/games/{game_id}/match/history`               | Player's match history |
| POST   | `/v1/games/{game_id}/party`                       | Create party           |
| POST   | `/v1/games/{game_id}/party/invite`                | Invite friend          |
| POST   | `/v1/games/{game_id}/party/accept`                | Accept invite          |
| POST   | `/v1/games/{game_id}/party/leave`                 | Leave party            |
| GET    | `/v1/games/{game_id}/party/{party_id}`            | Get party state        |
| POST   | `/v1/games/{game_id}/fleet/warmup`                | Trigger fleet warmup   |
| GET    | `/v1/games/{game_id}/fleet/status`                | Fleet capacity         |
| POST   | `/v1/games/{game_id}/session/connect`             | Validate player session ID |

This list is the **goal**, not the current state. Phase 1 ports
existing handlers and reshapes paths to match.
