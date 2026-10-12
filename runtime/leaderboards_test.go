package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// leaderboardCreateCall records one LeaderboardCreate call.
type leaderboardCreateCall struct {
	id            string
	authoritative bool
	sortOrder     string
	operator      string
	resetSchedule string
	enableRanks   bool
}

// fakeLeaderboardNk is the slice of NakamaModule the leaderboard
// code uses: board creation and reads, record listing, account
// reads, and storage reads. Boards created through it become
// readable, the way Nakama's cache behaves. Any other method panics
// through the nil embedded interface.
type fakeLeaderboardNk struct {
	runtime.NakamaModule

	creates   []leaderboardCreateCall
	createErr map[string]error
	boards    map[string]*api.Leaderboard

	// records lists each board's records in board order.
	records   map[string][]*api.LeaderboardRecord
	accounts  map[string]*api.Account
	pending   map[string]bool
	listErr   error
	listCalls int
	// listGate, when set, is received from before each listing, so
	// a test can hold a read open.
	listGate chan struct{}
	// listEntered, when set, gets a value as each listing starts.
	listEntered chan struct{}
}

func newFakeLeaderboardNk() *fakeLeaderboardNk {
	return &fakeLeaderboardNk{
		createErr: map[string]error{},
		boards:    map[string]*api.Leaderboard{},
		records:   map[string][]*api.LeaderboardRecord{},
		accounts:  map[string]*api.Account{},
		pending:   map[string]bool{},
	}
}

func (f *fakeLeaderboardNk) LeaderboardCreate(
	_ context.Context, id string, authoritative bool,
	sortOrder, operator, resetSchedule string,
	_ map[string]interface{}, enableRanks bool,
) error {
	f.creates = append(f.creates, leaderboardCreateCall{
		id: id, authoritative: authoritative, sortOrder: sortOrder,
		operator: operator, resetSchedule: resetSchedule,
		enableRanks: enableRanks,
	})
	if err := f.createErr[id]; err != nil {
		return err
	}
	if _, exists := f.boards[id]; exists {
		// Idempotent, and never changes an existing board.
		return nil
	}
	board := &api.Leaderboard{
		Id:            id,
		SortOrder:     1,
		Operator:      api.Operator_BEST,
		Authoritative: authoritative,
	}
	if sortOrder == "asc" {
		board.SortOrder = 0
	}
	switch operator {
	case "set":
		board.Operator = api.Operator_SET
	case "incr":
		board.Operator = api.Operator_INCREMENT
	case "decr":
		board.Operator = api.Operator_DECREMENT
	}
	if resetSchedule != "" {
		board.NextReset = 1767225600
	}
	f.boards[id] = board
	return nil
}

func (f *fakeLeaderboardNk) LeaderboardsGetId(
	_ context.Context, ids []string,
) ([]*api.Leaderboard, error) {
	out := []*api.Leaderboard{}
	for _, id := range ids {
		if board, ok := f.boards[id]; ok {
			out = append(out, board)
		}
	}
	return out, nil
}

func (f *fakeLeaderboardNk) LeaderboardRecordsList(
	_ context.Context, id string, ownerIDs []string, limit int,
	cursor string, _ int64,
) ([]*api.LeaderboardRecord, []*api.LeaderboardRecord, string, string,
	error) {
	if f.listEntered != nil {
		f.listEntered <- struct{}{}
	}
	if f.listGate != nil {
		<-f.listGate
	}
	f.listCalls++
	if f.listErr != nil {
		return nil, nil, "", "", f.listErr
	}
	if len(ownerIDs) != 0 {
		panic("the public leaderboard never filters by owner")
	}
	start := 0
	if cursor != "" {
		start = len(cursor)
	}
	all := f.records[id]
	if start > len(all) {
		start = len(all)
	}
	end := start + limit
	next := ""
	if end < len(all) {
		// The cursor's length is the next offset.
		next = strings.Repeat("c", end)
	} else {
		end = len(all)
	}
	return all[start:end], nil, next, "", nil
}

func (f *fakeLeaderboardNk) AccountsGetId(
	_ context.Context, userIDs []string,
) ([]*api.Account, error) {
	out := []*api.Account{}
	for _, id := range userIDs {
		if account, ok := f.accounts[id]; ok {
			out = append(out, account)
		}
	}
	return out, nil
}

func (f *fakeLeaderboardNk) StorageRead(
	_ context.Context, reads []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	out := []*api.StorageObject{}
	for _, read := range reads {
		if read.Collection != accountDeletionCollection ||
			read.Key != accountDeletionKey {
			panic("unexpected storage read: " + read.Collection)
		}
		if f.pending[read.UserID] {
			out = append(out, &api.StorageObject{
				Collection: read.Collection,
				Key:        read.Key,
				UserId:     read.UserID,
				Value:      `{"user_id":"` + read.UserID + `"}`,
			})
		}
	}
	return out, nil
}

func TestParseLeaderboardConfigs(t *testing.T) {
	t.Run("hopnbop", func(t *testing.T) {
		boards, err := parseLeaderboardConfigs([]byte(
			`{"leaderboards":[{"id":"ffa","sort":"desc",` +
				`"operator":"best","reset_schedule":null}]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		want := []leaderboardConfig{
			{ID: "ffa", SortOrder: "desc", Operator: "best"},
		}
		if !reflect.DeepEqual(boards, want) {
			t.Errorf("got %+v, want %+v", boards, want)
		}
	})
	t.Run("defaults-and-aliases", func(t *testing.T) {
		boards, err := parseLeaderboardConfigs([]byte(
			`{"leaderboards":[` +
				`{"id":"plain"},` +
				`{"id":"weekly","sort":"Ascending",` +
				`"operator":"increment","reset_schedule":" 0 0 * * 1 "},` +
				`{"id":"laps","sort":"asc","operator":"decrement"},` +
				`{"id":"latest","operator":"set"}]}`))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		want := []leaderboardConfig{
			{ID: "plain", SortOrder: "desc", Operator: "best"},
			{ID: "weekly", SortOrder: "asc", Operator: "incr",
				ResetSchedule: "0 0 * * 1"},
			{ID: "laps", SortOrder: "asc", Operator: "decr"},
			{ID: "latest", SortOrder: "desc", Operator: "set"},
		}
		if !reflect.DeepEqual(boards, want) {
			t.Errorf("got %+v, want %+v", boards, want)
		}
	})
	t.Run("none-declared", func(t *testing.T) {
		for _, raw := range []string{``, `{}`, `{"leaderboards":[]}`,
			`{"leaderboards":null}`} {
			boards, err := parseLeaderboardConfigs([]byte(raw))
			if err != nil || len(boards) != 0 {
				t.Errorf("%q: got (%+v, %v), want no boards", raw,
					boards, err)
			}
		}
	})
	invalid := map[string]string{
		"empty-id":      `{"leaderboards":[{"id":""}]}`,
		"upper-case-id": `{"leaderboards":[{"id":"FFA"}]}`,
		"dash-in-id":    `{"leaderboards":[{"id":"all-time"}]}`,
		"long-id": `{"leaderboards":[{"id":"` +
			strings.Repeat("a", 65) + `"}]}`,
		"duplicate-id":    `{"leaderboards":[{"id":"ffa"},{"id":"ffa"}]}`,
		"unknown-sort":    `{"leaderboards":[{"id":"ffa","sort":"up"}]}`,
		"unknown-op":      `{"leaderboards":[{"id":"ffa","operator":"max"}]}`,
		"not-a-list":      `{"leaderboards":"ffa"}`,
		"not-json":        `{leaderboards`,
		"numeric-reset":   `{"leaderboards":[{"id":"ffa","reset_schedule":7}]}`,
		"id-not-a-string": `{"leaderboards":[{"id":7}]}`,
	}
	for name, raw := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLeaderboardConfigs([]byte(raw)); err == nil {
				t.Errorf("%s should be rejected", raw)
			}
		})
	}
}

func TestFindLeaderboardConfig(t *testing.T) {
	games := newTestGames(t, map[string]string{
		"hopnbop": hopnbopLeaderboardConfigJSON,
		"broken":  `{"game_id":"broken","leaderboards":"ffa"}`,
	})
	gc, _ := games.Get("hopnbop")
	if lb, ok := findLeaderboardConfig(gc, "ffa"); !ok || lb.ID != "ffa" {
		t.Errorf("ffa: got (%+v, %t)", lb, ok)
	}
	if _, ok := findLeaderboardConfig(gc, "weekly"); ok {
		t.Error("an undeclared board was found")
	}
	broken, _ := games.Get("broken")
	if _, ok := findLeaderboardConfig(broken, "ffa"); ok {
		t.Error("a malformed list should declare no boards")
	}
	if _, ok := findLeaderboardConfig(nil, "ffa"); ok {
		t.Error("a nil config should declare no boards")
	}
}

func TestEnsureGameLeaderboardsCreatesAuthoritativeRankedBoards(
	t *testing.T,
) {
	nk := newFakeLeaderboardNk()
	boards := []leaderboardConfig{
		{ID: "ffa", SortOrder: "desc", Operator: "best"},
		{ID: "weekly", SortOrder: "asc", Operator: "incr",
			ResetSchedule: "0 0 * * 1"},
	}
	err := ensureGameLeaderboards(
		context.Background(), testLogger{}, nk, "hopnbop", boards)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	want := []leaderboardCreateCall{
		{id: "hopnbop_ffa", authoritative: true, sortOrder: "desc",
			operator: "best", enableRanks: true},
		{id: "hopnbop_weekly", authoritative: true, sortOrder: "asc",
			operator: "incr", resetSchedule: "0 0 * * 1",
			enableRanks: true},
	}
	if !reflect.DeepEqual(nk.creates, want) {
		t.Errorf("creates = %+v, want %+v", nk.creates, want)
	}
}

func TestEnsureGameLeaderboardsReportsEveryFailure(t *testing.T) {
	nk := newFakeLeaderboardNk()
	nk.createErr["hopnbop_bad"] = errors.New(
		"expects reset schedule to be a valid CRON expression")
	boards := []leaderboardConfig{
		{ID: "bad", SortOrder: "desc", Operator: "best",
			ResetSchedule: "whenever"},
		{ID: "ffa", SortOrder: "desc", Operator: "best"},
	}
	err := ensureGameLeaderboards(
		context.Background(), testLogger{}, nk, "hopnbop", boards)
	if err == nil || !strings.Contains(err.Error(), "hopnbop_bad") ||
		!strings.Contains(err.Error(), "CRON") {
		t.Fatalf("err = %v, want one naming hopnbop_bad", err)
	}
	if _, ok := nk.boards["hopnbop_ffa"]; !ok {
		t.Error("a failed board stopped the next one being created")
	}
}

func TestEnsureAllGameLeaderboards(t *testing.T) {
	nk := newFakeLeaderboardNk()
	games := newTestGames(t, map[string]string{
		"hopnbop": hopnbopLeaderboardConfigJSON,
		"other": `{"game_id":"other","leaderboards":[` +
			`{"id":"laps","sort":"asc"}]}`,
		"broken": `{"game_id":"broken","leaderboards":[{"id":"BAD"}]}`,
		"none":   `{"game_id":"none"}`,
	})
	ensureAllGameLeaderboards(context.Background(), testLogger{}, nk, games)
	got := []string{}
	for _, call := range nk.creates {
		got = append(got, call.id)
	}
	// games.List() is sorted by game_id.
	want := []string{"hopnbop_ffa", "other_laps"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("created %v, want %v", got, want)
	}
}

func TestLeaderboardDrift(t *testing.T) {
	want := leaderboardConfig{ID: "ffa", SortOrder: "desc", Operator: "best"}
	matching := &api.Leaderboard{
		SortOrder: 1, Operator: api.Operator_BEST, Authoritative: true,
	}
	if drift := leaderboardDrift(matching, want); len(drift) != 0 {
		t.Errorf("a matching board drifted: %v", drift)
	}
	differing := &api.Leaderboard{
		SortOrder: 0, Operator: api.Operator_INCREMENT, NextReset: 99,
	}
	drift := strings.Join(leaderboardDrift(differing, want), "; ")
	for _, part := range []string{
		"sort is asc", "operator is incr", "resets is true",
		"not authoritative",
	} {
		if !strings.Contains(drift, part) {
			t.Errorf("drift %q is missing %q", drift, part)
		}
	}
}

func TestEnsureGameLeaderboardsLeavesExistingBoardsAlone(t *testing.T) {
	// A board made some other way (the console, an older runtime)
	// keeps its settings; the drift is only logged.
	nk := newFakeLeaderboardNk()
	nk.boards["hopnbop_ffa"] = &api.Leaderboard{
		Id: "hopnbop_ffa", SortOrder: 0, Operator: api.Operator_SET,
	}
	err := ensureGameLeaderboards(
		context.Background(), testLogger{}, nk, "hopnbop",
		[]leaderboardConfig{{ID: "ffa", SortOrder: "desc", Operator: "best"}})
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if board := nk.boards["hopnbop_ffa"]; board.Operator != api.Operator_SET {
		t.Error("an existing board was changed")
	}
}

func TestLeaderboardNames(t *testing.T) {
	if leaderboardSortOrderName(0) != "asc" ||
		leaderboardSortOrderName(1) != "desc" {
		t.Error("sort order names")
	}
	names := map[api.Operator]string{
		api.Operator_BEST:      "best",
		api.Operator_SET:       "set",
		api.Operator_INCREMENT: "incr",
		api.Operator_DECREMENT: "decr",
	}
	for operator, want := range names {
		if got := leaderboardOperatorName(operator); got != want {
			t.Errorf("operator %v = %q, want %q", operator, got, want)
		}
	}
}

// TestRegisterGameRejectsInvalidLeaderboards: a bad leaderboards list
// is refused before the config is stored. The store has no DB, so a
// call that reached the upsert would panic.
func TestRegisterGameRejectsInvalidLeaderboards(t *testing.T) {
	games := &perGameConfig{
		games: map[string]*GameConfig{},
		admin: newAdminGate(testAdminKey),
	}
	ctx := ctxWithQuery(map[string][]string{"admin_key": {testAdminKey}})
	payload := `{"schema_version":1,"game_id":"hopnbop",` +
		`"display_name":"Hop 'n Bop","edgegap_app_slug":"hopnbop-server",` +
		`"protocol_version":2,"display_version":"0.48.0",` +
		`"leaderboards":[{"id":"ffa","operator":"max"}]}`
	_, err := games.RegisterGameRpc(ctx, testLogger{}, nil, nil, payload)
	if got := errorCode(err); got != 3 {
		t.Fatalf("code = %d (%v), want 3", got, err)
	}
	if !strings.Contains(err.Error(), "operator") {
		t.Errorf("want the operator error, got %v", err)
	}
}

// hopnbopLeaderboardConfigJSON is hopnbop's registered config, cut
// to the fields the leaderboard code reads.
const hopnbopLeaderboardConfigJSON = `{
	"schema_version": 1,
	"game_id": "hopnbop",
	"display_name": "Hop 'n Bop",
	"leaderboards": [
		{"id": "ffa", "sort": "desc", "operator": "best",
			"reset_schedule": null}
	]
}`
