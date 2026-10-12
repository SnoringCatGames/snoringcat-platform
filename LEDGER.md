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

## In progress

## Next

## Residuals (known, not fixed)

- **SP1** · The host's deployed `pg-backup.sh` predates 2bfd448's guard.
  The repo copy (`infra/remote/pg-backup/pg-backup.sh`) stops the ERR
  trap from firing twice on a retention failure, which used to send two
  Discord pings blaming different lines. The copy on nakama-prod-1 was
  never redeployed. (Session 530145bc, platform-ops agent a94d4f4d,
  2026-10-11 11:55.)
- **SP2** · phase-b's UptimeRobot step still calls the v2 API
  (`scripts/phase-b.ps1:373-416`: getAlertContacts, newAlertContact,
  getMonitors, newMonitor). On this free plan v2 `newMonitor` refuses
  monitor creation while v3 works, so a rebuild cannot recreate the three
  monitors. (Same report; the plan quirk is recorded in claude-config
  `rules/scheduling.md`.)
- **SP3** · The cost-monitor `.env` on the host has no `GITHUB_TOKEN`, so
  its GitHub Actions spend is a placeholder zero (`cost-monitor.sh:759`
  needs `GITHUB_TOKEN` and `GITHUB_ORG`), and weekly-cost-review quotes
  that zero. (Same report.)
- **SP9** · `addons/snoringcat_platform_client/core/matchmaker_api_client.gd:653`
  prints `Preview device_id=%s` to stdout and godot.log. A device id
  logs that account in, so it is a credential. It matters more once
  crash reports upload log excerpts (hopnbop
  `docs/design/crash_report_review.md` point 9). (Session d8031183,
  2026-10-11.)
- **SP10** · CLAUDE.md says the `nakama-runtime` workflow runs gofmt; it
  runs only go vet, go test and staticcheck. Five committed runtime files
  aren't gofmt-clean (friends_limits.go, runtime_status.go,
  player_data.go, main.go, per_game_config.go). Format them in their own
  commit before adding a gofmt gate. (Same session.)
- **SP11** · CLAUDE.md's "No custom tables" is stale: the per-game config
  `games` table exists (`newPerGameConfig`, idempotent DDL at
  InitModule). The crash-report runtime commit (hopnbop H1) should fix it
  and add the new file to the Runtime modules list. (Same session.)

## Ideas (not scheduled)

## Done

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
