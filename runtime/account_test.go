package main

import (
	"encoding/json"
	"testing"
)

func TestParseLeaderboardIDs(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		gc := &GameConfig{
			Raw: json.RawMessage(`{
				"leaderboards":[
					{"id":"ffa"},
					{"id":"weekly"}
				]
			}`),
		}
		got := parseLeaderboardIDs(gc)
		want := []string{"ffa", "weekly"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("empty-id-dropped", func(t *testing.T) {
		gc := &GameConfig{
			Raw: json.RawMessage(
				`{"leaderboards":[{"id":""},{"id":"ffa"}]}`),
		}
		got := parseLeaderboardIDs(gc)
		if len(got) != 1 || got[0] != "ffa" {
			t.Fatalf("got %v, want [ffa]", got)
		}
	})

	t.Run("nil", func(t *testing.T) {
		if got := parseLeaderboardIDs(nil); got != nil {
			t.Fatalf("nil should return nil, got %v", got)
		}
	})

	t.Run("empty-raw", func(t *testing.T) {
		if got := parseLeaderboardIDs(&GameConfig{}); got != nil {
			t.Fatalf("empty Raw should return nil, got %v", got)
		}
	})

	t.Run("malformed", func(t *testing.T) {
		gc := &GameConfig{Raw: json.RawMessage(`{not`)}
		if got := parseLeaderboardIDs(gc); got != nil {
			t.Fatalf("malformed should return nil, got %v", got)
		}
	})

	t.Run("missing-block", func(t *testing.T) {
		gc := &GameConfig{Raw: json.RawMessage(`{}`)}
		if got := parseLeaderboardIDs(gc); len(got) != 0 {
			t.Fatalf("missing block should return empty, got %v",
				got)
		}
	})
}

// TestLeaderboardIDsToScrubPrefixesAndLegacy — every registered
// game's leaderboards get prefixed with `{game_id}_`, AND the
// legacy bare "ffa" is always appended. Both invariants matter for
// the cascade scrub during the Stage 3.6 rollout window where some
// rows still live on the un-prefixed board.
func TestLeaderboardIDsToScrubPrefixesAndLegacy(t *testing.T) {
	games := newTestGames(t, map[string]string{
		"hopnbop": `{
			"game_id":"hopnbop",
			"display_name":"H",
			"edgegap_app_slug":"s",
			"protocol_version":1,
			"display_version":"1",
			"leaderboards":[{"id":"ffa"},{"id":"weekly"}]
		}`,
		"another": `{
			"game_id":"another",
			"display_name":"A",
			"edgegap_app_slug":"s",
			"protocol_version":1,
			"display_version":"1",
			"leaderboards":[{"id":"global"}]
		}`,
	})
	got := leaderboardIDsToScrub(games)
	wantContains := []string{
		"hopnbop_ffa", "hopnbop_weekly",
		"another_global", "ffa",
	}
	gotSet := map[string]bool{}
	for _, id := range got {
		gotSet[id] = true
	}
	for _, want := range wantContains {
		if !gotSet[want] {
			t.Errorf("missing leaderboard %q in scrub list %v",
				want, got)
		}
	}
	// Dedup: bare "ffa" should appear once even though hopnbop's
	// raw config also lists "ffa". leaderboardIDsToScrub prefixes
	// hopnbop's entry to "hopnbop_ffa", so the only "ffa" in the
	// result is the legacy fallback.
	bareCount := 0
	for _, id := range got {
		if id == "ffa" {
			bareCount++
		}
	}
	if bareCount != 1 {
		t.Errorf("bare 'ffa' appeared %d times, want 1: %v",
			bareCount, got)
	}
}

func TestLeaderboardIDsToScrubEmptyRegistry(t *testing.T) {
	games := newTestGames(t, nil)
	got := leaderboardIDsToScrub(games)
	// Even with no registered games, the legacy bare board must
	// be in the list so a fresh-deploy cascade still scrubs
	// pre-Stage-3.6 rows.
	if len(got) != 1 || got[0] != "ffa" {
		t.Fatalf("empty registry should return [ffa], got %v",
			got)
	}
}
