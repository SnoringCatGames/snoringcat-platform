package main

import (
	"context"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakePartyGuardNk serves the party invite / join tests and
// the raw-call guard tests. Any other method panics through
// the nil embedded interface.
//
// Layout: leader leads party-1 (members leader, inviter,
// member), target is outside it. blocked maps a user to the
// users they have blocked. memberships maps a user to their
// group edges for the already-in-a-party check.
type fakePartyGuardNk struct {
	runtime.NakamaModule

	blocked     map[string][]string
	memberships map[string][]*api.UserGroupList_UserGroup
	added       []string
}

func (f *fakePartyGuardNk) GroupsGetId(
	_ context.Context, ids []string,
) ([]*api.Group, error) {
	switch ids[0] {
	case "party-1":
		return []*api.Group{
			{
				Id:        "party-1",
				Name:      "party-hopnbop",
				CreatorId: "leader",
				EdgeCount: 3,
				MaxCount:  4,
			},
		}, nil
	case "clan-1":
		return []*api.Group{
			{Id: "clan-1", Name: "clan-hopnbop"},
		}, nil
	}
	return nil, nil
}

func (f *fakePartyGuardNk) GroupUsersList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.GroupUserList_GroupUser, string, error) {
	return []*api.GroupUserList_GroupUser{
		{User: &api.User{Id: "leader"},
			State: wrapperspb.Int32(0)},
		{User: &api.User{Id: "inviter"},
			State: wrapperspb.Int32(2)},
		{User: &api.User{Id: "member"},
			State: wrapperspb.Int32(2)},
	}, "", nil
}

func (f *fakePartyGuardNk) StorageRead(
	_ context.Context, reads []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	for _, read := range reads {
		if read.Collection == partyInviteCodeCollection {
			return []*api.StorageObject{
				{Value: `{"party_id":"party-1"}`},
			}, nil
		}
	}
	return nil, nil
}

func (f *fakePartyGuardNk) StorageDelete(
	_ context.Context, _ []*runtime.StorageDelete,
) error {
	return nil
}

func (f *fakePartyGuardNk) GroupUsersAdd(
	_ context.Context, _ string, _ string, userIDs []string,
) error {
	f.added = append(f.added, userIDs...)
	return nil
}

func (f *fakePartyGuardNk) FriendsList(
	_ context.Context, userID string, _ int,
	_ *int, _ string,
) ([]*api.Friend, string, error) {
	friends := []*api.Friend{}
	for _, id := range f.blocked[userID] {
		friends = append(
			friends, &api.Friend{User: &api.User{Id: id}})
	}
	return friends, "", nil
}

func (f *fakePartyGuardNk) UserGroupsList(
	_ context.Context, userID string, _ int,
	_ *int, _ string,
) ([]*api.UserGroupList_UserGroup, string, error) {
	return f.memberships[userID], "", nil
}

func (f *fakePartyGuardNk) NotificationSend(
	_ context.Context, _ string, _ string,
	_ map[string]interface{}, _ int, _ string, _ bool,
) error {
	return nil
}

func partyUserGroup(
	id string, name string, state int32,
) *api.UserGroupList_UserGroup {
	return &api.UserGroupList_UserGroup{
		Group: &api.Group{Id: id, Name: name},
		State: wrapperspb.Int32(state),
	}
}

func TestPartyInviteRefusesBlockedTarget(t *testing.T) {
	nk := &fakePartyGuardNk{
		blocked: map[string][]string{"target": {"inviter"}},
	}
	_, err := partyInviteRpc(
		refreshCtx("inviter"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"party_id":"party-1","target_id":"target"}`)
	if errorCode(err) != 7 {
		t.Fatalf("blocked target: err = %v, want code 7", err)
	}
	if len(nk.added) != 0 {
		t.Fatalf("blocked target was added: %v", nk.added)
	}
}

func TestPartyInviteRefusesTargetInAnotherParty(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"target": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
	}
	_, err := partyInviteRpc(
		refreshCtx("inviter"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"party_id":"party-1","target_id":"target"}`)
	if errorCode(err) != 6 {
		t.Fatalf("busy target: err = %v, want code 6", err)
	}
	if len(nk.added) != 0 {
		t.Fatalf("busy target was added: %v", nk.added)
	}
}

func TestPartyInviteAllowsPendingInviteElsewhere(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"target": {
				partyUserGroup("party-2", "party-other", 3),
			},
		},
	}
	_, err := partyInviteRpc(
		refreshCtx("inviter"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"party_id":"party-1","target_id":"target"}`)
	if err != nil {
		t.Fatalf("pending target: err = %v, want nil", err)
	}
	if len(nk.added) != 1 || nk.added[0] != "target" {
		t.Fatalf("added = %v, want [target]", nk.added)
	}
}

func TestPartyJoinByCodeRefusesBlockedMember(t *testing.T) {
	nk := &fakePartyGuardNk{
		blocked: map[string][]string{"member": {"joiner"}},
	}
	_, err := partyJoinByCodeRpc(
		refreshCtx("joiner"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"code":"ABC234"}`)
	if errorCode(err) != 7 {
		t.Fatalf("blocked member: err = %v, want code 7", err)
	}
	if len(nk.added) != 0 {
		t.Fatalf("joiner was added: %v", nk.added)
	}
}

func TestPartyJoinByCodeAllowsUnblockedJoiner(t *testing.T) {
	nk := &fakePartyGuardNk{}
	_, err := partyJoinByCodeRpc(
		refreshCtx("joiner"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"code":"ABC234"}`)
	if err != nil {
		t.Fatalf("clean join: err = %v, want nil", err)
	}
	if len(nk.added) != 1 || nk.added[0] != "joiner" {
		t.Fatalf("added = %v, want [joiner]", nk.added)
	}
}

func TestPartyGuardRefusesRawCallsOnPartyGroups(t *testing.T) {
	nk := &fakePartyGuardNk{}
	ctx := refreshCtx("leader")
	add := &api.AddGroupUsersRequest{
		GroupId: "party-1", UserIds: []string{"target"}}
	if out, err := beforeAddGroupUsersPartyGuard(
		ctx, testLogger{}, nil, nk, add,
	); err != nil || out != add {
		t.Fatalf("leader add: out=%v err=%v", out, err)
	}
	if _, err := beforeAddGroupUsersPartyGuard(
		refreshCtx("inviter"), testLogger{}, nil, nk, add,
	); errorCode(err) != 7 {
		t.Fatalf("member add: err = %v, want code 7", err)
	}
	blockedNk := &fakePartyGuardNk{
		blocked: map[string][]string{"leader": {"target"}},
	}
	if _, err := beforeAddGroupUsersPartyGuard(
		ctx, testLogger{}, nil, blockedNk, add,
	); errorCode(err) != 7 {
		t.Fatalf("blocked add: err = %v, want code 7", err)
	}
	busyNk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"target": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
	}
	if _, err := beforeAddGroupUsersPartyGuard(
		ctx, testLogger{}, nil, busyNk, add,
	); errorCode(err) != 6 {
		t.Fatalf("busy add: err = %v, want code 6", err)
	}

	if _, err := beforeBanGroupUsersPartyGuard(
		ctx, testLogger{}, nil, nk,
		&api.BanGroupUsersRequest{GroupId: "party-1"},
	); errorCode(err) != 7 {
		t.Fatalf("ban: err = %v, want code 7", err)
	}
	if _, err := beforePromoteGroupUsersPartyGuard(
		ctx, testLogger{}, nil, nk,
		&api.PromoteGroupUsersRequest{GroupId: "party-1"},
	); errorCode(err) != 7 {
		t.Fatalf("promote: err = %v, want code 7", err)
	}
	if _, err := beforeDemoteGroupUsersPartyGuard(
		ctx, testLogger{}, nil, nk,
		&api.DemoteGroupUsersRequest{GroupId: "party-1"},
	); errorCode(err) != 7 {
		t.Fatalf("demote: err = %v, want code 7", err)
	}
	if _, err := beforeUpdateGroupPartyGuard(
		ctx, testLogger{}, nil, nk,
		&api.UpdateGroupRequest{GroupId: "party-1"},
	); errorCode(err) != 7 {
		t.Fatalf("update: err = %v, want code 7", err)
	}
	if _, err := beforeDeleteGroupPartyGuard(
		ctx, testLogger{}, nil, nk,
		&api.DeleteGroupRequest{GroupId: "party-1"},
	); errorCode(err) != 7 {
		t.Fatalf("delete: err = %v, want code 7", err)
	}
}

func TestPartyGuardPassesOtherGroups(t *testing.T) {
	nk := &fakePartyGuardNk{}
	ctx := refreshCtx("leader")
	add := &api.AddGroupUsersRequest{GroupId: "clan-1"}
	if out, err := beforeAddGroupUsersPartyGuard(
		ctx, testLogger{}, nil, nk, add,
	); err != nil || out != add {
		t.Fatalf("add clan: out=%v err=%v", out, err)
	}
	del := &api.DeleteGroupRequest{GroupId: "clan-1"}
	if out, err := beforeDeleteGroupPartyGuard(
		ctx, testLogger{}, nil, nk, del,
	); err != nil || out != del {
		t.Fatalf("delete clan: out=%v err=%v", out, err)
	}
	upd := &api.UpdateGroupRequest{GroupId: "missing-1"}
	if out, err := beforeUpdateGroupPartyGuard(
		ctx, testLogger{}, nil, nk, upd,
	); err != nil || out != upd {
		t.Fatalf("update missing: out=%v err=%v", out, err)
	}
}
