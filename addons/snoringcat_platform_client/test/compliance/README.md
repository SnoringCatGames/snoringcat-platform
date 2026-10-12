# Platform compliance test suite

GUT tests that consuming games run from their own CI on every
submodule bump of `snoringcat-platform`. When a platform change
breaks a game's expected behavior, the game's CI catches it on
the bump PR rather than at runtime in production.

Tests hit real Nakama HTTP endpoints (`/healthcheck`,
`/v2/account/*`, `/v2/rpc/*`) and the realtime socket, and assert
on the contracts the SDK depends on, including the shape of JWTs,
the round-trip of profile/settings writes, the auth gates on
protected routes, the registration of server-to-server runtime
RPCs, and the matchmaker hook's notifications.

## Target: a local stack, never production by default

The suite creates accounts, friendships, groups, and storage rows
on whatever backend it reaches, so it only talks to a loopback
`PLATFORM_API_URL` (`127.0.0.1`, `localhost`, `::1`). There is no
default URL. Any other target, production included, also needs
`ALLOW_PROD_COMPLIANCE=1`. A refused request is never sent: the
helper push_error()s, which GUT counts as a test failure, so a
misconfigured run goes red instead of quietly hitting production.

## Running from a consuming game

In hopnbop, one command builds the runtime plugin, starts a local
Nakama + Postgres in mock-deploy mode, runs the suite, and tears
everything down:

```pwsh
pwsh scripts/local-smoke-test.ps1
```

See hopnbop's `infra/dev/README.md` for flags and the manual
steps. To run GUT directly against a stack you already have:

```bash
PLATFORM_API_URL=http://127.0.0.1:7350 \
NAKAMA_SERVER_KEY=defaultkey NAKAMA_HTTP_KEY=defaulthttpkey \
GAME_ADMIN_KEY=defaultadminkey \
godot --headless --path . -s addons/gut/gut_cmdln.gd \
    -gdir=res://addons/snoringcat_platform_client/test/compliance \
    -gexit
```

| Env var              | What it is                          |
|----------------------|-------------------------------------|
| `PLATFORM_API_URL`   | Required. Base URL of the Nakama under test. Loopback only, unless `ALLOW_PROD_COMPLIANCE=1`. |
| `ALLOW_PROD_COMPLIANCE` | Set to `1` to allow a non-loopback `PLATFORM_API_URL`, production included. |
| `NAKAMA_SERVER_KEY`  | Used for `Basic` auth on `/v2/account/authenticate/*` (anon sign-in). Without it, auth-dependent tests `pending()`. |
| `NAKAMA_HTTP_KEY`    | Used as `?http_key=` on `/v2/rpc/*` for server-to-server runtime RPCs. Without it, those tests `pending()`. |
| `GAME_ADMIN_KEY`     | The runtime's admin key, sent as `?admin_key=` by `test_admin_key.gd`'s accept test. Without it, that one test `pending()`s; the refusal tests don't need it. |

The matchmaker tests also need the runtime started with
`EDGEGAP_MOCK_DEPLOY=true` (they read it from `runtime_status`),
so they never allocate a real Edgegap deploy. They `pending()`
otherwise.

## Modes

The suite runs in one of two modes, controlled by
`PLATFORM_COMPLIANCE_MODE`:

- **`live`** (default) — Tests hit the backend at
  `PLATFORM_API_URL`.
- **`mock`** — Reserved for a future phase. `HTTPRequest` will
  be intercepted and responses fixtured from the OpenAPI schema
  for fully offline CI runs. Tests currently `pending()` in
  this mode.

## What gets tested

| Subsystem       | Test file                  | Coverage |
|-----------------|----------------------------|----------|
| Version         | `test_version.gd`          | `/healthcheck`, `version_check` RPC, `runtime_status` (Edgegap config sanity, no `${...}` placeholders). |
| Admin key       | `test_admin_key.gd`        | `register_game` refuses the HTTP key alone and a wrong `admin_key`, and accepts the right one (stopping at validation, so nothing is written); `runtime_status` reports `admin_key_configured` without the key. |
| Auth (anon)     | `test_auth_anon.gd`        | Anon device sign-in returns a 3-segment JWT with a user-id claim; token unlocks `/v2/account`. |
| Auth (link)     | `test_auth_link.gd`        | Linking a second device to an anon account preserves the user_id; unlinking does not delete the account. |
| Auth (refresh)  | `test_token_refresh.gd`    | `/v2/account/session/refresh` returns a fresh access+refresh token pair; refresh-derived token unlocks `/v2/account`; garbage tokens rejected without 5xx'ing. |
| Account         | `test_account.gd`          | `/v2/account` GET returns the user block; `display_name` update round-trips. |
| Account delete  | `test_account_delete.gd`   | `DELETE /v2/account` removes a one-shot account end-to-end (re-auth fails post-delete). Stage 1.5 soft delete: `delete_account` queues and anonymizes, `get_account_deletion_status` reports it, `cancel_account_deletion` restores the display name (including an empty one) and refuses a second cancel. |
| Account delete cascade | `test_friends_account_delete_cascade.gd` | Two-user: A↔B mutual, A invokes `delete_account` RPC → B's friend list loses A, A's friend list is empty, A's display_name is `[deleted]`, `get_account_deletion_status` reports `pending=true`. Guards Stage 1.4 friends cascade + Stage 1.5 sign-in-during-grace. |
| Friends         | `test_friends.gd`          | `/v2/friend` GET returns a well-formed list; add-with-bogus-id rejects without 5xx'ing. |
| Friends (2-user) | `test_friends_multiuser.gd` | Two real anon accounts: A → request → B accepts → both see each other as FRIEND. Canaries the `multi_session_anon` helper. |
| Party           | `test_party.gd`            | Group create → leave roundtrip via `/v2/group`. |
| Party (invite)  | `test_party_invite_flow.gd` | Two-user closed-group invite: A invites B, B's group state is JoinRequest(3), B accepts via `/join`, state flips to Member(2). Guards Stage 1.2/1.3 member-list shape and Stage 5.11 pending-vs-active split. |
| Settings        | `test_settings.gd`         | Storage write-then-read round-trip via `/v2/storage`. |
| Presence        | `test_presence.gd`         | `update_and_get_presence` RPC writes presence + returns online friends; rejects http_key callers (auth gate sanity). |
| Presence (filter) | `test_presence_game_filter.gd` | Three-user visibility check: A↔B mutual, A→C pending. A's presence read sees B but not C, and B's record carries `game_id`. Guards Stage 3.2/3.3 mutual-only filter + game_id field. |
| Player stats    | `test_player_stats.gd`     | `get_player_stats` (caller + explicit player_id forms; unranked defaults), `get_match_history` (always returns array, never null). |
| Data export     | `test_data_export.gd`      | `export_player_data` envelope shape (generated_at, account, storage_objects, leaderboard_records, friends); GDPR-required path. |
| Matchmaking     | `test_matchmaking.gd`      | Two players' run-scoped tickets pair; both get `match_ready` naming the same mock deploy, with distinct session ids, ENet transport, a `wss://` signaling URL, and ports. Gated on `EDGEGAP_MOCK_DEPLOY=true`. |
| Matchmaking cancel | `test_matchmaking_cancel_race.gd` | Two scenarios: a cancelled ticket stays out of the pool (a compatible second ticket doesn't pair with it, then a positive control proves a real pairing fits the window), and post-match cancel safety (cancel on a consumed ticket is safe, socket still usable). Gated on `EDGEGAP_MOCK_DEPLOY=true`. |
| Matchmaking failure | `test_matchmaking_failure_modes.gd` | Stage 3.9 protocol-mismatch path: an out-of-date client paired with a current one aborts the match before any Edgegap allocation. Both get `match_failed` (`reason=protocol_mismatch`, `expected`, `message`); only the out-of-date one gets `got`. Gated on `EDGEGAP_MOCK_DEPLOY=true`. |
| Transport sel   | `test_transport_selection.gd` | The WebRTC cross-play transport-selection rule (any `web` → `webrtc`, else `enet`) via the `transport_select` runtime RPC. Catches refactor-induced rule drift without burning real Edgegap allocations. |
| Match loopback  | `test_match_loopback.gd`   | Server-to-server runtime RPCs (`register_server`, `match_end`, `record_client_ip`) are registered and reject malformed input. A forged `register_server` doesn't let `match_end` write a stranger's history (gated on `EDGEGAP_MOCK_DEPLOY=true`). |
| API surface     | `test_api_surface.gd`      | Unauthenticated calls to the SDK's HTTP routes return 401 (catches accidental gate removal). Bare `/v2/rpc/<name>` without an `http_key` does not execute. |
| Socket auth     | `test_socket_auth.gd`      | Realtime WSS endpoint accepts a valid session and refuses a garbage-signature token with HTTP 401 at the upgrade. |
| Socket matchmaker | `test_socket_matchmaker.gd` | `add_matchmaker_async` returns a ticket id; `remove_matchmaker_async` cleans up. The ticket can never match, so nothing allocates on any target. |
| Socket presence | `test_socket_presence.gd`  | `update_status_async` succeeds; `follow_users_async` on an online user returns that user's presence. Nothing in the SDK or hopnbop calls these yet. |
| Socket chat     | `test_socket_chat.gd`      | Join → send → echo round-trip on a room channel. |

Read-only tests use a stable device id (`compliance-anon-fixed-1`)
so runs reuse the same Nakama account instead of bloating the
users table. The `compliance-` prefix lets ops grep + prune
these later. Anything that mutates an account destructively
(`delete_account`, blocks, matchmaking) uses one-shot accounts
from `multi_session_anon()` and hard-deletes them in `after_each`.

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
6. Wrap any response body you put in an assertion message in
   `_helper.redact_tokens()`. GUT prints the message on failure,
   and at `-glog=2` and above on success too, and auth responses
   carry live session and refresh tokens.
7. Update the table above.

### Multi-user tests

`_helper.multi_session_anon(count)` mints `count` independent
anonymous accounts in one call, returning
`[{token, refresh_token, user_id, username, device_id}, ...]`.
Device ids are one-shot per run (prefix `compliance-multi-`)
so concurrent CI runs don't collide.

Pair with `_helper.delete_one_shot_account(user)` in `after_each`
when you want strict cleanup; otherwise the rows linger for
grep-and-prune. See `test_friends_multiuser.gd` for the
canonical pattern.

### Socket tests

`compliance_socket_helper.gd` wraps the Nakama SDK's
`NakamaSocket`. It exposes `session_from_token(jwt)`,
`create_socket()`, `connect_with_timeout(sock, session, sec)`,
`connect_users(users)` (one connected socket per
`multi_session_anon` user), `wait_until(condition, sec)`,
`wait_for_signal_with_timeout(obj, name, sec)`, and
`NotificationCapture`, which records `matchmaker_matched`,
`match_ready`, `match_failed`, and `party_matchmaking_start` for
one socket. See `test_matchmaking.gd` for the multi-user pattern
and `test_socket_chat.gd` for the single-user one.

Rules these tests learned the hard way:

- **Never assign to a captured local in a signal callback.**
  GDScript lambdas capture locals by value, so `var seen := false`
  plus `func(): seen = true` only changes the lambda's copy. Write
  into a Dictionary, an Array, or an object's fields
  (`NotificationCapture`). The helper used to get this wrong, and
  every realtime test pended for months without running.
- **Await every `*_async` call.** Calling a coroutine through a
  Variant without `await` is a runtime SCRIPT ERROR in Godot 4.7.
- **Matchmaker tickets need `min_count >= 2`.** Nakama 3.25
  rejects smaller ones. Pair two one-shot users, and scope their
  tickets with a run-unique property in the query
  (`+properties.compliance_run:<id>`) so they can only match each
  other.
- **Expected engine errors must be marked handled.** GUT 9.7
  fails a test on any engine error logged during it. See the
  refused-upgrade check in `test_socket_auth.gd`.
