//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// fakePrefsNk captures storage writes for party_set_prefs.
// Any other method panics through the nil embedded interface.
type fakePrefsNk struct {
	runtime.NakamaModule

	writeCalls int
	writes     []*runtime.StorageWrite
}

func (f *fakePrefsNk) GroupsGetId(
	_ context.Context, _ []string,
) ([]*api.Group, error) {
	return []*api.Group{
		{Id: "party-1", Name: "party-hopnbop", CreatorId: "u1"},
	}, nil
}

func (f *fakePrefsNk) StorageRead(
	_ context.Context, _ []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	return nil, nil
}

func (f *fakePrefsNk) StorageWrite(
	_ context.Context, writes []*runtime.StorageWrite,
) ([]*api.StorageObjectAck, error) {
	f.writeCalls++
	f.writes = writes
	return nil, nil
}

func (f *fakePrefsNk) GroupUsersList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.GroupUserList_GroupUser, string, error) {
	return nil, "", nil
}

func (f *fakePrefsNk) NotificationSend(
	_ context.Context, _ string, _ string,
	_ map[string]interface{}, _ int, _ string, _ bool,
) error {
	return nil
}

func prefsCtx(userID string) context.Context {
	ctx := context.WithValue(
		context.Background(),
		runtime.RUNTIME_CTX_USER_ID, userID)
	return context.WithValue(
		ctx, runtime.RUNTIME_CTX_VARS,
		map[string]string{"game_id": "hopnbop"})
}

func TestSetPrefsWritesAtomically(t *testing.T) {
	nk := &fakePrefsNk{}
	_, err := partySetPrefsRpc(
		prefsCtx("u1"), testLogger{}, nk, newTestGames(t, nil),
		`{"party_id":"party-1","mode_id":"ffa",`+
			`"prefs":{"inclusion":["level_1"]},`+
			`"are_cheats_enabled":true,"networked_cheats":[]}`)
	if err != nil {
		t.Fatalf("set prefs: %v", err)
	}
	if nk.writeCalls != 1 || len(nk.writes) != 3 {
		t.Fatalf(
			"calls=%d writes=%d, want 1 call of 3",
			nk.writeCalls, len(nk.writes))
	}
}

func TestSetPrefsRefusedForMember(t *testing.T) {
	nk := &fakePrefsNk{}
	_, err := partySetPrefsRpc(
		prefsCtx("u2"), testLogger{}, nk, newTestGames(t, nil),
		`{"party_id":"party-1","mode_id":"ffa"}`)
	if errorCode(err) != 7 {
		t.Fatalf("member set prefs: err = %v, want code 7", err)
	}
}
