//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
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

// fakeDeletionNk answers only StorageRead, for the SP47
// refresh-revocation tests. Any other method panics through
// the nil embedded interface.
type fakeDeletionNk struct {
	runtime.NakamaModule

	pending    bool
	readErr    error
	metricAdds int
}

func (f *fakeDeletionNk) MetricsCounterAdd(
	_ string, _ map[string]string, _ int64,
) {
	f.metricAdds++
}

func (f *fakeDeletionNk) StorageRead(
	_ context.Context, _ []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	if !f.pending {
		return nil, nil
	}
	return []*api.StorageObject{{
		Collection: accountDeletionCollection,
		Key:        accountDeletionKey,
		UserId:     "user-1",
		Value:      `{"user_id":"user-1"}`,
	}}, nil
}

func refreshCtx(userID string) context.Context {
	return context.WithValue(
		context.Background(), runtime.RUNTIME_CTX_USER_ID, userID)
}

func TestRefreshAllowedWithoutPendingDeletion(t *testing.T) {
	nk := &fakeDeletionNk{}
	in := &api.SessionRefreshRequest{
		Token: "refresh", Vars: map[string]string{"game_id": "hopnbop"}}
	out, err := beforeSessionRefresh(
		refreshCtx("user-1"), testLogger{}, nil, nk,
		newTestGames(t, nil), in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out != in {
		t.Fatalf("out = %v, want the request back", out)
	}
}

func TestRefreshRefusedWithPendingDeletion(t *testing.T) {
	nk := &fakeDeletionNk{pending: true}
	in := &api.SessionRefreshRequest{
		Token: "refresh", Vars: map[string]string{"game_id": "hopnbop"}}
	_, err := beforeSessionRefresh(
		refreshCtx("user-1"), testLogger{}, nil, nk,
		newTestGames(t, nil), in)
	if errorCode(err) != 16 {
		t.Fatalf("err = %v, want UNAUTHENTICATED (16)", err)
	}
}

func TestRefreshFailsOpenOnStorageError(t *testing.T) {
	nk := &fakeDeletionNk{readErr: errors.New("db down")}
	in := &api.SessionRefreshRequest{
		Token: "refresh", Vars: map[string]string{"game_id": "hopnbop"}}
	if _, err := beforeSessionRefresh(
		refreshCtx("user-1"), testLogger{}, nil, nk,
		newTestGames(t, nil), in); err != nil {
		t.Fatalf("err = %v, want fail-open nil", err)
	}
	if nk.metricAdds != 1 {
		t.Fatalf("metricAdds = %d, want the failure counted once",
			nk.metricAdds)
	}
}

// Nakama calls the refresh hook before it parses the refresh
// token, so the context carries no user id; the hook must read
// the uid claim from the token itself or the deletion check
// never runs (SP84).
func TestRefreshRefusedFromTokenClaimWithoutCtxUser(t *testing.T) {
	nk := &fakeDeletionNk{pending: true}
	in := &api.SessionRefreshRequest{
		Token: unsignedRefreshToken("user-1"),
		Vars:  map[string]string{"game_id": "hopnbop"}}
	_, err := beforeSessionRefresh(
		context.Background(), testLogger{}, nil, nk,
		newTestGames(t, nil), in)
	if errorCode(err) != 16 {
		t.Fatalf("err = %v, want UNAUTHENTICATED (16)", err)
	}
}

func TestRefreshAllowedFromTokenClaimWithoutPending(t *testing.T) {
	nk := &fakeDeletionNk{}
	in := &api.SessionRefreshRequest{
		Token: unsignedRefreshToken("user-1"),
		Vars:  map[string]string{"game_id": "hopnbop"}}
	if _, err := beforeSessionRefresh(
		context.Background(), testLogger{}, nil, nk,
		newTestGames(t, nil), in); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

// fakePresence is a minimal runtime.Presence for the socket
// disconnect tests.
type fakePresence struct {
	userID    string
	sessionID string
}

// Nakama tracks every socket's notifications presence as
// hidden (server/socket_ws.go), so the fake does too.
func (p *fakePresence) GetHidden() bool      { return true }
func (p *fakePresence) GetPersistence() bool { return false }
func (p *fakePresence) GetUsername() string  { return "" }
func (p *fakePresence) GetStatus() string    { return "" }
func (p *fakePresence) GetReason() runtime.PresenceReason {
	return 0
}
func (p *fakePresence) GetUserId() string    { return p.userID }
func (p *fakePresence) GetSessionId() string { return p.sessionID }
func (p *fakePresence) GetNodeId() string    { return "" }

// fakeDeleteAccountNk records the session-revocation calls a
// delete makes. Everything else the cascade touches answers
// empty or nil through the methods below; unlisted methods
// panic through the nil embedded interface.
type fakeDeleteAccountNk struct {
	runtime.NakamaModule

	presences     []runtime.Presence
	logoutErr     error
	disconnectErr error

	logouts     []string
	disconnects []string
	streamModes []uint8
	streamFlags [][2]bool
}

func (f *fakeDeleteAccountNk) AccountGetId(
	_ context.Context, _ string,
) (*api.Account, error) {
	return nil, errors.New("no account")
}

func (f *fakeDeleteAccountNk) StorageWrite(
	_ context.Context, _ []*runtime.StorageWrite,
) ([]*api.StorageObjectAck, error) {
	return nil, nil
}

func (f *fakeDeleteAccountNk) StorageRead(
	_ context.Context, _ []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	return nil, nil
}

func (f *fakeDeleteAccountNk) StorageDelete(
	_ context.Context, _ []*runtime.StorageDelete,
) error {
	return nil
}

func (f *fakeDeleteAccountNk) FriendsList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.Friend, string, error) {
	return nil, "", nil
}

func (f *fakeDeleteAccountNk) UserGroupsList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.UserGroupList_UserGroup, string, error) {
	return nil, "", nil
}

func (f *fakeDeleteAccountNk) LeaderboardRecordDelete(
	_ context.Context, _ string, _ string,
) error {
	return nil
}

func (f *fakeDeleteAccountNk) SessionLogout(
	userID, _, _ string,
) error {
	f.logouts = append(f.logouts, userID)
	return f.logoutErr
}

func (f *fakeDeleteAccountNk) StreamUserList(
	mode uint8, _ string, _ string, _ string,
	includeHidden bool, includeNotHidden bool,
) ([]runtime.Presence, error) {
	f.streamModes = append(f.streamModes, mode)
	f.streamFlags = append(
		f.streamFlags, [2]bool{includeHidden, includeNotHidden})
	// Mirror Nakama's ListByStream: with neither flag
	// the result is empty, and each presence comes
	// back only when its hidden state is included.
	var out []runtime.Presence
	for _, presence := range f.presences {
		if (presence.GetHidden() && includeHidden) ||
			(!presence.GetHidden() && includeNotHidden) {
			out = append(out, presence)
		}
	}
	return out, nil
}

func (f *fakeDeleteAccountNk) SessionDisconnect(
	_ context.Context, sessionID string,
	_ ...runtime.PresenceReason,
) error {
	f.disconnects = append(f.disconnects, sessionID)
	return f.disconnectErr
}

func deleteCtx() context.Context {
	ctx := context.WithValue(
		context.Background(), runtime.RUNTIME_CTX_USER_ID, "user-1")
	return context.WithValue(
		ctx, runtime.RUNTIME_CTX_VARS,
		map[string]string{"game_id": "hopnbop"})
}

func TestDeleteAccountRevokesSessionsAndSockets(t *testing.T) {
	nk := &fakeDeleteAccountNk{presences: []runtime.Presence{
		&fakePresence{userID: "user-1", sessionID: "sess-1"},
		&fakePresence{userID: "user-1", sessionID: "sess-2"},
	}}
	out, err := deleteAccountRpc(
		deleteCtx(), testLogger{}, nil, nk, newTestGames(t, nil), "")
	if err != nil {
		t.Fatalf("err = %v, want nil (out %s)", err, out)
	}
	if len(nk.logouts) != 1 || nk.logouts[0] != "user-1" {
		t.Fatalf("logouts = %v, want [user-1]", nk.logouts)
	}
	if len(nk.streamModes) != 1 || nk.streamModes[0] != 0 {
		t.Fatalf("streamModes = %v, want the notifications stream [0]",
			nk.streamModes)
	}
	if len(nk.disconnects) != 2 {
		t.Fatalf("disconnects = %v, want both sessions", nk.disconnects)
	}
}

func TestDeleteAccountFailsClosedOnLogoutError(t *testing.T) {
	nk := &fakeDeleteAccountNk{logoutErr: errors.New("session store down")}
	_, err := deleteAccountRpc(
		deleteCtx(), testLogger{}, nil, nk, newTestGames(t, nil), "")
	if err == nil {
		t.Fatal("err = nil, want the delete to fail closed")
	}
}

func TestDeleteAccountFailsClosedOnDisconnectError(t *testing.T) {
	nk := &fakeDeleteAccountNk{
		presences: []runtime.Presence{
			&fakePresence{userID: "user-1", sessionID: "sess-1"}},
		disconnectErr: errors.New("socket registry down"),
	}
	_, err := deleteAccountRpc(
		deleteCtx(), testLogger{}, nil, nk, newTestGames(t, nil), "")
	if err == nil {
		t.Fatal("err = nil, want the delete to fail closed")
	}
}

// unsignedRefreshToken builds a JWT-shaped token carrying only
// a uid claim. The hook reads it unverified; Nakama verifies
// the real token right after the hook runs.
func unsignedRefreshToken(userID string) string {
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	return enc(`{"alg":"HS256","typ":"JWT"}`) + "." +
		enc(`{"uid":"`+userID+`","exp":9999999999}`) + "." +
		enc("signature")
}
