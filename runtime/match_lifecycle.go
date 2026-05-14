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
// As of the gamelift-removal cleanup, NAKAMA_HTTP_KEY is also
// embedded in client builds (so pre-auth `version_check` can
// run without a session). That makes server-to-server gating
// alone insufficient — these RPCs add their own integrity
// checks below to prevent forged calls from polluting state.
//
// `edgegap` is optional: it's nil when the matchmaker hook is
// disabled (no EDGEGAP_TOKEN). When set, MatchEndRpc uses it to
// terminate the Edgegap deployment so we stop paying for the
// container the moment the match ends, rather than waiting for
// Edgegap's 24h app-version-level max_duration to fire.
type matchLifecycle struct {
	edgegap *edgegapClient
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

// readMatchMetadata returns the per-match metadata fleet_allocator
// stashed when the deploy was created. Empty metadata (no row, or
// unreadable row) returns the zero value with no error — the caller
// falls back to legacy behavior (bare leaderboard ID, etc.).
func readMatchMetadata(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	requestID string,
) matchMetadata {
	out := matchMetadata{}
	rows, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: matchMetadataCollection,
		Key:        requestID,
	}})
	if err != nil {
		logger.Warn(
			"match metadata read for %s: %v",
			requestID, err)
		return out
	}
	if len(rows) == 0 {
		return out
	}
	if err := json.Unmarshal(
		[]byte(rows[0].Value), &out); err != nil {
		logger.Warn(
			"match metadata decode for %s: %v",
			requestID, err)
		return matchMetadata{}
	}
	return out
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
//     registration; otherwise the call is rejected. This forces
//     a forger to first call register_server (also hardened) for
//     a request_id Edgegap actually allocated, which they don't
//     control.
//   - winner_id, when non-empty, must be one of the players.
//   - Per-player score/kills/bumps are bounded so a tampered
//     payload can't flood the leaderboard with int64-max scores.
//   - Storage delete of the registration is what makes match_end
//     dedup itself: a second call for the same request_id finds
//     no registration and is rejected.
//   - Synthetic-probe matches (request_id in the
//     synthetic_matches collection) skip leaderboard/history
//     writes so the synthetic-match-probe job doesn't pollute
//     real player stats.
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
	// observed at allocation time. Missing metadata (pre-Stage-3.6
	// match, or a metadata write that lost to a Postgres blip)
	// falls back to a bare leaderboard ID so the match still
	// records — better to write to the legacy board than to drop
	// a real player's result.
	metadata := readMatchMetadata(
		ctx, logger, nk, args.RequestID)
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

	// Terminate the Edgegap deployment so the container stops
	// billing immediately. Without this, the container lingers
	// until Edgegap's per-app-version `max_duration` cap (24h)
	// hits — which on a quiet day racks up tens of container-
	// hours of idle cost. Non-fatal: 404 means Edgegap already
	// terminated it (e.g. crash exit), other errors get logged
	// and the cost-monitor's EDGEGAP_ACTIVE_HARD threshold is
	// the safety net for accumulating leaks.
	if m.edgegap != nil {
		if err := m.edgegap.Stop(ctx, args.RequestID); err != nil {
			logger.Warn(
				"edgegap stop failed for %s: %v",
				args.RequestID, err)
		} else {
			logger.Info(
				"edgegap deployment %s terminated",
				args.RequestID)
		}
	}

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
	if err := nk.StorageDelete(ctx, cancelDeletes); err != nil {
		logger.Warn("storage delete: %v", err)
	}

	if m.edgegap != nil {
		if err := m.edgegap.Stop(ctx, args.RequestID); err != nil {
			logger.Warn(
				"edgegap stop failed for %s: %v",
				args.RequestID, err)
		} else {
			logger.Info(
				"edgegap deployment %s terminated (cancelled)",
				args.RequestID)
		}
	}

	resp, _ := json.Marshal(map[string]any{"ok": true})
	return string(resp), nil
}

// Static check at package load.
var _ = errors.New
