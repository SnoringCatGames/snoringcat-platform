package main

import "testing"

// TestIsPresenceVisible pins the visibility rule that decides
// whether a friend shows up at all. The pre-fix runtime dropped
// every record whose status wasn't literally "online", which meant
// a player in a match — the one state with an interesting
// rich_presence — vanished from their friends' lists.
func TestIsPresenceVisible(t *testing.T) {
	const now int64 = 1_000_000

	cases := []struct {
		name string
		rec  presenceRecord
		want bool
	}{
		{
			name: "online and fresh is visible",
			rec: presenceRecord{
				Status:    presenceStatusOnline,
				UpdatedAt: now,
			},
			want: true,
		},
		{
			name: "in_match is visible, not dropped",
			rec: presenceRecord{
				Status:    presenceStatusInMatch,
				UpdatedAt: now,
			},
			want: true,
		},
		{
			name: "explicit offline is hidden",
			rec: presenceRecord{
				Status:    presenceStatusOffline,
				UpdatedAt: now,
			},
			want: false,
		},
		{
			name: "stale heartbeat is hidden",
			rec: presenceRecord{
				Status:    presenceStatusOnline,
				UpdatedAt: now - presenceStaleAfterSeconds - 1,
			},
			want: false,
		},
		{
			name: "exactly at the staleness bound is visible",
			rec: presenceRecord{
				Status:    presenceStatusOnline,
				UpdatedAt: now - presenceStaleAfterSeconds,
			},
			want: true,
		},
		{
			name: "in_match but stale is hidden",
			rec: presenceRecord{
				Status:    presenceStatusInMatch,
				UpdatedAt: now - presenceStaleAfterSeconds - 1,
			},
			want: false,
		},
		{
			name: "missing updated_at is hidden",
			rec: presenceRecord{
				Status:    presenceStatusOnline,
				UpdatedAt: 0,
			},
			want: false,
		},
		{
			name: "unknown status is visible when fresh",
			rec: presenceRecord{
				Status:    "spectating",
				UpdatedAt: now,
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPresenceVisible(tc.rec, now); got != tc.want {
				t.Errorf(
					"isPresenceVisible(%+v) = %v, want %v",
					tc.rec, got, tc.want)
			}
		})
	}
}

// TestNormalizePresenceStatus guards the closed-set mapping. An
// unrecognized status must degrade to "online" (present, activity
// unknown) rather than to "offline", which would make a
// newer-client friend silently disappear for older peers.
func TestNormalizePresenceStatus(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", presenceStatusOnline},
		{presenceStatusOnline, presenceStatusOnline},
		{presenceStatusInMatch, presenceStatusInMatch},
		{presenceStatusOffline, presenceStatusOffline},
		{"spectating", presenceStatusOnline},
		{"ONLINE", presenceStatusOnline},
	}

	for _, tc := range cases {
		if got := normalizePresenceStatus(tc.in); got != tc.want {
			t.Errorf(
				"normalizePresenceStatus(%q) = %q, want %q",
				tc.in, got, tc.want)
		}
	}
}

func TestPresenceKey(t *testing.T) {
	cases := []struct {
		gameID string
		want   string
	}{
		{"", "current"},
		{"hopnbop", "hopnbop/current"},
		{"another_game", "another_game/current"},
	}
	for _, tc := range cases {
		t.Run(tc.gameID+"/", func(t *testing.T) {
			got := presenceKey(tc.gameID)
			if got != tc.want {
				t.Fatalf("presenceKey(%q) = %q, want %q",
					tc.gameID, got, tc.want)
			}
		})
	}
}

func TestGameIDFromKey(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"current", ""}, // legacy bare key.
		{"hopnbop/current", "hopnbop"},
		{"another_game/current", "another_game"},
		{"malformed", ""}, // no slash → empty rather than mis-attribute.
		{"a/b/c", "a"},    // first segment wins.
		{"", ""},
		{"/leading-slash", ""},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			got := gameIDFromKey(tc.key)
			if got != tc.want {
				t.Fatalf("gameIDFromKey(%q) = %q, want %q",
					tc.key, got, tc.want)
			}
		})
	}
}

// TestPresenceKeyRoundtrip locks the contract: gameIDFromKey is the
// left-inverse of presenceKey for any non-empty game_id, so a
// presence row stored at presenceKey(g) can be re-attributed to g
// after a read where the GameID field is missing (Stage 3
// migration window). The empty-game_id case sits outside this
// roundtrip because it intentionally crosses the
// legacy-vs-namespaced boundary.
func TestPresenceKeyRoundtrip(t *testing.T) {
	for _, gid := range []string{"hopnbop", "another", "g-with-dash"} {
		got := gameIDFromKey(presenceKey(gid))
		if got != gid {
			t.Errorf("gameIDFromKey(presenceKey(%q)) = %q, "+
				"want %q", gid, got, gid)
		}
	}
}
