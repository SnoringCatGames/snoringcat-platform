# snoringcat-platform ledger

Open work for snoringcat-platform: the client SDK, the Nakama runtime,
and the prod-host scripts (phase-a/phase-b, infra/remote). It covers
follow-ups, known residuals, decisions waiting on Levi, and ideas.
Convention: claude-config `rules/ledger.md`. IDs (`SP<n>`) are stable and
never reused. Game-side items live in hopnbop's `LEDGER.md`.

Session pointers like "session 530145bc, agent a94d4f4d" refer to Claude
Code transcripts under `~/.claude/projects/` (kept 30 days). Each item
states its facts, so it stands on its own after the transcript is gone.

Items stay short (the diet, claude-config CC64, 2026-10-15): one to three lines; an
In progress handle is its six fields, one line each; a Done entry is
one line, `- **SP<n>** · <what>. Done in <commit> (<date>).` Shortened
items keep their full text in `docs/ledger_notes.md`, and older Done
entries move to `docs/ledger_done.md`; the next ID counts both files.

## Urgent

- **SP83** · Prod Nakama needs a request-body guard at the proxy and a
  memory limit on its container (H104 1B F18). Patch prepared and
  tested locally 2026-10-30 (hopnbop `.claude/probes/H104/F18/`);
  it is committed and deployed together once Levi approves the
  deploy. Details: hopnbop `.claude/probes/H104/stage1-B.md`
  (local).


## Waiting on Levi

- **SP84** · SP47's revocation (sessions and sockets end at
  delete time) is built and tested but held for a release:
  the released suite gains one unlisted failure, a fixture
  that reads the account back with the pre-delete token
  (players sign in again and are unaffected). Patch:
  hopnbop `.claude/probes/SP84/sp84.patch`; ship it with the
  next client release. Reopens SP47 (H104 1B F11 to F13).
- **SP82** · Dependabot alerts are on but blind to the Pulumi module
  (checked 2026-10-29; detail in `docs/ledger_notes.md`): Levi to OK
  adding `.github/dependabot.yml` (gomod,
  `/infra/pulumi/snoringcat-platform`) so SP81's advisories surface.
- **SP81** · Pulumi pins bumped (go.mod/go.sum; tidy, build, vet pass);
  Levi to run `pulumi preview` in
  `infra/pulumi/snoringcat-platform` (expect no changes; needs prod
  credentials).
- **SP12** · Facebook sign-in is probably broken (SI-2): the SDK sends
  the OAuth code where Nakama wants an access token. Recommended: hide
  Facebook until it works (report section 7 row 11).
- **SP13** · Custom-ID, Game Center and FB Instant sign-ins skip the
  `game_id` auth hook, so anyone with the server key can create
  accounts. Recommended: refuse them until needed (report 7 row 11).
- **SP14** · Match notifications never expire: every match leaves a
  persistent `match_ready` or `match_failed` notification per player.
  Offered: expire them (report ops RT2).
- **SP15** · Nakama's environment gets `GOOGLE_OAUTH_CLIENT_SECRET` and
  `FACEBOOK_APP_SECRET` (docker-compose, `nakama-env.ps1`, `config.yml`)
  but nothing reads them. Offered: remove them from the host.
- **SP16** · `delete_account` leaves the public leaderboard cache stale:
  a deleted player stays listed up to 60 s (about 2 min in a browser).
  Offered: one refresh call in the deletion flow (ops L3).
- **SP17** · Google sign-in has never run end to end with a real
  account. Needs Levi at a browser for two minutes:
  `scripts/test-google-auth.py` (6ffa5fd).
- **SP69** · One account can queue from two devices (TD-1, TODO L112)
  (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.1 list,
  section 7 row 7)
- **SP78** · nakama-prod-1 has been up 141 days; its kernel predates
  fixes since early June unless livepatched. Hetzner lists
  CVE-2026-99999 (PwnEscape, disclosed 10-15, still Investigating).
  Check whether the guest needs the patch and pick a reboot window
  (weekly-cost-review 2026-10-21); question
- **SP92** · Report H104 1B F18 (a Nakama 3.25.0 server issue) to
  Heroic Labs privately, through their security contact rather than a
  public issue; after the proxy guard is deployed.
- **SP93** · A prod query for stored avatar URLs and display names that
  fail the SP42 checks, then a cleanup (H104 1B F4).

## In progress

## Next

- **SP32** · A party invite is a forced join (M; batch 2;
  hopnbop `docs/design/H37_systems_audit.md` 3.2). HELD for
  the next release (2026-10-29): refusing offline invites
  and the consent flow change what 0.56.0 clients see;
  gate is shipping with the first client that handles both
- **SP45** · A refresh emits the sign-in's `auth_completed(true, "")`
  (SL-1; `global.gd:388-392`). Fix: a separate `session_refreshed`
  signal (S; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.1)
- **SP46** · An expected export failure files a crash report (DX-1).
  acct; exec-b's offline export run (XS; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.1 list)
- **SP65** · Grafana rules for 5xx, 429 and Nakama RPC errors (OP6) (S;
  batch 5; hopnbop `docs/design/H37_systems_audit.md` 3.5)
- **SP56** · Close the DR drill's gaps. The drill was a tabletop
  exercise, not an outage (M; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.5, 2.2)
- **SP64** · Nothing validates or reloads the Caddyfile on deploy; the
  Caddy build is unpinned (RL5, RL6) (S; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.5)
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
- **SP62** · Unknown config keys are ignored, and the other sync gaps
  (C4, C5) (XS to S; batch 7; hopnbop `docs/design/H37_systems_audit.md`
  3.4)
- **SP85** · Party invite and join, server-only checks that need no
  client change: a blocked target, a target already in another party,
  and raw group calls on party groups. Reopens the party half of SP36;
  consent (SP32) stays held for the release (H104 1A SP32-1, SP33-C,
  SP36-2).
- **SP86** · SP33 follow-ups: ready state outlives a start and a
  departure; one start at a time per party; a test that an unready
  member cannot start. Reopens SP33, whose Done line cites 8cf642b, not
  0499a41 (H104 1A SP33-A, B, D, E, F).
- **SP87** · SP59 follow-ups: bound the `settings` keys and the
  party-prefs inputs, one live party per creator, `recent_players` off
  the client-writable list (with hopnbop allowlist seeds if 0.56.0
  never writes it), tests that fail with the hook unregistered (H104 1B
  F14 to F17).
- **SP88** · SP42 follow-ups: exact avatar host patterns, names stored
  as checked, invisible characters and `[Deleted]` look-alikes refused,
  a compliance test that the hook is wired (H104 1B F3 runtime half,
  F5, F6).
- **SP89** · SDK auth: a refresh result can be taken as a link result,
  or written back after a delete or sign-out; the delete needs its own
  failure path and must wait for a pending link or refresh (H104 1A
  SP41-2, SP41-3; 1B F9, F10).
- **SP90** · SDK auth store (SP44): an unreadable `auth.cfg` re-mints a
  guest, a guest file without an id locks out, `_install_key` ignores a
  failed save. TS-1 is accepted as obfuscation (Claude, 2026-10-30):
  document it in the code and the audit (H104 1A SP44-1 to 4).
- **SP91** · Presence rows written before 475d22a keep their old
  permissions: migrate them if the 0.56.0 compliance matrix stays
  green, and fix the comment that says no migration is needed. Plus
  SP36 nits (H104 1A SP36-1, SP36-3 to 5).

## Residuals (known, not fixed)

- **SP2** · phase-b's UptimeRobot step still calls the v2 API
  (`scripts/phase-b.ps1:373-416`), whose `newMonitor` this free plan
  refuses (v3 works), so a rebuild can't recreate the monitors.
- **SP3** · The host's cost-monitor `.env` lacks `GITHUB_TOKEN`
  (`cost-monitor.sh:759`), so its GitHub Actions spend is a placeholder
  zero that weekly-cost-review quotes (unchecked on the host).
- **SP9** · `matchmaker_api_client.gd:653` prints `Preview device_id=%s`
  to stdout and godot.log; a device id logs that account in, so it is a
  credential. Crash reports redact it (6b0dc6d7); the log line remains.

- **SP18** · `match_cancel`: a match participant who knows the
  request_id can tear down that live match. Closing it needs a per-match
  secret handed to the game server (shared with SP27).
- **SP19** · `register_server` accepts made-up request_ids, so junk
  registration rows can pile up. (Agent a654b0d8.) H37 (10-14): still
  true.
- **SP22** · Prod Nakama warns that `console.username` is the default
  (`admin`). The console listens on 127.0.0.1:7351 only. (Agent
  abbaef7c.) H37 (10-14): still true by config, low.
- **SP23** · phase-a's Verify step creates a junk anonymous account
  (`phase-a-smoke-*`) on every run; also purge orphaned guest accounts
  (AN-1), which `account_cron.go` ignores.
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
  (SI-6, never run on a device); Apple is listed with no flow (SI-3) (M;
  batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.1)
- **SP54** · `AutoRemove: true` discards game-server container logs
  (`rt:local_allocator.go:219`) (S; batch 5; hopnbop
  `docs/design/H37_systems_audit.md` 3.5)
- **SP77** · Edgegap bills a $1 minimum in any month with a deployment
  and the weekly probe deploys there, so the bill is at least $1/mo while
  cost-monitor shows $0.00 (it tracked none of the 5 October probe
  deploys and has no floor). Add the floor; check the Edgegap invoice
  (infra/remote/cost-monitor/cost-monitor.sh:412; weekly-cost-review
  2026-10-21); residual
- **SP79** · cost-monitor Hetzner MTD is a hard-coded cpx21@hil rate
  (infra/remote/cost-monitor/cost-monitor.sh:239) that cannot see a
  reprice, and no job compares it with an invoice; check the latest
  Hetzner invoice is about $14.59 before tax (weekly-cost-review
  2026-10-14 finding 1); residual
- **SP80** · cost-monitor.sh:257 reads .datacenter.location.name, which
  Hetzner removed 2026-07-01, so the location is always the hil
  fallback; read the server location field (weekly-cost-review
  2026-10-14 finding 4); residual
- **SP59** · Any session can write unbounded storage (RL3) (M; batch 5;
  hopnbop `docs/design/H37_systems_audit.md` 3.5). Runtime half
  done in 3c65a1e (2026-10-29): storage allowlist plus size cap
  deployed (run 40929281553). Left for Levi: the per-IP Caddy
  zone (prod host config); residual

## Ideas (not scheduled)

- **SP52** · Tests for signaling-proxy and geoip-sidecar. exec-a (vet
  clean) (M; hopnbop `docs/design/H37_systems_audit.md` 3.5 list)
- **SP57** · Run one real DR rebuild (M; hopnbop
  `docs/design/H37_systems_audit.md` 3.5, 2.2)
- **SP94** · SP32 consent could be server-only (a pending-invite state
  plus an invitation row), so it might not need a client release; check
  it against the 0.56.0 compliance matrix first (H104 1A SP32-2).

## Done

- **SP20** · `Platform.api` / `core/api_client.gd` was still created at startup; its only caller is gone. Done in 99c9195 (2026-10-29).
- **SP21** · `README.md` claimed a `v1.4.0` tag; this repo has never had a tag. Done in 99c9195 (2026-10-29).
- **SP53** · Docs: CLAUDE.md "Testing" and "Code Style" claims, MULTI_GAME_ROADMAP, a dead plan path; SP10's gofmt set. Done in 7a5399e (2026-10-29).
- **SP10** · CLAUDE.md said `nakama-runtime` runs gofmt; four runtime files weren't gofmt-clean. Done in 7a5399e (2026-10-29).
- **SP49** · V17's broken format string in `sdk:test/compliance/test_account_delete.gd:118`. Done in 6987291 (2026-10-29).
- **SP50** · Client SDK CI runs no tests (S; batch 6; hopnbop `docs/design/H37_systems_audit.md` 3.5). Done in e9c98f8 (2026-10-29).
- **SP58** · match_end is not atomic and nothing monitors it (S1, OP2); the stale-row sweep (RT1) (M; batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.4). Done in 12ab013 (2026-10-29).
- **SP71** · `match_cancel` without a registration is a no-op; test matches leave rows; two wrong test comments (XS to S; batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.3). Done in 8d7fce3 (2026-10-29).
- **SP28** · A Cancel during `start_matchmaking` leaves a ghost ticket (S; batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.3). Done in 3e2bc1d (2026-10-29).
- **SP29** · One player's 120 s timeout cancels the whole matched set's allocation (S; batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.3). Done in 6145f01489b6e003012a6e4c30b953acd41a65a0 (2026-10-29).
- **SP30** · No match forms for anyone while an allocation runs (M; batch 3; hopnbop `docs/design/H37_systems_audit.md` 3.3). Done in de57ca1 (2026-10-29).
- **SP44** · Guest device id is not crypto-random (SI-1); token store passphrase is weak (TS-1). Done in b2eb9cd (2026-10-29).
- **SP33** · Any member can pull everyone into a match load (M; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2). Done in 8cf642b (2026-10-29).
- **SP34** · The creator cannot leave while others remain (TODO L127) (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.2, section 7 row 5). Done in d1768f2 (2026-10-29).
- **SP42** · Server-side validation of names and avatar URLs (PI-1, DN-2) (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.1). Done in d37393f (2026-10-29).
- **SP61** · The global crash cap starved servers; HTTP-key callers now draw only on the server budget. Done in e5ae4e6 (2026-10-29).
- **SP40** · Notification rows older than 30 days are pruned by the account cron (server half of H54). Done in 078d9b8 (2026-10-29).
- **SP36** · Blocking stops friend requests and nothing else (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2). Done in 475d22a (2026-10-29).
- **SP37** · Party chat has no server-side length, rate or block limits (soc F9); dead routes. Done in c8e0864 (2026-10-29).
- **SP35** · 50 pending outgoing friend requests block every accept (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2). Done in bbac2e0 (2026-10-29).
- **SP47** · Sessions are not revoked on account delete (DEL-3). acct (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1 list). Done in 4ccc33b (2026-10-29).
- **SP43** · A refresh during a pending link is taken as the link's success (LK-1) (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1). Done in 22055ff (2026-10-29).
- **SP41** · A failed `delete_account` is reported as a failed sign-in (DEL-2); root cause `sdk:core/auth_api_client.gd:890,896` (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1). Done in 69c4b6b (2026-10-29).
- **SP51** · Pin staticcheck in `nakama-runtime.yml:44` (XS; batch 6; hopnbop `docs/design/H37_systems_audit.md` 3.5). Done in 9139b38 (2026-10-29).
