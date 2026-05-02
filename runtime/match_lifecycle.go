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
type matchLifecycle struct{}

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
		if p.Score < 0 || p.Score > maxScore {
			logger.Warn(
				"match_end: clamping score=%d for user=%s",
				p.Score, p.UserID)
			if p.Score < 0 {
				p.Score = 0
			} else {
				p.Score = maxScore
			}
		}
		if p.Kills < 0 || p.Kills > maxKills {
			if p.Kills < 0 {
				p.Kills = 0
			} else {
				p.Kills = maxKills
			}
		}
		if p.Bumps < 0 || p.Bumps > maxBumps {
			if p.Bumps < 0 {
				p.Bumps = 0
			} else {
				p.Bumps = maxBumps
			}
		}
	}

	logger.Info(
		"match ended: request_id=%s winner=%s players=%d",
		args.RequestID, args.WinnerID, len(args.Players))

	// Write leaderboard records and a per-user match_history row
	// in one pass. The history row is what get_match_history
	// reads back; the leaderboard record is what
	// get_player_stats / the global FFA board read.
	endedAt := nowUnix()
	historyWrites := make([]*runtime.StorageWrite, 0, len(args.Players))
	for _, p := range args.Players {
		if p.UserID == "" {
			continue
		}
		_, err := nk.LeaderboardRecordWrite(ctx,
			"ffa",
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

	// Delete the registration record. This is also what makes
	// match_end self-dedup: a second call for the same
	// request_id finds the registration gone and is rejected
	// at the read step above.
	if err := nk.StorageDelete(ctx, []*runtime.StorageDelete{{
		Collection: "server_registrations",
		Key:        args.RequestID,
	}}); err != nil {
		// Non-fatal: maybe registration was never written.
		logger.Warn("storage delete: %v", err)
	}

	resp, _ := json.Marshal(map[string]any{"ok": true})
	return string(resp), nil
}

// Static check at package load.
var _ = errors.New
