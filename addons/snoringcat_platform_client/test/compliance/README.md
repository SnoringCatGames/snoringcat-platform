# Platform compliance test suite

GUT tests that consuming games run from their own CI on every
submodule bump of `snoringcat-platform`. When a platform change
breaks a game's expected behavior, the game's CI catches it on
the bump PR rather than at runtime in production.

The suite targets the **live Nakama backend** at
`https://nakama.snoringcat.games` by default. Tests hit real
HTTP endpoints (`/healthcheck`, `/v2/account/*`, `/v2/rpc/*`)
and assert on the contracts the SDK depends on, including the
shape of JWTs, the round-trip of profile/settings writes, the
auth gates on protected routes, and the registration of
server-to-server runtime RPCs.

## Running from a consuming game

```bash
godot --headless --path . -s addons/gut/gut_cmdln.gd \
    -gdir=res://addons/snoringcat_platform_client/test/compliance \
    -gexit
```

The suite reads two env vars for credentials. Without them,
auth-dependent tests skip with a clear `pending()` message.

| Env var              | What it is                          |
|----------------------|-------------------------------------|
| `NAKAMA_SERVER_KEY`  | Used for `Basic` auth on `/v2/account/authenticate/*` (anon sign-in). |
| `NAKAMA_HTTP_KEY`    | Used as `?http_key=` on `/v2/rpc/*` for server-to-server runtime RPCs. |
| `PLATFORM_API_URL`   | Optional. Overrides the default base URL (e.g. for a future staging instance). |

## Modes

The suite runs in one of two modes, controlled by
`PLATFORM_COMPLIANCE_MODE`:

- **`live`** (default) — Tests hit the real Nakama backend.
  Used by consuming games' submodule-bump CI and the platform
  repo's hourly synthetic monitor.
- **`mock`** — Reserved for a future phase. `HTTPRequest` will
  be intercepted and responses fixtured from the OpenAPI schema
  for fully offline CI runs. Tests currently `pending()` in
  this mode.

## What gets tested

| Subsystem       | Test file                  | Coverage |
|-----------------|----------------------------|----------|
| Version         | `test_version.gd`          | `/healthcheck`, `version_check` RPC, `runtime_status` (Edgegap config sanity, no `${...}` placeholders). |
| Auth (anon)     | `test_auth_anon.gd`        | Anon device sign-in returns a 3-segment JWT with a user-id claim; token unlocks `/v2/account`. |
| Auth (link)     | `test_auth_link.gd`        | Linking a second device to an anon account preserves the user_id; unlinking does not delete the account. |
| Auth (refresh)  | `test_token_refresh.gd`    | `/v2/account/session/refresh` returns a fresh access+refresh token pair; refresh-derived token unlocks `/v2/account`; garbage tokens rejected without 5xx'ing. |
| Account         | `test_account.gd`          | `/v2/account` GET returns the user block; `display_name` update round-trips. |
| Account delete  | `test_account_delete.gd`   | `DELETE /v2/account` removes a one-shot account end-to-end (re-auth fails post-delete). Custom `delete_account` RPC documented but not yet implemented. |
| Friends         | `test_friends.gd`          | `/v2/friend` GET returns a well-formed list; add-with-bogus-id rejects without 5xx'ing. |
| Party           | `test_party.gd`            | Group create → leave roundtrip via `/v2/group`. |
| Settings        | `test_settings.gd`         | Storage write-then-read round-trip via `/v2/storage`. |
| Presence        | `test_presence.gd`         | `update_and_get_presence` RPC writes presence + returns online friends; rejects http_key callers (auth gate sanity). |
| Player stats    | `test_player_stats.gd`     | `get_player_stats` (caller + explicit player_id forms; unranked defaults), `get_match_history` (always returns array, never null). |
| Data export     | `test_data_export.gd`      | `export_player_data` envelope shape (generated_at, account, storage_objects, leaderboard_records, friends); GDPR-required path. |
| Matchmaking     | `test_matchmaking.gd`      | Matchmaker hook is registered (via `runtime_status`); full socket flow flagged pending. |
| Transport sel   | `test_transport_selection.gd` | The WebRTC cross-play transport-selection rule (any `web` → `webrtc`, else `enet`) via the `transport_select` runtime RPC. Catches refactor-induced rule drift without burning real Edgegap allocations. |
| Match loopback  | `test_match_loopback.gd`   | Server-to-server runtime RPCs (`register_server`, `match_end`, `record_client_ip`) are registered and reject malformed input. |
| API surface     | `test_api_surface.gd`      | Unauthenticated calls to the SDK's HTTP routes return 401 (catches accidental gate removal). Bare `/v2/rpc/<name>` without an `http_key` does not execute. |
| Socket auth     | `test_socket_auth.gd`      | Realtime WSS endpoint accepts a valid session and rejects a garbage-signature token. |
| Socket matchmaker | `test_socket_matchmaker.gd` | `add_matchmaker_async` returns a ticket id; `remove_matchmaker_async` cleans up. |
| Socket presence | `test_socket_presence.gd`  | `update_status_async` and `follow_users_async` round-trip without errors (powers the online-friends UI). |
| Socket chat     | `test_socket_chat.gd`      | Join → send → echo round-trip on a room channel. |

Tests use a stable device id (`compliance-anon-fixed-1`) so
runs reuse the same Nakama account instead of bloating the
users table. The `compliance-` prefix lets ops grep + prune
these later.

## Adding a new test

1. Add a `test_<feature>.gd` file in this directory.
2. `extends GutTest`.
3. Preload `compliance_helper.gd` and instantiate it in
   `before_each` via `add_child_autofree`.
4. Use `_helper.http_get/post/put/delete()`,
   `_helper.nakama_anon_session()`, `_helper.http_key_rpc()`,
   or `_helper.session_rpc()` instead of building HTTP
   requests directly.
5. Skip with `pending()` when an env var is missing or when
   `is_live_mode()` is false.
6. Update the table above.
