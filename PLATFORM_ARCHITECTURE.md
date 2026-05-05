# Platform architecture reference

> **Load this doc when working on backend, multiplayer infra,
> auth, matchmaking, game-server allocation, observability, or
> per-game config in any Snoring Cat Games title.**
>
> This is the **live production architecture** (Nakama +
> Hetzner + Edgegap), as of Phase F completion (2026-05-03).
> The previous AWS GameLift design is preserved only as
> archeology in `hopnbop_private/docs/archive/platform-pivot-discussion.md`
> and `hopnbop_private/MIGRATION_PLAN.md`.

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
  `hopnbop_private/MIGRATION_PLAN.md` and the migration state
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
│  grafana.snoringcat.games:443 → Grafana                      │
└────────────────┬─────────────────────────────────────────────┘
                 │
┌────────────────▼─────────────────────────────────────────────┐
│  Nakama (Hetzner CPX11, Hillsboro)                           │
│  - REST/gRPC/realtime endpoints                              │
│  - Go runtime modules:                                       │
│    - fleet_allocator.go (Edgegap allocator hook)             │
│    - match_lifecycle.go (server registration, match end)     │
│    - per_game_config.go (loads game.yaml at startup)         │
│    - protocol_version.go (per-game version checker)          │
│  - Built-in: auth, friends, parties, matchmaker, leaderboards│
│  - /metrics endpoint scraped by Prometheus                   │
└────────────────┬─────────────────────────────────────────────┘
                 │ private network only
┌────────────────▼─────────────────────────────────────────────┐
│  Postgres 16 (Hetzner CPX11, Hillsboro)                      │
│  - Nakama schema (users, friends, leaderboards, etc.)        │
│  - games config table (per-game settings + protocol_version) │
│  - server_pool tables (DIY Hetzner allocator, future)        │
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

Operations:
┌──────────────────────────────────────────────────────────────┐
│  Prometheus + Grafana + Loki (on Nakama box)                 │
│  Scrapes: Nakama, Postgres, node_exporter (both boxes),      │
│  Caddy. Logs: Nakama, game servers (via stdout → Loki).      │
│  Alerts → Discord webhook.                                   │
│  External: UptimeRobot synthetic checks.                     │
│  Cost monitor: systemd timer polling Hetzner + Edgegap APIs. │
└──────────────────────────────────────────────────────────────┘
```

**Hostnames:**
- `nakama.snoringcat.games` — Nakama HTTP + realtime WS.
- `grafana.snoringcat.games` — Grafana UI (basic auth).
- `nakama-staging.snoringcat.games` — staging Nakama (CI).
- Game-server hostnames are Edgegap-assigned (per-deployment IPs).

**Ports:**
- 443 (Caddy → Nakama 7350 / Grafana).
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

Required for GDPR / CCPA / app-store TOS. Implementation:

1. Player clicks "Delete my account" in settings (double-confirm
   dialog).
2. Client calls `delete_account` Nakama RPC.
3. RPC flow:
   - Mark `accounts.deletion_scheduled_at = now() + 30 days`.
   - Anonymize: replace display name with `[deleted]`, scrub
     email, etc.
   - Schedule a Nakama cron job for hard-delete after 30 days.
4. During grace period, player can sign back in to cancel.
5. After grace: hard-delete from `accounts`, cascade to
   `friends`, anonymize match-history rows (keep aggregate
   leaderboard scores anonymized).
6. Backups are purged in the next post-grace backup cycle.

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

---

## Per-game configuration

Each game declares itself via `game.yaml` shipped in its
project. Backend loads it at Nakama startup.

```yaml
# Example: hopnbop_private/game.yaml
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
  cross_play: true
  rules:
    - is_web_compatible: true
leaderboards:
  - id: hopnbop_kills_alltime
    sort: desc
    operator: best
    reset_schedule: null
  - id: hopnbop_kills_weekly
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

**Storage:** at backend startup, `per_game_config.go` reads each
known game's `game.yaml` (paths configured via env var) and
upserts a row into the Postgres `games` table:

```sql
CREATE TABLE games (
  game_id TEXT PRIMARY KEY,
  display_name TEXT NOT NULL,
  edgegap_app_slug TEXT NOT NULL,
  protocol_version INTEGER NOT NULL,
  display_version TEXT NOT NULL,
  config JSONB NOT NULL,         -- full game.yaml content
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Runtime modules read from this table (cached in process memory,
invalidated on update RPC).

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
2. Update `nakama.snoringcat.games` Nakama config (env vars).
3. Restart Nakama.
4. Old secret remains valid for the provider's grace period
   (~7 days for Google).

### Cost monitor

Hourly systemd timer on the Nakama host that polls Hetzner
Cloud, Edgegap, Cloudflare R2, Cloudflare Pages, and GitHub
Actions APIs for month-to-date usage; pings Discord on
threshold crossings and posts a daily summary.

**Source files (this repo):**
`infra/remote/cost-monitor/{cost-monitor.sh, cost-monitor.service,
cost-monitor.timer}`. Deployed by Pulumi to
`/opt/snoringcat/cost-monitor/` on the Nakama box.

**Trigger:** `cost-monitor.timer` —
`OnCalendar=*-*-* *:00:00 UTC`, `RandomizedDelaySec=60`.

**Behaviour:**
- Silent on most runs.
- One routine "Billing status" Discord summary per day, at
  `DAILY_SUMMARY_HOUR_UTC` (default `9` = 09:00 UTC).
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

**Config:** `/opt/snoringcat/cost-monitor/.env` on the host —
provider tokens (`HCLOUD_TOKEN`, `EDGEGAP_TOKEN`,
`CLOUDFLARE_API_TOKEN`, `GITHUB_TOKEN`), threshold overrides,
and the `DISCORD_WEBHOOK_URL` (separate copy from
`~/.claude/jobs/discord-config.json`).

**Inspect:**
```bash
ssh nakama@nakama.snoringcat.games
systemctl status cost-monitor.timer
journalctl -u cost-monitor.service -n 50
cat /var/lib/snoringcat/cost-monitor-state.json
```

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
| Migration plan | `hopnbop_private/MIGRATION_PLAN.md` |
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

1. Daily cost script's last Discord message has the breakdown.
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
- Migration phases (lives in `hopnbop_private/MIGRATION_PLAN.md`).
- Pre-flight credential setup (lives in `MIGRATION_PLAN.md`).
- Cost decision rationale (lives in
  `hopnbop_private/docs/archive/platform-pivot-discussion.md`, archived
  post-migration).
