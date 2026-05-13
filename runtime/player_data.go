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
//   - export_player_data:  GDPR data export

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

// ---------------------------------------------------------------
// export_player_data (GDPR)
// ---------------------------------------------------------------

type exportResponse struct {
	GeneratedAt        string                   `json:"generated_at"`
	Account            map[string]any           `json:"account"`
	StorageObjects     []map[string]any         `json:"storage_objects"`
	LeaderboardRecords []map[string]any         `json:"leaderboard_records"`
	Friends            []map[string]any         `json:"friends"`
}

// exportPlayerDataRpcFactory threads the per-game config store
// through so the handler can enforce game_id on the session.
// Export is currently cross-game (lists every storage object the
// user owns); the game_id check is a session-identity assertion
// only, not a per-game filter.
func exportPlayerDataRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		db *sql.DB,
		nk runtime.NakamaModule,
		payload string,
	) (string, error) {
		return exportPlayerDataRpc(
			ctx, logger, db, nk, games, payload)
	}
}

// exportPlayerDataRpc returns the calling user's account,
// storage objects, leaderboard records, and friend list — the
// data needed to satisfy a GDPR data export request. Limited to
// the caller's own data (server-to-server callers should use the
// admin export endpoint, not this RPC).
func exportPlayerDataRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	games *perGameConfig,
	_ string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	gameID, err := requireGameID(ctx, games)
	if err != nil {
		return "", err
	}

	resp := exportResponse{
		GeneratedAt:        time.Now().UTC().Format(time.RFC3339),
		StorageObjects:     []map[string]any{},
		LeaderboardRecords: []map[string]any{},
		Friends:            []map[string]any{},
	}

	// Account (display name, avatar, linked providers, etc.).
	if account, err := nk.AccountGetId(ctx, userID); err == nil && account != nil {
		resp.Account = map[string]any{
			"user_id":      userID,
			"display_name": account.User.DisplayName,
			"username":     account.User.Username,
			"avatar_url":   account.User.AvatarUrl,
			"lang_tag":     account.User.LangTag,
			"location":     account.User.Location,
			"timezone":     account.User.Timezone,
			"created_at":   account.User.CreateTime.GetSeconds(),
		}
	} else if err != nil {
		logger.Warn("account read for export: %v", err)
	}

	// All storage objects owned by this user across all
	// collections. Direct SQL rather than nk.StorageList because
	// the latter requires a non-empty collection filter (its SQL
	// has WHERE collection = $1); passing "" silently returns
	// zero rows. See the matching note in account.go's cascade
	// for the longer rationale. Capped at 1000 rows so a
	// pathological-export user can't block Nakama. Order keeps
	// the response stable across re-runs for diffability.
	if db != nil {
		// `user_id` is a UUID column; explicit ::uuid cast for the
		// same type-safety reasons as account.go's scrub.
		rows, qErr := db.QueryContext(
			ctx,
			`SELECT collection, key, value, create_time, update_time
			 FROM storage
			 WHERE user_id = $1::uuid
			 ORDER BY collection, key
			 LIMIT 1000`,
			userID,
		)
		if qErr != nil {
			logger.Warn(
				"storage list for export: %v", qErr)
		} else {
			defer rows.Close()
			for rows.Next() {
				var (
					collection string
					key        string
					value      string
					createTime time.Time
					updateTime time.Time
				)
				if scanErr := rows.Scan(
					&collection, &key, &value,
					&createTime, &updateTime); scanErr != nil {
					logger.Warn(
						"storage row scan for export: %v",
						scanErr)
					continue
				}
				var v any
				if jErr := json.Unmarshal(
					[]byte(value), &v); jErr != nil {
					v = value
				}
				resp.StorageObjects = append(
					resp.StorageObjects,
					map[string]any{
						"collection": collection,
						"key":        key,
						"value":      v,
						"created_at": createTime.Unix(),
						"updated_at": updateTime.Unix(),
					})
			}
			if rowsErr := rows.Err(); rowsErr != nil {
				logger.Warn(
					"storage rows iter for export: %v",
					rowsErr)
			}
		}
	}

	// Friends list with state for each.
	if friends, _, err := nk.FriendsList(
		ctx, userID, 100, nil, ""); err == nil {
		for _, f := range friends {
			if f.User == nil {
				continue
			}
			state := int32(-1)
			if f.State != nil {
				state = f.State.Value
			}
			resp.Friends = append(resp.Friends, map[string]any{
				"user_id":      f.User.Id,
				"display_name": f.User.DisplayName,
				"state":        state,
			})
		}
	} else {
		logger.Warn("friends list for export: %v", err)
	}

	// FFA leaderboard record. Scoped to the caller's session
	// game_id (Stage 3.6); cross-game export is not currently
	// supported here — the caller only gets the leaderboard
	// record for the game they're signed into. Add more boards
	// here as we add them.
	leaderboardID := gameScopedLeaderboardID(gameID, "ffa")
	if records, _, _, _, err := nk.LeaderboardRecordsList(
		ctx, leaderboardID, []string{userID}, 1, "", 0); err == nil {
		for _, r := range records {
			resp.LeaderboardRecords = append(resp.LeaderboardRecords,
				map[string]any{
					"leaderboard_id": r.LeaderboardId,
					"score":          r.Score,
					"subscore":       r.Subscore,
					"num_score":      r.NumScore,
					"metadata":       r.Metadata,
				})
		}
	} else if err != nil {
		logger.Warn("leaderboard read for export: %v", err)
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
