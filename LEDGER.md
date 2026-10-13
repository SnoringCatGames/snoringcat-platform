# snoringcat-platform ledger

Open work for snoringcat-platform: the client SDK, the Nakama runtime,
and the prod-host scripts (phase-a/phase-b, infra/remote). It covers
follow-ups, known residuals, decisions waiting on Levi, and ideas.
Convention: claude-config `rules/ledger.md`. IDs (`SP<n>`) are stable and
never reused. Game-side items live in hopnbop's `LEDGER.md`.

Session pointers like "session 530145bc, agent a94d4f4d" refer to Claude
Code transcripts under `~/.claude/projects/` (kept 30 days). Each item
states its facts, so it stands on its own after the transcript is gone.

## Waiting on Levi

- **SP12** · Facebook sign-in is probably broken for everyone: the SDK
  passes Facebook's OAuth code to Nakama where Nakama expects an access
  token. The button is live in the account and upgrade panels. Offered:
  verify, then fix with a server-side code exchange like Google's
  (the hopnbop.net token function). Read from code, not tested live.
  (Session 530145bc, tooling agent a42eb164 and data-rights agent
  aceea38c, 2026-10-12.)
- **SP13** · Custom-ID, Game Center and Facebook Instant sign-ins skip
  the `game_id` auth hook, so anyone with the public server key can
  create accounts that way. Offered: close them. (Agent aceea38c.)
- **SP14** · Match notifications never expire: every match leaves a
  persistent `match_ready` or `match_failed` notification in each
  player's inbox, and nothing deletes them. Offered: expire them.
  (Agent aceea38c.)
- **SP15** · `GOOGLE_OAUTH_CLIENT_SECRET` and `FACEBOOK_APP_SECRET` are
  passed into Nakama's environment (`infra/remote/nakama/docker-compose.yml`,
  `scripts/lib/nakama-env.ps1`, `config.yml`) but nothing reads them;
  Google's secret is only used by the hopnbop.net token function.
  Offered: remove them from the host. (Agent a42eb164.)
- **SP16** · `delete_account` doesn't refresh the public leaderboard
  cache, so a deleted player can stay listed for up to 60 s (about 2 min
  in a browser). Offered: one call in the deletion flow. (Leaderboard
  agent abbaef7c, 2026-10-12.)
- **SP17** · Google sign-in has never run end to end with a real
  account. Needs Levi at a browser for two minutes:
  `python scripts/test-google-auth.py` (fixed in 24b59a4; it now fails if
  the token lacks a claim Nakama needs, and reports whether an email
  came back). (Agents a42eb164 and aceea38c.)

## In progress

## Next

## Residuals (known, not fixed)

- **SP2** · phase-b's UptimeRobot step still calls the v2 API
  (`scripts/phase-b.ps1:373-416`: getAlertContacts, newAlertContact,
  getMonitors, newMonitor). On this free plan v2 `newMonitor` refuses
  monitor creation while v3 works, so a rebuild cannot recreate the three
  monitors. (Session 530145bc, platform-ops agent a94d4f4d, 2026-10-11
  11:55; the plan quirk is recorded in claude-config
  `rules/scheduling.md`.)
- **SP3** · The cost-monitor `.env` on the host has no `GITHUB_TOKEN`, so
  its GitHub Actions spend is a placeholder zero (`cost-monitor.sh:759`
  needs `GITHUB_TOKEN` and `GITHUB_ORG`), and weekly-cost-review quotes
  that zero. (Same report.)
- **SP9** · `addons/snoringcat_platform_client/core/matchmaker_api_client.gd:653`
  prints `Preview device_id=%s` to stdout and godot.log. A device id
  logs that account in, so it is a credential. Crash reports redact it
  now (hopnbop 6b0dc6d7), but the log line remains. (Session d8031183,
  2026-10-11.)
- **SP10** · CLAUDE.md says the `nakama-runtime` workflow runs gofmt; it
  runs only go vet, go test and staticcheck. Five committed runtime files
  aren't gofmt-clean (friends_limits.go, runtime_status.go,
  player_data.go, main.go, per_game_config.go). Format them in their own
  commit before adding a gofmt gate. (Same session.)
- **SP18** · `match_cancel`: a match participant who knows the match's
  request_id can tear down that live match. Closing it needs a per-match
  secret handed to the game server. (Admin-key agent a654b0d8,
  2026-10-12.)
- **SP19** · `register_server` accepts made-up request_ids, so junk
  registration rows can pile up. (Agent a654b0d8.)
- **SP20** · `Platform.api` / `core/api_client.gd` is still created at
  startup; its only caller was the deleted `platform_smoke_test.gd`.
  (Agent a42eb164.)
- **SP21** · `README.md` still claims a `v1.4.0` tag; this repo has
  never had one. (Agent a42eb164.)
- **SP22** · Prod Nakama warns that `console.username` is the default
  (`admin`). The console listens on 127.0.0.1:7351 only. (Agent
  abbaef7c.)
- **SP23** · phase-a's Verify step creates a junk anonymous account
  (device id `phase-a-smoke-*`) on every run; one leftover was deleted
  from prod on 2026-10-11. (Session 530145bc.)
- **SP24** · `get_player_stats` keeps `rating` and `matches` as
  deprecated aliases for older clients (392ab52). Remove them once no
  0.47.0 clients remain. (Agent abbaef7c.)
- **SP25** · The vendored nakama-godot `follow_users_async` raises when
  none of the followed users are online. Nothing calls it today.
  (Compliance agent aee09f5d, 2026-10-11.)

## Ideas (not scheduled)

## Done

- **SP1** · The host's `pg-backup.sh` predated 2bfd448's double-ping
  guard. Done 2026-10-11: phase-b's PgBackup step redeployed the repo
  copy (with the rotated Postgres password) and its test backup passed.
- **SP4** · The addon's `.uid` files were untracked. Godot minted
  different script UIDs on Windows (CRLF) and Linux (LF) checkouts, so
  every Linux CI build lost hopnbop's Platform autoload. Done in b2eecad
  (2026-10-11). hopnbop's CI bridge checks the autoload UID since
  78b1d311, bumped in a1a7753f.
- **SP5** · The Nakama client ran at nakama-godot's default DEBUG level.
  It logged bearer and refresh tokens, the server key and device ids to
  stdout and `user://logs/godot.log` on players' machines. Done in
  76a9572 (2026-10-11); the SDK bump in hopnbop is 50cc1087.
- **SP6** · A failed session refresh was retried every frame for as long
  as the game ran, by real players' clients too
  (`auth_api_client.gd`, cooldown only after success). Done in 8b891cd
  (2026-10-11): backoff, and sign-out when Nakama rejects the token.
- **SP7** · DR rebuild: phase-a/b wrote a partial `/opt/nakama/.env`
  (no Edgegap, signaling-domain or local-allocator entries, so
  matchmaking was off; SIGNALING_HMAC_SECRET was dropped). Done in
  cd0cc6e (2026-10-11), checked against prod's 27 values. The job scripts
  reach the host by DNS name since claude-config f4874d3.
- **SP8** · cost-monitor ran on the Pages deploy token. A dedicated
  read-only `CLOUDFLARE_COST_MONITOR_TOKEN` is now deployed and
  documented. Done in ce07eb9 (2026-10-11).
- **SP11** · CLAUDE.md's "No custom tables" was stale. Done in af566b9
  (2026-10-11).
