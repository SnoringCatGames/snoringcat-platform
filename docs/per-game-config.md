# Per-game configuration

Every game registered with the platform has a row in the DynamoDB
`games` config table, keyed by `game_id`. Lambda handlers read
this row to route fleet operations, look up matchmaker names,
present cross-game presence info, etc.

> **Status:** target schema. Implemented in Phase 1.

## Schema

| Field                 | Type    | Description |
|-----------------------|---------|-------------|
| `game_id`             | string  | Lowercase slug (PK). e.g. `hopnbop`, `nextgame`. |
| `display_name`        | string  | Human-friendly name shown in friend presence badges. |
| `icon_url`            | string  | Public URL to the game's badge icon (16x16 or 32x32). |
| `fleet_id`            | string  | GameLift container fleet ID. Empty disables fleet ops. |
| `gamelift_location`   | string  | AWS region for the fleet. |
| `matchmaker_name`     | string  | GameLift FlexMatch configuration name. |
| `matchmaker_ruleset`  | string  | Ruleset name (used in deploy automation). |
| `physics_fps`         | number  | Network tick rate (60 for ENet, 30 for WebRTC). |
| `base_dns_zone`       | string  | Route 53 hosted zone for game session DNS records. |
| `supported_transports`| list    | One or more of `enet`, `webrtc`, `websocket`. |
| `protocol_version`    | number  | Latest supported game protocol_version. |
| `latest_game_version` | string  | Display version, e.g. `0.30.0`. |
| `feature_flags`       | map     | Per-game feature toggles (gameplay tweaks, etc.). |
| `created_at`          | number  | Unix timestamp. |
| `updated_at`          | number  | Unix timestamp. |

## Registration

(TODO Phase 5: `scripts/register-with-platform.ps1` in each game's repo.)

## Caching

Lambda handlers cache the games table in warm container memory
with a ~5 minute TTL. Updates therefore propagate within 5
minutes without redeploy. For immediate propagation, redeploy
the affected stack to recycle warm containers.

## Adding a game

1. Stand up the new GameLift fleet, container group definition,
   and matchmaker for the new game (per-game work; not handled
   by this stack).
2. Insert the `games` row with the resulting fleet_id and
   matchmaker_name.
3. The new game's client uses `Platform.initialize({game_id, ...})`
   and the backend automatically routes to the right fleet.
