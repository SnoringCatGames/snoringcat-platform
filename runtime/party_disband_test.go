//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeDisbandNk answers the reads and writes party_disband
// makes. Any other method panics through the nil embedded
// interface (which never calls home: notify is best-effort
// through NotificationSend below).
type fakeDisbandNk struct {
	runtime.NakamaModule

	deletedGroup string
	deletes      []*runtime.StorageDelete
}

func (f *fakeDisbandNk) GroupsGetId(
	_ context.Context, _ []string,
) ([]*api.Group, error) {
	return []*api.Group{
		{Id: "party-1", Name: "party-hopnbop", CreatorId: "u1"},
	}, nil
}

func (f *fakeDisbandNk) StorageRead(
	_ context.Context, _ []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	return nil, nil
}

func (f *fakeDisbandNk) GroupUsersList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.GroupUserList_GroupUser, string, error) {
	return []*api.GroupUserList_GroupUser{
		{User: &api.User{Id: "u1"}, State: wrapperspb.Int32(0)},
		{User: &api.User{Id: "u2"}, State: wrapperspb.Int32(2)},
	}, "", nil
}

func (f *fakeDisbandNk) NotificationSend(
	_ context.Context, _ string, _ string,
	_ map[string]interface{}, _ int, _ string, _ bool,
) error {
	return nil
}

func (f *fakeDisbandNk) GroupDelete(
	_ context.Context, id string,
) error {
	f.deletedGroup = id
	return nil
}

func (f *fakeDisbandNk) StorageDelete(
	_ context.Context, deletes []*runtime.StorageDelete,
) error {
	f.deletes = deletes
	return nil
}

func disbandCtx(userID string) context.Context {
	ctx := context.WithValue(
		context.Background(),
		runtime.RUNTIME_CTX_USER_ID, userID)
	return context.WithValue(
		ctx, runtime.RUNTIME_CTX_VARS,
		map[string]string{"game_id": "hopnbop"})
}

func TestPartyDisbandByLeader(t *testing.T) {
	nk := &fakeDisbandNk{}
	out, err := partyDisbandRpc(
		disbandCtx("u1"), testLogger{}, nk,
		newTestGames(t, nil), `{"party_id":"party-1"}`)
	if err != nil {
		t.Fatalf("disband: %v (out %s)", err, out)
	}
	if nk.deletedGroup != "party-1" {
		t.Fatalf("deleted group = %q, want party-1", nk.deletedGroup)
	}
	if len(nk.deletes) == 0 {
		t.Fatalf("no party rows deleted")
	}
}

func TestPartyDisbandRefusedForMember(t *testing.T) {
	nk := &fakeDisbandNk{}
	_, err := partyDisbandRpc(
		disbandCtx("u2"), testLogger{}, nk,
		newTestGames(t, nil), `{"party_id":"party-1"}`)
	if errorCode(err) != 7 {
		t.Fatalf("member disband: err = %v, want code 7", err)
	}
	if nk.deletedGroup != "" {
		t.Fatalf("group deleted by a non-leader")
	}
}
