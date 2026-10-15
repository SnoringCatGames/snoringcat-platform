# snoringcat-platform ledger

Open work for snoringcat-platform: the client SDK, the Nakama runtime,
and the prod-host scripts (phase-a/phase-b, infra/remote). It covers
follow-ups, known residuals, decisions waiting on Levi, and ideas.
Convention: claude-config `rules/ledger.md`. IDs (`SP<n>`) are stable and
never reused. Game-side items live in hopnbop's `LEDGER.md`.

Session pointers like "session 530145bc, agent a94d4f4d" refer to Claude
Code transcripts under `~/.claude/projects/` (kept 30 days). Each item
states its facts, so it stands on its own after the transcript is gone.

## Urgent

- **SP27** · Session ids can be computed (G1.2): random per-slot tokens
  and a token-to-user-id map; lands before H41 (M; batch 2; hopnbop
  `docs/design/H37_systems_audit.md` 3.3, 0.1 row 4)

## Waiting on Levi

- **SP12** · Facebook sign-in is probably broken for everyone: the SDK
  passes Facebook's OAuth code to Nakama where Nakama expects an access
  token. The button is live in the account and upgrade panels. Offered:
  verify, then fix with a server-side code exchange like Google's
  (the hopnbop.net token function). Read from code, not tested live.
  (Session 530145bc, tooling agent a42eb164 and data-rights agent
  aceea38c, 2026-10-12.) H37 (10-14): Broken by code (acct SI-2); both
  panels offer Facebook and the privacy text names it. Recommended: hide
  Facebook until it works (report section 7 row 11).
- **SP13** · Custom-ID, Game Center and Facebook Instant sign-ins skip
  the `game_id` auth hook, so anyone with the public server key can
  create accounts that way. Offered: close them. (Agent aceea38c.) H37
  (10-14): still true for Custom, Game Center and FB Instant; Email is
  already refused. Recommended: refuse the three until they are needed
  (report section 7 row 11).
- **SP14** · Match notifications never expire: every match leaves a
  persistent `match_ready` or `match_failed` notification in each
  player's inbox, and nothing deletes them. Offered: expire them.
  (Agent aceea38c.) H37 (10-14): still true (report ops RT2).
- **SP15** · `GOOGLE_OAUTH_CLIENT_SECRET` and `FACEBOOK_APP_SECRET` are
  passed into Nakama's environment (`infra/remote/nakama/docker-compose.yml`,
  `scripts/lib/nakama-env.ps1`, `config.yml`) but nothing reads them;
  Google's secret is only used by the hopnbop.net token function.
  Offered: remove them from the host. (Agent a42eb164.) H37 (10-14):
  still true.
- **SP16** · `delete_account` doesn't refresh the public leaderboard
  cache, so a deleted player can stay listed for up to 60 s (about 2 min
  in a browser). Offered: one call in the deletion flow. (Leaderboard
  agent abbaef7c, 2026-10-12.) H37 (10-14): still true (report ops L3).
- **SP17** · Google sign-in has never run end to end with a real
  account. Needs Levi at a browser for two minutes:
  `python scripts/test-google-auth.py` (fixed in 6ffa5fd; it now fails if
  the token lacks a claim Nakama needs, and reports whether an email
  came back). (Agents a42eb164 and aceea38c.)
- **SP34** · The creator cannot leave while others remain (TODO L127)
  (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.2, section 7
  row 5)
- **SP69** · One account can queue from two devices (TD-1, TODO L112)
  (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.1 list,
  section 7 row 7)

## In progress

## Next

- **SP32** · A party invite is a forced join. Fix
  `sdk:test/compliance/test_party_invite_flow.gd:136-140` with it, since
  it accepts the join. Urgent candidate (report section 7 row 1) (M to
  L; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2, 0.1 row 2)
- **SP33** · Any member can pull everyone into a match load (M; batch 2;
  hopnbop `docs/design/H37_systems_audit.md` 3.2)
- **SP41** · A failed `delete_account` is reported as a failed sign-in
  (DEL-2); root cause `sdk:core/auth_api_client.gd:890,896` (S; batch 2;
  hopnbop `docs/design/H37_systems_audit.md` 3.1)
- **SP43** · A refresh during a pending link is taken as the link's
  success (LK-1) (S; batch 2; hopnbop `docs/design/H37_systems_audit.md`
  3.1)
- **SP35** · 50 pending outgoing friend requests block every accept (S;
  batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2)
- **SP36** · Blocking stops friend requests and nothing else (S; batch
  2; hopnbop `docs/design/H37_systems_audit.md` 3.2)
- **SP44** · The guest device id is not crypto-random (SI-1); the token
  store passphrase is weak (TS-1). acct: SI-1 Suspect, low-medium; TS-1
  Believed, low. Fix: a random 128-bit device id; a real key per
  platform (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1
  list)
- **SP47** · Sessions are not revoked on account delete (DEL-3). acct
  (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1 list)
- **SP30** · No match forms for anyone while an allocation runs (M;
  batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.3)
- **SP29** · One player's 120 s timeout cancels the whole matched set's
  allocation (S; batch 3; hopnbop `docs/design/H37_systems_audit.md`
  3.3)
- **SP28** · A Cancel during `start_matchmaking` leaves a ghost ticket
  (S; batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.3)
- **SP71** · `match_cancel` without a registration is a no-op; test
  matches leave rows; two wrong test comments (XS to S; batch 3; hopnbop
  `docs/design/H37_systems_audit.md` 3.3)
- **SP58** · match_end is not atomic and nothing monitors it (S1, OP2);
  the stale-row sweep (RT1) (M; batch 3; hopnbop
  `docs/design/H37_systems_audit.md` 3.4)
- **SP40** · The notification poll is frozen on the oldest 50 rows
  (server half) (M; batch 4; hopnbop `docs/design/H37_systems_audit.md`
  3.2)
- **SP42** · Server-side validation of names and avatar URLs (PI-1,
  DN-2) (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.1)
- **SP37** · Party chat has no server-side length, rate or block limits;
  stale comments and dead code. soc F9. Fix: limits in the chat hook;
  delete the dead routes (S; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.2 list)
- **SP45** · Every hourly refresh emits the same `auth_completed(true,
  "")` as a sign-in, so subscribers redo sign-in work (SL-1). acct:
  Believed, low-medium. `_handle_auth_success` (1282) emits it;
  `global.gd:388-392` then refetches and merges cloud settings, and four
  more listeners react. Fix: a separate `session_refreshed` signal that
  sign-in listeners ignore (S; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.1 list)
- **SP46** · An expected export failure files a crash report (DX-1).
  acct; exec-b's offline export run (XS; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.1 list)
- **SP65** · Grafana rules for 5xx, 429 and Nakama RPC errors (OP6) (S;
  batch 5; hopnbop `docs/design/H37_systems_audit.md` 3.5)
- **SP56** · Close the DR drill's gaps. The drill was a tabletop
  exercise, not an outage (M; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.5, 2.2)
- **SP61** · The 500 per day global crash cap starves servers (CR4) (S;
  batch 5; hopnbop `docs/design/H37_systems_audit.md` 3.4)
- **SP64** · Nothing validates or reloads the Caddyfile on deploy; the
  Caddy build is unpinned (RL5, RL6) (S; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.5)
- **SP59** · Any session can write unbounded storage (RL3) (M; batch 5;
  hopnbop `docs/design/H37_systems_audit.md` 3.5)
- **SP60** · The public leaderboard read scans 5 pages, and guests are
  recorded on the board (L2) (S to M; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.4)
- **SP66** · Read the Postgres errors that prod-health-check counts
  (OP4) (S; batch 5; hopnbop `docs/design/H37_systems_audit.md` 3.5)
- **SP67** · Pruning for the three unbounded stores (RT3) (S; batch 5;
  hopnbop `docs/design/H37_systems_audit.md` 3.5)
- **SP68** · Paginate `get_match_history` (OP7) (S; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.5)
- **SP55** · A dev stack with the local allocator (build item 5b) (M;
  batch 6; hopnbop `docs/design/H37_systems_audit.md` section 4 step 3,
  2.2)
- **SP38** · A Go fake-`nk` harness for runtime hooks (S to M; batch 6;
  hopnbop `docs/design/H37_systems_audit.md` section 4 step 5, 1.3 row
  1)
- **SP39** · Two- and three-user compliance cases (S to M; batch 6;
  hopnbop `docs/design/H37_systems_audit.md` section 4 step 5, 1.3 row
  5)
- **SP63** · A test for account_cron (RT4) (S; batch 6; hopnbop
  `docs/design/H37_systems_audit.md` 3.5)
- **SP50** · Client SDK CI runs no tests (S; batch 6; hopnbop
  `docs/design/H37_systems_audit.md` 3.5)
- **SP51** · Pin staticcheck in `nakama-runtime.yml:44` (XS; batch 6;
  hopnbop `docs/design/H37_systems_audit.md` 3.5)
- **SP49** · V17's broken format string in
  `sdk:test/compliance/test_account_delete.gd:118` (XS; batch 7; hopnbop
  `docs/design/H37_systems_audit.md` section 5 row 2)
- **SP62** · Unknown config keys are ignored, and the other sync gaps
  (C4, C5) (XS to S; batch 7; hopnbop `docs/design/H37_systems_audit.md`
  3.4)

## Residuals (known, not fixed)

- **SP2** · phase-b's UptimeRobot step still calls the v2 API
  (`scripts/phase-b.ps1:373-416`: getAlertContacts, newAlertContact,
  getMonitors, newMonitor). On this free plan v2 `newMonitor` refuses
  monitor creation while v3 works, so a rebuild cannot recreate the three
  monitors. (Session 530145bc, platform-ops agent a94d4f4d, 2026-10-11
  11:55; the plan quirk is recorded in claude-config
  `rules/scheduling.md`.) H37 (10-14): still true
  (`scripts/phase-b.ps1:373-416`).
- **SP3** · The cost-monitor `.env` on the host has no `GITHUB_TOKEN`, so
  its GitHub Actions spend is a placeholder zero (`cost-monitor.sh:759`
  needs `GITHUB_TOKEN` and `GITHUB_ORG`), and weekly-cost-review quotes
  that zero. (Same report.) H37 (10-14): Believed still true (not
  checked on the host).
- **SP9** · `addons/snoringcat_platform_client/core/matchmaker_api_client.gd:653`
  prints `Preview device_id=%s` to stdout and godot.log. A device id
  logs that account in, so it is a credential. Crash reports redact it
  now (hopnbop 6b0dc6d7), but the log line remains. (Session d8031183,
  2026-10-11.) H37 (10-14): still true.
- **SP10** · CLAUDE.md says the `nakama-runtime` workflow runs gofmt; it
  runs only go vet, go test and staticcheck. Five committed runtime files
  aren't gofmt-clean (friends_limits.go, runtime_status.go,
  player_data.go, main.go, per_game_config.go). Format them in their own
  commit before adding a gofmt gate. (Same session.) H37 (10-14): still
  true; the set is now friends_limits, helpers_test, local_allocator,
  main and party.
- **SP18** · `match_cancel`: a match participant who knows the match's
  request_id can tear down that live match. Closing it needs a per-match
  secret handed to the game server. (Admin-key agent a654b0d8,
  2026-10-12.) H37 (10-14): still true; shares the per-match-secret fix
  with SP27.
- **SP19** · `register_server` accepts made-up request_ids, so junk
  registration rows can pile up. (Agent a654b0d8.) H37 (10-14): still
  true.
- **SP20** · `Platform.api` / `core/api_client.gd` is still created at
  startup; its only caller was the deleted `platform_smoke_test.gd`.
  (Agent a42eb164.)
- **SP21** · `README.md` still claims a `v1.4.0` tag; this repo has
  never had one. (Agent a42eb164.)
- **SP22** · Prod Nakama warns that `console.username` is the default
  (`admin`). The console listens on 127.0.0.1:7351 only. (Agent
  abbaef7c.) H37 (10-14): still true by config, low.
- **SP23** · phase-a's Verify step creates a junk anonymous account
  (device id `phase-a-smoke-*`) on every run; one leftover was deleted
  from prod on 2026-10-11. (Session 530145bc.) H37 (10-14): also purge
  orphaned guest accounts (AN-1); `account_cron.go` has nothing for
  them.
- **SP24** · `get_player_stats` keeps `rating` and `matches` as
  deprecated aliases for older clients (019499b). Remove them once no
  0.47.0 clients remain. (Agent abbaef7c.) H37 (10-14): still true.
- **SP25** · The vendored nakama-godot `follow_users_async` raises when
  none of the followed users are online. Nothing calls it today.
  (Compliance agent aee09f5d, 2026-10-11.) H37 (10-14): dormant.
- **SP31** · Couch co-op totals are uncapped: up to 16 bunnies in a
  4-user match. mm (read). Fix: cap the total at ticket or join time (S;
  batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.3 list)
- **SP48** · Desktop loopback OAuth is fragile (SI-4); Android sign-in
  (SI-6); Apple is listed with no flow (SI-3). acct: SI-4 Suspect; SI-6
  Believed, not run on a device; SI-3 Believed (M; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.1 list)
- **SP54** · `AutoRemove: true` discards game-server container logs
  (`rt:local_allocator.go:219`) (S; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.5)
- **SP53** · Docs: CLAUDE.md "Testing" and "Code Style" claims,
  MULTI_GAME_ROADMAP, a dead plan path; SP10's gofmt set (S; batch 7;
  hopnbop `docs/design/H37_systems_audit.md` 3.5)

## Ideas (not scheduled)

- **SP52** · Tests for signaling-proxy and geoip-sidecar. exec-a (vet
  clean) (M; hopnbop `docs/design/H37_systems_audit.md` 3.5 list)
- **SP57** · Run one real DR rebuild (M; hopnbop
  `docs/design/H37_systems_audit.md` 3.5, 2.2)

## Done

- **SP70** · Match rows listable by any signed-in player. Fixed:
  PermissionRead 0 on `server_registrations`, `match_metadata`
  and `synthetic_matches` writes; server-side reads unaffected.
  Rows written before the fix keep their old permission until
  they expire. New Go test asserts the registration row is 0/0.
  Runtime deploy still required (nakama-runtime workflow).
- **SP26** · Local port pool leaked a UDP/TCP pair per match.
  Fixed: allocations record their pair by request_id and Stop
  releases it (idempotent); Go test covers release and reuse.
  Both done 2026-10-14, batch 1.

- **SP1** · The host's `pg-backup.sh` predated ba6c421's double-ping
  guard. Done 2026-10-11: phase-b's PgBackup step redeployed the repo
  copy (with the rotated Postgres password) and its test backup passed.
- **SP4** · The addon's `.uid` files were untracked. Godot minted
  different script UIDs on Windows (CRLF) and Linux (LF) checkouts, so
  every Linux CI build lost hopnbop's Platform autoload. Done in da8394d
  (2026-10-11). hopnbop's CI bridge checks the autoload UID since
  78b1d311, bumped in a1a7753f.
- **SP5** · The Nakama client ran at nakama-godot's default DEBUG level.
  It logged bearer and refresh tokens, the server key and device ids to
  stdout and `user://logs/godot.log` on players' machines. Done in
  e8ad312 (2026-10-11); the SDK bump in hopnbop is 50cc1087.
- **SP6** · A failed session refresh was retried every frame for as long
  as the game ran, by real players' clients too
  (`auth_api_client.gd`, cooldown only after success). Done in 187912d
  (2026-10-11): backoff, and sign-out when Nakama rejects the token.
- **SP7** · DR rebuild: phase-a/b wrote a partial `/opt/nakama/.env`
  (no Edgegap, signaling-domain or local-allocator entries, so
  matchmaking was off; SIGNALING_HMAC_SECRET was dropped). Done in
  97fc542 (2026-10-11), checked against prod's 27 values. The job scripts
  reach the host by DNS name since claude-config f4874d3.
- **SP8** · cost-monitor ran on the Pages deploy token. A dedicated
  read-only `CLOUDFLARE_COST_MONITOR_TOKEN` is now deployed and
  documented. Done in 8c31f22 (2026-10-11).
- **SP11** · CLAUDE.md's "No custom tables" was stale. Done in 6081b82
  (2026-10-11).
