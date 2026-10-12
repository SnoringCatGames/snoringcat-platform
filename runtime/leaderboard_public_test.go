package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testBoardID = "hopnbop_ffa"

// testClock is a settable clock for the cache.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// publicLeaderboardFixture is a handler over a fake Nakama holding
// hopnbop's ffa board, with a settable clock. boards is the shared
// reader, for get_player_stats tests.
type publicLeaderboardFixture struct {
	nk      *fakeLeaderboardNk
	clock   *testClock
	games   *perGameConfig
	boards  *publicLeaderboardReader
	handler http.HandlerFunc
}

func newPublicLeaderboardFixture(t *testing.T) *publicLeaderboardFixture {
	t.Helper()
	nk := newFakeLeaderboardNk()
	nk.boards[testBoardID] = &api.Leaderboard{
		Id: testBoardID, SortOrder: 1, Operator: api.Operator_BEST,
		Authoritative: true,
	}
	clock := &testClock{now: time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)}
	boards := newPublicLeaderboardReader(testLogger{}, nk)
	boards.cache.now = clock.Now
	games := newTestGames(t, map[string]string{
		"hopnbop": hopnbopLeaderboardConfigJSON,
	})
	return &publicLeaderboardFixture{
		nk:      nk,
		clock:   clock,
		games:   games,
		boards:  boards,
		handler: publicLeaderboardHTTPHandler(boards, games),
	}
}

// addPlayer gives the board a record for a listable player, in board
// order (call it in descending score order).
func (f *publicLeaderboardFixture) addPlayer(
	id, username, displayName string, score int64,
) {
	f.nk.accounts[id] = &api.Account{
		User: &api.User{
			Id:          id,
			Username:    username,
			DisplayName: displayName,
			AvatarUrl:   "https://example.invalid/avatar/" + id,
		},
		Email:   username + "@example.invalid",
		Devices: []*api.AccountDevice{{Id: "device-" + id}},
	}
	f.nk.records[testBoardID] = append(f.nk.records[testBoardID],
		&api.LeaderboardRecord{
			LeaderboardId: testBoardID,
			OwnerId:       id,
			Username:      nil,
			Score:         score,
			Metadata:      `{"kills":3,"bumps":4}`,
		})
}

// get runs one GET with the given query string.
func (f *publicLeaderboardFixture) get(query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		publicLeaderboardHTTPPath+query, nil)
	req.Header.Set("Origin", "https://hopnbop.net")
	rec := httptest.NewRecorder()
	f.handler(rec, req)
	return rec
}

// decode parses a 200 response.
func decodePublicLeaderboard(
	t *testing.T,
	rec *httptest.ResponseRecorder,
) publicLeaderboardResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp publicLeaderboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return resp
}

const hopnbopQuery = "?game_id=hopnbop&leaderboard=ffa"

func TestPublicLeaderboardValidation(t *testing.T) {
	cases := []struct {
		name   string
		query  string
		status int
		code   string
	}{
		{"no-params", "", 400, "missing_game_id"},
		{"empty-game", "?game_id=&leaderboard=ffa", 400, "missing_game_id"},
		{"no-leaderboard", "?game_id=hopnbop", 400, "missing_leaderboard"},
		{"limit-zero", hopnbopQuery + "&limit=0", 400, "invalid_limit"},
		{"limit-negative", hopnbopQuery + "&limit=-5", 400, "invalid_limit"},
		{"limit-over-max", hopnbopQuery + "&limit=101", 400, "invalid_limit"},
		{"limit-word", hopnbopQuery + "&limit=all", 400, "invalid_limit"},
		{"limit-empty", hopnbopQuery + "&limit=", 400, "invalid_limit"},
		{"limit-fraction", hopnbopQuery + "&limit=2.5", 400, "invalid_limit"},
		{"unknown-game", "?game_id=ghost&leaderboard=ffa", 404, "unknown_game"},
		{"unknown-board", "?game_id=hopnbop&leaderboard=weekly", 404,
			"unknown_leaderboard"},
		{"prefixed-board", "?game_id=hopnbop&leaderboard=hopnbop_ffa", 404,
			"unknown_leaderboard"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicLeaderboardFixture(t)
			rec := f.get(tc.query)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)",
					rec.Code, tc.status, rec.Body.String())
			}
			var body publicLeaderboardError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
			if body.Version != publicLeaderboardVersion ||
				body.Error != tc.code || body.Message == "" {
				t.Errorf("body = %+v, want error %q", body, tc.code)
			}
			assertPublicHeaders(t, rec)
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if f.nk.listCalls != 0 {
				t.Error("a refused request read the leaderboard")
			}
		})
	}
}

func TestPublicLeaderboardErrorsDoNotEchoInput(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	rec := f.get("?game_id=%3Cscript%3E&leaderboard=ffa")
	if strings.Contains(rec.Body.String(), "script") {
		t.Errorf("error body echoes the request: %s", rec.Body.String())
	}
}

// assertPublicHeaders checks the headers every response carries.
func assertPublicHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Cross-Origin-Resource-Policy": "cross-origin",
		"X-Content-Type-Options":       "nosniff",
		"Content-Type":                 "application/json",
	}
	for name, value := range want {
		if got := rec.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}

func TestPublicLeaderboardResponseShape(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "alpha_user",
		"Alpha", 300)
	rec := f.get(hopnbopQuery)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	assertPublicHeaders(t, rec)
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", cc)
	}
	if age := rec.Header().Get("Age"); age != "0" {
		t.Errorf("Age = %q, want 0 for a fresh read", age)
	}

	// The exact field allowlist, at both levels.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wantTop := []string{"entries", "game_id", "generated_at",
		"leaderboard", "next_reset_at", "operator", "sort_order", "version"}
	if got := sortedKeys(raw); !reflect.DeepEqual(got, wantTop) {
		t.Errorf("top-level keys = %v, want %v", got, wantTop)
	}
	entries, _ := raw["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", raw["entries"])
	}
	entry, _ := entries[0].(map[string]any)
	if got := sortedKeys(entry); !reflect.DeepEqual(got,
		[]string{"display_name", "rank", "score"}) {
		t.Errorf("entry keys = %v", got)
	}

	resp := decodePublicLeaderboard(t, rec)
	want := publicLeaderboardResponse{
		Version:     1,
		GameID:      "hopnbop",
		Leaderboard: "ffa",
		SortOrder:   "desc",
		Operator:    "best",
		NextResetAt: nil,
		GeneratedAt: "2026-10-12T04:00:00Z",
		Entries: []publicLeaderboardEntry{
			{Rank: 1, DisplayName: "Alpha", Score: 300},
		},
	}
	if !reflect.DeepEqual(resp, want) {
		t.Errorf("response = %+v, want %+v", resp, want)
	}
	if !strings.Contains(rec.Body.String(), `"next_reset_at":null`) {
		t.Errorf("a board that never resets needs next_reset_at null: %s",
			rec.Body.String())
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestPublicLeaderboardEmptyBoardIsAnEmptyList(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	rec := f.get(hopnbopQuery)
	if !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Errorf("want entries [], got %s", rec.Body.String())
	}
}

func TestPublicLeaderboardListsOnlyListablePlayers(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	// Board order, best first. Only Alpha, Bravo, and Charlie may be
	// listed; every other record would outrank them.
	f.addPlayer("00000000-0000-0000-0000-00000000000a", "deleted_user",
		anonymizedDisplayName, 990)
	f.addPlayer("00000000-0000-0000-0000-00000000000b", "pending_user",
		"Renamed During Grace", 980)
	f.nk.pending["00000000-0000-0000-0000-00000000000b"] = true
	f.addPlayer("00000000-0000-0000-0000-00000000000c", "banned_user",
		"Banned", 970)
	f.nk.accounts["00000000-0000-0000-0000-00000000000c"].DisableTime =
		&timestamppb.Timestamp{Seconds: 1760000000}
	f.addPlayer("00000000-0000-0000-0000-00000000000d", "probe-desk-0",
		"Probe Bunny", 960)
	f.addPlayer("00000000-0000-0000-0000-00000000000e", "Probe-Desk-1",
		"Probe Shouting", 955)
	f.addPlayer("00000000-0000-0000-0000-00000000000f", "anon_user", "", 950)
	f.addPlayer("00000000-0000-0000-0000-000000000010", "blank_user",
		" \t ", 940)
	f.addPlayer("00000000-0000-0000-0000-000000000011", "deleted_spaced",
		"  [deleted] ", 935)
	// A record whose account is gone.
	f.addPlayer("00000000-0000-0000-0000-000000000012", "gone_user",
		"Gone", 930)
	delete(f.nk.accounts, "00000000-0000-0000-0000-000000000012")
	f.addPlayer("11111111-1111-1111-1111-111111111111", "alpha_user",
		"  Alpha  ", 300)
	f.addPlayer("22222222-2222-2222-2222-222222222222", "bravo_user",
		"Bravo", 300)
	f.addPlayer("33333333-3333-3333-3333-333333333333", "probelike",
		"Charlie", 100)

	resp := decodePublicLeaderboard(t, f.get(hopnbopQuery))
	want := []publicLeaderboardEntry{
		{Rank: 1, DisplayName: "Alpha", Score: 300},
		{Rank: 1, DisplayName: "Bravo", Score: 300},
		{Rank: 3, DisplayName: "Charlie", Score: 100},
	}
	if !reflect.DeepEqual(resp.Entries, want) {
		t.Errorf("entries = %+v, want %+v", resp.Entries, want)
	}
}

func TestPublicLeaderboardNeverRevealsIdentifiers(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	ids := []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
	}
	f.addPlayer(ids[0], "alpha_user", "Alpha", 300)
	f.addPlayer(ids[1], "bravo_user", "Bravo", 200)
	body := f.get(hopnbopQuery).Body.String()
	for _, id := range ids {
		for _, secret := range []string{
			id, "device-" + id, "avatar/" + id,
		} {
			if strings.Contains(body, secret) {
				t.Errorf("response reveals %q: %s", secret, body)
			}
		}
	}
	for _, secret := range []string{
		"alpha_user", "bravo_user", "@example.invalid", "kills", "bumps",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("response reveals %q: %s", secret, body)
		}
	}
}

func TestPublicLeaderboardEscapesHTMLInNames(t *testing.T) {
	// JSON with <, >, and & escaped can't be read as markup even by
	// a careless consumer.
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "x",
		`<img src=x onerror=alert(1)>&`, 10)
	rec := f.get(hopnbopQuery)
	if strings.ContainsAny(rec.Body.String(), "<>&") {
		t.Errorf("raw markup characters in %s", rec.Body.String())
	}
	resp := decodePublicLeaderboard(t, rec)
	if resp.Entries[0].DisplayName != `<img src=x onerror=alert(1)>&` {
		t.Errorf("name did not round-trip: %q", resp.Entries[0].DisplayName)
	}
}

func TestPublicLeaderboardLimits(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	// 160 listed players across two pages, plus hidden ones mixed in,
	// so the read has to page to collect 100.
	for i := 0; i < 160; i++ {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		name := fmt.Sprintf("Player %03d", i)
		if i%4 == 0 {
			name = ""
		}
		f.addPlayer(id, fmt.Sprintf("user%d", i), name, int64(10000-i))
	}
	cases := map[string]int{
		"":           publicLeaderboardDefaultLimit,
		"&limit=1":   1,
		"&limit=7":   7,
		"&limit=100": 100,
	}
	for query, want := range cases {
		resp := decodePublicLeaderboard(t, f.get(hopnbopQuery+query))
		if len(resp.Entries) != want {
			t.Errorf("%q: %d entries, want %d", query, len(resp.Entries), want)
			continue
		}
		for i, entry := range resp.Entries {
			if entry.Rank != int64(i+1) {
				t.Errorf("%q: entry %d has rank %d", query, i, entry.Rank)
				break
			}
			if entry.DisplayName == "" {
				t.Errorf("%q: entry %d has no name", query, i)
				break
			}
		}
	}
	// Every limit was served from one read, which needed two pages to
	// list 100 players (75 per 100 records are listable).
	if f.nk.listCalls != 2 {
		t.Errorf("listed %d pages, want 2 (one read)", f.nk.listCalls)
	}
}

func TestPublicLeaderboardStopsPagingAtTheCap(t *testing.T) {
	// A board where nobody is listable must not be read end to end.
	f := newPublicLeaderboardFixture(t)
	for i := 0; i < 1000; i++ {
		f.addPlayer(fmt.Sprintf("00000000-0000-0000-0000-%012d", i),
			fmt.Sprintf("user%d", i), "", int64(5000-i))
	}
	resp := decodePublicLeaderboard(t, f.get(hopnbopQuery))
	if len(resp.Entries) != 0 {
		t.Errorf("listed %d nameless players", len(resp.Entries))
	}
	if f.nk.listCalls != publicLeaderboardMaxPages {
		t.Errorf("read %d pages, want %d", f.nk.listCalls,
			publicLeaderboardMaxPages)
	}
}

func TestPublicLeaderboardCachesEachBoardForItsTTL(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "a", "Alpha", 300)
	first := decodePublicLeaderboard(t, f.get(hopnbopQuery))

	// A new score inside the TTL isn't visible yet.
	f.addPlayer("22222222-2222-2222-2222-222222222222", "b", "Bravo", 200)
	f.clock.Advance(59 * time.Second)
	rec := f.get(hopnbopQuery + "&limit=10")
	cached := decodePublicLeaderboard(t, rec)
	if len(cached.Entries) != 1 || cached.GeneratedAt != first.GeneratedAt {
		t.Errorf("served a new read inside the TTL: %+v", cached)
	}
	if age := rec.Header().Get("Age"); age != "59" {
		t.Errorf("Age = %q, want 59", age)
	}
	if f.nk.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1", f.nk.listCalls)
	}

	// At the TTL it is read again.
	f.clock.Advance(time.Second)
	fresh := decodePublicLeaderboard(t, f.get(hopnbopQuery))
	if len(fresh.Entries) != 2 || fresh.GeneratedAt == first.GeneratedAt {
		t.Errorf("not re-read after the TTL: %+v", fresh)
	}
	if f.nk.listCalls != 2 {
		t.Errorf("listCalls = %d, want 2", f.nk.listCalls)
	}
}

func TestPublicLeaderboardInvalidateRereadsInsideTheTTL(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "a", "Alpha", 300)
	decodePublicLeaderboard(t, f.get(hopnbopQuery))

	// match_end records a score and invalidates: the next request
	// reads it, well inside the TTL.
	f.addPlayer("22222222-2222-2222-2222-222222222222", "b", "Bravo", 200)
	f.clock.Advance(5 * time.Second)
	f.boards.invalidate()
	rec := f.get(hopnbopQuery)
	fresh := decodePublicLeaderboard(t, rec)
	if len(fresh.Entries) != 2 {
		t.Errorf("served the invalidated copy: %+v", fresh)
	}
	if age := rec.Header().Get("Age"); age != "0" {
		t.Errorf("Age = %q, want 0", age)
	}

	// The new copy is cached as usual.
	f.clock.Advance(5 * time.Second)
	decodePublicLeaderboard(t, f.get(hopnbopQuery))
	if f.nk.listCalls != 2 {
		t.Errorf("listCalls = %d, want 2", f.nk.listCalls)
	}
}

func TestPublicLeaderboardInvalidationDuringAReadForcesAnother(
	t *testing.T,
) {
	// A score recorded while a read is in flight may be missing from
	// it, so that read's copy must not answer the next request.
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "a", "Alpha", 300)
	f.nk.listGate = make(chan struct{})
	f.nk.listEntered = make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.get(hopnbopQuery)
	}()
	<-f.nk.listEntered
	f.boards.invalidate()
	close(f.nk.listGate)
	<-done

	decodePublicLeaderboard(t, f.get(hopnbopQuery))
	if f.nk.listCalls != 2 {
		t.Errorf("listCalls = %d, want 2: the copy read across the"+
			" invalidation was served", f.nk.listCalls)
	}
}

func TestPublicLeaderboardConcurrentRequestsShareOneRead(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "a", "Alpha", 300)
	f.nk.listGate = make(chan struct{})
	f.nk.listEntered = make(chan struct{}, 64)

	const requests = 20
	var wg sync.WaitGroup
	codes := make([]int, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.get(hopnbopQuery).Code
		}(i)
	}
	// One read starts; hold it open while the others pile up.
	<-f.nk.listEntered
	time.Sleep(50 * time.Millisecond)
	close(f.nk.listGate)
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: status %d", i, code)
		}
	}
	if f.nk.listCalls != 1 {
		t.Errorf("%d concurrent requests made %d reads, want 1",
			requests, f.nk.listCalls)
	}
}

func TestPublicLeaderboardServesStaleCopyWhileReadsFail(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "a", "Alpha", 300)
	first := decodePublicLeaderboard(t, f.get(hopnbopQuery))

	f.nk.listErr = errors.New("connection refused")
	f.clock.Advance(2 * time.Minute)
	rec := f.get(hopnbopQuery)
	stale := decodePublicLeaderboard(t, rec)
	if stale.GeneratedAt != first.GeneratedAt || len(stale.Entries) != 1 {
		t.Errorf("want the last good copy, got %+v", stale)
	}
	if age := rec.Header().Get("Age"); age != "120" {
		t.Errorf("Age = %q, want 120 (older than max-age)", age)
	}
	if f.nk.listCalls != 2 {
		t.Fatalf("listCalls = %d, want 2", f.nk.listCalls)
	}

	// Inside the retry interval nothing is read.
	f.clock.Advance(publicLeaderboardRetryInterval - time.Second)
	decodePublicLeaderboard(t, f.get(hopnbopQuery))
	if f.nk.listCalls != 2 {
		t.Errorf("retried inside the interval: listCalls = %d",
			f.nk.listCalls)
	}

	// Past the stale limit the route says it is unavailable.
	f.clock.Advance(publicLeaderboardStaleTTL)
	rec = f.get(hopnbopQuery)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code,
			rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "10" {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	assertPublicHeaders(t, rec)

	// It recovers on the next read after the interval.
	f.nk.listErr = nil
	f.clock.Advance(publicLeaderboardRetryInterval)
	if resp := decodePublicLeaderboard(t, f.get(hopnbopQuery)); len(
		resp.Entries) != 1 {
		t.Errorf("did not recover: %+v", resp)
	}
}

func TestPublicLeaderboardFirstReadFailureIsUnavailable(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.nk.listErr = errors.New("connection refused")
	rec := f.get(hopnbopQuery)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body publicLeaderboardError
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Error != "unavailable" {
		t.Errorf("error = %q", body.Error)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Error("the response leaks the internal error")
	}
}

func TestPublicLeaderboardBoardNeverCreated(t *testing.T) {
	// Declared in game.yaml but missing in Nakama: a server fault,
	// not an empty board.
	f := newPublicLeaderboardFixture(t)
	delete(f.nk.boards, testBoardID)
	if rec := f.get(hopnbopQuery); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestPublicLeaderboardReportsLiveBoardSettings(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.nk.boards[testBoardID] = &api.Leaderboard{
		Id: testBoardID, SortOrder: 0, Operator: api.Operator_INCREMENT,
		NextReset: 1792800000, Authoritative: true,
	}
	resp := decodePublicLeaderboard(t, f.get(hopnbopQuery))
	if resp.SortOrder != "asc" || resp.Operator != "incr" {
		t.Errorf("settings = %s/%s", resp.SortOrder, resp.Operator)
	}
	if resp.NextResetAt == nil || *resp.NextResetAt != "2026-10-24T00:00:00Z" {
		t.Errorf("next_reset_at = %v", resp.NextResetAt)
	}
}

func TestPublicLeaderboardMethods(t *testing.T) {
	f := newPublicLeaderboardFixture(t)
	f.addPlayer("11111111-1111-1111-1111-111111111111", "a", "Alpha", 300)

	options := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions,
		publicLeaderboardHTTPPath+hopnbopQuery, nil)
	req.Header.Set("Origin", "https://hopnbop.net")
	req.Header.Set("Access-Control-Request-Method", "GET")
	f.handler(options, req)
	if options.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d", options.Code)
	}
	if got := options.Header().Get("Access-Control-Allow-Methods"); got !=
		publicLeaderboardAllowedMethods {
		t.Errorf("Access-Control-Allow-Methods = %q", got)
	}
	if options.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("OPTIONS lacks Access-Control-Allow-Origin")
	}

	head := httptest.NewRecorder()
	f.handler(head, httptest.NewRequest(http.MethodHead,
		publicLeaderboardHTTPPath+hopnbopQuery, nil))
	if head.Code != http.StatusOK ||
		head.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("HEAD = %d %v", head.Code, head.Header())
	}

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
	} {
		rec := httptest.NewRecorder()
		f.handler(rec, httptest.NewRequest(method,
			publicLeaderboardHTTPPath+hopnbopQuery, strings.NewReader("{}")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
		if rec.Header().Get("Allow") != publicLeaderboardAllowedMethods {
			t.Errorf("%s Allow = %q", method, rec.Header().Get("Allow"))
		}
	}
}

func TestRankPublicLeaderboard(t *testing.T) {
	c := func(name string, score, subscore int64) publicLeaderboardCandidate {
		return publicLeaderboardCandidate{
			displayName: name, score: score, subscore: subscore,
		}
	}
	cases := []struct {
		name  string
		in    []publicLeaderboardCandidate
		ranks []int64
	}{
		{"empty", nil, []int64{}},
		{"distinct", []publicLeaderboardCandidate{
			c("a", 3, 0), c("b", 2, 0), c("c", 1, 0)}, []int64{1, 2, 3}},
		{"tie-in-middle", []publicLeaderboardCandidate{
			c("a", 3, 0), c("b", 2, 0), c("c", 2, 0), c("d", 1, 0)},
			[]int64{1, 2, 2, 4}},
		{"all-tied", []publicLeaderboardCandidate{
			c("a", 5, 0), c("b", 5, 0), c("c", 5, 0)}, []int64{1, 1, 1}},
		{"subscore-breaks-tie", []publicLeaderboardCandidate{
			c("a", 5, 2), c("b", 5, 1), c("c", 5, 1)}, []int64{1, 2, 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rankPublicLeaderboard(tc.in)
			ranks := make([]int64, len(got))
			for i, entry := range got {
				ranks[i] = entry.Rank
				if entry.DisplayName != tc.in[i].displayName ||
					entry.Score != tc.in[i].score {
					t.Errorf("entry %d = %+v, from %+v", i, entry, tc.in[i])
				}
			}
			if !reflect.DeepEqual(ranks, tc.ranks) {
				t.Errorf("ranks = %v, want %v", ranks, tc.ranks)
			}
		})
	}
}
