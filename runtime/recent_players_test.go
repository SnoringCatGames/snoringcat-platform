package main

import (
	"testing"

	"github.com/heroiclabs/nakama-common/api"
)

// TestUniqueUserIDsDedupesAndDropsEmpty locks the contract that
// the recent-players writer's input deduplication tolerates a
// match payload with duplicate or empty user_ids — both can come
// from a misbehaving game server or a corrupt rejoin path, and
// neither should silently produce extra rows or a row with an
// empty UUID.
func TestUniqueUserIDsDedupesAndDropsEmpty(t *testing.T) {
	t.Run("dedupes", func(t *testing.T) {
		got := uniqueUserIDs([]matchEndPlayer{
			{UserID: "a"},
			{UserID: "b"},
			{UserID: "a"}, // dupe
		})
		if len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Fatalf("got %v, want [a b]", got)
		}
	})

	t.Run("drops-empty", func(t *testing.T) {
		got := uniqueUserIDs([]matchEndPlayer{
			{UserID: ""},
			{UserID: "a"},
			{UserID: ""},
		})
		if len(got) != 1 || got[0] != "a" {
			t.Fatalf("got %v, want [a]", got)
		}
	})

	t.Run("preserves-order", func(t *testing.T) {
		got := uniqueUserIDs([]matchEndPlayer{
			{UserID: "z"},
			{UserID: "a"},
			{UserID: "m"},
		})
		if len(got) != 3 ||
			got[0] != "z" || got[1] != "a" || got[2] != "m" {
			t.Fatalf("got %v, want [z a m]", got)
		}
	})

	t.Run("empty-input", func(t *testing.T) {
		got := uniqueUserIDs(nil)
		if len(got) != 0 {
			t.Fatalf("nil input should return empty, got %v",
				got)
		}
	})
}

// TestBuildRecentPlayerWritesPairCount — N players produce
// N*(N-1) writes. Each owner gets one row per OTHER player.
// Skipping a missing user (UserGet failure) or a soft-deleted
// user (display_name=="[deleted]") drops rows from BOTH sides:
// the missing/deleted user is excluded as `other`, but they
// still appear as `owner` for their other rows.
func TestBuildRecentPlayerWritesPairCount(t *testing.T) {
	t.Run("two-players-two-writes", func(t *testing.T) {
		got := buildRecentPlayerWrites(
			[]string{"a", "b"},
			map[string]*api.User{
				"a": {Id: "a", Username: "ua", DisplayName: "A"},
				"b": {Id: "b", Username: "ub", DisplayName: "B"},
			},
			1000,
		)
		if len(got) != 2 {
			t.Fatalf("want 2 writes, got %d", len(got))
		}
	})

	t.Run("four-players-twelve-writes", func(t *testing.T) {
		got := buildRecentPlayerWrites(
			[]string{"a", "b", "c", "d"},
			map[string]*api.User{
				"a": {Id: "a", Username: "ua", DisplayName: "A"},
				"b": {Id: "b", Username: "ub", DisplayName: "B"},
				"c": {Id: "c", Username: "uc", DisplayName: "C"},
				"d": {Id: "d", Username: "ud", DisplayName: "D"},
			},
			1000,
		)
		if len(got) != 12 {
			t.Fatalf("want 12 writes for 4 players, got %d",
				len(got))
		}
	})

	t.Run("solo-no-writes", func(t *testing.T) {
		got := buildRecentPlayerWrites(
			[]string{"a"},
			map[string]*api.User{
				"a": {Id: "a", Username: "ua", DisplayName: "A"},
			},
			1000,
		)
		if len(got) != 0 {
			t.Fatalf("solo match should write 0 rows, got %d",
				len(got))
		}
	})

	t.Run("missing-other-skipped", func(t *testing.T) {
		// `b` was in the match but UsersGetId didn't return a
		// record (e.g. concurrent delete). `a`'s row for `b`
		// should be skipped; the inverse direction goes
		// through, since `b` is still a valid owner even if
		// `a` is the only other player.
		got := buildRecentPlayerWrites(
			[]string{"a", "b"},
			map[string]*api.User{
				"a": {Id: "a", Username: "ua", DisplayName: "A"},
				// b missing
			},
			1000,
		)
		if len(got) != 1 {
			t.Fatalf("missing other should drop 1 of 2 writes,"+
				" got %d", len(got))
		}
		if got[0].UserID != "b" || got[0].Key != "a" {
			t.Fatalf("expected b owns row keyed by a, got"+
				" owner=%s key=%s", got[0].UserID, got[0].Key)
		}
	})

	t.Run("deleted-other-skipped", func(t *testing.T) {
		// `b` is mid-soft-delete (display_name anonymized).
		// Same drop semantics as missing.
		got := buildRecentPlayerWrites(
			[]string{"a", "b"},
			map[string]*api.User{
				"a": {Id: "a", Username: "ua", DisplayName: "A"},
				"b": {
					Id: "b", Username: "ub",
					DisplayName: anonymizedDisplayName,
				},
			},
			1000,
		)
		if len(got) != 1 {
			t.Fatalf("[deleted] other should drop 1 of 2"+
				" writes, got %d", len(got))
		}
		if got[0].UserID != "b" {
			t.Fatalf("expected b owns row, got owner=%s",
				got[0].UserID)
		}
	})

	t.Run("self-pair-skipped", func(t *testing.T) {
		// Defensive: a dup-id slip past uniqueUserIDs would
		// still not produce a self-paired row (owner=other).
		got := buildRecentPlayerWrites(
			[]string{"a", "a"},
			map[string]*api.User{
				"a": {Id: "a", Username: "ua", DisplayName: "A"},
			},
			1000,
		)
		if len(got) != 0 {
			t.Fatalf("self-pair should produce 0 rows, got %d",
				len(got))
		}
	})
}

// TestBuildRecentPlayerWritesValueShape — the per-row value
// payload mirrors the recentPlayerRecord JSON shape, so the
// client-side cache can deserialize without a schema-version
// dance. Locks the value contract.
func TestBuildRecentPlayerWritesValueShape(t *testing.T) {
	got := buildRecentPlayerWrites(
		[]string{"a", "b"},
		map[string]*api.User{
			"a": {Id: "a", Username: "ua", DisplayName: "A"},
			"b": {Id: "b", Username: "ub", DisplayName: "B"},
		},
		1700000000,
	)
	if len(got) != 2 {
		t.Fatalf("want 2 writes, got %d", len(got))
	}
	want := `{"user_id":"b","username":"ub","display_name":"B","matched_at":1700000000}`
	if got[0].UserID != "a" || got[0].Key != "b" ||
		got[0].Value != want {
		t.Fatalf("a-owns-b row drift:\n got owner=%s key=%s val=%s\n want owner=a key=b val=%s",
			got[0].UserID, got[0].Key, got[0].Value, want)
	}
	if got[0].Collection != recentPlayersCollection {
		t.Fatalf("collection drift: got %q, want %q",
			got[0].Collection, recentPlayersCollection)
	}
	if got[0].PermissionRead != 1 || got[0].PermissionWrite != 0 {
		t.Fatalf("permission drift: got read=%d write=%d,"+
			" want read=1 write=0",
			got[0].PermissionRead, got[0].PermissionWrite)
	}
}

// TestSortAndCapRecentPlayers — descending matched_at order +
// cap truncation. Stable sort preserves insertion order on
// ties (matters because two players matched at the same
// second is realistic on a fast end-of-match).
func TestSortAndCapRecentPlayers(t *testing.T) {
	t.Run("sorts-desc-by-matched-at", func(t *testing.T) {
		got := sortAndCapRecentPlayers(
			[]recentPlayerRecord{
				{UserID: "a", MatchedAt: 100},
				{UserID: "b", MatchedAt: 300},
				{UserID: "c", MatchedAt: 200},
			},
			10,
		)
		want := []string{"b", "c", "a"}
		for i, w := range want {
			if got[i].UserID != w {
				t.Errorf("[%d] got %s, want %s",
					i, got[i].UserID, w)
			}
		}
	})

	t.Run("truncates-to-cap", func(t *testing.T) {
		input := make([]recentPlayerRecord, 5)
		for i := range input {
			input[i] = recentPlayerRecord{
				UserID:    string(rune('a' + i)),
				MatchedAt: int64(i),
			}
		}
		got := sortAndCapRecentPlayers(input, 3)
		if len(got) != 3 {
			t.Fatalf("want len 3, got %d", len(got))
		}
		// Highest matched_at first → e, d, c.
		if got[0].UserID != "e" || got[1].UserID != "d" ||
			got[2].UserID != "c" {
			t.Fatalf("got %+v, want [e d c]", got)
		}
	})

	t.Run("under-cap-no-truncation", func(t *testing.T) {
		got := sortAndCapRecentPlayers(
			[]recentPlayerRecord{
				{UserID: "a", MatchedAt: 100},
				{UserID: "b", MatchedAt: 200},
			},
			50,
		)
		if len(got) != 2 {
			t.Fatalf("want 2, got %d", len(got))
		}
	})

	t.Run("stable-on-ties", func(t *testing.T) {
		// Same matched_at — insertion order should be preserved.
		got := sortAndCapRecentPlayers(
			[]recentPlayerRecord{
				{UserID: "first", MatchedAt: 100},
				{UserID: "second", MatchedAt: 100},
				{UserID: "third", MatchedAt: 100},
			},
			10,
		)
		want := []string{"first", "second", "third"}
		for i, w := range want {
			if got[i].UserID != w {
				t.Errorf(
					"[%d] tie order drift: got %s, want %s",
					i, got[i].UserID, w)
			}
		}
	})

	t.Run("empty-input", func(t *testing.T) {
		got := sortAndCapRecentPlayers(nil, 10)
		if len(got) != 0 {
			t.Fatalf("nil in should round-trip empty, got %v",
				got)
		}
	})
}

// TestRecentPlayersCapConstantStable — guards the response cap.
// Bumping this changes the maximum-ever payload of
// list_recent_players, which clients render directly; a silent
// bump should require an explicit roadmap-aware decision.
func TestRecentPlayersCapConstantStable(t *testing.T) {
	if recentPlayersCap != 50 {
		t.Fatalf(
			"recentPlayersCap drifted to %d; roadmap pins at 50."+
				" Bumping requires updating MULTI_GAME_ROADMAP.md"+
				" §7.6 + checking client paging assumptions.",
			recentPlayersCap)
	}
}
