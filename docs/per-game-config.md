# Per-game configuration

Every game registered with the platform has a row in the DynamoDB
`snoringcat-games` table, keyed by `game_id`. Lambda handlers read
this row to route fleet operations, look up matchmaker names,
present cross-game presence info, etc.

> **Status:** schema live in production. Currently registered
> games: `hopnbop`.

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
| `protocol_version`    | number  | Latest supported game `protocol_version`. |
| `latest_game_version` | string  | Display version, e.g. `0.31.0`. |
| `feature_flags`       | map     | Per-game feature toggles (gameplay tweaks, etc.). |
| `created_at`          | number  | Unix timestamp. |
| `updated_at`          | number  | Unix timestamp. |

## Caching

Lambda handlers cache the games table in warm container memory
with a ~5 minute TTL. Updates therefore propagate within 5
minutes without a redeploy. For immediate propagation, redeploy
the affected stack to recycle warm containers.

## Adding a new game

1. **Stand up GameLift infrastructure** for the new game (this
   is per-game work; the platform stack does not own these):
   - Container fleet (typically derived from
     [`godot-gamelift-session-manager`](https://github.com/SnoringCatGames/godot-gamelift-session-manager)'s
     Docker bits).
   - FlexMatch matchmaker configuration + ruleset.
   - Container group definition pinned to the fleet.

2. **Insert the `games` row** with the resulting fleet_id and
   matchmaker_name. Quick-and-dirty:

   ```bash
   aws dynamodb put-item \
     --table-name snoringcat-games \
     --region us-west-2 \
     --profile <profile> \
     --item '{
       "game_id":              {"S": "yourgame"},
       "display_name":         {"S": "Your Game"},
       "icon_url":             {"S": "https://..."},
       "fleet_id":             {"S": "containerfleet-..."},
       "gamelift_location":    {"S": "us-west-2"},
       "matchmaker_name":      {"S": "yourgame-ffa-matchmaker"},
       "physics_fps":          {"N": "60"},
       "supported_transports": {"L": [{"S": "enet"}, {"S": "webrtc"}]},
       "protocol_version":     {"N": "1"},
       "latest_game_version":  {"S": "0.1.0"},
       "created_at":           {"N": "1714185600"},
       "updated_at":           {"N": "1714185600"}
     }'
   ```

   A `register-with-platform.ps1` helper that wraps this is
   intended to live in each game's repo (Phase 5 work).

3. **Bake the `game_id` into the game client** when initializing
   the SDK. The current addon doesn't yet have a unified
   `Platform.initialize`; today, the JWT carries `game_id` from
   `auth_handler` based on the value passed to
   `/v1/auth/anon` etc. Until `Platform.initialize` lands, the
   game just needs to send `game_id` on its sign-in calls and
   the backend routes to the right fleet automatically.

## Cross-game effects

Once a `games` row exists, this stack will:
- Accept JWT tokens with that `game_id` claim
- Route `/v1/matchmaking/*`, `/v1/session/*`, and
  `/v1/fleet/*` to the configured fleet
- Reject `/v1/party/invite` calls when inviter and invitee are
  in different games (returns `400 CROSS_GAME_INVITE`)
- Surface the `display_name` + `icon_url` in friends-UI
  presence badges (consumer game does the rendering)
