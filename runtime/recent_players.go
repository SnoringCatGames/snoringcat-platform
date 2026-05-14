package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Recent-players tracking. Every time a real (non-synthetic) match
// ends with 2+ players, each matched pair gets a `recent_players`
// storage row recorded from each owner's perspective. The client
// surfaces the list in a "Recent Players" panel where the user can
// add fellow-matched players as friends.
//
// Storage layout:
//   collection = "recent_players"
//   key        = "{other_user_id}"  (one row per other player)
//   user_id    = "{owner_id}"        (owner of the row)
//   value      = { user_id, username, display_name, matched_at }
//   read perm  = 1 (owner-only — the list is personal data)
//   write perm = 0 (server-only — match_end is the sole writer)
//
// Keying by the other user's UUID means re-matching the same
// player just overwrites their existing row with a fresher
// matched_at. No deduplication scan needed.
//
// Stage 7.6 of MULTI_GAME_ROADMAP.md.

const (
	recentPlayersCollection = "recent_players"
	// recentPlayersCap is the response cap surfaced via
	// list_recent_players. The storage itself isn't pruned; once
	// a player is over the cap the older rows just stay
	// invisible. The 8.16/7.7 cascade still scrubs them on
	// account delete via the storage-scrub SQL.
	recentPlayersCap = 50
)

type recentPlayerRecord struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	MatchedAt   int64  `json:"matched_at"`
}

// writeRecentPlayersForMatch records a recent_players row for
// every (owner, other) pair in `players`. Called from MatchEndRpc
// after the existing leaderboard / match_history writes. Skipped
// for solo matches (no pairs to record) and for synthetic-probe
// matches (which already skip leaderboard writes).
//
// Best-effort: per-call failures are logged and the function
// returns. The matchEnd RPC keeps succeeding even when recent-
// players bookkeeping breaks, because the leaderboard write is
// the user-visible critical path.
func writeRecentPlayersForMatch(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	players []matchEndPlayer,
) {
	ids := uniqueUserIDs(players)
	if len(ids) < 2 {
		return
	}
	users, err := nk.UsersGetId(ctx, ids, nil)
	if err != nil {
		logger.Warn(
			"recent_players UsersGetId: %v", err)
		return
	}
	byID := make(map[string]*api.User, len(users))
	for _, u := range users {
		if u != nil {
			byID[u.Id] = u
		}
	}
	now := nowUnix()
	writes := buildRecentPlayerWrites(ids, byID, now)
	if len(writes) == 0 {
		return
	}
	if _, err := nk.StorageWrite(ctx, writes); err != nil {
		logger.Warn("recent_players write: %v", err)
	}
}

// uniqueUserIDs extracts non-empty, de-duplicated user_ids from
// the match-end payload. Extracted for testability and to keep
// writeRecentPlayersForMatch focused on the I/O.
func uniqueUserIDs(players []matchEndPlayer) []string {
	ids := make([]string, 0, len(players))
	seen := map[string]bool{}
	for _, p := range players {
		if p.UserID == "" || seen[p.UserID] {
			continue
		}
		seen[p.UserID] = true
		ids = append(ids, p.UserID)
	}
	return ids
}

// buildRecentPlayerWrites composes the per-pair StorageWrite list
// for a match. Each entry in `ids` owns N-1 rows (one per other
// player). Soft-deleted users (display_name=="[deleted]") are
// excluded as the "other" side — their account is mid-cascade and
// the row would be a ghost. They still own their own rows during
// the grace window; cascade-clear scrubs those on hard-delete.
func buildRecentPlayerWrites(
	ids []string,
	byID map[string]*api.User,
	now int64,
) []*runtime.StorageWrite {
	writes := make(
		[]*runtime.StorageWrite, 0, len(ids)*(len(ids)-1))
	for _, owner := range ids {
		for _, other := range ids {
			if owner == other {
				continue
			}
			otherUser := byID[other]
			if otherUser == nil {
				continue
			}
			if otherUser.DisplayName == anonymizedDisplayName {
				continue
			}
			rec := recentPlayerRecord{
				UserID:      other,
				Username:    otherUser.Username,
				DisplayName: otherUser.DisplayName,
				MatchedAt:   now,
			}
			value, _ := json.Marshal(rec)
			writes = append(writes, &runtime.StorageWrite{
				Collection:      recentPlayersCollection,
				Key:             other,
				UserID:          owner,
				Value:           string(value),
				PermissionRead:  1,
				PermissionWrite: 0,
			})
		}
	}
	return writes
}

type listRecentPlayersResp struct {
	RecentPlayers []recentPlayerRecord `json:"recent_players"`
}

// listRecentPlayersRpcFactory wires the RPC to the per-game
// config so the standard requireGameID check runs. Recent-
// players itself is cross-game (we don't filter by which game
// produced the match) — the game_id gate is a session-identity
// assertion only.
func listRecentPlayersRpcFactory(
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
		_ string,
	) (string, error) {
		return listRecentPlayersRpc(ctx, logger, nk, games)
	}
}

// listRecentPlayersRpc returns the caller's recent-players list
// sorted by matched_at descending, capped at recentPlayersCap.
// Stage 7.6.
func listRecentPlayersRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}

	// Paginate. nk.StorageList accepts a non-empty collection
	// (we pass our own here, unlike the cascade scrub) and
	// returns rows owned by `userID` in this collection. Capped
	// at 5 pages × 100 = 500 entries to bound the read; the
	// response is then sorted + truncated to recentPlayersCap.
	cursor := ""
	rows := make([]recentPlayerRecord, 0, recentPlayersCap)
	for i := 0; i < 5; i++ {
		objects, next, sErr := nk.StorageList(
			ctx, "", userID,
			recentPlayersCollection, 100, cursor)
		if sErr != nil {
			logger.Warn(
				"recent_players list for %s: %v",
				userID, sErr)
			break
		}
		for _, obj := range objects {
			var rec recentPlayerRecord
			if jErr := json.Unmarshal(
				[]byte(obj.Value), &rec); jErr != nil {
				continue
			}
			rows = append(rows, rec)
		}
		if next == "" {
			break
		}
		cursor = next
	}

	rows = sortAndCapRecentPlayers(rows, recentPlayersCap)

	out, _ := json.Marshal(listRecentPlayersResp{
		RecentPlayers: rows,
	})
	return string(out), nil
}

// sortAndCapRecentPlayers sorts by matched_at desc, truncates to
// `cap`. Extracted for testability so the cap-plus-order contract
// is locked without a Nakama mock.
func sortAndCapRecentPlayers(
	rows []recentPlayerRecord,
	cap int,
) []recentPlayerRecord {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].MatchedAt > rows[j].MatchedAt
	})
	if cap >= 0 && len(rows) > cap {
		rows = rows[:cap]
	}
	return rows
}
