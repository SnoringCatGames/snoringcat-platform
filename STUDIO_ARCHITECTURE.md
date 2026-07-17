# Snoring Cat Studio Architecture

> **What this is:** the engineering atlas for everything Snoring
> Cat Games builds and runs. Read this first when you're new to a
> repo, returning after a break, or trying to figure out which
> service does what.
>
> **What this is not:** a runbook for any single game's
> mechanics, or a substitute for `MIGRATION_PLAN.md` (the active
> off-AWS migration's executable plan) or `PLATFORM_ARCHITECTURE.md`
> (the runtime detail of the Nakama+Edgegap platform). This doc
> sits one level higher than both of those.
>
> **Audience:** Levi (the only human contributor today), and any
> Claude Code / agent session that needs broad context to make
> good decisions.
>
> **Last reviewed:** 2026-04-27. This doc decays. Update the
> "Last reviewed" date when you do a full pass; update individual
> sections opportunistically as things change. Reality always
> wins; if this doc disagrees with the code or a service
> dashboard, the code / dashboard is right and this doc is stale.

---

## Studio at a glance

- **Snoring Cat LLC** &mdash; sole proprietor: Levi Lindsey.
  Single-person indie studio, registered in Washington State.
- **Focus:** small multiplayer 2D games built in Godot. Pixel-art,
  platformer-leaning, mostly competitive / party formats.
- **Current head game:** Hop 'n Bop (4-player platform fighter,
  rollback netcode, in active development).
- **Earlier titles:** Squirrel Away, Inner-Tube Climber,
  Meteor Power, Momma Duck (all shipped, mostly maintenance-only),
  Dark Time (Chrome new-tab extension).
- **Open-source addons:** Surfacer, Scaffolder, Surface Tiler
  (Godot AI / pathfinding / tilemap libraries from earlier
  projects).
- **Primary dev environment:** two Windows machines (desktop +
  laptop), PowerShell as default shell with Git Bash available.
  Pacific timezone.

### Domains

| Domain | Purpose | Host (today) | Host (post-migration) |
|---|---|---|---|
| `snoringcat.games` | Studio marketing site | Heroku (levi.dev monorepo) | Cloudflare Pages |
| `www.snoringcat.games` | redirect to apex | Heroku | Cloudflare Pages |
| `snoringcatgames.com` | alias for `snoringcat.games` | Heroku | Cloudflare Pages |
| `www.snoringcatgames.com` | alias | Heroku | Cloudflare Pages |
| `hopnbop.net` | Hop 'n Bop web build + legal | AWS S3 + CloudFront | Cloudflare Pages (Phase F of migration) |
| `nakama.snoringcat.games` | Multiplayer backend | (does not exist yet) | Hetzner CPX21, Caddy + Nakama + Postgres co-tenant |
| `nakama-staging.snoringcat.games` | Staging Nakama | (does not exist yet) | Hetzner CX21 (Phase G) |
| `levi.dev`, `levilindsey.com` | Levi's personal portfolio | Heroku | (unchanged) |
| `devlog.levi.dev` | Devlog | Blogger / Google | (unchanged) |

DNS for `snoringcat.games`, `snoringcatgames.com`, and
`hopnbop.net` is managed in the unified **Hetzner Console**
(formerly the standalone `dns.hetzner.com`, going read-only
2026-05-20).

---

## Top-level architecture decisions and why

These decisions have been ratified over time. Each links to the
deeper doc that argues for it.

### Off AWS / onto Nakama + Hetzner + Edgegap

**Decision date:** 2026-04-26. **Doc:**
`hopnbop/docs/archive/platform-pivot-discussion.md`.

GameLift's idle baseline cost was 78% of the AWS bill, and the
operational complexity (warmup machinery, port-remapping,
nginx TLS detection, WebRTC GDExtension patches, Route 53
DNS pre-warming, two-port-only constraint) was enormous. We
pivoted to **Nakama on Hetzner** for the metadata layer and
**Edgegap** for game-server orchestration. Trade-offs:

- **Pros:** flat cost curve at indie scale, true scale-to-zero,
  5-15s cold starts (vs. 3-5min on GameLift), 615+ Edgegap
  regions (vs. ~30), deletes a huge fraction of operational
  tribal knowledge.
- **Cons:** new vendors to learn, marginal vendor risk on
  Edgegap (specialized middleware, two competitors died in
  2026: Hathora and Unity Multiplay).
- **Vendor risk mitigation:** the architecture is structured
  so the only vendor-dependent piece is the cheapest to
  replace. If Edgegap goes away, ~1 week of work writes a
  ~500-line Go allocator on Hetzner. Game server image,
  Nakama runtime, client SDK all unchanged.

### Nakama (self-hosted, not Heroic Cloud)

**Doc:** `PLATFORM_ARCHITECTURE.md`.

Nakama maps almost 1:1 onto what we'd otherwise build from
scratch (auth, friends, parties, matchmaker, leaderboards,
tournaments, presence, storage, real-time sockets). Apache 2.0
OSS, fork-able, battle-tested by Zynga, Paradox, Gram Games.
We self-host on a Hetzner CPX21 (~$8/mo cap, AMD shared in
Hillsboro; upsized in-place from CPX11 on 2026-06-01) rather than paying $600/mo for Heroic Cloud's entry
tier &mdash; managed Nakama is priced for studios with
employees.

Runtime extensibility constraint: **Go, Lua, or TypeScript
only**. We chose Go (matches Pulumi, performant, statically
typed). Rust would require forking Nakama; not worth the
maintenance burden.

### Cloudflare Pages for static sites

**Doc:** the `DEPLOYMENT.md` files in `snoringcat.games` and
(post-Phase-F) `hopnbop`.

Both static sites (`snoringcat.games` marketing + `hopnbop.net`
game-export) move to Cloudflare Pages. Free tier covers indie
scale. `_redirects` and `_headers` files give us declarative
control over routing and the COOP/COEP/CORP headers Godot 4
needs. Auto-deploys on git push. No cold-start.

### age-encrypted credentials in claude-config dotfiles

**Decision date:** 2026-04-27. Considered 1Password (rejected:
user wanted personal 1Password account separate from studio
work) and a plain local file (rejected: cross-machine drift +
no encryption at rest).

`age` (multi-recipient encryption with X25519 + ChaCha20-Poly1305)
is simple, free, and doesn't add a service dependency. Each
machine has its own private key; private keys never leave the
machine. Encrypted blobs live in the existing `claude-config`
private GitHub repo, syncing via the repo's PostToolUse
auto-push hook. See `MIGRATION_PLAN.md` &rarr; "Pre-flight
manual checklist" for the workflow.

### Pulumi for IaC (partial scope)

**Doc:** `MIGRATION_PLAN.md` &rarr; recommended improvements
&rarr; #7.

Used for **Hetzner Cloud (compute) + Cloudflare (DNS) + AWS
decommission** only. Not used for Edgegap (no clean Pulumi
provider) or Docker Compose / Nakama runtime config (just
files on a box). Reason: phased autonomous execution benefits
from idempotent declarative state &mdash; if a phase fails
halfway, Pulumi recovers cleanly.

State stored in Cloudflare R2 bucket `hopnbop-pulumi-state-r2`
(S3-compat backend, accessed via R2 API tokens scoped to that
bucket). Encryption passphrase (`PULUMI_CONFIG_PASSPHRASE`) is
in the age-encrypted credentials.

### GitHub for source and CI

Default. SnoringCatGames org for studio-shared repos;
levilindsey personal account for individual / cross-studio
items. CI via GitHub Actions: `pr-validate.yml` per repo +
nightly smoke + tag-driven release (Phase G of migration adds
the standardized templates).

### Discord for ops alerts

Reuses an existing channel + webhook (`hopnbop` Discord guild).
Webhook URL stored in `~/.claude/jobs/discord-config.json`
(local-only, gitignored). Helpers in
`~/.claude/jobs/Send-Discord.ps1`. Used for: cost summaries,
critical alerts (Nakama down, Postgres saturation, allocation
failures), warning alerts (CPU saturation, slow queries),
release notes, scheduler heartbeats.

### UptimeRobot for synthetic / blackbox monitoring

External-perspective monitor that complements Grafana's
internal-perspective. Checks `nakama.snoringcat.games/healthcheck`,
the website, the game server WS endpoint, etc. Free tier (50
monitors, 5-min interval). Pings Discord on failure.

### Cloudflare DNS for all studio domains

All DNS is on Cloudflare. Each domain is its own Cloudflare
zone:

- `snoringcat.games`
- `snoringcatgames.com`
- `hopnbop.net` (after Phase F migration)

Hetzner DNS is **not** used. Originally we'd planned Hetzner
DNS for studio domains, but Cloudflare Pages requires the
domain to be a Cloudflare zone before you can attach it as a
custom domain — so DNS moved to Cloudflare during pre-flight,
which also gives free DDoS protection, faster propagation, and
single-pane management with Pages.

All subdomain records (`nakama.snoringcat.games`,
`grafana.snoringcat.games`, `nakama-staging.snoringcat.games`)
are managed by Pulumi via the Cloudflare provider as part of
the `snoringcat-platform` stack. Records that just point at
Hetzner public IPs are DNS-only (gray cloud, not proxied) since
Nakama uses long-lived WebSocket / gRPC traffic that doesn't
benefit from the Cloudflare edge proxy.

(For context, the Hetzner DNS standalone console at
`dns.hetzner.com` went read-only on 2026-05-20 anyway, and was
folded into the unified `console.hetzner.com` &mdash; but this
doesn't affect us since we're not using Hetzner DNS.)

---

## Distributed systems topology (post-migration target)

```
                    Player (Godot client, native or web)
                        │
                        ├──https─────────► Cloudflare Pages
                        │                  │  - snoringcat.games (marketing)
                        │                  └─ hopnbop.net (Godot web export)
                        │
                        ├──wss────────────► Caddy (TLS termination)
                        │                   │
                        │                   ▼
                        │                   Nakama (Hetzner CPX21, Hillsboro)
                        │                   │  - REST + gRPC + realtime
                        │                   │  - Go runtime modules:
                        │                   │    - fleet_allocator.go
                        │                   │    - match_lifecycle.go
                        │                   │    - per_game_config.go
                        │                   │    - protocol_version.go
                        │                   ▼
                        │                   Postgres 16 (same Hetzner CPX21,
                        │                                docker-compose
                        │                                co-tenant)
                        │                   - Nakama schema
                        │                   - games config
                        │
                        └──UDP─────────────► Edgegap deployment (per match)
                                            - Godot Linux server export
                                            - Vanilla webrtc-native v1.0.9
                                            - Allocated by Nakama
                                              fleet_allocator on
                                              MatchmakerMatched
                                            - Reports back via
                                              register_server +
                                              match_end RPCs
```

Operations stack (same Hetzner box as Nakama):

```
Prometheus  ─►  Grafana  ─►  Discord webhook (alerts)
   ▲              ▲
   │              │
postgres_exporter, node_exporter, Caddy, Nakama metrics, Loki
                          ▲
                  UptimeRobot blackbox checks
                          │
              External: hits public endpoints
```

Auxiliary one-shot infrastructure:

- **AWS** (during decommission window): SSO via `aws sso login
  --profile hopnbop`, Pulumi adopt-and-destroy stack
  `aws-decommission`, then gone.
- **CloudFront + S3** for `hopnbop-website`: gone after Phase F.
- **Cloudflare** is fully integrated into the architecture (not
  optional / not auxiliary): all DNS zones, all static-site
  hosting via Pages.

---

## Repo map

All paths are absolute on the dev machine
(`C:\Users\lsl\Repositories\...`).

### Active games

#### `SnoringCatGames/hopnbop` (private)

The flagship game. Godot 4.7 multiplayer 2D platform fighter
with rollback netcode.

- **Path:** `Repositories/hopnbop/`
- **Language:** GDScript primarily; some Python for the AWS
  backend (being deleted in Phase F); Bash + PowerShell for
  deploy scripts.
- **Submodules:**
  - `addons/rollback_netcode/` &rarr;
    `SnoringCatGames/godot-rollback-netcode`
  - `addons/gamelift_session_manager/` &rarr;
    `SnoringCatGames/godot-gamelift-session-manager` (will be
    renamed `godot-platform-session-manager` in Phase D)
  - `gamelift-gdextension/vcpkg/` &rarr; upstream Microsoft vcpkg
  - `third_party/snoringcat-platform/` &rarr;
    `SnoringCatGames/snoringcat-platform`
- **Deploy targets:**
  - Backend: AWS SAM (will be deleted Phase F) &rarr; Nakama
    runtime (Phase A).
  - Game server: AWS GameLift container fleet (Phase F gone)
    &rarr; Edgegap (Phase C).
  - Web build: AWS S3 + CloudFront (Phase F gone) &rarr;
    Cloudflare Pages.
  - Native exports: Windows / macOS / Linux + Steam (TBD
    distribution).
- **Key docs:** `CLAUDE.md`, `MULTI_GAME_ROADMAP.md`, plus
  `third_party/snoringcat-platform/PLATFORM_ARCHITECTURE.md`
  (loaded on demand). Historical / archived in
  `hopnbop/docs/archive/`: `MIGRATION_PLAN.md`,
  `platform-pivot-discussion.md`, `BUILD.md`,
  `DISTRIBUTED_SYSTEMS_PLAN.md`,
  `FRIENDS_PARTY_MATCHMAKING_AUDIT.md`, and
  `test-architecture-plan.md`.

#### `SnoringCatGames/ggj26` (private)

Game jam project. Lighter weight, intended to consume the
shared platform once it's ready.

- **Path:** `Repositories/ggj26/`
- **Language:** GDScript.
- **Status:** dormant between jams.

### Shared studio infra

#### `SnoringCatGames/snoringcat-platform` (private)

The shared multiplayer-platform package: Nakama runtime modules
+ Godot client SDK + compliance tests. Consumed by each game
as a submodule under `third_party/`.

- **Path:** `Repositories/hopnbop/third_party/snoringcat-platform`
  (no standalone checkout outside the game's submodule today).
- **Language:** Go (Nakama runtime modules), GDScript (client
  SDK and addon).
- **Deploy targets:**
  - Runtime modules: built and dropped into the Nakama box's
    Docker Compose volume by `nakama-runtime.yml`, which fires
    on pushes to `main` touching `runtime/**` (or manually).
    This repo has no tags and cuts no releases.
  - Godot addon: ships inside each game's submodule.
- **Key docs:** `CLAUDE.md`, `PLATFORM_ARCHITECTURE.md`, this
  file (`STUDIO_ARCHITECTURE.md`), `README.md`.

#### `SnoringCatGames/snoringcat.games` (private)

The studio marketing site at `snoringcat.games`. Static site
on Cloudflare Pages.

- **Path:** `Repositories/snoringcat.games/`
- **Language:** HTML, vanilla JS, CSS.
- **Deploy:** Cloudflare Pages auto-deploys from `main` on push.
- **Key docs:** `README.md`, `DEPLOYMENT.md`. Routing rules in
  `public/_redirects`. Headers in `public/_headers`.

### Open-source Godot addons

These are Levi's older work, mostly maintenance mode. Mentioned
on the studio site, GitHub-redirect URLs in `_redirects`.

#### `SnoringCatGames/godot-rollback-netcode` (public)

Rollback netcode for Godot. Actively used by Hop 'n Bop.
Submoduled into game projects.

#### `SnoringCatGames/godot-gamelift-session-manager` (public)

Will be renamed `godot-platform-session-manager` in Phase D
of the migration. Becomes a multi-provider addon
(LocalOnly, Preview, Edgegap-via-Nakama, eventual
Hetzner-via-Nakama).

#### `SnoringCatGames/scaffolder` (public)

Godot framework: UI scaling, navigation, audio helpers, perf
tracking. Used by older games. Hop 'n Bop has graduated past it.

#### `SnoringCatGames/surfacer` (public)

Godot 2D platformer pathfinding library. Used by Squirrel Away.

#### `SnoringCatGames/surface_tiler` (public)

Advanced autotiling for Godot. Companion to Surfacer.

### Levi's personal repos that interact with the studio

#### `levilindsey/levi.dev` (public)

Personal portfolio site at `levi.dev` and `levilindsey.com`.
**Migrated 2026-04-30** from an Express.js monorepo on Heroku
to **Cloudflare Pages + Pages Functions** (`build.js`,
`functions/[[catchall]].js`, with a R2-backed Pages Function
for the `kittenbaticorn` heavy wasm). Auto-deploys via GitHub
Actions on push to `main`.

- **Path:** `Repositories/levi.dev/`
- **Touch points with studio:** none currently. The
  `apps/snoring-cat/` directory was extracted to the
  `snoringcat.games` repo on 2026-04-27, and the path-based
  routing for `snoringcat.games` was removed when that domain
  cut over to Cloudflare Pages.

#### `levilindsey/claude-config` (private)

Levi's Claude Code dotfiles + scheduled-job scripts. Symlinks
into `~/.claude/`. Auto-pushes via PostToolUse hook.

- **Path:** `Repositories/claude-config/`
- **Includes:** `CLAUDE.md` (user-global instructions), `jobs/`
  (scheduled scripts), `rules/` (rule docs), `settings.json`.
- **Studio touch points:**
  - `secrets/hopnbop-migration.recipients` (age public keys)
  - `secrets/hopnbop-migration.env.age` (encrypted creds, Phase
    A onward)
  - `secrets/hopnbop-migration-{nakama,postgres}-ssh.age`
    (encrypted SSH keys)
  - `jobs/Send-Discord.ps1` and `jobs/discord-config.json`
    (used by both studio ops and personal jobs)
- **Sync mechanism:** PostToolUse auto-push on Claude Code
  changes; manual `git push` for human-edited changes.

---

## Service inventory

For each service: where the dashboard lives, how to access,
where credentials live, and how to rotate. Rotation procedure
detail is in `hopnbop/docs/archive/MIGRATION_PLAN.md` &rarr; "Key and
credential rotation."

### Hetzner Cloud (compute only — DNS is on Cloudflare)

- **Dashboard:** https://console.hetzner.com
- **Account:** `admin@snoringcat.games`
- **Project:** `snoringcat-platform`
- **What's hosted:** Single Nakama box (CPX21, upsized from CPX11 on 2026-06-01) running Nakama
  + Postgres + Caddy in one docker-compose stack; staging box
  (CX21, after Phase G), private network, firewall.
- **API token:** `HCLOUD_TOKEN` &mdash; in age-encrypted
  credentials. Permissions: Read &amp; Write at project level.
- **Cost:** ~$8/mo (CPX21 capped + minor bandwidth + cents
  for R2 backups). Staging box (CX21, after Phase G) would
  add ~$7-10/mo. The 2026-05-06 consolidation collapsed the
  original 2x CPX11 (Nakama + separate Postgres + full obs
  stack) into a single CPX11 with the obs stack stripped to
  fit on 2 GB RAM. Stage 7.11 (2026-05-13) re-introduced a
  lightweight obs subset (Prometheus + Grafana +
  node-exporter + postgres-exporter; Loki + Promtail still
  off) onto the same single-host stack &mdash; the box runs
  with ~1.3 GB RAM headroom even with obs back on. ARM (CAX)
  is EU-only; NA latency is what forces us into the CPX tier
  in Hillsboro.
- **Where to look first:** Cloud "Servers" tab.

### Cloudflare (DNS + Pages)

- **Dashboard:** https://dash.cloudflare.com
- **Account:** Levi's personal Cloudflare account (`admin@snoringcat.games`)
- **Zones:** `snoringcat.games`, `snoringcatgames.com`,
  `hopnbop.net` (post-Phase-F). Each is a separate zone.
- **Pages projects:**
  - `snoringcat-games` &mdash; deploys from
    `SnoringCatGames/snoringcat.games` repo. Custom domains:
    `snoringcat.games`, `www.snoringcat.games`,
    `snoringcatgames.com`, `www.snoringcatgames.com`.
  - `hopnbop-website` &mdash; (post-Phase-F) deploys from
    `SnoringCatGames/hopnbop`'s `web/` build. Custom
    domains: `hopnbop.net`, `www.hopnbop.net`.
- **API token:** `CLOUDFLARE_API_TOKEN` &mdash; in
  age-encrypted credentials. Scopes: Account &rarr; Cloudflare
  Pages: Edit; Account &rarr; Account Settings: Read; Zone
  &rarr; DNS: Edit (all zones).
- **Account ID:** `CLOUDFLARE_ACCOUNT_ID` &mdash; in
  age-encrypted credentials.
- **Cost:** $0 on free tier (unlimited bandwidth on Pages, no
  proxy charges).
- **Where to look first:** Zones for DNS; Workers &amp; Pages
  for static site projects; Logs / Analytics per zone for
  traffic patterns.

### Edgegap

- **Dashboard:** https://app.edgegap.com
- **Account:** `admin@snoringcat.games`
- **Org:** Snoring Cat LLC (slug TBD &mdash; see Phase C of
  migration to resolve via API)
- **What's hosted:** game-server containers (currently:
  Hop 'n Bop). Pushed via Edgegap Docker registry.
- **API token:** `EDGEGAP_TOKEN` &mdash; in age-encrypted
  credentials.
- **Cost:** $0.138/active-vCPU-hour. Scale-to-zero, so ~$0
  when no matches running.
- **Where to look first:** "Apps" tab for hopnbop-server;
  "Deployments" tab for active game servers.

### GitHub

- **Org:** SnoringCatGames (https://github.com/SnoringCatGames)
- **Personal account:** levilindsey
  (https://github.com/levilindsey)
- **Token:** `GITHUB_TOKEN` (a personal access token with
  `repo`, `workflow`, `admin:org`) in age-encrypted credentials.
- **CI:** GitHub Actions free tier. Workflows live in each
  repo's `.github/workflows/`.
- **Cost:** $0 on free tier (2000 Actions minutes/mo for
  private repos; public is unlimited).

### Discord

- **Server:** Snoring Cat Games guild (also linked from games
  via `https://discord.gg/QX939SF7nb`).
- **Webhook:** in `~/.claude/jobs/discord-config.json` (gitignored).
  Used by alerts, scheduler, ops scripts.
- **Webhook URL also in age-encrypted credentials** as
  `DISCORD_WEBHOOK_URL` so phase scripts can post.
- **Cost:** $0.

### UptimeRobot

- **Dashboard:** https://uptimerobot.com
- **Account:** `admin@snoringcat.games`
- **API key:** `UPTIMEROBOT_API_KEY` in age-encrypted
  credentials.
- **Monitors (post-migration):** Nakama healthcheck, website,
  game-server WS endpoint.
- **Cost:** $0 on free tier (50 monitors, 5-min interval).

### Google Cloud Console (OAuth + Analytics)

- **Console:** https://console.cloud.google.com
- **Project:** "Snoring Cat Games" (or similar)
- **Used for:**
  - Google OAuth client (web app for Nakama auth):
    `GOOGLE_OAUTH_CLIENT_ID`, `GOOGLE_OAUTH_CLIENT_SECRET` in
    age-encrypted credentials.
  - Google Analytics 4 (game telemetry, website analytics):
    GA4 property ID `G-TSV2TNLHJ9` (snoringcat.games site).
- **Cost:** $0 (free tier).

### Meta for Developers (Facebook OAuth)

- **Dashboard:** https://developers.facebook.com
- **App:** "Snoring Cat Games" (Consumer app type, Web Login
  product).
- **Credentials:** `FACEBOOK_APP_ID`, `FACEBOOK_APP_SECRET` in
  age-encrypted credentials.
- **App Review:** required for production access; takes 1-7
  days. Submit during Phase A or Phase B; doesn't block other
  work.
- **Cost:** $0.

### SendGrid (legacy, may sunset)

- **Dashboard:** https://app.sendgrid.com
- **Used by:** `levi.dev` Heroku deploy (gesture-log emails,
  data-deletion notification emails). After Phase F, neither
  feature exists; SendGrid can be cancelled if no other
  consumer.

### AWS (decommissioned)

- **Account:** 270469481989 (open but empty).
- **Status:** zero resources as of 2026-05-04. Phase F
  (2026-05-03) deleted the GameLift / Lambda / S3 / CloudFront /
  Route 53 surface for Hop'n'Bop; the orphan
  `snoringcat-platform-backend` SAM stack and
  `hopnbop-pulumi-state` S3 bucket were cleaned up the next day
  alongside the Pulumi-state migration to R2.
- **Whether to fully close the account:** Levi decides after
  the 14-day soak window. Closing forfeits the account number;
  not closing keeps the door open for any future AWS-only
  service we might want.

### Apple Developer / Steamworks / Epic Online Services

Not yet active. Required if/when shipping to those platforms.
Levi will create accounts at that time. Apple Dev Program
$99/yr, Steam Direct $100/game, Epic free.

### Domain registrar

- **`snoringcat.games`, `snoringcatgames.com`:** registered at
  **Squarespace** (formerly Google Domains). Nameservers
  delegated to Cloudflare. Manage at
  https://account.squarespace.com.
- **`hopnbop.net`:** TBD registrar; nameservers will be
  delegated to Cloudflare in Phase F (currently delegated to
  AWS Route 53).
- **`levi.dev`, `levilindsey.com`:** Levi's personal domains;
  not part of the studio infrastructure.

---

## Common procedures

### Set up a new dev machine

1. Clone all relevant repos under `~/Repositories/`.
2. `git submodule update --init --recursive` in each game repo.
3. Install: Godot 4.7, Docker Desktop, Node.js, Go, Pulumi CLI,
   `wrangler` (Cloudflare CLI), `gh` CLI, `hcloud` CLI,
   `edgegap` CLI, `age`, AWS CLI v2 with SSO config.
4. Set up the age private key per `MIGRATION_PLAN.md` &rarr;
   "Pre-flight" &rarr; section 0.
5. Append the new machine's age public key to
   `claude-config/secrets/hopnbop-migration.recipients` from
   any existing machine; commit + push.
6. From any existing machine, re-encrypt
   `hopnbop-migration.env.age` and the SSH `.age` files using
   the updated recipients list. Commit + push.
7. On the new machine: pull `claude-config`, decrypt to
   `~/.hopnbop-migration/` per `MIGRATION_PLAN.md` step 12.
8. Create the `~/.claude/secrets` junction to
   `~/Repositories/claude-config/secrets`:
   `cmd /c mklink /J $HOME\.claude\secrets $HOME\Repositories\claude-config\secrets`.
9. `aws sso login --profile hopnbop` to refresh AWS creds.

### Rotate a credential

See `MIGRATION_PLAN.md` &rarr; "Key and credential rotation"
for the full matrix. Quick reference:

- **Routine rotation** (annual): rotate Hetzner / Edgegap /
  GitHub tokens. ~30 min total.
- **Lost machine / suspected compromise:** rotate everything.
  ~2 hours. Old `.age` blobs in `claude-config` git history
  remain decryptable by the lost key, so credential rotation is
  what actually protects you.

### Deploy a backend (Nakama runtime) change

1. Edit Go code in `snoringcat-platform/backend/runtime/`.
2. Open PR. `pr-validate.yml` runs `go test`, `go vet`,
   `staticcheck`, plus the compliance suite against an
   ephemeral Nakama+Postgres in Docker Compose.
3. Merge. Tag `v*.*.*`.
4. `release.yml` builds the new runtime image, SSHes to the
   Nakama box, runs `docker compose up -d` with the new image.
   Nakama hot-swaps the runtime module &mdash; zero downtime.

### Deploy a game-server change

1. Bump `project.godot::config/version`. Bump
   `protocol_version` if the network protocol changes (per-game,
   only the affected game's version &mdash; see
   `PLATFORM_ARCHITECTURE.md` &rarr; "Per-game protocol
   versioning").
2. Bump matching `display_version` (and `protocol_version`) in
   the game's `game.yaml`.
3. Tag the game's repo.
4. `release.yml` exports the Linux server `.pck`, builds the
   Docker image, pushes to Edgegap registry, bumps the Edgegap
   fleet version. Old game-server containers finish their
   matches; new matches use the new version.

### Deploy a game-client change

1. Tag the game's repo.
2. `release.yml` exports for all platforms, deploys web build
   via `wrangler pages deploy web/` to the corresponding
   Cloudflare Pages project, uploads native exports as GitHub
   release artifacts (or to Steam if applicable).

### Deploy a website change (snoringcat.games or hopnbop.net)

1. Edit and push to `main`.
2. Cloudflare Pages auto-deploys. ~30s.

### Add a new game to the platform

1. Create a new game repo under `SnoringCatGames`.
2. Submodule `snoringcat-platform` under `third_party/`.
3. Author `game.yaml` per the schema in
   `PLATFORM_ARCHITECTURE.md`.
4. Reference `PLATFORM_ARCHITECTURE.md` from the new game's
   `CLAUDE.md` (single line: "When working on backend or
   matchmaking, read this.").
5. Add a card to `snoringcat.games/public/index.html`. Add
   `_redirects` entries for the new game's paths.
6. Create an Edgegap app for the new game.
7. Restart Nakama; new game appears in the `games` table.
   Players can connect.
8. (Optional) Promote infra to Pulumi modules if 2+ games &amp;
   the duplication is real.

### Respond to a Discord alert

1. Read the alert. Severity (Critical/Warning/Info) is in the
   message. Sources: cost-monitor (cost / Edgegap-active
   thresholds), prod-health-check Claude job (containers,
   logs, vitals, backup), UptimeRobot (external liveness).
2. Pull recent logs ad-hoc: `ssh root@nakama.snoringcat.games
   "docker logs nakama --since 1h | tail -100"` (Loki/Grafana
   were dropped in the 2026-05-06 consolidation; logs live in
   the host's docker daemon now).
3. Cross-reference UptimeRobot (external view) and Cloudflare
   analytics (CDN view).
4. Common causes: Nakama OOM &rarr; restart container; Postgres
   slow query &rarr; check pg_stat_activity; Edgegap allocation
   failures &rarr; check their dashboard for capacity issues.
5. If unresolved in 30 min, manually scale Edgegap to 0 to
   stop accepting new matches while you debug.

### Audit costs

1. Daily Discord summary from the cost-monitor systemd timer
   on the Nakama box. Should arrive ~09:00 UTC.
2. Monthly: log in to each provider's dashboard and verify
   month-to-date spend. Hetzner, Edgegap, AWS (until closed),
   Cloudflare (free), GitHub (free).
3. Hard cap: cost script auto-shuts down Edgegap fleet if
   grand total &gt;$50/mo. Adjust threshold by editing
   `EMERGENCY_CAP` in `/opt/snoringcat/cost-monitor/`.

### Recover from an outage

- **Hetzner regional outage** (rare but happens): website still
  served by Cloudflare. Backend and games are down. No active-
  active redundancy at indie scale. Wait for Hetzner to recover.
- **Cloudflare Pages outage:** websites down; backend and games
  unaffected (they don't depend on Cloudflare). Status page:
  https://www.cloudflarestatus.com/.
- **Edgegap outage:** new matches can't be allocated;
  in-flight matches keep running. If extended, switch
  `Netcode.is_local_mode = true` in client and let players do
  local play / single-player.
- **Discord webhook down:** alerts silently fail. Email
  fallback to `admin@snoringcat.games` is configured in
  Grafana. Beyond that, UptimeRobot has its own email
  notifications.
- **Postgres data corruption:** restore from
  `Hetzner Storage Box backup` (taken nightly via cron). RPO
  is ~24h; no PITR setup at indie scale.

---

## Quick links

| What | Where |
|---|---|
| Active migration plan | `hopnbop/docs/archive/MIGRATION_PLAN.md` |
| Architecture decision context | `hopnbop/docs/archive/platform-pivot-discussion.md` |
| Runtime platform reference | `snoringcat-platform/PLATFORM_ARCHITECTURE.md` |
| **This studio overview** | `snoringcat-platform/STUDIO_ARCHITECTURE.md` |
| Levi's user CLAUDE.md | `claude-config/CLAUDE.md` |
| Workspace CLAUDE.md | `Repositories/CLAUDE.md` |
| Per-game CLAUDE.md | `<game>/CLAUDE.md` |
| Hetzner Console | https://console.hetzner.com |
| Edgegap Dashboard | https://app.edgegap.com |
| Cloudflare Dashboard | https://dash.cloudflare.com |
| GitHub org | https://github.com/SnoringCatGames |
| Discord guild | https://discord.gg/QX939SF7nb |
| UptimeRobot | https://uptimerobot.com |
| Google Cloud Console | https://console.cloud.google.com |
| Meta for Developers | https://developers.facebook.com |
| AWS console | https://signin.aws.amazon.com/ |

---

## See also

- `hopnbop/docs/archive/MIGRATION_PLAN.md` &mdash; the executable
  plan for the off-AWS migration.
- `hopnbop/docs/archive/platform-pivot-discussion.md` &mdash; the
  decision-doc that motivates the migration.
- `snoringcat-platform/PLATFORM_ARCHITECTURE.md` &mdash; the
  runtime detail of the Nakama+Edgegap platform.
- `snoringcat-platform/CLAUDE.md` &mdash; per-repo
  Claude-specific guidance.

This doc is **breadth-first**; the others are depth-first on
specific subjects. Start here if the question is "what services
do we use and how do they fit together"; go to the specific
docs if the question is "how do I implement / deploy / debug
specific thing X".
