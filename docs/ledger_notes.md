# Ledger notes

Text moved out of `LEDGER.md` by the ledger diet (`scripts/ledger_diet.mjs`
in knotmaze, K353; applied 2026-10-15), verbatim: the full text of items
that were shortened, moved or closed, under their ID. The ledger keeps each
item short and this file keeps the rest. New entries go at the bottom.

## SP12 (2026-10-15, shortened by the diet)

- **SP12** · Facebook sign-in is probably broken for everyone: the SDK
  passes Facebook's OAuth code to Nakama where Nakama expects an access
  token. The button is live in the account and upgrade panels. Offered:
  verify, then fix with a server-side code exchange like Google's
  (the hopnbop.net token function). Read from code, not tested live.
  (Session 530145bc, tooling agent a42eb164 and data-rights agent
  aceea38c, 2026-10-12.) H37 (10-14): Broken by code (acct SI-2); both
  panels offer Facebook and the privacy text names it. Recommended: hide
  Facebook until it works (report section 7 row 11).

## SP13 (2026-10-15, shortened by the diet)

- **SP13** · Custom-ID, Game Center and Facebook Instant sign-ins skip
  the `game_id` auth hook, so anyone with the public server key can
  create accounts that way. Offered: close them. (Agent aceea38c.) H37
  (10-14): still true for Custom, Game Center and FB Instant; Email is
  already refused. Recommended: refuse the three until they are needed
  (report section 7 row 11).

## SP14 (2026-10-15, shortened by the diet)

- **SP14** · Match notifications never expire: every match leaves a
  persistent `match_ready` or `match_failed` notification in each
  player's inbox, and nothing deletes them. Offered: expire them.
  (Agent aceea38c.) H37 (10-14): still true (report ops RT2).

## SP15 (2026-10-15, shortened by the diet)

- **SP15** · `GOOGLE_OAUTH_CLIENT_SECRET` and `FACEBOOK_APP_SECRET` are
  passed into Nakama's environment (`infra/remote/nakama/docker-compose.yml`,
  `scripts/lib/nakama-env.ps1`, `config.yml`) but nothing reads them;
  Google's secret is only used by the hopnbop.net token function.
  Offered: remove them from the host. (Agent a42eb164.) H37 (10-14):
  still true.

## SP16 (2026-10-15, shortened by the diet)

- **SP16** · `delete_account` doesn't refresh the public leaderboard
  cache, so a deleted player can stay listed for up to 60 s (about 2 min
  in a browser). Offered: one call in the deletion flow. (Leaderboard
  agent abbaef7c, 2026-10-12.) H37 (10-14): still true (report ops L3).

## SP17 (2026-10-15, shortened by the diet)

- **SP17** · Google sign-in has never run end to end with a real
  account. Needs Levi at a browser for two minutes:
  `python scripts/test-google-auth.py` (fixed in 6ffa5fd; it now fails if
  the token lacks a claim Nakama needs, and reports whether an email
  came back). (Agents a42eb164 and aceea38c.)

## SP32 (2026-10-15, shortened by the diet)

- **SP32** · A party invite is a forced join. Fix
  `sdk:test/compliance/test_party_invite_flow.gd:136-140` with it, since
  it accepts the join. Urgent candidate (report section 7 row 1) (M to
  L; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.2, 0.1 row 2)

## SP44 (2026-10-15, shortened by the diet)

- **SP44** · The guest device id is not crypto-random (SI-1); the token
  store passphrase is weak (TS-1). acct: SI-1 Suspect, low-medium; TS-1
  Believed, low. Fix: a random 128-bit device id; a real key per
  platform (S; batch 2; hopnbop `docs/design/H37_systems_audit.md` 3.1
  list)

## SP37 (2026-10-15, shortened by the diet)

- **SP37** · Party chat has no server-side length, rate or block limits;
  stale comments and dead code. soc F9. Fix: limits in the chat hook;
  delete the dead routes (S; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.2 list)

## SP45 (2026-10-15, shortened by the diet)

- **SP45** · Every hourly refresh emits the same `auth_completed(true,
  "")` as a sign-in, so subscribers redo sign-in work (SL-1). acct:
  Believed, low-medium. `_handle_auth_success` (1282) emits it;
  `global.gd:388-392` then refetches and merges cloud settings, and four
  more listeners react. Fix: a separate `session_refreshed` signal that
  sign-in listeners ignore (S; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.1 list)

## SP2 (2026-10-15, shortened by the diet)

- **SP2** · phase-b's UptimeRobot step still calls the v2 API
  (`scripts/phase-b.ps1:373-416`: getAlertContacts, newAlertContact,
  getMonitors, newMonitor). On this free plan v2 `newMonitor` refuses
  monitor creation while v3 works, so a rebuild cannot recreate the three
  monitors. (Session 530145bc, platform-ops agent a94d4f4d, 2026-10-11
  11:55; the plan quirk is recorded in claude-config
  `rules/scheduling.md`.) H37 (10-14): still true
  (`scripts/phase-b.ps1:373-416`).

## SP3 (2026-10-15, shortened by the diet)

- **SP3** · The cost-monitor `.env` on the host has no `GITHUB_TOKEN`, so
  its GitHub Actions spend is a placeholder zero (`cost-monitor.sh:759`
  needs `GITHUB_TOKEN` and `GITHUB_ORG`), and weekly-cost-review quotes
  that zero. (Same report.) H37 (10-14): Believed still true (not
  checked on the host).

## SP9 (2026-10-15, shortened by the diet)

- **SP9** · `addons/snoringcat_platform_client/core/matchmaker_api_client.gd:653`
  prints `Preview device_id=%s` to stdout and godot.log. A device id
  logs that account in, so it is a credential. Crash reports redact it
  now (hopnbop 6b0dc6d7), but the log line remains. (Session d8031183,
  2026-10-11.) H37 (10-14): still true.

## SP10 (2026-10-15, shortened by the diet)

- **SP10** · CLAUDE.md says the `nakama-runtime` workflow runs gofmt; it
  runs only go vet, go test and staticcheck. Five committed runtime files
  aren't gofmt-clean (friends_limits.go, runtime_status.go,
  player_data.go, main.go, per_game_config.go). Format them in their own
  commit before adding a gofmt gate. (Same session.) H37 (10-14): still
  true; the set is now friends_limits, helpers_test, local_allocator,
  main and party.

## SP18 (2026-10-15, shortened by the diet)

- **SP18** · `match_cancel`: a match participant who knows the match's
  request_id can tear down that live match. Closing it needs a per-match
  secret handed to the game server. (Admin-key agent a654b0d8,
  2026-10-12.) H37 (10-14): still true; shares the per-match-secret fix
  with SP27.

## SP23 (2026-10-15, shortened by the diet)

- **SP23** · phase-a's Verify step creates a junk anonymous account
  (device id `phase-a-smoke-*`) on every run; one leftover was deleted
  from prod on 2026-10-11. (Session 530145bc.) H37 (10-14): also purge
  orphaned guest accounts (AN-1); `account_cron.go` has nothing for
  them.

## SP48 (2026-10-15, shortened by the diet)

- **SP48** · Desktop loopback OAuth is fragile (SI-4); Android sign-in
  (SI-6); Apple is listed with no flow (SI-3). acct: SI-4 Suspect; SI-6
  Believed, not run on a device; SI-3 Believed (M; batch 4; hopnbop
  `docs/design/H37_systems_audit.md` 3.1 list)
