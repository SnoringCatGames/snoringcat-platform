package main

import (
	"testing"
)

// TestFindBlockedPairs locks the directed-pair detection
// contract: any A→B (where A blocked B) where both A and B are
// in matchedUserIDs counts as a blocked pair, regardless of
// direction. Pair order is deterministic (lower user_id first)
// and de-duplicated.
func TestFindBlockedPairs(t *testing.T) {
	t.Run("empty match no pairs", func(t *testing.T) {
		got := findBlockedPairs(nil, nil)
		if len(got) != 0 {
			t.Fatalf("want 0 pairs, got %d", len(got))
		}
	})

	t.Run("no blocks no pairs", func(t *testing.T) {
		got := findBlockedPairs(
			[]string{"a", "b", "c"},
			map[string]map[string]struct{}{},
		)
		if len(got) != 0 {
			t.Fatalf("want 0 pairs, got %v", got)
		}
	})

	t.Run("one-way block detected", func(t *testing.T) {
		// a has blocked b; b has not blocked a. Still a pair.
		got := findBlockedPairs(
			[]string{"a", "b"},
			map[string]map[string]struct{}{
				"a": {"b": {}},
			},
		)
		if len(got) != 1 {
			t.Fatalf("want 1 pair, got %v", got)
		}
		if got[0] != [2]string{"a", "b"} {
			t.Fatalf("want pair [a b], got %v", got[0])
		}
	})

	t.Run("two-way block de-duplicated", func(t *testing.T) {
		// Both directions present; should produce a single pair.
		got := findBlockedPairs(
			[]string{"a", "b"},
			map[string]map[string]struct{}{
				"a": {"b": {}},
				"b": {"a": {}},
			},
		)
		if len(got) != 1 {
			t.Fatalf("want 1 pair, got %v", got)
		}
	})

	t.Run("pair order is stable", func(t *testing.T) {
		// Lexicographically lower user_id should come first
		// regardless of which direction the block was issued.
		got := findBlockedPairs(
			[]string{"zebra", "alpha"},
			map[string]map[string]struct{}{
				"zebra": {"alpha": {}},
			},
		)
		if len(got) != 1 {
			t.Fatalf("want 1 pair, got %v", got)
		}
		if got[0][0] != "alpha" || got[0][1] != "zebra" {
			t.Fatalf("want pair [alpha zebra], got %v", got[0])
		}
	})

	t.Run("block outside match is ignored", func(t *testing.T) {
		// a blocked c, but c is not in the match. Should not
		// produce a pair.
		got := findBlockedPairs(
			[]string{"a", "b"},
			map[string]map[string]struct{}{
				"a": {"c": {}},
			},
		)
		if len(got) != 0 {
			t.Fatalf("want 0 pairs, got %v", got)
		}
	})

	t.Run("self-block does not produce a pair", func(t *testing.T) {
		// Defensive: if a row claims a blocked themself
		// (shouldn't happen — block_user RPC rejects it — but
		// stale rows could exist), the same-user case is
		// filtered out by the `a == b` guard.
		got := findBlockedPairs(
			[]string{"a", "b"},
			map[string]map[string]struct{}{
				"a": {"a": {}},
			},
		)
		if len(got) != 0 {
			t.Fatalf("want 0 pairs, got %v", got)
		}
	})

	t.Run("multiple pairs in larger match", func(t *testing.T) {
		// 4-player match with two distinct blocked pairs:
		//   a ↔ b (mutual)
		//   c → d (one-way)
		// Plus an irrelevant block of e (not in match).
		got := findBlockedPairs(
			[]string{"a", "b", "c", "d"},
			map[string]map[string]struct{}{
				"a": {"b": {}, "e": {}},
				"b": {"a": {}},
				"c": {"d": {}},
			},
		)
		if len(got) != 2 {
			t.Fatalf("want 2 pairs, got %v", got)
		}
		// Order of pair-list isn't part of the contract, but
		// both expected pairs should appear.
		want := map[[2]string]bool{
			{"a", "b"}: true,
			{"c", "d"}: true,
		}
		for _, p := range got {
			if !want[p] {
				t.Fatalf("unexpected pair in result: %v", p)
			}
		}
	})
}
