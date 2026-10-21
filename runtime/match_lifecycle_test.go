package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// fakeMatchNk is the slice of NakamaModule MatchEndRpc uses. Storage
// rows are keyed "collection/key". Every write, delete and
// leaderboard record is captured, so the rejection tests can prove
// a refused call changed nothing. The embedded interface is nil:
// any other method panics, which flags a code path the test didn't
// expect.
type fakeMatchNk struct {
	runtime.NakamaModule
	rows        map[string]string
	readErrors  map[string]error
	writes      []*runtime.StorageWrite
	deletes     []string
	leaderboard []string
}

func (f *fakeMatchNk) StorageRead(
	_ context.Context, reads []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	out := []*api.StorageObject{}
	for _, r := range reads {
		if err := f.readErrors[r.Collection]; err != nil {
			return nil, err
		}
		if v, ok := f.rows[r.Collection+"/"+r.Key]; ok {
			out = append(out, &api.StorageObject{
				Collection: r.Collection, Key: r.Key, Value: v,
			})
		}
	}
	return out, nil
}

func (f *fakeMatchNk) StorageWrite(
	_ context.Context, writes []*runtime.StorageWrite,
) ([]*api.StorageObjectAck, error) {
	f.writes = append(f.writes, writes...)
	return nil, nil
}

func (f *fakeMatchNk) StorageDelete(
	_ context.Context, deletes []*runtime.StorageDelete,
) error {
	for _, d := range deletes {
		f.deletes = append(f.deletes, d.Collection+"/"+d.Key)
	}
	return nil
}

func (f *fakeMatchNk) LeaderboardRecordWrite(
	_ context.Context, id, ownerID, _ string, _, _ int64,
	_ map[string]interface{}, _ *int,
) (*api.LeaderboardRecord, error) {
	f.leaderboard = append(f.leaderboard, id+"/"+ownerID)
	return &api.LeaderboardRecord{}, nil
}

func (f *fakeMatchNk) UsersGetId(
	context.Context, []string, []string,
) ([]*api.User, error) {
	return nil, nil
}

func (f *fakeMatchNk) MetricsGaugeSet(string, map[string]string, float64) {}

func (f *fakeMatchNk) sideEffects() int {
	return len(f.writes) + len(f.deletes) + len(f.leaderboard)
}

// historyOwners lists the users given a match_history row, and
// whether each row says they won.
func (f *fakeMatchNk) historyOwners(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, w := range f.writes {
		if w.Collection != "match_history" {
			continue
		}
		var entry struct {
			IsWinner bool `json:"is_winner"`
		}
		if err := json.Unmarshal([]byte(w.Value), &entry); err != nil {
			t.Fatalf("decode history row: %v", err)
		}
		out[w.UserID] = entry.IsWinner
	}
	return out
}

const (
	testRequestID = "req-123"
	testUserA     = "11111111-1111-1111-1111-111111111111"
	testUserB     = "22222222-2222-2222-2222-222222222222"
	testOutsider  = "99999999-9999-9999-9999-999999999999"
)

// newMatchNk returns storage holding a server registration for
// testRequestID and, when metadata is non-empty, its match_metadata
// row.
func newMatchNk(metadata string) *fakeMatchNk {
	rows := map[string]string{
		"server_registrations/" + testRequestID: `{"request_id":"` +
			testRequestID + `","server_ip":"203.0.113.9",` +
			`"server_port":4433}`,
	}
	if metadata != "" {
		rows[matchMetadataCollection+"/"+testRequestID] = metadata
	}
	return &fakeMatchNk{rows: rows}
}

func matchEndPayload(winner string, userIDs ...string) string {
	args := matchEndArgs{RequestID: testRequestID, WinnerID: winner}
	for _, id := range userIDs {
		args.Players = append(args.Players, matchEndPlayer{
			UserID: id, Score: 10, Kills: 1,
		})
	}
	out, _ := json.Marshal(args)
	return string(out)
}

const testMetadataAB = `{"game_id":"hopnbop","allocated_at":1,` +
	`"user_ids":["` + testUserA + `","` + testUserB + `"]}`

func TestMatchEndRejectsUnallocatedRequestID(t *testing.T) {
	// register_server accepts any request_id from an HTTP-key
	// caller. Without the allocator's metadata row, match_end must
	// refuse, and change nothing.
	nk := newMatchNk("")
	_, err := (&matchLifecycle{}).MatchEndRpc(
		context.Background(), testLogger{}, nil, nk,
		matchEndPayload(testOutsider, testOutsider))
	if got := errorCode(err); got != 5 {
		t.Fatalf("code = %d (%v), want 5 (NOT_FOUND)", got, err)
	}
	if nk.sideEffects() != 0 {
		t.Errorf("refused call had side effects: writes=%d"+
			" deletes=%v leaderboard=%v",
			len(nk.writes), nk.deletes, nk.leaderboard)
	}
}

func TestMatchEndRejectsUnregisteredRequestID(t *testing.T) {
	nk := &fakeMatchNk{rows: map[string]string{
		matchMetadataCollection + "/" + testRequestID: testMetadataAB,
	}}
	_, err := (&matchLifecycle{}).MatchEndRpc(
		context.Background(), testLogger{}, nil, nk,
		matchEndPayload("", testUserA))
	if got := errorCode(err); got != 5 {
		t.Fatalf("code = %d (%v), want 5", got, err)
	}
	if nk.sideEffects() != 0 {
		t.Error("refused call had side effects")
	}
}

func TestMatchEndMetadataReadErrorIsRetryable(t *testing.T) {
	nk := newMatchNk(testMetadataAB)
	nk.readErrors = map[string]error{
		matchMetadataCollection: errors.New("connection reset"),
	}
	_, err := (&matchLifecycle{}).MatchEndRpc(
		context.Background(), testLogger{}, nil, nk,
		matchEndPayload("", testUserA))
	if got := errorCode(err); got != 13 {
		t.Fatalf("code = %d (%v), want 13 (INTERNAL, retried)", got, err)
	}
	if nk.sideEffects() != 0 {
		t.Error("failed call had side effects")
	}
}

func TestMatchEndRejectsOnlyOutsiders(t *testing.T) {
	// A forged call naming only users who weren't matched must be
	// refused without side effects, so the real server's match_end
	// still finds its registration afterwards.
	nk := newMatchNk(testMetadataAB)
	_, err := (&matchLifecycle{}).MatchEndRpc(
		context.Background(), testLogger{}, nil, nk,
		matchEndPayload(testOutsider, testOutsider))
	if got := errorCode(err); got != 3 {
		t.Fatalf("code = %d (%v), want 3", got, err)
	}
	if nk.sideEffects() != 0 {
		t.Errorf("refused call had side effects: deletes=%v"+
			" leaderboard=%v", nk.deletes, nk.leaderboard)
	}
}

func TestMatchEndRecordsOnlyMatchedPlayers(t *testing.T) {
	nk := newMatchNk(testMetadataAB)
	out, err := (&matchLifecycle{}).MatchEndRpc(
		context.Background(), testLogger{}, nil, nk,
		matchEndPayload(testOutsider, testUserA, testOutsider))
	if err != nil {
		t.Fatalf("match_end: %v", err)
	}
	if !strings.Contains(out, `"ok":true`) {
		t.Errorf("response = %s", out)
	}
	wantBoard := []string{"hopnbop_ffa/" + testUserA}
	if !reflect.DeepEqual(nk.leaderboard, wantBoard) {
		t.Errorf("leaderboard writes = %v, want %v",
			nk.leaderboard, wantBoard)
	}
	history := nk.historyOwners(t)
	if len(history) != 1 {
		t.Fatalf("history rows for %v, want only %s",
			history, testUserA)
	}
	if won, ok := history[testUserA]; !ok || won {
		// The reported winner was the outsider: nobody wins.
		t.Errorf("history[%s] = (won=%t, present=%t), want"+
			" present and not a win", testUserA, won, ok)
	}
	for _, want := range []string{
		"server_registrations/" + testRequestID,
		matchMetadataCollection + "/" + testRequestID,
	} {
		found := false
		for _, d := range nk.deletes {
			found = found || d == want
		}
		if !found {
			t.Errorf("cleanup missing %s (deletes=%v)", want, nk.deletes)
		}
	}
}

func TestMatchEndLegacyMetadataSkipsPlayerCheck(t *testing.T) {
	// Rows written before user_ids existed: matches in flight when
	// this runtime deployed must still record their results.
	nk := newMatchNk(`{"game_id":"hopnbop","allocated_at":1}`)
	_, err := (&matchLifecycle{}).MatchEndRpc(
		context.Background(), testLogger{}, nil, nk,
		matchEndPayload(testUserB, testUserA, testUserB))
	if err != nil {
		t.Fatalf("match_end: %v", err)
	}
	if len(nk.leaderboard) != 2 {
		t.Errorf("leaderboard writes = %v, want both players",
			nk.leaderboard)
	}
	if won := nk.historyOwners(t)[testUserB]; !won {
		t.Error("winner lost its win on a legacy-metadata match")
	}
}

func TestMatchEndInvalidatesThePublicLeaderboard(t *testing.T) {
	boards := newPublicLeaderboardReader(testLogger{}, nil)
	_, err := (&matchLifecycle{boards: boards}).MatchEndRpc(
		context.Background(), testLogger{}, nil, newMatchNk(testMetadataAB),
		matchEndPayload(testUserA, testUserA, testUserB))
	if err != nil {
		t.Fatalf("match_end: %v", err)
	}
	if got := boards.cache.generation.Load(); got != 1 {
		t.Errorf("cache generation = %d after match_end, want 1", got)
	}
}

func TestMatchEndKeepsThePublicLeaderboardCacheWhenNothingIsRecorded(
	t *testing.T,
) {
	// A synthetic probe match records no scores, and a refused call
	// records nothing: neither may cost a board read.
	boards := newPublicLeaderboardReader(testLogger{}, nil)
	lifecycle := &matchLifecycle{boards: boards}
	synthetic := newMatchNk(testMetadataAB)
	synthetic.rows[syntheticMatchCollection+"/"+testRequestID] = `{}`
	if _, err := lifecycle.MatchEndRpc(
		context.Background(), testLogger{}, nil, synthetic,
		matchEndPayload(testUserA, testUserA, testUserB)); err != nil {
		t.Fatalf("synthetic match_end: %v", err)
	}
	if len(synthetic.leaderboard) != 0 {
		t.Fatalf("synthetic match wrote %v", synthetic.leaderboard)
	}
	if _, err := lifecycle.MatchEndRpc(
		context.Background(), testLogger{}, nil, newMatchNk(""),
		matchEndPayload(testUserA, testUserA)); err == nil {
		t.Fatal("match_end accepted an unallocated request_id")
	}
	if got := boards.cache.generation.Load(); got != 0 {
		t.Errorf("cache generation = %d, want 0", got)
	}
}

func TestRestrictToMatchedPlayers(t *testing.T) {
	matched := []string{testUserA, testUserB}
	players := []matchEndPlayer{
		{UserID: testUserA, Score: 1},
		{UserID: testOutsider, Score: 2},
		{UserID: "", Score: 3},
		{UserID: testUserB, Score: 4},
	}
	kept, dropped := restrictToMatchedPlayers(players, matched)
	wantKept := []matchEndPlayer{
		{UserID: testUserA, Score: 1},
		{UserID: "", Score: 3},
		{UserID: testUserB, Score: 4},
	}
	if !reflect.DeepEqual(kept, wantKept) {
		t.Errorf("kept = %+v, want %+v", kept, wantKept)
	}
	if !reflect.DeepEqual(dropped, []string{testOutsider}) {
		t.Errorf("dropped = %v, want [%s]", dropped, testOutsider)
	}
	if !hasReportedUser(kept) {
		t.Error("hasReportedUser(kept) = false")
	}
	if hasReportedUser([]matchEndPlayer{{UserID: ""}}) {
		t.Error("an entry without a user_id counted as a user")
	}
	if !includesUser(kept, testUserB) || includesUser(kept, testOutsider) {
		t.Error("includesUser disagrees with kept")
	}
}

// SP70: server_registrations holds a live server's IP, port and
// FQDN. The row must be server-only (PermissionRead 0): with
// PermissionRead 2 any signed-in player could list match rows and
// find a running server to connect to.
func TestRegisterServerWritesServerOnlyRow(t *testing.T) {
	nk := &fakeMatchNk{rows: map[string]string{}}
	payload := `{"request_id":"` + testRequestID +
		`","server_ip":"203.0.113.9","server_port":4433,` +
		`"server_fqdn":"srv.example"}`
	if _, err := (&matchLifecycle{}).RegisterServerRpc(
		context.Background(), testLogger{}, nil, nk, payload,
	); err != nil {
		t.Fatalf("register_server: %v", err)
	}
	if len(nk.writes) != 1 {
		t.Fatalf("writes = %d, want 1", len(nk.writes))
	}
	w := nk.writes[0]
	if w.Collection != "server_registrations" {
		t.Fatalf("collection = %q, want server_registrations",
			w.Collection)
	}
	if w.PermissionRead != 0 || w.PermissionWrite != 0 {
		t.Fatalf("permissions read/write = %d/%d, want 0/0 (SP70)",
			w.PermissionRead, w.PermissionWrite)
	}
}

func TestMatchMetadataUserIDsRoundTrip(t *testing.T) {
	out, _ := json.Marshal(matchMetadata{GameID: "hopnbop"})
	if strings.Contains(string(out), "user_ids") {
		t.Errorf("empty user_ids should be omitted: %s", out)
	}
	var back matchMetadata
	if err := json.Unmarshal([]byte(testMetadataAB), &back); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(back.UserIDs, []string{testUserA, testUserB}) {
		t.Errorf("user_ids = %v", back.UserIDs)
	}
}

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

func TestMatchEndHistoryCarriesLevelID(t *testing.T) {
	// The game server reports the level in match_stats (H49);
	// the history row must keep it.
	args := matchEndArgs{
		RequestID: testRequestID,
		WinnerID:  testUserA,
		Players: []matchEndPlayer{
			{UserID: testUserA, Score: 10, Kills: 1},
		},
		Stats: map[string]any{
			"duration_sec": 95.0,
			"level_id":     "level_2",
		},
	}
	payload, _ := json.Marshal(args)
	nk := newMatchNk(testMetadataAB)
	if _, err := (&matchLifecycle{}).MatchEndRpc(
		context.Background(), testLogger{}, nil, nk,
		string(payload)); err != nil {
		t.Fatalf("match_end: %v", err)
	}
	found := false
	for _, w := range nk.writes {
		if w.Collection != "match_history" {
			continue
		}
		found = true
		var entry struct {
			LevelID string `json:"level_id"`
		}
		if err := json.Unmarshal(
			[]byte(w.Value), &entry); err != nil {
			t.Fatalf("decode history row: %v", err)
		}
		if entry.LevelID != "level_2" {
			t.Errorf("level_id = %q, want level_2", entry.LevelID)
		}
	}
	if !found {
		t.Fatal("no match_history row written")
	}
}
