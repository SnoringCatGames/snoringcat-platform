package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Player-scoped read RPCs:
//   - get_player_stats:    rating + match count for one player
//   - get_match_history:   recent matches for the caller
//
// The GDPR data export (export_player_data) is in data_export.go.

// ---------------------------------------------------------------
// get_player_stats
// ---------------------------------------------------------------

type playerStatsArgs struct {
	PlayerID string `json:"player_id"`
}

type playerStatsResponse struct {
	PlayerID string `json:"player_id"`
	Rating   int64  `json:"rating"`
	Matches  int    `json:"matches"`
}

// getPlayerStatsRpcFactory threads the per-game config store
// through to the handler so the caller's session can be
// validated against the registered game_id and the leaderboard
// ID gets per-game scoping (Stage 3.6).
func getPlayerStatsRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		_ *sql.DB,
		nk runtime.NakamaModule,
		payload string,
	) (string, error) {
		return getPlayerStatsRpc(ctx, logger, nk, games, payload)
	}
}

// getPlayerStatsRpc returns rating + match count for the
// requested player. Rating is sourced from the `ffa` leaderboard
// (default 1500 for unranked players); match count is derived
// from the leaderboard record's NumScore (Nakama increments it
// on every submission).
func getPlayerStatsRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	caller, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	gameID, err := requireGameID(ctx, games)
	if err != nil {
		return "", err
	}
	args := playerStatsArgs{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return "", runtime.NewError(
				"invalid payload: "+err.Error(), 3)
		}
	}
	target := args.PlayerID
	if target == "" {
		target = caller
	}

	resp := playerStatsResponse{
		PlayerID: target,
		Rating:   1500,
		Matches:  0,
	}

	// LeaderboardRecordsList with ownerIds filters the listing to
	// just that owner's record (or none if unranked). The board
	// ID is scoped per game (Stage 3.6) so hopnbop's "ffa" board
	// doesn't collide with another game's same-named board.
	leaderboardID := gameScopedLeaderboardID(gameID, "ffa")
	records, _, _, _, err := nk.LeaderboardRecordsList(
		ctx, leaderboardID, []string{target}, 1, "", 0)
	if err != nil {
		// Leaderboard not yet created or transient error: return
		// the unranked defaults rather than 500ing.
		logger.Warn("leaderboard read for %s: %v", target, err)
		out, _ := json.Marshal(resp)
		return string(out), nil
	}
	if len(records) > 0 {
		r := records[0]
		resp.Rating = r.Score
		resp.Matches = int(r.NumScore)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---------------------------------------------------------------
// get_match_history
// ---------------------------------------------------------------

type matchHistoryEntry struct {
	MatchID    string         `json:"match_id"`
	EndedAt    int64          `json:"ended_at"`
	IsWinner   bool           `json:"is_winner"`
	Score      int            `json:"score"`
	Kills      int            `json:"kills"`
	Bumps      int            `json:"bumps"`
	OtherStats map[string]any `json:"other_stats,omitempty"`
}

type matchHistoryResponse struct {
	Matches []matchHistoryEntry `json:"matches"`
}

// getMatchHistoryRpcFactory threads the per-game config store
// through so the handler can enforce game_id on the session.
func getMatchHistoryRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		_ *sql.DB,
		nk runtime.NakamaModule,
		payload string,
	) (string, error) {
		return getMatchHistoryRpc(ctx, logger, nk, games, payload)
	}
}

// getMatchHistoryRpc lists recent matches for the calling user.
// Records are written by match_lifecycle.MatchEndRpc into a
// per-user storage collection ("match_history"). Most-recent
// first; capped at 50 to keep the payload bounded.
func getMatchHistoryRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	_ string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}

	objects, _, err := nk.StorageList(
		ctx, "", userID, "match_history", 50, "")
	if err != nil {
		logger.Warn("match_history list: %v", err)
		// Empty rather than 500 — the client falls back to "no
		// matches" cleanly.
		out, _ := json.Marshal(matchHistoryResponse{
			Matches: []matchHistoryEntry{},
		})
		return string(out), nil
	}

	resp := matchHistoryResponse{
		Matches: make([]matchHistoryEntry, 0, len(objects)),
	}
	for _, obj := range objects {
		var entry matchHistoryEntry
		if err := json.Unmarshal([]byte(obj.Value), &entry); err != nil {
			continue
		}
		resp.Matches = append(resp.Matches, entry)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// nowUnix is a small indirection so unit tests can stub the
// clock if we add them later.
func nowUnix() int64 { return time.Now().Unix() }
