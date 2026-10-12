package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
)

const (
	testLeaderID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	testCallerID = "cccccccc-cccc-cccc-cccc-cccccccccccc"
)

// hopnbopSession is a client session of userID signed into hopnbop
// (sessionCtx is in crash_report_test.go).
func hopnbopSession(userID string) context.Context {
	return sessionCtx(userID, "hopnbop")
}

// playerStats runs get_player_stats and decodes the raw JSON too, so a
// test can tell null from zero.
func playerStats(
	t *testing.T,
	f *publicLeaderboardFixture,
	callerID string,
	payload string,
) (playerStatsResponse, map[string]any) {
	t.Helper()
	rpc := getPlayerStatsRpcFactory(f.games, f.boards)
	out, err := rpc(hopnbopSession(callerID), testLogger{}, nil, f.nk, payload)
	if err != nil {
		t.Fatalf("get_player_stats: %v", err)
	}
	var resp playerStatsResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(out), &raw)
	return resp, raw
}

// statsFixture holds a leader far ahead of the caller.
func statsFixture(t *testing.T) *publicLeaderboardFixture {
	t.Helper()
	f := newPublicLeaderboardFixture(t)
	f.addPlayer(testLeaderID, "leader", "Leader", 900)
	f.addPlayer(testCallerID, "caller", "Caller", 300)
	f.nk.records[testBoardID][1].NumScore = 2
	return f
}

func TestGetPlayerStatsReadsTheCallersOwnRecord(t *testing.T) {
	// Before 2026-10-12 this read the board's top record, so every
	// player got the leader's 900.
	f := statsFixture(t)
	resp, raw := playerStats(t, f, testCallerID, "")
	if resp.PlayerID != testCallerID {
		t.Errorf("player_id = %q", resp.PlayerID)
	}
	if resp.BestScore == nil || *resp.BestScore != 300 {
		t.Errorf("best_score = %v, want 300", raw["best_score"])
	}
	if resp.Rank == nil || *resp.Rank != 2 {
		t.Errorf("rank = %v, want 2", raw["rank"])
	}
	if resp.Rating != 300 || resp.Matches != 2 {
		t.Errorf("deprecated rating/matches = %d/%d, want 300/2",
			resp.Rating, resp.Matches)
	}
	if f.nk.listCalls != 1 {
		t.Errorf("listCalls = %d: the rank should come from one cached"+
			" public read", f.nk.listCalls)
	}
}

func TestGetPlayerStatsForAnotherPlayer(t *testing.T) {
	f := statsFixture(t)
	resp, _ := playerStats(t, f, testCallerID,
		`{"player_id":"`+testLeaderID+`"}`)
	if resp.PlayerID != testLeaderID || resp.BestScore == nil ||
		*resp.BestScore != 900 || resp.Rank == nil || *resp.Rank != 1 {
		t.Errorf("leader's stats = %+v", resp)
	}
}

func TestGetPlayerStatsUnrankedIsNullNotZero(t *testing.T) {
	f := statsFixture(t)
	const newcomer = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	resp, raw := playerStats(t, f, newcomer, "")
	for _, field := range []string{"best_score", "rank"} {
		value, present := raw[field]
		if !present || value != nil {
			t.Errorf("%s = %v (present %t), want null", field, value,
				present)
		}
	}
	if resp.Rating != unrankedRating || resp.Matches != 0 {
		t.Errorf("deprecated rating/matches = %d/%d, want %d/0",
			resp.Rating, resp.Matches, unrankedRating)
	}
	// A player with no record can't be listed, so the board isn't
	// read for them.
	if f.nk.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0", f.nk.listCalls)
	}
}

func TestGetPlayerStatsUnlistedPlayerHasScoreButNoRank(t *testing.T) {
	// A guest (no display name) records scores but is never listed.
	f := statsFixture(t)
	f.nk.accounts[testCallerID].User.DisplayName = ""
	resp, raw := playerStats(t, f, testCallerID, "")
	if resp.BestScore == nil || *resp.BestScore != 300 {
		t.Errorf("best_score = %v, want 300", raw["best_score"])
	}
	if raw["rank"] != nil {
		t.Errorf("rank = %v, want null for an unlisted player", raw["rank"])
	}
}

func TestGetPlayerStatsWithoutABoardAnswersDefaults(t *testing.T) {
	f := statsFixture(t)
	delete(f.nk.boards, testBoardID)
	resp, raw := playerStats(t, f, testCallerID, "")
	if raw["best_score"] != nil || raw["rank"] != nil ||
		resp.Rating != unrankedRating {
		t.Errorf("want unranked defaults, got %v", raw)
	}
}

func TestGetPlayerStatsWithoutReaderLeavesRankNull(t *testing.T) {
	f := statsFixture(t)
	rpc := getPlayerStatsRpcFactory(f.games, nil)
	out, err := rpc(hopnbopSession(testCallerID), testLogger{}, nil, f.nk, "")
	if err != nil {
		t.Fatalf("get_player_stats: %v", err)
	}
	if !strings.Contains(out, `"rank":null`) ||
		!strings.Contains(out, `"best_score":300`) {
		t.Errorf("response = %s", out)
	}
}

func TestPublicSnapshotRanksMatchItsEntries(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.addPlayer(testLeaderID, "a", "Alpha", 500)
	f.addPlayer(testCallerID, "c", "Charlie", 500)
	f.addPlayer("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "b", "", 400)
	snapshot, err := f.boards.read(testBoardID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	want := map[string]int64{testLeaderID: 1, testCallerID: 1}
	if !reflect.DeepEqual(snapshot.ranks, want) {
		t.Errorf("ranks = %v, want %v", snapshot.ranks, want)
	}
	if rank, ok := f.boards.rankOf(testBoardID, testCallerID); !ok ||
		rank != 1 {
		t.Errorf("rankOf = (%d, %t)", rank, ok)
	}
	if _, ok := f.boards.rankOf(
		testBoardID, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"); ok {
		t.Error("a nameless player has a public rank")
	}
}

// historyRow is a match_history storage object.
func historyRow(key string, endedAt int64, won bool, score, kills,
	bumps int) *api.StorageObject {
	value, _ := json.Marshal(matchHistoryEntry{
		MatchID: key, EndedAt: endedAt, IsWinner: won,
		Score: score, Kills: kills, Bumps: bumps,
	})
	return &api.StorageObject{
		Collection: matchHistoryCollection, Key: key,
		UserId: testCallerID, Value: string(value),
	}
}

// matchHistory runs get_match_history for the caller.
func matchHistory(t *testing.T, nk *fakeLeaderboardNk) (
	matchHistoryResponse, string,
) {
	t.Helper()
	games := newTestGames(t, map[string]string{
		"hopnbop": hopnbopLeaderboardConfigJSON,
	})
	rpc := getMatchHistoryRpcFactory(games)
	out, err := rpc(hopnbopSession(testCallerID), testLogger{}, nil, nk, "")
	if err != nil {
		t.Fatalf("get_match_history: %v", err)
	}
	var resp matchHistoryResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	return resp, out
}

func TestGetMatchHistoryListsNewestFirstWithTotals(t *testing.T) {
	// Keys (request ids) sort unrelated to time, and there are more
	// rows than one page and than the listing limit.
	nk := newFakeLeaderboardNk()
	const rows = 130
	var wantWins, wantKills, wantBumps int
	for i := 0; i < rows; i++ {
		// Key order runs backwards in time for the first half.
		endedAt := int64(1760000000 + (i*37)%rows*60)
		won := i%3 == 0
		nk.history[testCallerID] = append(nk.history[testCallerID],
			historyRow(fmt.Sprintf("req-%03d", i), endedAt, won, i, i%5,
				i%7))
		if won {
			wantWins++
		}
		wantKills += i % 5
		wantBumps += i % 7
	}
	nk.history[testCallerID] = append(nk.history[testCallerID],
		&api.StorageObject{Key: "broken", Value: "{not json"})

	resp, _ := matchHistory(t, nk)
	if len(resp.Matches) != matchHistoryLimit {
		t.Fatalf("listed %d, want %d", len(resp.Matches), matchHistoryLimit)
	}
	for i := 1; i < len(resp.Matches); i++ {
		if resp.Matches[i].EndedAt > resp.Matches[i-1].EndedAt {
			t.Fatalf("not newest first at %d: %d after %d", i,
				resp.Matches[i].EndedAt, resp.Matches[i-1].EndedAt)
		}
	}
	newest := int64(1760000000 + (rows-1)*60)
	if resp.Matches[0].EndedAt != newest {
		t.Errorf("first match ended %d, want the newest %d",
			resp.Matches[0].EndedAt, newest)
	}
	want := matchHistoryTotals{
		Matches: rows, Wins: wantWins, Kills: wantKills, Bumps: wantBumps,
	}
	if resp.Totals != want {
		t.Errorf("totals = %+v, want %+v (every row but the broken one)",
			resp.Totals, want)
	}
}

func TestGetMatchHistoryEmptyIsAnEmptyList(t *testing.T) {
	_, out := matchHistory(t, newFakeLeaderboardNk())
	if !strings.Contains(out, `"matches":[]`) ||
		!strings.Contains(out,
			`"totals":{"matches":0,"wins":0,"kills":0,"bumps":0}`) {
		t.Errorf("response = %s", out)
	}
}

func TestGetMatchHistoryReadFailureAnswersEmpty(t *testing.T) {
	nk := newFakeLeaderboardNk()
	nk.historyErr = errors.New("connection reset")
	nk.history[testCallerID] = []*api.StorageObject{
		historyRow("req-1", 1, true, 1, 1, 1),
	}
	_, out := matchHistory(t, nk)
	if !strings.Contains(out, `"matches":[]`) ||
		!strings.Contains(out, `"matches":0`) {
		t.Errorf("response = %s", out)
	}
}
