package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Leaderboards declared in each game's game.yaml.
//
// A game lists its boards under `leaderboards:` (id, sort, operator,
// reset_schedule). The runtime creates each one in Nakama as
// `{game_id}_{id}` (gameScopedLeaderboardID): the ID match_end writes
// to, and the one get_player_stats, export_player_data, the deletion
// cascade, and the public leaderboard route read. Creation runs at
// InitModule for every registered game and again after each
// register_game, so a newly declared board exists without a restart.
//
// Until 2026-10-12 nothing created them. Production had no
// leaderboard at all, so every match_end leaderboard write failed
// with "leaderboard not found" (logged as a warning) and no player
// ever got a score.
//
// Boards are created authoritative: Nakama refuses record writes from
// client sessions, so the only writer is match_end, which records
// results only for players the matchmaker put in an allocated match.
// A client-writable board would let any player post any score.
//
// Nakama never changes an existing board's settings; LeaderboardCreate
// is a no-op for an ID that exists. ensureGameLeaderboards logs a
// warning when a live board's sort order, operator, reset schedule
// presence, or authority differs from what it would create. Applying
// a change means deleting the board, and with it every record, and
// letting the runtime recreate it.

// leaderboardIDPattern bounds a declared board ID. The Nakama ID is
// `{game_id}_{id}`, and lowercase letters, digits, and underscores
// keep that a safe URL parameter and log token.
var leaderboardIDPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// leaderboardSortOrders maps game.yaml `sort` values, including
// Nakama's long spellings, to the tokens LeaderboardCreate takes.
// Empty means Nakama's default, descending.
var leaderboardSortOrders = map[string]string{
	"":           "desc",
	"desc":       "desc",
	"descending": "desc",
	"asc":        "asc",
	"ascending":  "asc",
}

// leaderboardOperators maps game.yaml `operator` values to the
// tokens LeaderboardCreate takes. Empty means Nakama's default,
// best.
var leaderboardOperators = map[string]string{
	"":          "best",
	"best":      "best",
	"set":       "set",
	"incr":      "incr",
	"increment": "incr",
	"decr":      "decr",
	"decrement": "decr",
}

// leaderboardConfig is one validated, normalized `leaderboards` entry.
type leaderboardConfig struct {
	// ID is the logical board ID from game.yaml, without the
	// game_id prefix.
	ID string
	// SortOrder is "asc" or "desc".
	SortOrder string
	// Operator is "best", "set", "incr", or "decr".
	Operator string
	// ResetSchedule is a cron expression, or "" for a board that
	// never resets. Nakama validates it at creation.
	ResetSchedule string
}

// parseLeaderboardConfigs reads and validates the `leaderboards` list
// of a game's raw config. A config without the key declares no
// boards. Any invalid entry fails the whole list, so register_game
// refuses the config before storing it.
func parseLeaderboardConfigs(raw []byte) ([]leaderboardConfig, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var blob struct {
		Leaderboards []struct {
			ID            string  `json:"id"`
			Sort          string  `json:"sort"`
			Operator      string  `json:"operator"`
			ResetSchedule *string `json:"reset_schedule"`
		} `json:"leaderboards"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil, fmt.Errorf("leaderboards: %w", err)
	}
	out := make([]leaderboardConfig, 0, len(blob.Leaderboards))
	seen := map[string]bool{}
	for i, lb := range blob.Leaderboards {
		if !leaderboardIDPattern.MatchString(lb.ID) {
			return nil, fmt.Errorf(
				"leaderboards[%d].id %q must be 1-64 lowercase"+
					" letters, digits, or underscores", i, lb.ID)
		}
		if seen[lb.ID] {
			return nil, fmt.Errorf(
				"leaderboards[%d].id %q is declared twice", i, lb.ID)
		}
		seen[lb.ID] = true
		sortOrder, ok := leaderboardSortOrders[strings.ToLower(lb.Sort)]
		if !ok {
			return nil, fmt.Errorf(
				"leaderboards[%d].sort %q must be \"asc\" or \"desc\"",
				i, lb.Sort)
		}
		operator, ok := leaderboardOperators[strings.ToLower(lb.Operator)]
		if !ok {
			return nil, fmt.Errorf(
				"leaderboards[%d].operator %q must be \"best\", \"set\","+
					" \"incr\", or \"decr\"", i, lb.Operator)
		}
		reset := ""
		if lb.ResetSchedule != nil {
			reset = strings.TrimSpace(*lb.ResetSchedule)
		}
		out = append(out, leaderboardConfig{
			ID:            lb.ID,
			SortOrder:     sortOrder,
			Operator:      operator,
			ResetSchedule: reset,
		})
	}
	return out, nil
}

// findLeaderboardConfig returns the board a game declares under id.
func findLeaderboardConfig(
	gc *GameConfig,
	id string,
) (leaderboardConfig, bool) {
	if gc == nil {
		return leaderboardConfig{}, false
	}
	boards, err := parseLeaderboardConfigs(gc.Raw)
	if err != nil {
		return leaderboardConfig{}, false
	}
	for _, lb := range boards {
		if lb.ID == id {
			return lb, true
		}
	}
	return leaderboardConfig{}, false
}

// ensureGameLeaderboards creates every board in boards for gameID,
// authoritative and with ranks enabled. Boards that already exist
// are left as they are (see leaderboardDrift). It tries every board
// and returns one error naming each that failed.
func ensureGameLeaderboards(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	gameID string,
	boards []leaderboardConfig,
) error {
	var failed []string
	for _, lb := range boards {
		id := gameScopedLeaderboardID(gameID, lb.ID)
		if err := nk.LeaderboardCreate(
			ctx, id, true, lb.SortOrder, lb.Operator,
			lb.ResetSchedule, nil, true); err != nil {
			logger.Error("leaderboard %s: create failed: %v", id, err)
			failed = append(failed, fmt.Sprintf("%s (%v)", id, err))
			continue
		}
		live, err := nk.LeaderboardsGetId(ctx, []string{id})
		if err != nil || len(live) == 0 {
			logger.Warn(
				"leaderboard %s: created, but reading it back failed: %v",
				id, err)
			continue
		}
		if drift := leaderboardDrift(live[0], lb); len(drift) > 0 {
			logger.Warn(
				"leaderboard %s differs from game.yaml: %s. Nakama"+
					" never changes an existing leaderboard; delete it"+
					" (and its records) to recreate it.",
				id, strings.Join(drift, "; "))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf(
			"could not create leaderboard(s): %s",
			strings.Join(failed, ", "))
	}
	return nil
}

// ensureAllGameLeaderboards runs ensureGameLeaderboards for every
// registered game. Failures are logged, never fatal: the rest of the
// runtime must load even when one board can't be created.
func ensureAllGameLeaderboards(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
) {
	for _, gc := range games.List() {
		boards, err := parseLeaderboardConfigs(gc.Raw)
		if err != nil {
			logger.Error(
				"game %s: %v; created none of its leaderboards",
				gc.GameID, err)
			continue
		}
		if err := ensureGameLeaderboards(
			ctx, logger, nk, gc.GameID, boards); err != nil {
			logger.Error("game %s: %v", gc.GameID, err)
		}
	}
}

// leaderboardDrift lists the ways a live board differs from the one
// ensureGameLeaderboards would create for want. Nakama does not
// report a board's cron expression, only whether it resets, so only
// that is compared.
func leaderboardDrift(live *api.Leaderboard, want leaderboardConfig) []string {
	var drift []string
	if got := leaderboardSortOrderName(live.GetSortOrder()); got != want.SortOrder {
		drift = append(drift, fmt.Sprintf(
			"sort is %s, game.yaml says %s", got, want.SortOrder))
	}
	if got := leaderboardOperatorName(live.GetOperator()); got != want.Operator {
		drift = append(drift, fmt.Sprintf(
			"operator is %s, game.yaml says %s", got, want.Operator))
	}
	resets := live.GetNextReset() != 0
	if wantResets := want.ResetSchedule != ""; resets != wantResets {
		drift = append(drift, fmt.Sprintf(
			"resets is %t, game.yaml says %t", resets, wantResets))
	}
	if !live.GetAuthoritative() {
		drift = append(drift,
			"it is not authoritative, so clients can write records")
	}
	return drift
}

// leaderboardSortOrderName names Nakama's sort order value (0
// ascending, 1 descending) the way game.yaml spells it.
func leaderboardSortOrderName(sortOrder uint32) string {
	if sortOrder == 0 {
		return "asc"
	}
	return "desc"
}

// leaderboardOperatorName names Nakama's operator the way game.yaml
// spells it.
func leaderboardOperatorName(operator api.Operator) string {
	switch operator {
	case api.Operator_SET:
		return "set"
	case api.Operator_INCREMENT:
		return "incr"
	case api.Operator_DECREMENT:
		return "decr"
	default:
		return "best"
	}
}
