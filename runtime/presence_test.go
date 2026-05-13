package main

import "testing"

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
