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
type matchLifecycle struct{}

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
	logger.Info("server registered: request_id=%s ip=%s:%d", args.RequestID, args.ServerIP, args.ServerPort)

	// Persist registration in Nakama Storage so it can be looked
	// up. Collection: server_registrations, key: request_id.
	value, _ := json.Marshal(args)
	_, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
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
	RequestID  string                 `json:"request_id"`
	WinnerID   string                 `json:"winner_id"`
	Players    []matchEndPlayer       `json:"players"`
	Stats      map[string]any         `json:"stats,omitempty"`
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
	logger.Info("match ended: request_id=%s winner=%s players=%d", args.RequestID, args.WinnerID, len(args.Players))

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

	// Delete the registration record.
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
