package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Player-scoped read RPCs:
//   - get_player_stats:    best score + public rank for one player
//   - get_match_history:   recent matches + totals for the caller
//
// The GDPR data export (export_player_data) is in data_export.go.

// ---------------------------------------------------------------
// get_player_stats
// ---------------------------------------------------------------

type playerStatsArgs struct {
	PlayerID string `json:"player_id"`
}

// unrankedRating is the deprecated `rating` value for a player with no
// record yet: an AWS-era Elo placeholder.
const unrankedRating = 1500

type playerStatsResponse struct {
	PlayerID string `json:"player_id"`
	// BestScore is the player's best single-match score on the game's
	// `ffa` board, or null before their first recorded match.
	BestScore *int64 `json:"best_score"`
	// Rank is the player's rank on the public leaderboard, the list
	// the website and the in-game leaderboard show, or null when they
	// aren't listed there (no recorded score, no display name, queued
	// for deletion, a probe account, or outside its top 100).
	Rank *int64 `json:"rank"`
	// Rating is deprecated: best_score, or 1500 before the first
	// recorded match. A best score is not a rating, and the 1500
	// placeholder outranks most real scores. Kept for older clients.
	Rating int64 `json:"rating"`
	// Matches is deprecated: Nakama's num_score, which on a "best"
	// board counts the times the best score was set, not the matches
	// played (get_match_history's totals.matches). Kept for older
	// clients.
	Matches int `json:"matches"`
}

// getPlayerStatsRpcFactory threads the per-game config store
// through to the handler so the caller's session can be
// validated against the registered game_id and the leaderboard
// ID gets per-game scoping (Stage 3.6). boards supplies the public
// rank; nil leaves rank null.
func getPlayerStatsRpcFactory(
	games *perGameConfig,
	boards *publicLeaderboardReader,
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
		return getPlayerStatsRpc(
			ctx, logger, nk, games, boards, payload)
	}
}

// getPlayerStatsRpc returns the requested player's standing on the
// game's `ffa` board: their best score from their own record, and
// their rank on the public leaderboard.
//
// Until 2026-10-12 it read the first return value of
// LeaderboardRecordsList, which is the board's top page, not the
// owner's record, so every player would have been given the leader's
// score. It never showed, because no board existed.
func getPlayerStatsRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	boards *publicLeaderboardReader,
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
		Rating:   unrankedRating,
		Matches:  0,
	}

	// The board ID is scoped per game (Stage 3.6) so hopnbop's "ffa"
	// board doesn't collide with another game's same-named board.
	leaderboardID := gameScopedLeaderboardID(gameID, "ffa")
	if record, err := ownerLeaderboardRecord(
		ctx, nk, leaderboardID, target); err != nil {
		// Leaderboard not yet created or transient error: return
		// the unranked defaults rather than 500ing.
		logger.Warn("leaderboard read for %s: %v", target, err)
	} else if record != nil {
		score := record.GetScore()
		resp.BestScore = &score
		resp.Rating = score
		resp.Matches = int(record.GetNumScore())
		// Only a player with a record can be listed, so only they
		// need the board read.
		if boards != nil {
			if rank, ok := boards.rankOf(leaderboardID, target); ok {
				resp.Rank = &rank
			}
		}
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ownerLeaderboardRecord returns userID's own record on a board, or
// nil when they have none. LeaderboardRecordsList's first return value
// is the board's top page whatever the owner filter; the owner's
// record comes back in the second, and a zero limit skips the page.
func ownerLeaderboardRecord(
	ctx context.Context,
	nk runtime.NakamaModule,
	leaderboardID string,
	userID string,
) (*api.LeaderboardRecord, error) {
	_, owned, _, _, err := nk.LeaderboardRecordsList(
		ctx, leaderboardID, []string{userID}, 0, "", 0)
	if err != nil {
		return nil, err
	}
	for _, record := range owned {
		if record.GetOwnerId() == userID {
			return record, nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------
// get_match_history
// ---------------------------------------------------------------

const (
	// matchHistoryCollection holds one row per player per recorded
	// match, keyed by the match's request id (match_lifecycle.go).
	matchHistoryCollection = "match_history"
	// matchHistoryLimit is how many matches get_match_history lists.
	matchHistoryLimit = 50
	// matchHistoryPageSize and matchHistoryMaxPages bound one read of
	// a player's history: 10,000 matches.
	matchHistoryPageSize = 100
	matchHistoryMaxPages = 100
)

type matchHistoryEntry struct {
	MatchID    string         `json:"match_id"`
	EndedAt    int64          `json:"ended_at"`
	IsWinner   bool           `json:"is_winner"`
	Score      int            `json:"score"`
	Kills      int            `json:"kills"`
	Bumps      int            `json:"bumps"`
	LevelID    string         `json:"level_id,omitempty"`
	OtherStats map[string]any `json:"other_stats,omitempty"`
}

// matchHistoryTotals sums every recorded match of the player, not
// just the listed ones.
type matchHistoryTotals struct {
	Matches int `json:"matches"`
	Wins    int `json:"wins"`
	Kills   int `json:"kills"`
	Bumps   int `json:"bumps"`
}

type matchHistoryResponse struct {
	Matches []matchHistoryEntry `json:"matches"`
	Totals  matchHistoryTotals  `json:"totals"`
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

// getMatchHistoryRpc lists the calling user's matches, newest first,
// at most matchHistoryLimit, with totals over all of them. Records
// are written by match_lifecycle.MatchEndRpc into a per-user storage
// collection ("match_history").
//
// Rows are keyed by request id, so storage order says nothing about
// time: until 2026-10-12 this returned the first 50 rows by key,
// which past 50 matches is not the latest 50.
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

	matches, totals, err := readMatchHistory(ctx, nk, userID)
	if err != nil {
		logger.Warn("match_history list: %v", err)
		// Empty rather than 500: the client falls back to "no
		// matches" cleanly.
		matches = nil
		totals = matchHistoryTotals{}
	}
	if matches == nil {
		matches = []matchHistoryEntry{}
	}
	out, err := json.Marshal(matchHistoryResponse{
		Matches: matches,
		Totals:  totals,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// readMatchHistory reads every match_history row of userID (up to
// matchHistoryPageSize * matchHistoryMaxPages), totals them, and
// returns the newest matchHistoryLimit, newest first. Rows that don't
// decode are skipped and not counted.
func readMatchHistory(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) ([]matchHistoryEntry, matchHistoryTotals, error) {
	var all []matchHistoryEntry
	totals := matchHistoryTotals{}
	cursor := ""
	for page := 0; page < matchHistoryMaxPages; page++ {
		objects, next, err := nk.StorageList(
			ctx, "", userID, matchHistoryCollection,
			matchHistoryPageSize, cursor)
		if err != nil {
			return nil, matchHistoryTotals{}, err
		}
		for _, object := range objects {
			var entry matchHistoryEntry
			if err := json.Unmarshal(
				[]byte(object.GetValue()), &entry); err != nil {
				continue
			}
			totals.Matches++
			if entry.IsWinner {
				totals.Wins++
			}
			totals.Kills += entry.Kills
			totals.Bumps += entry.Bumps
			all = append(all, entry)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].EndedAt != all[j].EndedAt {
			return all[i].EndedAt > all[j].EndedAt
		}
		return all[i].MatchID > all[j].MatchID
	})
	if len(all) > matchHistoryLimit {
		all = all[:matchHistoryLimit]
	}
	return all, totals, nil
}

// nowUnix is a small indirection so unit tests can stub the
// clock if we add them later.
func nowUnix() int64 { return time.Now().Unix() }
