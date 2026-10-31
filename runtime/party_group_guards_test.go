package main

import (
	"context"
	"strconv"
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

	blocked       map[string][]string
	memberships   map[string][]*api.UserGroupList_UserGroup
	members       map[string][]*api.GroupUserList_GroupUser
	added         []string
	deletedGroups []string
	// gainedMember, when set, replaces members for a group
	// once an add has landed, so a test can give a party a
	// second member between a check and its re-read.
	gainedMember map[string][]*api.GroupUserList_GroupUser
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
	case "party-2":
		return []*api.Group{
			{Id: "party-2", Name: "party-other", MaxCount: 4},
		}, nil
	}
	return nil, nil
}

func (f *fakePartyGuardNk) GroupUsersList(
	_ context.Context, groupID string, _ int, _ *int,
	_ string,
) ([]*api.GroupUserList_GroupUser, string, error) {
	if len(f.added) > 0 && f.gainedMember != nil {
		if m, ok := f.gainedMember[groupID]; ok {
			return m, "", nil
		}
	}
	if f.members != nil && groupID != "party-1" {
		return f.members[groupID], "", nil
	}
	if f.members != nil && f.members["party-1"] != nil {
		return f.members["party-1"], "", nil
	}
	return []*api.GroupUserList_GroupUser{
		{User: &api.User{Id: "leader"},
			State: wrapperspb.Int32(0)},
		{User: &api.User{Id: "inviter"},
			State: wrapperspb.Int32(2)},
		{User: &api.User{Id: "member"},
			State: wrapperspb.Int32(2)},
	}, "", nil
}

func (f *fakePartyGuardNk) GroupDelete(
	_ context.Context, groupID string,
) error {
	f.deletedGroups = append(f.deletedGroups, groupID)
	return nil
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
	_ context.Context, userID string, limit int,
	_ *int, cursor string,
) ([]*api.UserGroupList_UserGroup, string, error) {
	all := f.memberships[userID]
	offset := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, "", err
		}
		offset = n
	}
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	next := ""
	if end < len(all) {
		next = strconv.Itoa(end)
	} else {
		end = len(all)
	}
	return all[offset:end], next, nil
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

func TestPartyInviteAllowsSoloTarget(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"target": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "target"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	_, err := partyInviteRpc(
		refreshCtx("inviter"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"party_id":"party-1","target_id":"target"}`)
	if err != nil {
		t.Fatalf("solo target: err = %v, want nil", err)
	}
	if len(nk.added) != 1 || nk.added[0] != "target" {
		t.Fatalf("added = %v, want [target]", nk.added)
	}
}

func TestPartyJoinByCodeDisbandsSoloParty(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"joiner": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "joiner"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	_, err := partyJoinByCodeRpc(
		refreshCtx("joiner"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"code":"ABC234"}`)
	if err != nil {
		t.Fatalf("solo join: err = %v, want nil", err)
	}
	if len(nk.deletedGroups) != 1 ||
		nk.deletedGroups[0] != "party-2" {
		t.Fatalf(
			"deletedGroups = %v, want [party-2]",
			nk.deletedGroups)
	}
}

func TestPartyJoinByCodeRefusesSharedParty(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"joiner": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "joiner"},
					State: wrapperspb.Int32(2)},
				{User: &api.User{Id: "other"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	_, err := partyJoinByCodeRpc(
		refreshCtx("joiner"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		`{"code":"ABC234"}`)
	if errorCode(err) != 6 {
		t.Fatalf("shared join: err = %v, want code 6", err)
	}
	if len(nk.added) != 0 {
		t.Fatalf("joiner was added: %v", nk.added)
	}
}

func TestJoinGuardRefusesSharedPartyAllowsSolo(t *testing.T) {
	shared := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"joiner": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "joiner"},
					State: wrapperspb.Int32(2)},
				{User: &api.User{Id: "other"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	req := &api.JoinGroupRequest{GroupId: "party-1"}
	if _, err := beforeJoinGroupPartyGuard(
		refreshCtx("joiner"), testLogger{}, nil, shared, req,
	); errorCode(err) != 6 {
		t.Fatalf("shared join: err = %v, want code 6", err)
	}
	solo := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"joiner": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "joiner"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	if out, err := beforeJoinGroupPartyGuard(
		refreshCtx("joiner"), testLogger{}, nil, solo, req,
	); err != nil || out != req {
		t.Fatalf("solo join: out=%v err=%v", out, err)
	}
}

func TestAfterJoinDisbandsSoloParty(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"target": {
				partyUserGroup("party-1", "party-hopnbop", 2),
				partyUserGroup("party-2", "party-other", 2),
			},
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "target"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	err := afterJoinGroupHook(
		refreshCtx("target"), testLogger{}, nil, nk,
		&api.JoinGroupRequest{GroupId: "party-1"})
	if err != nil {
		t.Fatalf("after join: err = %v", err)
	}
	if len(nk.deletedGroups) != 1 ||
		nk.deletedGroups[0] != "party-2" {
		t.Fatalf(
			"deletedGroups = %v, want [party-2]",
			nk.deletedGroups)
	}
}

func TestPartyMembershipsPagesThroughGroups(t *testing.T) {
	all := []*api.UserGroupList_UserGroup{}
	for i := 0; i < 149; i++ {
		all = append(all, partyUserGroup(
			"clan-x", "clan-x", 2))
	}
	all = append(
		all, partyUserGroup("party-2", "party-other", 2))
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"target": all,
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "target"},
					State: wrapperspb.Int32(2)},
				{User: &api.User{Id: "other"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	parties, err := partyMemberships(
		context.Background(), nk, "target", "party-1")
	if err != nil {
		t.Fatalf("memberships: err = %v", err)
	}
	if parties["party-2"] != 2 {
		t.Fatalf("parties = %v, want party-2 with 2", parties)
	}
	if sharedParty(parties) != "party-2" {
		t.Fatalf("sharedParty = %v", sharedParty(parties))
	}
}

func TestCreateGroupDisbandsCallersSoloParty(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"leader": {
				partyUserGroup("party-2", "party-other", 2),
			},
		},
		members: map[string][]*api.GroupUserList_GroupUser{
			"party-2": {
				{User: &api.User{Id: "leader"},
					State: wrapperspb.Int32(2)},
			},
		},
	}
	req := &api.CreateGroupRequest{Name: "party-new"}
	out, err := beforeCreateGroupPartyGuard(
		refreshCtx("leader"), testLogger{}, nil, nk, req)
	if err != nil || out != req {
		t.Fatalf("create: out=%v err=%v", out, err)
	}
	if len(nk.deletedGroups) != 1 ||
		nk.deletedGroups[0] != "party-2" {
		t.Fatalf(
			"deletedGroups = %v, want [party-2]",
			nk.deletedGroups)
	}
}

func TestCreateGroupRefusedInSharedParty(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"member": {
				partyUserGroup("party-1", "party-hopnbop", 2),
			},
		},
	}
	req := &api.CreateGroupRequest{Name: "party-new"}
	out, err := beforeCreateGroupPartyGuard(
		refreshCtx("member"), testLogger{}, nil, nk, req)
	if out != nil || errorCode(err) != 6 {
		t.Fatalf("create: out=%v err=%v, want code 6", out, err)
	}
	if len(nk.deletedGroups) != 0 {
		t.Fatalf(
			"deletedGroups = %v, want none", nk.deletedGroups)
	}
}

func TestCreateGroupIgnoresNonPartyNames(t *testing.T) {
	nk := &fakePartyGuardNk{
		memberships: map[string][]*api.UserGroupList_UserGroup{
			"leader": {
				partyUserGroup("party-1", "party-hopnbop", 0),
			},
		},
	}
	req := &api.CreateGroupRequest{Name: "clan-new"}
	out, err := beforeCreateGroupPartyGuard(
		refreshCtx("leader"), testLogger{}, nil, nk, req)
	if err != nil || out != req {
		t.Fatalf("create: out=%v err=%v", out, err)
	}
	if len(nk.deletedGroups) != 0 {
		t.Fatalf(
			"deletedGroups = %v, want none", nk.deletedGroups)
	}
}
