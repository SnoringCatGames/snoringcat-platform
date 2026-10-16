# Ledger done

Done items moved out of `LEDGER.md` by the ledger diet
(`scripts/ledger_diet.mjs` in knotmaze, K353; applied 2026-10-15), verbatim,
under their ID: every entry resolved before 2026-10-12 or undated, and the
full text of the newer ones, which the ledger keeps as a one-line entry. New
entries go at the bottom.

## SP70 (Done, undated)

- **SP70** · Match rows listable by any signed-in player. Fixed:
  PermissionRead 0 on `server_registrations`, `match_metadata`
  and `synthetic_matches` writes; server-side reads unaffected.
  Rows written before the fix keep their old permission until
  they expire. New Go test asserts the registration row is 0/0.
  Runtime deploy still required (nakama-runtime workflow).

## SP26 (Done 2026-10-14)

- **SP26** · Local port pool leaked a UDP/TCP pair per match.
  Fixed: allocations record their pair by request_id and Stop
  releases it (idempotent); Go test covers release and reuse.
  Both done 2026-10-14, batch 1.

## SP1 (Done 2026-10-11)

- **SP1** · The host's `pg-backup.sh` predated ba6c421's double-ping
  guard. Done 2026-10-11: phase-b's PgBackup step redeployed the repo
  copy (with the rotated Postgres password) and its test backup passed.

## SP4 (Done 2026-10-11)

- **SP4** · The addon's `.uid` files were untracked. Godot minted
  different script UIDs on Windows (CRLF) and Linux (LF) checkouts, so
  every Linux CI build lost hopnbop's Platform autoload. Done in da8394d
  (2026-10-11). hopnbop's CI bridge checks the autoload UID since
  78b1d311, bumped in a1a7753f.

## SP5 (Done 2026-10-11)

- **SP5** · The Nakama client ran at nakama-godot's default DEBUG level.
  It logged bearer and refresh tokens, the server key and device ids to
  stdout and `user://logs/godot.log` on players' machines. Done in
  e8ad312 (2026-10-11); the SDK bump in hopnbop is 50cc1087.

## SP6 (Done 2026-10-11)

- **SP6** · A failed session refresh was retried every frame for as long
  as the game ran, by real players' clients too
  (`auth_api_client.gd`, cooldown only after success). Done in 187912d
  (2026-10-11): backoff, and sign-out when Nakama rejects the token.

## SP7 (Done 2026-10-11)

- **SP7** · DR rebuild: phase-a/b wrote a partial `/opt/nakama/.env`
  (no Edgegap, signaling-domain or local-allocator entries, so
  matchmaking was off; SIGNALING_HMAC_SECRET was dropped). Done in
  97fc542 (2026-10-11), checked against prod's 27 values. The job scripts
  reach the host by DNS name since claude-config f4874d3.

## SP8 (Done 2026-10-11)

- **SP8** · cost-monitor ran on the Pages deploy token. A dedicated
  read-only `CLOUDFLARE_COST_MONITOR_TOKEN` is now deployed and
  documented. Done in 8c31f22 (2026-10-11).

## SP11 (Done 2026-10-11)

- **SP11** · CLAUDE.md's "No custom tables" was stale. Done in 6081b82
  (2026-10-11).
