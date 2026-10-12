# Platform architecture reference

> **Load this doc when working on backend, multiplayer infra,
> auth, matchmaking, game-server allocation, observability, or
> per-game config in any Snoring Cat Games title.**
>
> This is the **live production architecture** (Nakama +
> Hetzner + Edgegap), as of Phase F completion (2026-05-03).
> The previous AWS GameLift design is preserved only as
> archeology in `hopnbop/docs/archive/platform-pivot-discussion.md`
> and `hopnbop/docs/archive/MIGRATION_PLAN.md`.

This file is intentionally **not** loaded automatically by every
session — it's referenced from each game's project-level
CLAUDE.md so it's only read when relevant. Do not duplicate the
contents into game-level CLAUDE.md; cross-reference instead.

---

## Status

- **Architecture:** Nakama (self-hosted) + Postgres + Edgegap +
  Caddy on Hetzner Cloud.
- **Multi-game design:** every component carries a `game_id`
  label / column; one platform instance hosts N games concurrently.
- **Source of truth for migration progress:**
  `hopnbop/docs/archive/MIGRATION_PLAN.md` and the migration state
  file at `~/.hopnbop-migration/state.json`.

---

## Topology

```
┌──────────────────────────────────────────────────────────────┐
│  Player client (Godot, native or web)                        │
│  - addons/nakama/ (nakama-godot SDK)                         │
│  - addons/platform-session-manager/ (Edgegap provider)       │
│  - src/core/{auth,backend,friends,party,crash}_client.gd     │
└────────────────┬─────────────────────────────────────────────┘
                 │ wss + UDP
                 ▼
┌──────────────────────────────────────────────────────────────┐
│  Caddy (TLS termination + reverse proxy)                     │
│  nakama.snoringcat.games:443  → Nakama HTTP/WS               │
└────────────────┬─────────────────────────────────────────────┘
                 │
┌────────────────▼─────────────────────────────────────────────┐
│  Nakama (Hetzner CPX21, Hillsboro)                           │
│  - REST/gRPC/realtime endpoints                              │
│  - Go runtime modules:                                       │
│    - fleet_allocator.go (Edgegap allocator hook)             │
│    - match_lifecycle.go (server registration, match end,     │
│      Edgegap stop)                                           │
│    - per_game_config.go (loads game.yaml at startup)         │
│    - protocol_version.go (per-game version checker)          │
│  - Built-in: auth, friends, parties, matchmaker, leaderboards│
└────────────────┬─────────────────────────────────────────────┘
                 │ docker bridge (172.18.0.0/16)
┌────────────────▼─────────────────────────────────────────────┐
│  Postgres 16 (same Hetzner CPX21, docker-compose co-tenant)  │
│  - Nakama schema (users, friends, leaderboards, etc.)        │
│  - games config table (per-game settings + protocol_version) │
│  - Nightly pg_dumpall → Cloudflare R2 (pg-backups/ prefix    │
│    in hopnbop-pulumi-state-r2 bucket); 7-day retention.      │
└──────────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────────┐
│  Edgegap (managed, 615+ regions)                             │
│  - Allocates game-server containers on demand                │
│  - 5-15s cold start                                          │
│  - Per-match player-IP-aware placement                       │
│  - True scale-to-zero                                        │
│  - Allocated by Nakama runtime via REST API                  │
└────────────────┬─────────────────────────────────────────────┘
                 │
┌────────────────▼─────────────────────────────────────────────┐
│  Game server container (per match)                           │
│  - Godot Linux export                                        │
│  - addons/platform-session-manager/EdgegapPlatformProvider   │
│  - Registers with Nakama via signed register_server RPC      │
│  - Reports match end via match_end RPC                       │
│  - Transports: ENet (UDP), WebRTC (UDP DataChannel),         │
│    WebSocket (legacy)                                        │
└──────────────────────────────────────────────────────────────┘

Operations (single-host, lightweight obs — 2026-05-13 Stage 7.11):
┌──────────────────────────────────────────────────────────────┐
│  systemd timers on the Nakama box (installed by phase-b):    │
│  - cost-monitor.timer (every 5 min): Hetzner / Edgegap-      │
│    active / R2 (every bucket) / CF Pages builds / GH Actions │
│    usage → daily summary into the service-status JSONL       │
│    + threshold pings straight to Discord.                    │
│  - pg-backup.timer (nightly 03:11 UTC): pg_dumpall → R2.     │
│  - geoip-refresh.timer (monthly, the 5th): DB-IP country     │
│    MMDB for geoip-sidecar.                                   │
│  - desktop-drain-watch.timer (daily 16:37 UTC): pages        │
│    Discord when the desktop stops draining the status        │
│    queue (oldest undrained entry >36h).                      │
│  External / off-box:                                         │
│  - UptimeRobot, 3 monitors at 5 min: Nakama healthcheck      │
│    (GET), hopnbop.net, levi.dev.                             │
│  - Daily prod-health-check script (05:51 PT) → status        │
│    trail: container/disk/memory/error-log/backup/Edgegap-    │
│    leak checks (Discord only on failure).                    │
│  In-box obs (re-introduced 2026-05-13):                      │
│  - Prometheus scrapes nakama:9099 (built-in metrics +        │
│    `snoringcat_alloc_seconds` custom timer), caddy:2019,     │
│    node-exporter:9100, postgres-exporter:9187,               │
│    cadvisor:8080 (every container, incl. local match         │
│    containers), geoip-sidecar:8080. 30d retention.           │
│  - Grafana at https://grafana.snoringcat.games provisions    │
│    Prometheus as default datasource + 5 alert rules          │
│    (nakama-down / postgres-down / postgres-conn-saturation   │
│    / disk-usage-high / cpu-sustained-high), Discord contact  │
│    point.                                                    │
│  - Loki + Promtail intentionally NOT brought back; logs      │
│    stay on `journalctl` / `docker logs <container>`. The     │
│    configs (`loki-config.yml`, `promtail-config.yml`) are    │
│    preserved in `infra/remote/nakama/` for a future re-      │
│    introduction without reconstruction.                      │
└──────────────────────────────────────────────────────────────┘
```

**Containers on nakama-prod-1:** one compose project in
`/opt/nakama` (`infra/remote/nakama/docker-compose.yml`), 10
services. Checked against `docker ps` on 2026-10-11.

| Container | Role |
|---|---|
| `postgres` | Postgres 16, Nakama's database (co-tenant since the 2026-05-06 consolidation). Dumped nightly by `pg-backup.timer`. |
| `nakama` | Nakama 3.25 + the `snoringcat.so` runtime plugin. Spawns local match containers through the Docker socket (`LocalDockerAllocator`). |
| `caddy` | TLS + reverse proxy for `nakama.`, `signaling.` and `grafana.snoringcat.games` (custom build with `caddy-ratelimit`). |
| `signaling-proxy` | Stateless WebRTC signaling bridge behind `signaling.snoringcat.games`; verifies the HMAC-signed URL token. |
| `geoip-sidecar` | DB-IP country lookups for the hybrid allocator's North-America check; MMDB refreshed by `geoip-refresh.timer`. Also a Prometheus target. |
| `prometheus` | Scrapes nakama, caddy, node-exporter, postgres-exporter, cadvisor, geoip-sidecar. 30d retention. |
| `grafana` | Dashboards + the 5 alert rules, Discord contact point. |
| `cadvisor` | Per-container CPU / memory / network / filesystem metrics for Prometheus, including the on-demand local match containers. |
| `node-exporter` | Host metrics for Prometheus. |
| `postgres-exporter` | Postgres metrics for Prometheus. |

Local match containers (`allocator_mode: local` or `hybrid`) come
and go on demand and are not compose services.

**Hostnames:**
- `nakama.snoringcat.games` — Nakama HTTP + realtime WS.
- `nakama-staging.snoringcat.games` — staging Nakama (CI).
- Game-server hostnames are Edgegap-assigned (per-deployment IPs).

**Ports:**
- 443 (Caddy → Nakama 7350).
- 7351 (Nakama console, SSH-tunnel only).
- Game server: 4433/UDP (ENet + WebRTC), 4434/TCP (WS
  signaling). Edgegap forwards declared ports to host directly
  (no GameLift-style remapping).

---

## Identity layer

### Provider matrix

| Provider | Status | Notes |
|---|---|---|
| Anonymous (device ID) | always available | First-launch default |
| Google | OAuth Web | Configured in pre-flight |
| Facebook | OAuth Web | Configured in pre-flight |
| Apple | deferred | Requires Apple Developer Program |
| Steam | deferred | Requires Steamworks |
| Epic | deferred | Requires Epic Online Services |

### Account linking (anonymous → permanent)

A player launches the game and gets an anonymous account
(device-bound). Later they tap "Sign in with Google" or "Sign in
with Facebook" in settings, which calls
`Nakama.session.link_*()`. The anonymous account is upgraded in
place — same `player_id`, same friends, same progression. No
data loss.

If a player accidentally creates two accounts (anon on device A,
permanent on device B), Nakama's link API will error with a
"identity already linked" code; the client offers a merge UI
(future improvement, deferred).

### Account deletion

Required for GDPR / CCPA / app-store TOS. Implemented in
`runtime/account.go`, `runtime/account_cron.go`, and
`runtime/privacy.go`:

1. The player confirms "Delete Account" in settings (a
   type-the-word confirmation), and the client calls the
   `delete_account` RPC.
2. The RPC queues `account_deletion_queue/current` for the player,
   server-only, holding `{user_id, requested_at, scheduled_for}`
   and nothing else, and at once clears everything a cancellation
   doesn't need:
   - the profile: display name to `[deleted]`, avatar URL, any
     email that isn't a login credential (in practice every
     email), language, location, time zone, and metadata;
   - friends (both directions, blocks included); group
     memberships (a party the player created is deleted with its
     settings rows, and in other parties leadership passes on);
     presence; leaderboard records on every board, declared or
     not (deleted, not anonymized; the public leaderboard route
     can serve its cached copy for up to a minute more, see
     "Public leaderboard route"); every storage object the player
     owns except the queue row (match history, settings, recent
     players, the stored IP, ...); the friend code; notifications,
     both the player's and the ones they sent to others (a friend
     request names them); sent chat messages, with the group join
     and leave events Nakama records under the player's id; wallet
     history; and the rows naming the player in other players'
     recent-players lists.
3. What stays for the 30-day grace window is what a sign-back-in
   cancellation needs: the account row with its sign-in ids
   (provider ids, device ids, custom id) and Nakama's generated
   username. `get_account_deletion_status` lets the client prompt
   at sign-in; `cancel_account_deletion` deletes the queue row and
   clears the `[deleted]` marker in one transaction and restores
   nothing (the SDK re-applies the display name and picture from a
   Google sign-in made in the same session).
4. While the deletion is pending, the profile stays clear: every
   sign-in, link, and session refresh re-applies the scrub (a
   Google sign-in refills an empty avatar from the provider), and
   so does the hourly account cron, which also strips the profile
   copy that queue rows written before 2026-10-11 carried.
5. After 30 days the cron purges the friend code, the recent-
   players references, and the notifications the player sent,
   then calls `nk.AccountDeleteId(recorded=
   true)`: Nakama deletes the account row, cascading devices,
   storage, notifications, messages, friends, groups, and
   leaderboard records, and tombstones the user id. Nakama's own
   `DELETE /v2/account` runs the same reference purge through an
   after hook.
6. The nightly `pg_dumpall` keeps 7 days, so deleted data can
   remain in backups for up to 8 days.

### Data export

`export_player_data` (`runtime/data_export.go`) returns everything
stored about the caller as indented JSON, format version 2:
`account` (profile, any email, every sign-in id, the friend code),
`account_deletion`, `last_ip_address`, `friends` (blocked players
included), `groups` (with each party's leader, mode, and
preferences), `leaderboard_records` (every board, read from
Nakama's table), `storage_objects` (everything the player owns, no
cap), `notifications`, `chat_messages`, and `wallet_ledger`. A
section that can't be read fails the whole export rather than
returning part of it. Crash reports carry no account id
(`crash_report.go`), so they can't be included. The game saves
the server's text verbatim (`Platform.auth.last_export_json`): to
`user://export` on desktop, as a browser download on web.

### Personal data the platform doesn't keep

- **Email.** Clients don't request the email scope (SDK 0.6.0).
  Every sign-in, link, and session refresh drops an email a
  provider's profile put on the account (older clients still ask
  Google for one), and email sign-in and email linking are
  refused, so none is stored as a credential either.
- **Client IP, beyond the next match.** `record_client_ip` keeps
  the latest IP for the matchmaker's server choice, which reads it
  within seconds and ignores it after an hour; the account cron
  deletes records older than an hour, so one lives about two
  hours at most.

---

## Matchmaking & game-session lifecycle

```
1. Client: nakama.matchmaker_add(query, props)
2. Nakama matchmaker: groups players matching the query
3. Nakama hook: MatchmakerMatched fires
4. Runtime module fleet_allocator.go:
   a. Reads matched players' IPs
   b. POST /v1/deployments to Edgegap with players' IPs
      (Edgegap picks closest deployment region)
   c. Polls Edgegap deployment until status=READY (5-15s)
   d. Returns {host, port, jwt} to all matched players via
      Nakama notification. The host sent to clients is
      `s-<ip-with-dashes>.<SERVER_DNS_BASE>` (computed from
      the deploy's PublicIP), NOT Edgegap's `*.pr.edgegap.net`
      FQDN — see "Per-deploy DNS pre-warming" below for why.
5. Client receives notification, opens UDP/WS to
   {host, port}, presents JWT for auth
6. Game server validates JWT, calls register_server RPC to
   Nakama with deployment info
7. Match plays. Server sends periodic heartbeat to Nakama.
8. Match ends. Server calls match_end RPC with results.
9. Nakama updates leaderboards, match_history, etc.
10. Server exits cleanly. Edgegap reclaims the container.
```

### Per-deploy DNS pre-warming

Web clients connect to game servers via WSS, which means the
TLS handshake's SNI must match the cert. We hold a wildcard
cert for `*.<SERVER_DNS_BASE>` (default `game.hopnbop.net`)
issued by Let's Encrypt and rotated by `cert-rotate.yml`.
Edgegap's auto-assigned `*.pr.edgegap.net` FQDN doesn't match
this cert, so we can't use it for WSS — the browser rejects
the handshake.

The fix is to mint a deterministic per-deploy hostname and
register it in DNS just before notifying matched clients:

1. **Runtime hook** (`fleet_allocator.go` in this repo) is the
   sole owner. After Edgegap reports the deploy READY, it:
   a. Computes `s-<ip-with-dashes>.<SERVER_DNS_BASE>` from
      `status.PublicIP`.
   b. POSTs a 60s-TTL CF A record for that name pointing at
      the IP. The record's `comment` field carries
      `edgegap deploy=<id> created=<iso>` — used by the
      watchdog to age out stale records.
   c. Sends `match_ready` notifications with the freshly
      resolvable hostname as `server_fqdn`.
   The hook runs on Hetzner where logs are easy to read; a
   prior attempt to do this in the container's entrypoint was
   abandoned because Edgegap's stdout isn't accessible from
   outside, making diagnosis painful.
2. **DNS watchdog** (`infra/remote/dns-watchdog/`) is an
   hourly systemd timer on the Nakama host that lists
   `s-*.<SERVER_DNS_BASE>` records and deletes any whose
   `comment` field has a `created=` timestamp older than
   `MAX_RECORD_AGE_HOURS` (default 4h). This catches records
   from deploys that ended without an explicit cleanup —
   the runtime intentionally does not delete on match-end,
   because Edgegap's deploy teardown is async and a same-IP
   redeploy would race against the delete. Records without
   a parseable timestamp are left alone (defensive — a
   missing timestamp shouldn't blow away a manually-placed
   record).

Required env on the Nakama runtime container's
`/opt/nakama/config.yml` `runtime.env` block:

| Var | Purpose |
|---|---|
| `CLOUDFLARE_DNS_TOKEN` | Zone:DNS:Edit on the SERVER_DNS_BASE zone. Same value cert-rotate uses. |
| `CLOUDFLARE_DNS_ZONE_ID` | CF zone ID for SERVER_DNS_BASE (the apex zone hopnbop.net for game.hopnbop.net subdomains). |
| `SERVER_DNS_BASE` | Optional. Apex used to derive the hostname. Defaults to `game.hopnbop.net`. |

When the CF env vars are absent, the hook still computes
`server_fqdn` and sends it in the match-ready payload, but
skips the DNS write — native (ENet) matches keep working,
web matches won't connect because their browser can't
resolve the hostname.

The container itself (`infra/game-server/entrypoint.sh` in
the consuming game's repo) does NOT need any CF credentials.
It just starts nginx for TLS termination and exec's the
Godot server. DNS is fully a Nakama-side concern.

**Authority model:** server-authoritative. Game server runs
deterministic frame simulation (rollback netcode); clients
predict + reconcile.

**Transport selection:** based on matched players' platforms
(read from matchmaker query attributes). ENet for native-only
matches; WebRTC for any match including web players.

### Allocator backend selection

Each game's `allocator_mode` (game.yaml, synced into the `games`
table) picks where its game servers run. `runtime/allocator.go`
(`resolveAllocatorMode`) turns it into a concrete backend per
match:

| `allocator_mode` | Backend per match |
|---|---|
| `edgegap` or unset | Edgegap deployment (retry loop with continent fallback) |
| `local` | Container on the Nakama host (`LocalDockerAllocator`, pulls `local_image_ref`) |
| `hybrid` | Local when every matched player's IP classifies as US/CA (geoip sidecar, static `/8` fallback), Edgegap otherwise |

Hop 'n Bop runs `hybrid`. The concrete backend is persisted as
`match_metadata.allocator_kind`, so `match_end` / `match_cancel`
stop the right one, and is reported to clients as the
match_ready payload's `allocator` field (`"edgegap"` or
`"local"`). Local request ids also start with `local-`.

**Probe override (`probe_allocator`).** A synthetic probe match
(every matched ticket carries `probe: "true"`) may pin the
backend with the ticket string property
`probe_allocator: "edgegap" | "local"`. The pin beats
`allocator_mode` and skips the hybrid geo decision. It applies
only when every entry carries the same valid value. A non-probe
match, entries that disagree, an entry without the property, or
any other value is logged (`ignoring probe_allocator=...`) and
the match routes normally.

It exists because the synthetic-match-probe job (claude-config)
runs from an NA desktop, so under `hybrid` it always lands on the
local host and would never exercise Edgegap. That job pins
`edgegap` on one fixed weekday and fails unless the payload's
`allocator` is `edgegap` and the deployment is torn down
afterwards. Real players never get the override: the probe-marker
gate keeps it off their matches, and probe tickets pair only
with each other through the targeted query
`+properties.probe:true`.

---

## Per-game configuration

Each game declares itself via `game.yaml` shipped in its
project. Backend loads it at Nakama startup.

```yaml
# Example: hopnbop/game.yaml
game_id: hopnbop
display_name: Hop 'n Bop
edgegap_app_slug: hopnbop-server
protocol_version: 31           # see "Per-game protocol versioning"
display_version: 0.31.0
ports:
  game: 4433/udp                # ENet + WebRTC
  signaling: 4434/tcp           # WS signaling for WebRTC
transports: [enet, webrtc]
auth_providers: [anonymous, google, facebook]
matchmaker_rules:
  min_players: 2
  max_players: 4
  ticket_expiry_seconds: 60
  cross_play: true                # web + native can match
                                  # (transport falls back to WebRTC)
  rules: []                       # reserved for future filters
                                  # (skill bands, region pinning, etc.)
leaderboards:                     # created as {game_id}_{id};
  - id: ffa                       # see "Leaderboards" below
    sort: desc
    operator: best
    reset_schedule: null
  - id: weekly
    sort: desc
    operator: best
    reset_schedule: '0 0 * * 1'  # Mon midnight UTC
legal:
  terms_path: legal/en/terms.txt
  privacy_path: legal/en/privacy.txt
  data_deletion_path: legal/en/data_deletion.txt
  legal_version: 4
```

**Schema versioning:** the `game.yaml` schema itself is versioned
(`schema_version: 1` at the top). Schema migrations are managed
in `snoringcat-platform/backend/schemas/game_yaml/`. Adding a new
key without breaking existing games doesn't bump the schema; only
breaking changes do.

**Storage:** at backend startup, `per_game_config.go` ensures
the Postgres `games` table exists (idempotent DDL) and warms an
in-process cache from whatever rows are already there. The
runtime does NOT read `game.yaml` files from the filesystem —
the Nakama container has no access to game repos. Instead, each
game's deploy pipeline POSTs its `game.yaml` (converted to JSON)
to the operator-only `register_game` RPC, which upserts the row
and refreshes the cache.

```sql
CREATE TABLE games (
  game_id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  edgegap_app_slug TEXT NOT NULL,
  protocol_version INTEGER NOT NULL,
  display_version TEXT NOT NULL,
  config JSONB NOT NULL,         -- full game.yaml content as JSON
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Sync flow:

1. Game repo's `scripts/sync-game-config.ps1` parses
   `game.yaml`, validates required fields locally, converts to
   JSON.
2. POSTs to
   `https://nakama.../v2/rpc/register_game?http_key=...&admin_key=...&unwrap=true`
   with the JSON as body. `admin_key` is the runtime's
   `GAME_ADMIN_KEY` (next section).
3. Runtime `register_game` handler (HTTP key plus admin key)
   unmarshals, re-validates, upserts via `INSERT ... ON CONFLICT
   ... DO UPDATE`, then refreshes the in-process cache entry.
4. Clients read public projections via `get_game_config`.

Runtime modules read from the cache (refreshed on each
`register_game` write — no TTL today; a future background
refresh could pick up out-of-band Postgres edits).

### Runtime keys and RPC auth

Nakama runs a runtime RPC for one of two callers: a player's
session, or anyone presenting the runtime HTTP key
(`--runtime.http_key`, `?http_key=`). An RPC sees which one by
whether `RUNTIME_CTX_USER_ID` is set; `requireClientSession` and
`requireServerToServer` (`runtime/auth.go`) gate on that.

**The HTTP key is not a secret you can lean on.** hopnbop clients
embedded it from 2026-05-02 through 0.47.0, for one pre-sign-in
`version_check` call, in a public repo. Treat it as known until it
is rotated, and assume any HTTP-key caller might be hostile.

| Key | Who holds it | Where it lives |
|---|---|---|
| `NAKAMA_HTTP_KEY` | game servers (`register_server`, `match_end`, `match_cancel`), ops probes (`runtime_status`), clients up to 0.47.0 (`version_check`) | `credentials.env`; `/opt/nakama/.env`; the Edgegap app version's env (`game-server.yml` registers it); the local allocator passes it to its containers; GitHub secret `NAKAMA_HTTP_KEY` |
| `GAME_ADMIN_KEY` | the operator's deploy tooling only (`register_game`, `bulk_import`) | `credentials.env`; `/opt/nakama/.env` (rendered by `scripts/lib/nakama-env.ps1`); GitHub secret `GAME_ADMIN_KEY` in each game repo |

So each RPC an HTTP-key caller can reach carries its own check:

- **`register_game` and `bulk_import`** need both keys: the HTTP
  key, and `GAME_ADMIN_KEY` as the `admin_key` query parameter,
  compared in constant time (`runtime/admin_key.go`). Fail
  closed: while `GAME_ADMIN_KEY` is unset they refuse every call
  (HTTP 400, "GAME_ADMIN_KEY is not set"), and the rest of the
  runtime runs normally. `runtime_status` reports
  `admin_key_configured` (a boolean, never the key). A missing or
  wrong key is HTTP 401; a client session is 403.
- **`match_end`** writes results only for a `request_id` that has
  both a server registration and the `match_metadata` row the
  matchmaker hook wrote at allocation, and only for the users that
  row says were matched (`user_ids`). `register_server` accepts
  any `request_id`, so the registration alone proves nothing. What
  remains: a player in a match learns its `request_id` from
  `match_ready`, so with the HTTP key they can still forge bounded
  scores for that match or end it early (`match_cancel`). A
  per-deployment secret handed to the game server would close it.
- **Version check without a key.** `GET` or `POST
  /snoringcat/version_check` (`versionCheckHTTPPath`, registered
  with Nakama's `RegisterHttp`, so Nakama authenticates nothing)
  answers exactly like the `version_check` RPC. Clients after
  0.47.0 use it (`Platform.get_version_check_url()`), and
  `Platform.initialize` refuses a `nakama_http_key` config entry.
  Caddy allows 600 requests a minute per IP (the `version_check`
  zone; see "Leaderboards" → "Rate limit" for the 429).
- **Public leaderboard without a key.** `GET
  /snoringcat/leaderboard` is the other `RegisterHttp` route: a
  read-only, cached projection of a board for the game websites
  and the in-game leaderboard (see "Leaderboards").
- **Caddy's access log masks both keys** in `request.uri`
  (`format filter` in the `Caddyfile`), so neither reaches
  `access.log` or the caddy-access-analytics report.

The rest either need a player's session, which the HTTP key alone
can't supply, or return only public data (`version_check`,
`transport_select`). The header comment of `runtime/main.go` lists
every RPC by the callers it admits; keep it current when adding
one.

### Leaderboards

**Declared per game, created by the runtime.** A game lists its
boards in `game.yaml` under `leaderboards` (`id`, `sort`,
`operator`, `reset_schedule`). The runtime creates each one in
Nakama as `{game_id}_{id}` (hopnbop's `ffa` is `hopnbop_ffa`): at
boot for every registered game, and again after every
`register_game`, so a new board needs no restart
(`runtime/leaderboards.go`). `register_game` refuses an invalid
list (an ID that isn't 1-64 of `[a-z0-9_]`, a duplicate ID, an
unknown `sort` or `operator`) before it stores anything. When
Nakama refuses a board (a bad cron, say), the call fails with the
config already stored; fix `game.yaml` and register again.

Boards are **authoritative**: Nakama refuses record writes from
client sessions. The only writer is `match_end`, which records each
matched player's in-match score for an allocated, non-synthetic
match (see "Runtime keys and RPC auth"). With `sort: desc` and
`operator: best`, hopnbop's `ffa` keeps each player's best
single-match score.

**Nakama never changes an existing board.** `LeaderboardCreate` is
a no-op for an ID that exists, so editing `sort`, `operator` or
`reset_schedule` does nothing to a live board. The runtime logs
`leaderboard <id> differs from game.yaml: ...` (sort, operator,
whether it resets, and whether it is authoritative) at boot and on
`register_game`. To apply a change, delete the board in the Nakama
console, which deletes its records, and restart Nakama or register
again.

**History.** Nothing created boards before 2026-10-12. Production
had no leaderboard at all, so every `match_end` leaderboard write
since the migration failed with "leaderboard not found" (logged as
a warning), `get_player_stats` always answered its unranked
defaults, and the in-game and website leaderboards were empty.

**Readers:** the public route below (the game websites and, since
SDK 0.7.0, the in-game leaderboard, both through
`Platform.get_public_leaderboard_url`), `get_player_stats` (the
caller's own record, and their rank from the public route's
listing), `export_player_data` (the caller's records on every board,
read from Nakama's `leaderboard_record` table), and the deletion
cascade (`delete_account` deletes the caller's record on every board
they have one on, declared or not).

Until 2026-10-12 `get_player_stats` and `export_player_data` read
`LeaderboardRecordsList`'s first return value, which is the board's
top page whatever the owner filter, not the owner's records. Once a
board existed, every player would have been shown the leader's
score, and a data export would have carried other players' records.
No export was affected: production's board had no records before
both were fixed. `get_player_stats` now reads only the second return
value (the owner's record), and the export reads the player's own
rows (see "Data export").

#### Public leaderboard route

`GET https://nakama.snoringcat.games/snoringcat/leaderboard?game_id=<id>&leaderboard=<board>&limit=<n>`
(`runtime/leaderboard_public.go`) serves one board to the game
websites. It is registered with `RegisterHttp`, so Nakama
authenticates nothing: it needs no session and no key, and a page
must never send either.

| Parameter | |
|---|---|
| `game_id` | Required. A registered game. |
| `leaderboard` | Required. An ID the game's `leaderboards` declares, without the game prefix (`ffa`). |
| `limit` | Optional. 1 to 100, default 50. |

A 200 response (`version` is the schema version, bumped only for a
change existing clients can't read):

```json
{
  "version": 1,
  "game_id": "hopnbop",
  "leaderboard": "ffa",
  "sort_order": "desc",
  "operator": "best",
  "next_reset_at": null,
  "generated_at": "2026-10-12T04:09:20Z",
  "entries": [
    {"rank": 1, "display_name": "Moonhopper", "score": 4210},
    {"rank": 2, "display_name": "Bunny McBounce", "score": 3895},
    {"rank": 2, "display_name": "Pip", "score": 3895},
    {"rank": 4, "display_name": "Zap", "score": 2750}
  ]
}
```

`sort_order`, `operator` and `next_reset_at` (RFC 3339, null when
the board never resets) describe the live Nakama board. Errors are
JSON, `{"version": 1, "error": "<code>", "message": "..."}`: 400
`missing_game_id`, `missing_leaderboard`, `invalid_limit`; 404
`unknown_game`, `unknown_leaderboard`; 405 `method_not_allowed`;
503 `unavailable` (with `Retry-After: 10`). A message never echoes
the request.

**Privacy.** Per player the route returns exactly three fields:
`rank`, `display_name` and `score`. Never a user id, username,
avatar URL, device id, IP address, email, record time or record
metadata. A player is listed only when all of these hold:

- the account exists and isn't banned (no `disable_time`);
- it has no `account_deletion_queue` row, so a player in the
  deletion grace window is hidden whatever their display name says;
- its display name, trimmed, is neither empty nor `[deleted]`.
  Anonymous (device-only) accounts have none, so guests are never
  listed, the same as in the in-game leaderboard;
- its username doesn't start with `probe-` (the
  synthetic-match-probe's identities; claude-config's
  player-metrics-daily uses the same filter).

Linked accounts take their display name from the provider
(hopnbop's client sets it to the Google profile name when Google is
linked), so in practice a listed name is the name on the player's
Google or Facebook account. Ranks count listed players only, and
equal scores share a rank (1, 2, 2, 4).

**Load and freshness.** Each board is read from Postgres at most
once a minute, by one request while concurrent ones wait for its
result: per page of 100 records, one `LeaderboardRecordsList`, one
`AccountsGetId` and one storage read, and up to 5 pages to find 100
listable players. `match_end` invalidates the cache after it
records scores, so a finished match shows on the next request (one
extra read per match). Responses carry `Cache-Control: public,
max-age=60` and `Age`, so a browser keeps a copy only for the rest
of that minute: a new score shows on a page within 60 seconds, and a
deletion (which doesn't invalidate) within two minutes. When a read
fails, the last good copy is served for up to 10 minutes (so a
deletion made just before a Postgres outage can stay listed that
long), then 503.

**Rate limit.** Caddy allows 300 requests a minute per IP (the
`public_leaderboard` zone in `infra/remote/nakama/Caddyfile`), sized
for a school or cafe network where many players share one address;
the route's own cache already protects Postgres. A refusal is a 429
with `Retry-After`, `Access-Control-Allow-Origin: *` (so a page sees
the 429, not a failed fetch) and the route's JSON error shape,
`{"version": 1, "error": "rate_limited", ...}`. The unauthenticated
`/snoringcat/version_check` route has its own zone, 600 a minute per
IP, with the same 429. Until 2026-10-12 the leaderboard allowed 60 a
minute, its 429 carried no CORS headers, and the version check had
no limit.

**CORS.** Every response carries `Access-Control-Allow-Origin: *`
(Nakama's CORS middleware sets it too, and answers preflights before
the handler runs), `Cross-Origin-Resource-Policy: cross-origin` and
`X-Content-Type-Options: nosniff`. GET and HEAD are served, OPTIONS
answers 204, and anything else gets 405.

**Tests.** `runtime/leaderboard_public_test.go` and
`runtime/leaderboards_test.go`; compliance `test_public_leaderboard.gd`,
which puts scores in through the real matchmaker, `register_server`
and `match_end` path on the mock-deploy stack.

#### Player stats and match history

Both RPCs take the caller's client session and answer for the game
the session is signed into.

`get_player_stats` (payload `{}` for the caller, or
`{"player_id": "<user id>"}`) answers:

```json
{
  "player_id": "…",
  "best_score": 1750,
  "rank": 7,
  "rating": 1750,
  "matches": 2
}
```

- `best_score`: the player's own record on the game's `ffa` board
  (their best single-match score), or null before their first
  recorded match.
- `rank`: their rank on the public route's listing, the same number
  the websites and the in-game leaderboard show, or null when they
  aren't listed there (no record, anonymous, queued for deletion, a
  probe, or outside the top 100). It comes from the route's cached
  read, so it is never computed differently from the list.
- `rating` and `matches` are **deprecated**, kept for clients that
  still read them. `rating` was never a rating: it is `best_score`,
  or 1500 for a player with no record. `matches` is the record's
  `num_score`, which with `operator: best` counts only the matches
  that improved the best score, not matches played. Use
  `get_match_history`'s `totals.matches` for that.

`get_match_history` answers `{"matches": [...], "totals": {"matches",
"wins", "kills", "bumps"}}`. `matches` holds the caller's newest 50
`match_history` rows (`match_id`, `ended_at`, `is_winner`, `score`,
`kills`, `bumps`), newest first; `totals` sums every row, up to
10,000. Storage keys are request ids, not times, so the runtime
reads every row and sorts by `ended_at`; before 2026-10-12 it
returned the first 50 by key, which wasn't the newest.

The game's My Stats screen shows `best_score`, `rank`, the totals
and the latest ten matches. Tests: `runtime/player_stats_test.go`,
compliance `test_player_stats.gd` and `test_public_leaderboard.gd`.

---

## Per-game protocol versioning

**Why this exists.** Each game has its own client/server
network protocol. When game X ships a breaking protocol change
(new RPC, changed message shape, new replication property), only
clients matching X's new server protocol can connect. **Other
games on the same Nakama instance are completely unaffected.**

**Where the version lives:**

- **Source of truth:** `protocol_version: N` in the game's
  `game.yaml`.
- **Loaded into:** the `games.protocol_version` column at
  startup.
- **Exposed via:** `Platform.get_protocol_version(game_id)`
  Nakama RPC (unauthenticated).
- **Synced to:** `project.godot` `config/protocol_version` for
  the Godot client (must match `game.yaml`).

**Bump procedure (when shipping a breaking protocol change for
game X):**

1. Edit `game.yaml`: bump `protocol_version` (integer, +1).
2. Edit `project.godot`: bump
   `application/config/protocol_version` to match.
3. Build and deploy server image (release pipeline; via
   `release.yml`).
4. Roll Nakama runtime modules — config reloads from `game.yaml`
   on restart.
5. Old clients now hit the version-mismatch screen (handled
   client-side: `LOADING.UPDATE_REQUIRED` or equivalent) and are
   prompted to update.

**Critical: bumping game X's `protocol_version` has zero effect
on other games.** Game Y players continue normally. If you
accidentally bump game Y's version, you'd lock Y's players out
unnecessarily.

**Common mistakes to avoid:**

- ❌ Bumping `display_version` (e.g., `0.31.0` → `0.31.1`)
  thinking it's the protocol — `display_version` is cosmetic
  only and doesn't affect compatibility.
- ❌ Bumping `protocol_version` for any change that's not a
  breaking network change — non-breaking changes don't need a
  bump and bumping unnecessarily forces clients to update.
- ❌ Forgetting to keep `game.yaml` and `project.godot` in sync
  — both must read the same number.
- ❌ Bumping multiple games' `protocol_version` in one PR when
  only one game changed — bump only the affected game.

**When to bump (rules of thumb):**

- ✅ New required RPC method that older clients don't know about.
- ✅ Changed shape of a synced state (added/removed property,
  changed type).
- ✅ Changed match-result reporting format.
- ✅ Changed authentication handshake.
- ❌ Cosmetic / UI-only changes.
- ❌ Server-side logic changes that don't affect the wire
  protocol.
- ❌ Backend RPC changes that are backward-compatible (e.g.,
  added optional field).

**Client behavior on mismatch:**

- At app startup, client calls `Platform.get_protocol_version`
  for its game.
- If client's `protocol_version` < server's: show "update
  required" screen, link to store.
- If client's `protocol_version` > server's: show "server is
  out of date" screen (rare; happens if backend rolled back).
- Equal: proceed normally.

**Validation in CI:** `pr-validate.yml` checks that
`game.yaml::protocol_version` and
`project.godot::config/protocol_version` match. Mismatch fails
CI.

---

## Multi-game design principles

1. **Every state-bearing thing has `game_id`.** Match history,
   leaderboards, presence, parties, active sessions, fleet
   state. Even if there's only one game today.
2. **Identity is global, not per-game.** A player is one
   Snoring Cat account; their game-specific progress is in
   `game_profiles[player_id, game_id]`.
3. **Friends are global.** A friendship between A and B is one
   row, not per-game.
4. **Presence is single-row, current-game-aware.**
   `presence[player_id]` records current `game_id` and game
   state. Friends in any game see "Levi is online in Hop 'n
   Bop."
5. **Settings split into `global` and `game#{id}` scopes.**
   Master volume is global; game-specific keybindings are
   game-scoped.
6. **Compliance scope is global by default.** Account deletion
   removes the player from all games at once. Per-game
   privacy filters can be added later if a future game needs
   regional restriction.
7. **Per-game config is declarative**, in `game.yaml`. Adding a
   new game should require zero backend code changes (only
   adding the YAML and registering it).

---

## Operations & deployment

### Deploy a Nakama runtime change

1. Edit Go code in `snoringcat-platform/runtime/`.
2. Open PR. `pr-validate.yml` runs `go test`, `go vet`,
   `staticcheck`.
3. Merge. Tag a release.
4. `release.yml` builds the new runtime image, SCPs to Nakama
   box, runs `docker compose up -d` with new image (zero
   downtime — Nakama hot-swaps the runtime module).

### Deploy a game-server change

1. Bump `project.godot::config/version` (and
   `protocol_version` if breaking).
2. Bump `game.yaml::display_version` to match.
3. Tag the game's repo.
4. `release.yml` exports Linux server `.pck`, builds Docker
   image, pushes to Edgegap registry, bumps Edgegap fleet
   version. Old game-server containers finish their matches;
   new matches use the new version.

### Deploy a game-client change

1. Tag the game's repo.
2. `release.yml` exports Godot for all platforms, syncs web
   build to S3, invalidates CloudFront. Native exports
   uploaded as GitHub release artifacts (or Steam if
   applicable).

### Add a new game

1. Create new game project (e.g., `ggj26_private`).
2. Add `snoringcat-platform` as a submodule under
   `third_party/`.
3. Author `game.yaml`.
4. Reference `PLATFORM_ARCHITECTURE.md` from the new game's
   `CLAUDE.md`.
5. Add an Edgegap app for the new game.
6. Provision the new game's deploy keys, secrets, etc. (per
   game-checklist).
7. Restart Nakama; new game appears in `games` table. Players
   can connect.
8. (Optional) Promote infra to Pulumi modules if you have ≥2
   games and the duplication is real.

### Scale Edgegap fleet capacity

Edgegap auto-scales based on demand. To set bounds:

```bash
edgegap-cli app set-version --app hopnbop-server --version <v> \
  --max-deployments 50
```

Default: no upper bound. Lower bound: 0 (true scale-to-zero).

### Rotate OAuth secrets

1. Generate new secret in provider dev console (Google /
   Facebook).
2. Put it where it is read.
   - **Google:** the game's token broker, which is the
     `GOOGLE_OAUTH_CLIENT_SECRET` Pages secret on
     `hopnbop-website` (hopnbop's
     `web/functions/api/oauth/google/exchange.js` has the
     `wrangler` command), and `credentials.env`. Not Nakama: it
     checks Google ID tokens against Google's public keys, and
     nothing reads the copy `config.yml` passes into the runtime
     env.
   - **Facebook:** the `nakama.snoringcat.games` Nakama config
     (env vars), then restart Nakama.
3. Old secret remains valid for the provider's grace period
   (~7 days for Google).
4. Google: check sign-in with `python scripts/test-google-auth.py`,
   which goes through the broker as the client does, and with
   `--direct-google`, which uses the `credentials.env` copy. While
   the old secret still works, a pass doesn't prove the broker has
   the new one, so run it again once the old one stops working.

### Cost monitor

Systemd timer on the Nakama host (every 5 min) that polls
Hetzner Cloud, Edgegap, Cloudflare R2, Cloudflare Pages, and
GitHub Actions APIs for month-to-date usage; pings Discord on
threshold crossings and queues a daily summary for the morning
brief.

**Source files (this repo):**
`infra/remote/cost-monitor/{cost-monitor.sh, cost-monitor.service,
cost-monitor.timer}`. Installed to `/opt/snoringcat/cost-monitor/`
by `scripts/phase-b.ps1 -StartAt CostMonitor -StopAt CostMonitor`
(copies the files, renders `.env` from `credentials.env`, enables
the timer, runs it once). Not Pulumi.

**Trigger:** `cost-monitor.timer` — every 5 minutes
(`OnCalendar=*-*-* *:00,05,…,55:00 UTC`, `RandomizedDelaySec=30`,
`Persistent=true`). Tightened from hourly on 2026-06-01 because
short matches were starting and ending between hourly polls.

**Behaviour:**
- Silent on most runs.
- One routine cost summary per day, on the first run at or after
  `DAILY_SUMMARY_HOUR_UTC` (default `9` = 09:00 UTC). It is
  **not** posted to Discord: it goes to the service-status JSONL
  queue, which the desktop's `service-status-sync` drains into the
  status trail (`claude-config/jobs/status/`). (Direct posting
  stopped 2026-05-07; the ">=" rather than "== that hour" test
  means a host that was down at 09:00 still reports later that day
  instead of dropping the day.)
- **R2** is summed across every bucket in the account, since the
  10 GB free tier is account-wide (`hopnbop-assets` plus
  `hopnbop-pulumi-state-r2`, which holds Pulumi state and the
  pg-backups). It reads each bucket's `/usage` figure, one API call
  per bucket. Until 2026-10-11 it read only `R2_BUCKET`, which is
  now ignored (`R2_BUCKETS` can restrict the sum).
- **CF Pages** counts Cloudflare-run builds, which is what the 500/month
  free tier limits: deployments whose `deployment_trigger.type` is
  not `ad_hoc`. Direct uploads (`wrangler pages deploy` from
  `deploy-cf-pages.ps1` or CI) run no Cloudflare build and are
  reported separately as `cf_pages_direct_uploads`. Every
  `hopnbop-website` deployment to date is a direct upload. The
  `CF_PAGES_*` thresholds compare against builds only.
- Every run writes the latest usage into the state file: `r2_tracked`,
  `r2_gb`, `r2_bucket_count`, `r2_error`, `cf_pages_tracked`,
  `cf_pages_builds`, `cf_pages_direct_uploads`, `usage_measured_at`.
  claude-config's `weekly-cost-snapshot` reads them from there. A
  metric that could not be measured (no token, API error) is
  `*_tracked: false` with an empty value and prints "not tracked"
  in the summary, never a stand-in `0`.
- Any unhandled failure pings Discord directly (rate-limited to
  one ping per 6h via `/var/lib/snoringcat/cost-monitor-failed.stamp`,
  cleared on the next clean run). Before that existed, a fatal
  error just marked the systemd unit failed and the cost lines
  silently vanished from the morning brief.
- Threshold crossings (`BUDGET_WARN_LOW`, `_MID`, `_HIGH`,
  `EMERGENCY_CAP`, plus R2 storage bands and Cloudflare Pages
  build-count bands `CF_PAGES_WARN_BUILDS` /
  `CF_PAGES_HARD_BUILDS`) ping immediately, gated by
  `/var/lib/snoringcat/cost-monitor-state.json` so each
  threshold alerts once per month.
- **Emergency action:** crossing `EMERGENCY_CAP` (default
  `$50`) PATCHes the Edgegap app to `capacity_max=0`, halting
  new game-server allocations. Manual reset required.
  (The action is inline in `cost-monitor.sh`; there is no
  separate `emergency-shutdown.sh`.)

**Config:** `/opt/snoringcat/cost-monitor/.env` on the host,
rendered by phase-b from `credentials.env`: `HCLOUD_TOKEN`,
`EDGEGAP_TOKEN`, `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID`,
threshold overrides, and the `DISCORD_WEBHOOK_URL` (separate copy
from `~/.claude/jobs/discord-config.json`). The Cloudflare token is
`CLOUDFLARE_COST_MONITOR_TOKEN`, a dedicated account-owned read-only
token (Cloudflare Pages and Workers R2 Storage read), created and
deployed 2026-10-11. phase-b falls back to `CLOUDFLARE_PAGES_TOKEN`
(Pages:Edit + Workers R2 Storage:Edit + Account Settings:Read) only
if `credentials.env` loses it. `GITHUB_TOKEN` is not rendered
today, so the GitHub Actions block is skipped (its numbers read 0).

**Inspect:**
```bash
ssh root@nakama.snoringcat.games
systemctl status cost-monitor.timer
journalctl -u cost-monitor.service -n 50
cat /var/lib/snoringcat/cost-monitor-state.json
```

### Desktop dead-man (desktop-drain-watch)

Every job that watches prod (prod-liveness-ping, slo-burn-rate,
job-watchdog, backup-restore-smoke, ...) runs on Levi's desktop under
Task Scheduler, so when that machine is off or logged out, monitoring
and its alerting stop together. `desktop-drain-watch.timer` is the
off-desktop check for that. It runs daily at 16:37 UTC on the Nakama
host.

The desktop's `service-status-sync` drains
`/var/log/snoringcat/service-status.jsonl` daily at 07:03 PT. Phase A
moves it aside to `*.draining` and touches a fresh queue; phase B
deletes the staging file. So an entry left undrained in either file
for more than `DRAIN_WATCH_STALE_HOURS` (36) means the drain has
stopped: the desktop is dark, or the drain is broken. The script then
pages Discord, at most once per 20h while stale. With pg-backup's
03:11 UTC entry, one missed drain reads ~13.5h old at the check and
two consecutive misses ~37.5h. Recovery clears the stamp
(`/var/lib/snoringcat/desktop-drain-watch.stamp`) and queues a green
status entry. Blind spot: an empty queue reads fresh, but the
producers that fill it (pg-backup, cost-monitor) page on their own
failures.

- Source: `infra/remote/desktop-drain-watch/`, installed by
  `scripts/phase-b.ps1 -StartAt DrainWatch -StopAt DrainWatch`.
- Test without posting: `desktop-drain-watch.sh --dry-run`, plus
  `--queue <file> --stamp <file> --now <epoch>` to drive each branch.
- If `service-status-sync` is ever disabled on purpose, disable this
  timer too, or it pages daily.

### Crash reports

Game clients and dedicated game servers report crashes and logged
errors to the runtime's `crash_report` RPC
(`runtime/crash_report.go`). hopnbop's sender is
`src/core/crash_reporter.gd`; see its CLAUDE.md "Crash reporting".

**Callers.** A player calls it through their Nakama session
(anonymous accounts included) with `role: "client"`. A dedicated
server calls it with the runtime HTTP key (`?http_key=`, read from
`NAKAMA_HTTP_KEY` in the container environment) with
`role: "server"`. Any other pairing is rejected. No admin key is
involved.

**Payload** (JSON object, at most 16 KiB): `game_id` (must be
registered), `game_version`, `protocol_version` (a whole number;
`2.0` is accepted because Godot's JSON round trip produces it),
`platform` (`[a-z0-9_]{1,16}`, e.g. `windows`, `web`), `os` (at most
64 bytes), `role`, `message` (one line, at most 512 characters),
`signature` (16 lowercase hex characters: a hash the game computes
over the normalized first error lines), and optionally
`log_excerpt` (at most 12 KiB). Unknown fields are ignored and never
stored. The response is `{"accepted": true, "id": "<storage key>"}`.
Errors: 400 (gRPC 3) for any invalid report, which the game drops
instead of retrying; 429 (gRPC 8) past a rate limit; 500 (gRPC 13)
when the write fails.

**Privacy.** A report carries no account id, IP address, or device
id, and the runtime adds none. The caller's user id is used only as
the key of the in-memory rate limiter, never stored or logged. The
runtime redacts the message and excerpt again before storing them
(JWTs, bearer and basic credentials, secret-named and id-named
parameters and JSON fields such as `device_id=`, signaling-URL
tokens, UUIDs, home-directory user names, long hex and numeric ids,
emails, IPv4 addresses), on top of the game's own redaction. Game
logs from before SDK 0.5.2 carried session and refresh tokens.

**Limits** (in memory, reset when Nakama restarts, which every
runtime deploy does): 5 reports per hour per session user; 200 per
UTC day for all HTTP-key callers together; 500 per UTC day overall.
A rejected or invalid report uses no budget. The first rejection by
a daily cap each day logs a warning (`crash_report global daily cap
reached ...`), since a full global cap turns away every player's
report until midnight UTC. Because the caps reset on restart, the
prune also enforces a hard cap of 20,000 stored reports (about
320 MB at the size limit), dropping the oldest.

**Storage.** One object per report in the `crash_reports`
collection, owned by the system user
(`00000000-0000-0000-0000-000000000000`), permission read=0 and
write=0, so no client can read or write one. Keys are the UTC
receive time plus a random suffix
(`2026-10-11T18:30:05.123456Z-1a2b3c4d`), so key order is time
order. The value is the validated payload plus `received_at`. The
runtime logs one structured line per accepted report
(`crash_report accepted`, with `signature`, `game_id`,
`game_version`, `platform`, `role`, and the key).

**Retention.** A goroutine started from `InitModule` (the
`account_cron.go` mechanism) deletes reports older than 30 days,
once at boot and then hourly. It walks the system user's objects in
key order (Nakama lists one owner's storage `ORDER BY read, key`,
and every report has read=0) and stops at the first one inside the
window. Then it counts what is left with one read-only SQL query
and, past 20,000, deletes the oldest.

**Reading reports.** claude-config's daily `prod-health-check`
reports the last 24 hours (count, new signatures, top signatures)
and goes yellow on a new signature or a spike. Reports whose
`platform` is `test` are synthetic (post-deploy probes) and are
counted apart, never alerted on. To dig in, on the host:

```bash
docker exec -e PGPASSWORD=<pw> -i postgres psql --no-password \
  -h 127.0.0.1 -U nakama -d nakama
```

```sql
-- The last 24 hours, by signature.
SELECT value->>'signature' AS signature, count(*) AS reports,
       string_agg(DISTINCT value->>'game_version', ',') AS versions,
       string_agg(DISTINCT value->>'platform', ',') AS platforms,
       max(value->>'message') AS message
FROM storage
WHERE collection = 'crash_reports'
  AND create_time > now() - interval '24 hours'
GROUP BY 1 ORDER BY reports DESC;

-- One signature's newest report, with its log excerpt.
SELECT key, value->>'message', value->>'log_excerpt'
FROM storage
WHERE collection = 'crash_reports'
  AND value->>'signature' = '<signature>'
ORDER BY key DESC LIMIT 1;
```

Before 2026-10-11 nothing was stored: the RPC did not exist (the
Phase D rewrite pointed the game at it as "not yet implemented"),
and the game's reporter returned early on a removed token-store
lookup.

### Backups and disaster recovery

**Backups.** `pg-backup.timer` runs nightly at 03:11 UTC: a
`pg_dumpall` of the co-tenant Postgres (roles, ACLs, the `nakama`
DB) to `s3://hopnbop-pulumi-state-r2/pg-backups/postgres-YYYY-MM-DD.sql.gz`,
with 7-day retention and a Discord ping on any failure.
claude-config's weekly `backup-restore-smoke` replays the newest
dump into a scratch database and compares table counts against
live. The manual restore procedure (stop Nakama first; psql targets
the `postgres` maintenance DB, not `nakama`) is in
`hopnbop/CLAUDE.md` → "Backups".

**Recovery targets.** These are current targets, set 2026-10-11
from the 2026-10-07 DR drill ("db host vanishes"). Revise them
deliberately, after the next drill or a real recovery, rather than
letting them drift.

| | Target | Basis |
|---|---|---|
| **RPO** | Up to 24h of writes | Bounded by the nightly 03:11 UTC `pg_dumpall`. No WAL archiving or point-in-time recovery, so a host lost just before 03:11 loses that day. The 7-day R2 retention bounds how far back a restore can reach. |
| **RTO** | 60-90 min | The drill's estimate with its Gaps A and B fixed (both fixed 2026-10-11; 90-180 min before). Covers detection, triage, `phase-a.ps1 -RestoreBackup`, `phase-b.ps1`, the host-key re-pin and sync, and the smoke check. |

Matches already running on Edgegap survive a Nakama-host outage;
login, lobby and matchmaking are what go down.

**Full-host rebuild:**

1. `pwsh -File scripts/phase-a.ps1 -RestoreBackup`. Pulumi creates
   the host (the Cloudflare A records follow the new IP), then
   cloud-init runs, then **HostKeys** asks you to confirm the new
   ed25519 fingerprint in the Hetzner web console
   (`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`). It rewrites
   `claude-config/jobs/known_hosts` and
   `~/.hopnbop-migration/known_hosts`. Then come the compose files,
   the newest R2 dump restored before Nakama first boots, each
   game's local game-server image pulled, and verification.
2. Push the re-pinned `known_hosts` through claude-config:
   `& "$HOME\Repositories\claude-config\sync-if-changed.ps1"`. The
   desktop's hourly sync would otherwise push it within the hour, and
   the laptop has no scheduled sync. Until it lands, every scheduled
   prod job fails host-key verification.
3. A new IP needs nothing else: the claude-config jobs and skills SSH
   to `nakama.snoringcat.games`, which Pulumi repoints. HostKeys still
   lists any claude-config file that names the old IP.
4. `pwsh -File scripts/phase-b.ps1`: obs configs plus the host
   timers (cost-monitor, geoip-refresh, pg-backup,
   desktop-drain-watch).
5. Smoke check: the `hopnbop-check-prod` skill.

**`/opt/nakama/.env`** is rendered whole by
`scripts/lib/nakama-env.ps1`, which phase-a and phase-b share. It
writes exactly the variables `docker-compose.yml` reads: secrets and
the Edgegap app fallbacks from `credentials.env`, `LOCAL_PUBLIC_IP`
from the Pulumi IP, and the rest static or deliberately blank. The
run fails if the file and the compose file ever disagree. Until
2026-10-11 the scripts kept separate partial lists and the live file
had been completed by hand. A script-built host came up with the
matchmaker hook and the local allocator off, phase-b's rewrite
dropped `SIGNALING_HMAC_SECRET`, and a rebuilt Grafana kept its
default admin password. Checked against prod on 2026-10-11: all 27
rendered values match what the running containers use.

**Game-server releases need the image on the host too.** The local
allocator needs each game's `local_image_ref` already there. It
creates containers through the Docker Engine API, which never pulls,
and under `allocator_mode: hybrid` a failed local allocation fails
the match (there is no Edgegap fallback). phase-a pulls the images
after a rebuild. For a release, hopnbop's `sync-game-config.ps1`
pulls the new tag over SSH before registering it, and refuses to
register if the pull fails. A CI release (`release.yml`) has no SSH
key and skips the pull with a warning.

---

## Where things live

| Thing | Path / Location |
|---|---|
| Nakama runtime modules | `snoringcat-platform/runtime/*.go` |
| Per-game config | `<game_repo>/game.yaml` |
| Game-client SDK | `snoringcat-platform/addons/platform-client/` |
| nakama-godot SDK | `<game_repo>/addons/nakama/` |
| Platform-session-manager addon | `<game_repo>/addons/platform-session-manager/` |
| Pulumi infra (Hetzner) | `snoringcat-platform/infra/pulumi/snoringcat-platform/` |
| Nakama+observability remote configs | `snoringcat-platform/infra/remote/nakama/` |
| Postgres remote configs | `snoringcat-platform/infra/remote/postgres/` |
| Pulumi state | Cloudflare R2 `hopnbop-pulumi-state-r2` (S3-compat) |
| Cost monitor source | `snoringcat-platform/infra/remote/cost-monitor/` |
| Cost monitor deployed | `/opt/snoringcat/cost-monitor/` on Nakama host |
| Cost monitor state | `/var/lib/snoringcat/cost-monitor-state.json` on Nakama host |
| Desktop dead-man source | `snoringcat-platform/infra/remote/desktop-drain-watch/` |
| Desktop dead-man deployed | `/opt/snoringcat/desktop-drain-watch/` on Nakama host |
| Service-status queue | `/var/log/snoringcat/service-status.jsonl` on Nakama host (drained daily by the desktop) |
| Postgres backups | `s3://hopnbop-pulumi-state-r2/pg-backups/` (Cloudflare R2, 7 days) |
| Crash reports | Nakama storage, collection `crash_reports`, system user (30 days); see "Crash reports" |
| Leaderboards | Nakama, `{game_id}_{id}` per `game.yaml` `leaderboards` entry; public route `GET /snoringcat/leaderboard`; see "Leaderboards" |
| Pinned SSH host keys | `claude-config/jobs/known_hosts` (synced; every scheduled job); `~/.hopnbop-migration/known_hosts` (phase-a/b). phase-a's HostKeys step keeps both in step. |
| Migration plan | `hopnbop/docs/archive/MIGRATION_PLAN.md` |
| Migration state | `~/.hopnbop-migration/state.json` |
| Migration credentials | `~/.hopnbop-migration/credentials.env` (NEVER commit) |
| Discord webhook (Claude jobs) | `~/.claude/jobs/discord-config.json` (local-only) |
| Discord webhook (cost monitor) | `/opt/snoringcat/cost-monitor/.env` `DISCORD_WEBHOOK_URL` on Nakama host |

| Service | Hostname / Endpoint |
|---|---|
| Nakama prod | `nakama.snoringcat.games` |
| Nakama staging | `nakama-staging.snoringcat.games` |
| Grafana | `grafana.snoringcat.games` |
| Edgegap dashboard | `app.edgegap.com` |
| Hetzner Cloud console | `console.hetzner.cloud` |
| Hetzner DNS | `dns.hetzner.com` |

---

## Common runbook

### "Players report can't connect"

1. Check UptimeRobot — is `nakama.snoringcat.games/healthcheck`
   green?
2. Check Grafana → "Nakama" dashboard — is the Nakama process
   up? Connection count?
3. Check Edgegap dashboard — are deployments succeeding?
4. Check game's `protocol_version` mismatch — has someone
   bumped without a release?
5. Check Discord — any active alerts?

### "Costs spiked unexpectedly"

1. The morning brief's cost section has the breakdown (sourced
   from the cost-monitor's daily service-status entry). For the
   live numbers without waiting: `journalctl -u
   cost-monitor.service -n 20` on the Nakama box.
2. If Edgegap > expected: check fleet usage in Edgegap
   dashboard. Possibly a runaway match-creation loop.
3. If Hetzner > expected: probably a forgotten test instance.
   `hcloud server list`.
4. Emergency shutdown is the cost script's hard cap (default
   $50 grand total). On crossing, `cost-monitor.sh` PATCHes
   the Edgegap app to `capacity_max=0` automatically. Manual
   reset:
   ```bash
   curl -fsS -X PATCH \
     -H "Authorization: Token $EDGEGAP_TOKEN" \
     -H "Content-Type: application/json" \
     "https://api.edgegap.com/v1/app/$EDGEGAP_APP_NAME" \
     -d '{"capacity_max": 1}'
   ```
   See the "Cost monitor" section above for the trigger
   details.

### "Need to restart Nakama"

```bash
ssh nakama@nakama-prod-1.snoringcat.games
cd /opt/nakama
docker compose restart nakama
docker compose logs -f nakama
```

Postgres keeps running; Nakama reconnects on startup. Active
matches survive (game servers are independent). Players in the
lobby may see brief disconnect, auto-reconnect.

### "The Nakama host is gone" (full rebuild)

Follow "Backups and disaster recovery" above:
`scripts/phase-a.ps1 -RestoreBackup`, then `scripts/phase-b.ps1`.
**After a rebuild, re-pin the host keys (phase-a step HostKeys)**
and push `claude-config/jobs/known_hosts`, or every scheduled prod
job keeps failing host-key verification. Re-run the step on its own
with `-StartAt HostKeys -StopAt HostKeys`.

### "Need to recover from a failed Pulumi up"

`pulumi refresh` to sync state from actuals. `pulumi up` again
to converge. State is in S3 with versioning, so corruption is
recoverable: `pulumi stack export --version <prev>` →
`pulumi stack import`.

---

## Troubleshooting reference

(Game-specific issues — WebRTC ICE, Godot WSS limitations, etc.
— stay in each game's CLAUDE.md. This doc covers
platform-cross-cutting issues only.)

### Nakama runtime module fails to load

`docker compose logs nakama | grep "module"`. Common causes:

- Go build error (check release pipeline).
- Schema mismatch in `games` table (migration not applied).
- Missing env var (Google/Facebook secret).

### Edgegap deployment stuck in "BOOTING"

Likely the game-server image fails to start. Check Edgegap
deployment logs in their dashboard. Common cause: missing
`GAMELIFT_*` env var being read by old code paths that should
have been deleted in Phase D.

### Postgres connection refused from Nakama

Check Hetzner private network is up (`hcloud network describe
snoringcat-internal`). Check `pg_hba.conf` allows the Nakama
private IP CIDR. Check Postgres container is running on the
Postgres box.

### TLS cert renewal failed

Caddy auto-renews. If it stopped working: check Caddy logs
(`docker compose logs caddy | grep -i acme`). Most common
cause: DNS misconfiguration. Verify `nakama.snoringcat.games`
A record points at the right IP. Cert state lives in
`/var/lib/caddy/`.

---

## What this doc does NOT cover

- Game-specific gameplay logic (lives in each game's CLAUDE.md).
- AWS-era architecture (lives in each game's CLAUDE.md until
  migration completes; then deleted).
- Migration phases (lives in `hopnbop/docs/archive/MIGRATION_PLAN.md`).
- Pre-flight credential setup (lives in
  `hopnbop/docs/archive/MIGRATION_PLAN.md`).
- Cost decision rationale (lives in
  `hopnbop/docs/archive/platform-pivot-discussion.md`, archived
  post-migration).
