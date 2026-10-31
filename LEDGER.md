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


## Waiting on Levi

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
  public issue; after the proxy guard is deployed. Report drafted
  to the vendor's security contact; Levi sends it.

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



- **SP98** · Exercise the proxy request-body guard in CI: the dev
  compliance stack has no proxy, so nothing automated covers the guard
  the production Caddyfile enforces. Add a Caddy service to the dev
  stack built from the production image, point the suite at it, and
  assert an encoded request and an oversized body are refused at the
  proxy (SP83 follow-up).
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
- **SP95** · SP84 follow-up: a session issued in the same second as the
  delete outlives the revocation until its access token expires, since
  Nakama revokes by issue second. Refresh stays blocked.
- **SP100** · SP96 residual: a party of one can still gain a member
  between the join guard and the after-join hook, which leaves the
  player in two parties. The window is milliseconds; closing it needs
  the guard and the hook to agree on one read, which they cannot
  without a lock.
- **SP104** · SP88 residual: refusing every Cf character also refuses
  ZWNJ and ZWJ (Persian and Indic names, ZWJ emoji). Check the
  supported locales, or record it as a known limit (hopnbop H123 2.4).

## Ideas (not scheduled)

- **SP52** · Tests for signaling-proxy and geoip-sidecar. exec-a (vet
  clean) (M; hopnbop `docs/design/H37_systems_audit.md` 3.5 list)
- **SP57** · Run one real DR rebuild (M; hopnbop
  `docs/design/H37_systems_audit.md` 3.5, 2.2)
- **SP94** · SP32 consent could be server-only (a pending-invite state
  plus an invitation row), so it might not need a client release; check
  it against the 0.56.0 compliance matrix first (H104 1A SP32-2).

## Done

- **SP103** · The name clamp's kept output always passes `validateDisplayName` (control characters drop too); when nothing valid remains, the after-auth hook stores a generated name instead of keeping the invalid imported one. Done in f2d0698 (2026-10-30).
- **SP105** · Go files check out with LF (`*.go text eol=lf` in `.gitattributes`, checkout refreshed to w/lf), so a local `gofmt -l runtime` lists nothing instead of every file. Done in 7540b74 (2026-10-30).
- **SP102** · The consent path can no longer overwrite an unreadable auth file: `save_tokens()` moves it aside first (unique aside name) and writes nothing when the move fails; `login_anonymous()` keeps only its status message; `_do_anon_auth` no longer clears a refresh flag it never sets. Done in cd982de (2026-10-30).
- **SP91** · Presence rows written before server-only permissions are rewritten to 0/0 by a startup migration (idempotent; the comment that said no migration was needed is corrected), the rich-presence constant's doc comment is reattached, the presence RPC returns fixed error messages, and the cascade check asserts its presence read succeeded. Done in 4d534b6 (2026-10-30).
- **SP90** · SDK auth store (SP44): a failed load is recorded; an unreadable auth file is moved aside for a new guest instead of being minted over; a readable file with no guest id mints one; a key that cannot be written falls back to the previous passphrase. TS-1 accepted as obfuscation, stated in the store header and the H37 audit. Done in 8832a3b (2026-10-30).
- **SP89** · SDK auth: refresh results take their own route (never a link result, stored only while the store holds the token the attempt used); delete refuses during a link or unlink, waits out a refresh, and fails through delete_completed. Done in 7d39613 (2026-10-30).
- **SP88** · SP42 follow-ups: exact avatar host patterns; names stored as checked (trimmed on write, invisible characters and reserved-name look-alikes refused, letter or digit required); a compliance test that the account update hook is wired. Done in 03884fa (2026-10-30).
- **SP101** · SP87 review follow-ups: name-clamp reserved-name slicing and after-auth context reads; party-group create follows the join guard (refuses a caller in a shared party, disbands parties of one); gofmt step in the runtime workflow. Done in 4bef255 (2026-10-30).
- **SP99** · SP96 follow-ups: the after-join hook disbands the joiner's party of one only when the joiner is an active member of the joined party (a join request on a closed party ends nothing); party_join_by_code picks the parties of one from a fresh read after its add; the joiner-side refusals say 'you are already in a party'. Done in b8e20d5 (2026-10-30).
- **SP93** · A prod query for stored avatar URLs and display names that fail the SP42 checks, then a cleanup (H104 1B F4). A read-only count on prod (2026-10-30) found no stored display name and no stored avatar URL that fails the SP42 rules; the only non-standard names are accounts that never set one and the marker that account deletion writes, both expected. Nothing to clean up.
- **SP83** · Prod Nakama needs a request-body guard at the proxy and a memory limit on its container (H104 1B F18). Done in e17586f (2026-10-30).
- **SP87** · SP59 follow-ups: bound the `settings` keys and the party-prefs inputs, one live party per creator, `recent_players` off the client-writable list (with hopnbop allowlist seeds if 0.56.0 never writes it), tests that fail with the hook unregistered (H104 1B F14 to F17). Done in d4e12c9 (2026-10-30).
- **SP96** · SP85 follow-up: a party holding only the invited or joining player must not count as being in a party. Done in 2cf7a55 (2026-10-30).
- **SP97** · SP86 follow-ups: party_abort_matchmaking releases the party's start-guard entry so a fresh attempt is not refused inside the window; the start guard prunes entries older than its window; the party_matchmaking_start notification carries the member_ids roster that passed the ready gate, so followers check their own id in it instead of a ready flag the start already spent. Done in 66d6beb (2026-10-30).
- **SP86** · SP33 follow-ups: ready state outlives a start and a departure; one start at a time per party; a test that an unready member cannot start. Done in 999b3b5 (2026-10-30).
- **SP85** · Party invite and join, server-only checks that need no client change: a blocked target, a target already in another party, and raw group calls on party groups. Done in 1f3f8ca (2026-10-30).
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
- **SP33** · Any member can pull everyone into a match load (M; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2). Done in 0499a41 (2026-10-29); ready lifecycle and one-start-at-a-time in 999b3b5 (SP86), follower checks in hopnbop 3238dfd3 (H114).
- **SP34** · The creator cannot leave while others remain (TODO L127) (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.2, section 7 row 5). Done in d1768f2 (2026-10-29).
- **SP42** · Server-side validation of names and avatar URLs (PI-1, DN-2) (M; batch 4; hopnbop `docs/design/H37_systems_audit.md` 3.1). Done in d37393f (2026-10-29).
- **SP61** · The global crash cap starved servers; HTTP-key callers now draw only on the server budget. Done in e5ae4e6 (2026-10-29).
- **SP40** · Notification rows older than 30 days are pruned by the account cron (server half of H54). Done in 078d9b8 (2026-10-29).
- **SP36** · Blocking stops friend requests and nothing else (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2). Done in 475d22a (2026-10-29).
- **SP37** · Party chat has no server-side length, rate or block limits (soc F9); dead routes. Done in c8e0864 (2026-10-29).
- **SP35** · 50 pending outgoing friend requests block every accept (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2). Done in bbac2e0 (2026-10-29).
- **SP47** · Sessions are not revoked on account delete (DEL-3). acct (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1 list). Done in 4ccc33b (2026-10-29).
- **SP84** · Account deletion now ends every session and socket at delete time (completes SP47's revocation; the released client was checked against it: delete shows the queued toast, grace-window re-sign-in cancels cleanly). Done in 5d0adce, fixture follow-up f6fc9e3 (2026-10-30).
- **SP43** · A refresh during a pending link is taken as the link's success (LK-1) (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1). Done in 22055ff (2026-10-29).
- **SP41** · A failed `delete_account` is reported as a failed sign-in (DEL-2); root cause `sdk:core/auth_api_client.gd:890,896` (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1). Done in 69c4b6b (2026-10-29).
- **SP51** · Pin staticcheck in `nakama-runtime.yml:44` (XS; batch 6; hopnbop `docs/design/H37_systems_audit.md` 3.5). Done in 9139b38 (2026-10-29).
