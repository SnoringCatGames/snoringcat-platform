//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeKickNk answers the kick hook's reads. u1 leads, u2 is a
// member, u3 was invited. Any other method panics through the
// nil embedded interface.
type fakeKickNk struct {
	runtime.NakamaModule
}

func (f *fakeKickNk) GroupsGetId(
	_ context.Context, _ []string,
) ([]*api.Group, error) {
	return []*api.Group{
		{Id: "party-1", Name: "party-hopnbop", CreatorId: "u1"},
	}, nil
}

func (f *fakeKickNk) StorageRead(
	_ context.Context, _ []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	return nil, nil
}

func (f *fakeKickNk) GroupUsersList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.GroupUserList_GroupUser, string, error) {
	return []*api.GroupUserList_GroupUser{
		{User: &api.User{Id: "u1"}, State: wrapperspb.Int32(0)},
		{User: &api.User{Id: "u2"}, State: wrapperspb.Int32(2)},
		{User: &api.User{Id: "u3"}, State: wrapperspb.Int32(3)},
	}, "", nil
}

func kickCtx(userID string) context.Context {
	return context.WithValue(
		context.Background(),
		runtime.RUNTIME_CTX_USER_ID, userID)
}

func kickReq(targets ...string) *api.KickGroupUsersRequest {
	return &api.KickGroupUsersRequest{
		GroupId: "party-1", UserIds: targets}
}

func TestLeaderCanKickMember(t *testing.T) {
	in := kickReq("u2")
	out, err := beforeKickGroupUsersHook(
		kickCtx("u1"), testLogger{}, nil, &fakeKickNk{}, in)
	if err != nil || out != in {
		t.Fatalf("leader kick: out=%v err=%v", out, err)
	}
}

func TestMemberCannotKick(t *testing.T) {
	_, err := beforeKickGroupUsersHook(
		kickCtx("u2"), testLogger{}, nil, &fakeKickNk{},
		kickReq("u3"))
	if errorCode(err) != 7 {
		t.Fatalf("member kick: err = %v, want code 7", err)
	}
}

func TestLeaderCannotBeKicked(t *testing.T) {
	_, err := beforeKickGroupUsersHook(
		kickCtx("u1"), testLogger{}, nil, &fakeKickNk{},
		kickReq("u1"))
	if errorCode(err) != 7 {
		t.Fatalf("leader target: err = %v, want code 7", err)
	}
}

func TestKickNeedsMemberTarget(t *testing.T) {
	_, err := beforeKickGroupUsersHook(
		kickCtx("u1"), testLogger{}, nil, &fakeKickNk{},
		kickReq("ghost"))
	if errorCode(err) != 5 {
		t.Fatalf("ghost target: err = %v, want code 5", err)
	}
}
