package main

import "testing"

func TestGameScopedLeaderboardID(t *testing.T) {
	cases := []struct {
		gameID  string
		boardID string
		want    string
	}{
		{"", "ffa", "ffa"}, // legacy bootstrap fallback.
		{"hopnbop", "ffa", "hopnbop_ffa"},
		{"another", "weekly", "another_weekly"},
	}
	for _, tc := range cases {
		got := gameScopedLeaderboardID(tc.gameID, tc.boardID)
		if got != tc.want {
			t.Errorf("gameScopedLeaderboardID(%q, %q) = %q, "+
				"want %q", tc.gameID, tc.boardID, got, tc.want)
		}
	}
}

func TestClampPlayerStats(t *testing.T) {
	cases := []struct {
		name string
		in   matchEndPlayer
		want matchEndPlayer
	}{
		{
			name: "all-in-range",
			in:   matchEndPlayer{Score: 50, Kills: 5, Bumps: 3},
			want: matchEndPlayer{Score: 50, Kills: 5, Bumps: 3},
		},
		{
			name: "negative-floor-to-zero",
			in:   matchEndPlayer{Score: -1, Kills: -5, Bumps: -100},
			want: matchEndPlayer{Score: 0, Kills: 0, Bumps: 0},
		},
		{
			name: "above-ceiling-clamped",
			in: matchEndPlayer{
				Score: maxScore + 1,
				Kills: maxKills + 1,
				Bumps: maxBumps + 1,
			},
			want: matchEndPlayer{
				Score: maxScore,
				Kills: maxKills,
				Bumps: maxBumps,
			},
		},
		{
			name: "max-int-tamper",
			in: matchEndPlayer{
				Score: 1<<31 - 1,
				Kills: 1<<31 - 1,
				Bumps: 1<<31 - 1,
			},
			want: matchEndPlayer{
				Score: maxScore,
				Kills: maxKills,
				Bumps: maxBumps,
			},
		},
		{
			name: "at-ceiling-unchanged",
			in: matchEndPlayer{
				Score: maxScore,
				Kills: maxKills,
				Bumps: maxBumps,
			},
			want: matchEndPlayer{
				Score: maxScore,
				Kills: maxKills,
				Bumps: maxBumps,
			},
		},
		{
			name: "user-id-preserved",
			in: matchEndPlayer{
				UserID: "alice", Score: maxScore + 9999,
			},
			want: matchEndPlayer{
				UserID: "alice", Score: maxScore,
			},
		},
		{
			name: "mixed-clamp",
			in: matchEndPlayer{
				UserID: "bob",
				Score:  -10,
				Kills:  3,
				Bumps:  maxBumps + 5,
			},
			want: matchEndPlayer{
				UserID: "bob",
				Score:  0,
				Kills:  3,
				Bumps:  maxBumps,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in
			clampPlayerStats(&got)
			if got != tc.want {
				t.Errorf("clampPlayerStats(%+v) = %+v, want %+v",
					tc.in, got, tc.want)
			}
		})
	}
}
