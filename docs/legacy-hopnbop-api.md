# Legacy Hop 'n Bop API snapshot

This document is a frozen snapshot of the
**current** `hopnbop_private` backend API at the time of platform
extraction. It is the migration baseline — every endpoint here
must have an equivalent (or be deliberately deprecated) in the
new platform's `/v1/` API documented in `api-spec.md`.

Captured from `hopnbop_private/backend/template.yaml` at tag
`pre-platform-extraction`.

API base: `https://<api-id>.execute-api.us-west-2.amazonaws.com/prod`.
All paths are relative to that base.

## Auth (`/auth/*`)

| Method | Path                   | Handler                                  | Notes                          |
|--------|------------------------|------------------------------------------|--------------------------------|
| POST   | `/auth/login`          | `auth_handler.login`                     | OAuth provider sign-in         |
| POST   | `/auth/anon`           | `auth_handler.anonymous_login`           | Anonymous sign-in              |
| POST   | `/auth/guest`          | `auth_handler.guest_login`               | Guest sign-in                  |
| POST   | `/auth/refresh`        | `auth_handler.refresh`                   | Refresh JWT                    |
| POST   | `/auth/link`           | `auth_handler.link_account`              | Link OAuth identity            |
| POST   | `/auth/unlink`         | `auth_handler.unlink_account`            | Unlink identity                |
| POST   | `/auth/merge`          | `auth_handler.merge_accounts`            | Merge two accounts             |
| DELETE | `/auth/account`        | `auth_handler.delete_account`            | Delete account (GDPR)          |

## Player (`/player/*`, `/players/*`)

| Method | Path                          | Handler                                     | Notes                       |
|--------|-------------------------------|---------------------------------------------|-----------------------------|
| GET    | `/player/profile`             | `player_handler.get_player_profile`         | Profile + linked providers  |
| GET    | `/player/settings`            | `player_handler.get_player_settings`        | Cloud-synced settings       |
| PUT    | `/player/settings`            | `player_handler.save_player_settings`       | Cloud-synced settings       |
| GET    | `/player/history`             | `player_handler.get_match_history`          | Player's match history      |
| GET    | `/player/export`              | `player_handler.export_player_data`         | GDPR export                 |
| GET    | `/players/{player_id}/stats`  | `match_handler.get_player_stats`            | Cross-player stats lookup   |

## Matchmaking (`/matchmaking/*`)

| Method | Path                                  | Handler                                      |
|--------|---------------------------------------|----------------------------------------------|
| POST   | `/matchmaking/join`                   | `matchmaking_handler.join_matchmaking`       |
| POST   | `/matchmaking/start`                  | `matchmaking_handler.start_matchmaking`      |
| GET    | `/matchmaking/status/{ticket_id}`     | `matchmaking_handler.get_matchmaking_status` |
| POST   | `/matchmaking/leave`                  | `matchmaking_handler.leave_matchmaking`      |

## Match results & leaderboard

| Method | Path                  | Handler                              |
|--------|-----------------------|--------------------------------------|
| POST   | `/matches/result`     | `match_handler.submit_match_result`  |
| GET    | `/leaderboard`        | `match_handler.get_leaderboard`      |

## Sessions

| Method | Path                | Handler                                |
|--------|---------------------|----------------------------------------|
| GET    | `/session/active`   | `session_handler.get_active_session`   |

## Friends (`/friends/*`)

| Method | Path                          | Handler                              |
|--------|-------------------------------|--------------------------------------|
| GET    | `/friends`                    | `friends_handler.list_friends`       |
| POST   | `/friends/add`                | `friends_handler.add_friend`         |
| POST   | `/friends/remove`             | `friends_handler.remove_friend`      |
| POST   | `/friends/accept`             | `friends_handler.accept_request`     |
| POST   | `/friends/reject`             | `friends_handler.reject_request`     |
| POST   | `/friends/cancel`             | `friends_handler.cancel_request`     |
| GET    | `/friends/notifications`      | `friends_handler.get_notifications`  |
| POST   | `/friends/seen`               | `friends_handler.mark_seen`          |
| GET    | `/friends/search`             | `friends_handler.search_by_code`     |

## Presence (`/presence/*`)

| Method | Path                     | Handler                              |
|--------|--------------------------|--------------------------------------|
| POST   | `/presence/heartbeat`    | `presence_handler.heartbeat`         |

## Party (`/party/*`)

| Method | Path                | Handler                                  |
|--------|---------------------|------------------------------------------|
| POST   | `/party/create`     | `party_handler.create_party`             |
| POST   | `/party/invite`     | `party_handler.invite_to_party`          |
| POST   | `/party/join`       | `party_handler.join_party`               |
| POST   | `/party/leave`      | `party_handler.leave_party`              |
| POST   | `/party/kick`       | `party_handler.kick_from_party`          |
| GET    | `/party/status`     | `party_handler.get_party_status`         |
| POST   | `/party/start`      | `party_handler.start_party_matchmaking`  |

## Fleet warmup (`/fleet/*`)

| Method | Path              | Handler                          | Notes                       |
|--------|-------------------|----------------------------------|-----------------------------|
| POST   | `/fleet/warmup`   | `fleet_handler.warmup`           | Unauthenticated             |
| GET    | `/fleet/status`   | `fleet_handler.status`           | Unauthenticated             |

## Misc

| Method | Path               | Handler                              | Notes                              |
|--------|--------------------|--------------------------------------|------------------------------------|
| GET    | `/version`         | `version_handler.get_version`        | Game + protocol versions           |
| POST   | `/telemetry/crash` | `telemetry_handler.handle_crash_report` | Crash reports                   |
| POST   | `/internal/dns/warmup` | `matchmaking_handler.warmup_dns` | Internal: pre-warm DNS for session |

## Scheduled (EventBridge)

| Trigger          | Handler                                      | Purpose                               |
|------------------|----------------------------------------------|---------------------------------------|
| `rate(5 minutes)`| `fleet_handler.scheduled_idle_check`         | Scale fleet to 0 after 30 min idle    |
| Weekly cron      | `leaderboard_handler.reset_weekly_leaderboard` | Weekly leaderboard reset            |

## DynamoDB tables (current names)

- `hopnbop-players`
- `hopnbop-provider-mappings`
- `hopnbop-match-history`
- `hopnbop-consent-audit`
- `hopnbop-settings`
- `hopnbop-leaderboard`
- `hopnbop-friends`
- `hopnbop-parties`
- `hopnbop-active-sessions`
- `hopnbop-fleet-state`

## Mapping to new platform schema

See `api-spec.md` for the target API shape and the migration plan
`~/.claude/plans/in-general-i-ve-been-snoopy-pearl.md` for the full
table-by-table migration logic. Notable shape changes:

- `auth_handler.delete_account` (DELETE `/auth/account`) →
  POST `/v1/account/delete` (semantic move under `/account`).
- `player_handler.*` → split between global `/v1/account/*` and
  per-game `/v1/games/{game_id}/profile/*`.
- All matchmaking, party, leaderboard, session, fleet endpoints
  move under `/v1/games/{game_id}/...`.
- `friends_handler.list_friends` response gains a `presence` field
  per friend (game_id + status).
- `presence_handler.heartbeat` becomes PUT `/v1/presence`.
