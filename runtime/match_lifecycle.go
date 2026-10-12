package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/heroiclabs/nakama-common/runtime"
)

// matchLifecycle hosts RPCs called by the game-server containers.
// Both RPCs (`register_server`, `match_end`) are gated to
// server-to-server callers via requireServerToServer — they post
// authoritative state (server registration, leaderboard writes)
// that would let a malicious client tamper with match results
// otherwise. Game servers must call them with `?http_key=...`
// (the value of NAKAMA_HTTP_KEY on the Nakama host).
//
// The HTTP key shipped in client builds up to 0.47.0 (for the
// pre-auth `version_check`), so treat it as public: server-to-
// server gating alone proves nothing. These RPCs add their own
// integrity checks below to keep forged calls from polluting
// state. match_end only records results for a request_id the
// allocator itself recorded in match_metadata, and only for the
// users it matched into that request.
//
// `edgegap` is optional: it's nil when the matchmaker hook is
// disabled (no EDGEGAP_TOKEN). When set, MatchEndRpc uses it to
// terminate the Edgegap deployment so we stop paying for the
// container the moment the match ends, rather than waiting for
// Edgegap's 24h app-version-level max_duration to fire.
//
// `local` is optional: nil when no game's allocator_mode is
// "local" or "hybrid". When set, MatchEndRpc / MatchCancelRpc
// dispatch to it for matches whose match_metadata records
// AllocatorKind = "local".
type matchLifecycle struct {
	edgegap *edgegapClient
	local   *LocalDockerAllocator
}

// stopMatchByKind dispatches Stop to the right backend based on
// the persisted AllocatorKind. Empty / unrecognised values fall
// through to the historical edgegap path so pre-multi-backend
// metadata (or a metadata read that failed) still tears down
// existing Edgegap allocations the way it always has.
func (m *matchLifecycle) stopMatchByKind(
	ctx context.Context,
	kind string,
	requestID string,
) error {
	switch kind {
	case allocatorModeLocal:
		if m.local == nil {
			return errors.New(
				"local allocator stop called but matchLifecycle.local is nil")
		}
		return m.local.Stop(ctx, requestID)
	default:
		// "edgegap" or empty.
		if m.edgegap == nil {
			return nil
		}
		return m.edgegap.Stop(ctx, requestID)
	}
}

// gameScopedLeaderboardID returns the leaderboard ID for a given
// game's logical board. Stage 3.6 prefixes the bare logical name
// (e.g. "ffa") with the game_id so two games on one Nakama
// instance can both have an "ffa" board without colliding. Empty
// gameID returns the legacy bare name; the leaderboard.delete
// cascade scrubs both prefixes during the rollout window.
func gameScopedLeaderboardID(gameID, boardID string) string {
	if gameID == "" {
		return boardID
	}
	return gameID + "_" + boardID
}

// Reasonable upper bounds on per-player stats. A genuine match
// produces low-double-digit numbers; anything beyond this is
// either client tampering or a bug in our scoring code, and we
// don't want either polluting the leaderboard.
const (
	maxScore = 100000
	maxKills = 1000
	maxBumps = 1000
)

type registerServerArgs struct {
	RequestID  string `json:"request_id"`
	ServerIP   string `json:"server_ip"`
	ServerPort int    `json:"server_port"`
	ServerFqdn string `json:"server_fqdn"`
}

// RegisterServerRpc is called by the game server after it boots
// inside an Edgegap deployment. The server provides its
// connection details so Nakama can correlate the matchmaking
// request with the live server.
//
// Idempotent: repeat calls for the same request_id with the
// same connection info return ok without writing. A repeat call
// with DIFFERENT info is treated as a tamper attempt, logged,
// and rejected — once a real Edgegap deployment registers, no
// other caller can overwrite that entry.
func (m *matchLifecycle) RegisterServerRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	if err := requireServerToServer(ctx); err != nil {
		return "", err
	}
	args := registerServerArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError("invalid payload: "+err.Error(), 3)
	}
	if args.RequestID == "" {
		return "", runtime.NewError("request_id required", 3)
	}

	// Read existing registration so we can decide between
	// idempotent re-call and tamper attempt.
	existing, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: "server_registrations",
		Key:        args.RequestID,
	}})
	if err != nil {
		logger.Error("storage read: %v", err)
		return "", err
	}
	if len(existing) > 0 {
		var prev registerServerArgs
		if err := json.Unmarshal(
			[]byte(existing[0].Value), &prev); err != nil {
			logger.Warn(
				"existing registration for %s is unreadable; treating as fresh: %v",
				args.RequestID, err)
		} else if prev.ServerIP != args.ServerIP ||
			prev.ServerPort != args.ServerPort ||
			prev.ServerFqdn != args.ServerFqdn {
			logger.Warn(
				"register_server tamper attempt: request_id=%s prev_ip=%s:%d new_ip=%s:%d (rejected)",
				args.RequestID, prev.ServerIP, prev.ServerPort,
				args.ServerIP, args.ServerPort)
			return "", runtime.NewError(
				"request_id already registered with different connection info",
				6)
		} else {
			logger.Info(
				"server re-register (idempotent): request_id=%s",
				args.RequestID)
			resp, _ := json.Marshal(
				map[string]any{"ok": true, "idempotent": true})
			return string(resp), nil
		}
	}

	logger.Info(
		"server registered: request_id=%s ip=%s:%d",
		args.RequestID, args.ServerIP, args.ServerPort)
	value, _ := json.Marshal(args)
	_, err = nk.StorageWrite(ctx, []*runtime.StorageWrite{{
		Collection:      "server_registrations",
		Key:             args.RequestID,
		Value:           string(value),
		PermissionRead:  2,
		PermissionWrite: 0, // server-only writes; runtime overrides.
	}})
	if err != nil {
		logger.Error("storage write: %v", err)
		return "", err
	}

	resp, _ := json.Marshal(map[string]any{"ok": true})
	return string(resp), nil
}

type matchEndArgs struct {
	RequestID string           `json:"request_id"`
	WinnerID  string           `json:"winner_id"`
	Players   []matchEndPlayer `json:"players"`
	Stats     map[string]any   `json:"stats,omitempty"`
}

type matchEndPlayer struct {
	UserID string `json:"user_id"`
	Score  int    `json:"score"`
	Kills  int    `json:"kills"`
	Bumps  int    `json:"bumps"`
}

// clampPlayerStats coerces score/kills/bumps to the [0, max*]
// range in place. Extracted from MatchEndRpc so the bounding rule
// is unit-testable without a Nakama runtime mock.
func clampPlayerStats(p *matchEndPlayer) {
	if p.Score < 0 {
		p.Score = 0
	} else if p.Score > maxScore {
		p.Score = maxScore
	}
	if p.Kills < 0 {
		p.Kills = 0
	} else if p.Kills > maxKills {
		p.Kills = maxKills
	}
	if p.Bumps < 0 {
		p.Bumps = 0
	} else if p.Bumps > maxBumps {
		p.Bumps = maxBumps
	}
}

// lookupMatchMetadata returns the per-match metadata
// fleet_allocator stashed when the deploy was created, and whether
// a row exists. A storage error comes back as err so the caller can
// fail retryably. A row that exists but won't decode counts as
// found with zero-value metadata: only the runtime writes these
// rows, so a bad one is a bug to log, not a forgery.
func lookupMatchMetadata(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	requestID string,
) (matchMetadata, bool, error) {
	rows, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: matchMetadataCollection,
		Key:        requestID,
	}})
	if err != nil {
		return matchMetadata{}, false, err
	}
	if len(rows) == 0 {
		return matchMetadata{}, false, nil
	}
	out := matchMetadata{}
	if err := json.Unmarshal(
		[]byte(rows[0].Value), &out); err != nil {
		logger.Warn(
			"match metadata decode for %s: %v",
			requestID, err)
		return matchMetadata{}, true, nil
	}
	return out, true, nil
}

// readMatchMetadata is the best-effort form of lookupMatchMetadata
// for teardown paths. No row, or an unreadable one, returns the
// zero value: the caller falls back to legacy behavior (stop via
// Edgegap, etc.).
func readMatchMetadata(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	requestID string,
) matchMetadata {
	out, _, err := lookupMatchMetadata(ctx, logger, nk, requestID)
	if err != nil {
		logger.Warn(
			"match metadata read for %s: %v",
			requestID, err)
		return matchMetadata{}
	}
	return out
}

// restrictToMatchedPlayers drops match_end entries for users the
// allocator didn't match into the request, and returns the dropped
// user IDs. Entries with an empty user_id are kept: the writes skip
// them anyway, exactly as before this check existed.
//
// Dropping rather than rejecting the whole call matters because the
// user IDs come from each client's own declaration to the game
// server. One client claiming someone else's ID shouldn't void the
// match for everyone.
func restrictToMatchedPlayers(
	players []matchEndPlayer,
	matchedUserIDs []string,
) ([]matchEndPlayer, []string) {
	matched := make(map[string]bool, len(matchedUserIDs))
	for _, id := range matchedUserIDs {
		matched[id] = true
	}
	kept := make([]matchEndPlayer, 0, len(players))
	var dropped []string
	for _, p := range players {
		if p.UserID == "" || matched[p.UserID] {
			kept = append(kept, p)
			continue
		}
		dropped = append(dropped, p.UserID)
	}
	return kept, dropped
}

// hasReportedUser reports whether any entry names a user.
func hasReportedUser(players []matchEndPlayer) bool {
	for _, p := range players {
		if p.UserID != "" {
			return true
		}
	}
	return false
}

// includesUser reports whether userID has an entry in players.
func includesUser(players []matchEndPlayer, userID string) bool {
	for _, p := range players {
		if p.UserID == userID {
			return true
		}
	}
	return false
}

// isSyntheticMatch returns true when fleet_allocator persisted a
// synthetic-match marker for the given request_id. Used by
// match_end to skip leaderboard/history writes and by
// match_cancel to admit client-initiated cancellation.
func isSyntheticMatch(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	requestID string,
) bool {
	rows, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: syntheticMatchCollection,
		Key:        requestID,
	}})
	if err != nil {
		// Treat read failures as "not synthetic" — a Postgres
		// blip on the cancel path shouldn't pollute the
		// leaderboard, but we'd rather over-count than over-
		// trust an unreadable row.
		logger.Warn(
			"synthetic-match read for %s: %v",
			requestID, err)
		return false
	}
	return len(rows) > 0
}

// MatchEndRpc is called by the game server when a match ends. We
// post the result to the leaderboard and clean up the registration
// entry.
//
// Hardening:
//   - The request_id MUST correspond to an active server
//     registration; otherwise the call is rejected.
//   - It MUST also have a match_metadata row, which only the
//     allocator writes. register_server takes any request_id
//     from anyone holding the HTTP key, so the registration alone
//     doesn't prove a real allocation; the metadata row does. A
//     request_id without one is rejected without side effects.
//   - Results are recorded only for users the matchmaker matched
//     into this request (metadata.UserIDs). Other entries are
//     dropped; a call that names only outsiders is rejected
//     without side effects, so the real server's match_end still
//     goes through afterwards.
//   - winner_id, when non-empty, must be one of the players. A
//     winner who was dropped above is cleared.
//   - Per-player score/kills/bumps are bounded so a tampered
//     payload can't flood the leaderboard with int64-max scores.
//   - Storage delete of the registration is what makes match_end
//     dedup itself: a second call for the same request_id finds
//     no registration and is rejected.
//   - Synthetic-probe matches (request_id in the
//     synthetic_matches collection) skip leaderboard/history
//     writes so the synthetic-match-probe job doesn't pollute
//     real player stats.
//
// What remains: a player in a match learns its request_id from
// match_ready, so with the HTTP key they can still report forged
// scores (within the bounds) for that one match's players, or end
// it early. Closing that needs a per-deployment secret that the
// allocator hands the game server and match_end checks.
func (m *matchLifecycle) MatchEndRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	if err := requireServerToServer(ctx); err != nil {
		return "", err
	}
	args := matchEndArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError("invalid payload: "+err.Error(), 3)
	}
	if args.RequestID == "" || len(args.Players) == 0 {
		return "", runtime.NewError("request_id and players required", 3)
	}

	// Verify the request_id is a registered server.
	regs, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: "server_registrations",
		Key:        args.RequestID,
	}})
	if err != nil {
		logger.Error("server_registrations read: %v", err)
		return "", err
	}
	if len(regs) == 0 {
		logger.Warn(
			"match_end rejected: request_id=%s has no registration"+
				" (forged call, duplicate, or missed register_server)",
			args.RequestID)
		return "", runtime.NewError(
			"unknown request_id (no matching server registration)",
			5)
	}

	// Verify the allocator recorded this request_id. A read error
	// is INTERNAL (HTTP 500), which the game server retries once.
	metadata, allocated, err := lookupMatchMetadata(
		ctx, logger, nk, args.RequestID)
	if err != nil {
		logger.Error(
			"match_end: match metadata read for %s: %v",
			args.RequestID, err)
		return "", runtime.NewError("match metadata read failed", 13)
	}
	if !allocated {
		logger.Warn(
			"match_end rejected: request_id=%s has a server"+
				" registration but no allocation record (a forged"+
				" register_server, or the allocator's metadata"+
				" write failed)",
			args.RequestID)
		return "", runtime.NewError(
			"unknown request_id (no allocation record)", 5)
	}

	// Validate winner_id and bound per-player stats.
	playerIDs := make(map[string]bool, len(args.Players))
	for _, p := range args.Players {
		if p.UserID != "" {
			playerIDs[p.UserID] = true
		}
	}
	if args.WinnerID != "" && !playerIDs[args.WinnerID] {
		logger.Warn(
			"match_end rejected: winner_id=%s not in players",
			args.WinnerID)
		return "", runtime.NewError(
			"winner_id must be one of the reported players", 3)
	}
	// Keep only users the matchmaker put in this match. Metadata
	// written before UserIDs existed skips the check; those
	// matches end within the deploy's max duration.
	if len(metadata.UserIDs) > 0 {
		kept, dropped := restrictToMatchedPlayers(
			args.Players, metadata.UserIDs)
		if len(dropped) > 0 {
			logger.Warn(
				"match_end: request_id=%s dropped %d player(s) not"+
					" matched into it: %v",
				args.RequestID, len(dropped), dropped)
			if !hasReportedUser(kept) {
				return "", runtime.NewError(
					"no reported player was matched into this"+
						" request", 3)
			}
			args.Players = kept
			if args.WinnerID != "" &&
				!includesUser(kept, args.WinnerID) {
				logger.Warn(
					"match_end: request_id=%s winner %s was not"+
						" matched into it; recording no winner",
					args.RequestID, args.WinnerID)
				args.WinnerID = ""
			}
		}
	}
	for i := range args.Players {
		p := &args.Players[i]
		origScore := p.Score
		clampPlayerStats(p)
		if origScore != p.Score {
			logger.Warn(
				"match_end: clamped score=%d→%d for user=%s",
				origScore, p.Score, p.UserID)
		}
	}

	logger.Info(
		"match ended: request_id=%s winner=%s players=%d",
		args.RequestID, args.WinnerID, len(args.Players))

	synthetic := isSyntheticMatch(ctx, logger, nk, args.RequestID)
	if synthetic {
		logger.Info(
			"match %s is synthetic; skipping leaderboard + history writes",
			args.RequestID)
	}

	// Per-match metadata gives us the game_id fleet_allocator
	// observed at allocation time. A match whose players cast no
	// game_id vote (pre-Stage-3.10 clients) has an empty one and
	// falls back to the bare leaderboard ID.
	leaderboardID := gameScopedLeaderboardID(metadata.GameID, "ffa")

	// Write leaderboard records and a per-user match_history row
	// in one pass. The history row is what get_match_history
	// reads back; the leaderboard record is what
	// get_player_stats / the global FFA board read. Skipped for
	// synthetic probe matches.
	if !synthetic {
		endedAt := nowUnix()
		historyWrites := make([]*runtime.StorageWrite, 0, len(args.Players))
		for _, p := range args.Players {
			if p.UserID == "" {
				continue
			}
			_, err := nk.LeaderboardRecordWrite(ctx,
				leaderboardID,
				p.UserID,
				"",
				int64(p.Score),
				0,
				map[string]any{
					"kills": p.Kills,
					"bumps": p.Bumps,
				},
				nil)
			if err != nil {
				logger.Warn("leaderboard write for %s: %v", p.UserID, err)
			}

			entry := map[string]any{
				"match_id":  args.RequestID,
				"ended_at":  endedAt,
				"is_winner": p.UserID == args.WinnerID,
				"score":     p.Score,
				"kills":     p.Kills,
				"bumps":     p.Bumps,
			}
			value, _ := json.Marshal(entry)
			historyWrites = append(historyWrites, &runtime.StorageWrite{
				Collection:      "match_history",
				Key:             args.RequestID,
				UserID:          p.UserID,
				Value:           string(value),
				PermissionRead:  1, // owner-only.
				PermissionWrite: 0, // server-only writes.
			})
		}
		if len(historyWrites) > 0 {
			if _, err := nk.StorageWrite(ctx, historyWrites); err != nil {
				logger.Warn("match_history write: %v", err)
			}
		}
	}

	// Stage 7.6 recent-players bookkeeping. Records a row for
	// every (owner, other) pair so each matched player can
	// surface the others in a "Recent Players" UI and add them
	// as friends post-match. Skipped for solo matches (no pairs)
	// inside the helper. Runs even on synthetic matches — the
	// synthetic-probe job is single-player so the helper still
	// short-circuits, but a future synthetic multi-player flow
	// would record real recent-players entries. That's fine:
	// recent-players is per-owner, opt-in via the UI, and
	// covered by the GDPR cascade (Stage 7.7) like every other
	// user-owned storage row.
	writeRecentPlayersForMatch(
		ctx, logger, nk, args.Players)

	// Delete the registration record. This is also what makes
	// match_end self-dedup: a second call for the same
	// request_id finds the registration gone and is rejected
	// at the read step above. Bundle the match-metadata row in
	// the same call so we don't leave per-match garbage in
	// storage after the match resolves.
	cleanupDeletes := []*runtime.StorageDelete{
		{
			Collection: "server_registrations",
			Key:        args.RequestID,
		},
		{
			Collection: matchMetadataCollection,
			Key:        args.RequestID,
		},
	}
	if synthetic {
		cleanupDeletes = append(cleanupDeletes,
			&runtime.StorageDelete{
				Collection: syntheticMatchCollection,
				Key:        args.RequestID,
			})
	}
	if err := nk.StorageDelete(ctx, cleanupDeletes); err != nil {
		// Non-fatal: maybe registration was never written.
		logger.Warn("storage delete: %v", err)
	}

	// Terminate the game-server so the container stops billing
	// (Edgegap) or releases its port pair (local) immediately.
	// Without this, Edgegap deploys linger until the per-app-
	// version `max_duration` cap (24h) hits — quiet days rack
	// up tens of container-hours of idle cost. Non-fatal: 404
	// means already-stopped; other errors get logged and the
	// cost-monitor's EDGEGAP_ACTIVE_HARD threshold + the
	// edgegap-leak-sweep hourly job are the safety nets.
	if err := m.stopMatchByKind(
		ctx, metadata.AllocatorKind, args.RequestID); err != nil {
		logger.Warn(
			"%s stop failed for %s: %v",
			metadata.AllocatorKind, args.RequestID, err)
	} else if metadata.AllocatorKind != "" {
		logger.Info(
			"%s deployment %s terminated",
			metadata.AllocatorKind, args.RequestID)
	}
	recordAllocationEnd(nk, metadata.AllocatorKind)

	resp, _ := json.Marshal(map[string]any{"ok": true})
	return string(resp), nil
}

type matchCancelArgs struct {
	RequestID string `json:"request_id"`
	Reason    string `json:"reason,omitempty"`
}

// MatchCancelRpc is called by the game server when it's bailing
// out of an Edgegap deployment without producing match results
// (idle timeout with no clients connected, grace timeout with
// only one peer, mid-match all-clients-dropped). It deletes the
// registration and terminates the Edgegap deployment so the
// container stops billing immediately, mirroring MatchEndRpc's
// cleanup half. The difference: no leaderboard / match_history
// writes (there's nothing to record).
//
// Auth model:
//   - Server-to-server (no RUNTIME_CTX_USER_ID) is the normal
//     path: the in-container Godot calls this when its idle/grace
//     timer fires.
//   - Client sessions are admitted ONLY for synthetic-probe
//     matches (a `synthetic_matches` storage row exists for the
//     request_id). The synthetic-match-probe job uses this to
//     tear down its Edgegap deploy promptly after DataChannel-
//     open instead of waiting on the in-container idle timer.
//     Real client tampering is contained: a forger can only
//     cancel matches that the runtime itself flagged synthetic,
//     and synthetic matches don't write to the leaderboard.
//
// Other hardening (same anti-forgery model as MatchEndRpc): the
// request_id MUST correspond to an active server registration.
// A forger would have to first call register_server (also
// hardened) for a request_id Edgegap actually allocated.
//
// Idempotent the same way MatchEndRpc is — the storage delete
// is what dedups: a second call finds no registration and is
// returned as a silent no-op.
func (m *matchLifecycle) MatchCancelRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	args := matchCancelArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError("invalid payload: "+err.Error(), 3)
	}
	if args.RequestID == "" {
		return "", runtime.NewError("request_id required", 3)
	}

	// Verify the request_id is a registered server. Done before
	// the auth check so an already-cancelled match returns a
	// silent idempotent no-op regardless of caller — matters for
	// the synthetic-probe flow where two probes race to call
	// match_cancel; the second one finds the registration (and
	// the synthetic marker) already deleted by the first.
	regs, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: "server_registrations",
		Key:        args.RequestID,
	}})
	if err != nil {
		logger.Error("server_registrations read: %v", err)
		return "", err
	}
	if len(regs) == 0 {
		logger.Info(
			"match_cancel no-op: request_id=%s already cleaned up",
			args.RequestID)
		resp, _ := json.Marshal(
			map[string]any{"ok": true, "noop": true})
		return string(resp), nil
	}

	// Server-to-server callers always pass; client callers must
	// be cancelling a match that fleet_allocator flagged synthetic.
	userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	synthetic := isSyntheticMatch(ctx, logger, nk, args.RequestID)
	if userID != "" && !synthetic {
		return "", runtime.NewError(
			"forbidden: client cancel only allowed for synthetic matches",
			7)
	}

	logger.Info(
		"match cancelled: request_id=%s reason=%s synthetic=%t",
		args.RequestID, args.Reason, synthetic)

	cancelDeletes := []*runtime.StorageDelete{
		{
			Collection: "server_registrations",
			Key:        args.RequestID,
		},
		{
			Collection: matchMetadataCollection,
			Key:        args.RequestID,
		},
	}
	if synthetic {
		cancelDeletes = append(cancelDeletes,
			&runtime.StorageDelete{
				Collection: syntheticMatchCollection,
				Key:        args.RequestID,
			})
	}
	// Read the metadata BEFORE the delete so we know which
	// backend allocated this match. Missing metadata (pre-multi-
	// backend matches, or a metadata read that failed) falls
	// through stopMatchByKind's default to edgegap.
	cancelMetadata := readMatchMetadata(
		ctx, logger, nk, args.RequestID)
	if err := nk.StorageDelete(ctx, cancelDeletes); err != nil {
		logger.Warn("storage delete: %v", err)
	}

	if err := m.stopMatchByKind(
		ctx, cancelMetadata.AllocatorKind, args.RequestID); err != nil {
		logger.Warn(
			"%s stop failed for %s: %v",
			cancelMetadata.AllocatorKind, args.RequestID, err)
	} else if cancelMetadata.AllocatorKind != "" {
		logger.Info(
			"%s deployment %s terminated (cancelled)",
			cancelMetadata.AllocatorKind, args.RequestID)
	}
	recordAllocationEnd(nk, cancelMetadata.AllocatorKind)

	resp, _ := json.Marshal(map[string]any{"ok": true})
	return string(resp), nil
}

// Static check at package load.
var _ = errors.New
