package main

import (
	"context"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeStartNk serves the party start / ready-row tests.
// leader leads party-1 with member in it. ready holds the
// party_ready rows as userID -> ready; StorageDelete drops
// them and records the order. Any other method panics
// through the nil embedded interface.
type fakeStartNk struct {
	runtime.NakamaModule

	ready   map[string]bool
	deleted []string
}

func newFakeStartNk() *fakeStartNk {
	return &fakeStartNk{ready: map[string]bool{}}
}

func (f *fakeStartNk) GroupsGetId(
	_ context.Context, _ []string,
) ([]*api.Group, error) {
	return []*api.Group{
		{
			Id:        "party-1",
			Name:      "party-hopnbop",
			CreatorId: "leader",
			EdgeCount: 2,
			MaxCount:  4,
		},
	}, nil
}

func (f *fakeStartNk) GroupUsersList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.GroupUserList_GroupUser, string, error) {
	return []*api.GroupUserList_GroupUser{
		{User: &api.User{Id: "leader"},
			State: wrapperspb.Int32(0)},
		{User: &api.User{Id: "member"},
			State: wrapperspb.Int32(2)},
	}, "", nil
}

func (f *fakeStartNk) StorageRead(
	_ context.Context, reads []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	objects := []*api.StorageObject{}
	for _, read := range reads {
		if read.Collection != partyReadyCollection {
			continue
		}
		if f.ready[read.UserID] {
			objects = append(objects, &api.StorageObject{
				Collection: partyReadyCollection,
				Key:        read.Key,
				UserId:     read.UserID,
				Value:      `{"ready":true}`,
			})
		}
	}
	return objects, nil
}

func (f *fakeStartNk) StorageDelete(
	_ context.Context, deletes []*runtime.StorageDelete,
) error {
	for _, d := range deletes {
		if d.Collection != partyReadyCollection {
			continue
		}
		f.deleted = append(f.deleted, d.UserID)
		f.ready[d.UserID] = false
	}
	return nil
}

func (f *fakeStartNk) NotificationSend(
	_ context.Context, _ string, _ string,
	_ map[string]interface{}, _ int, _ string, _ bool,
) error {
	return nil
}

func TestPartyStartRefusesUnreadyMember(t *testing.T) {
	nk := newFakeStartNk()
	nk.ready["leader"] = true
	_, err := partyStartMatchmakingRpc(
		refreshCtx("leader"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		newPartyStartGuard(), `{"party_id":"party-1"}`)
	if errorCode(err) != 9 {
		t.Fatalf("unready start: err = %v, want code 9", err)
	}
}

func TestPartyStartSpendsReadiness(t *testing.T) {
	nk := newFakeStartNk()
	nk.ready["leader"] = true
	nk.ready["member"] = true
	_, err := partyStartMatchmakingRpc(
		refreshCtx("leader"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		newPartyStartGuard(), `{"party_id":"party-1"}`)
	if err != nil {
		t.Fatalf("ready start: err = %v, want nil", err)
	}
	if nk.ready["leader"] || nk.ready["member"] {
		t.Fatalf(
			"ready rows survived the start: %v", nk.ready)
	}
}

func TestPartyStartSingleFlight(t *testing.T) {
	nk := newFakeStartNk()
	nk.ready["leader"] = true
	nk.ready["member"] = true
	guard := newPartyStartGuard()
	_, err := partyStartMatchmakingRpc(
		refreshCtx("leader"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		guard, `{"party_id":"party-1"}`)
	if err != nil {
		t.Fatalf("first start: err = %v, want nil", err)
	}
	nk.ready["leader"] = true
	nk.ready["member"] = true
	_, err = partyStartMatchmakingRpc(
		refreshCtx("leader"), testLogger{}, nk,
		newTestGames(t, nil), newFriendsLimiter(),
		guard, `{"party_id":"party-1"}`)
	if errorCode(err) != 8 {
		t.Fatalf("second start: err = %v, want code 8", err)
	}
}

func TestPartyStartGuardWindow(t *testing.T) {
	guard := newPartyStartGuard()
	now := time.Unix(1000, 0)
	if !guard.tryStart("party-1", now) {
		t.Fatal("first start refused")
	}
	if guard.tryStart("party-1", now.Add(time.Second)) {
		t.Fatal("second start inside the window allowed")
	}
	if !guard.tryStart("party-2", now.Add(time.Second)) {
		t.Fatal("another party's start refused")
	}
	if !guard.tryStart(
		"party-1", now.Add(partyStartWindow+time.Second)) {
		t.Fatal("start after the window refused")
	}
}

func TestLeaveDeletesOnlyLeaverReadyRow(t *testing.T) {
	nk := newFakeStartNk()
	nk.ready["leader"] = true
	nk.ready["member"] = true
	err := afterLeaveGroupHook(
		refreshCtx("member"), testLogger{}, nil, nk,
		&api.LeaveGroupRequest{GroupId: "party-1"})
	if err != nil {
		t.Fatalf("leave hook: err = %v", err)
	}
	if nk.ready["member"] {
		t.Fatal("leaver's ready row survived")
	}
	if !nk.ready["leader"] {
		t.Fatal("survivor's ready row was deleted")
	}
}

func TestKickDeletesOnlyKickedReadyRow(t *testing.T) {
	nk := newFakeStartNk()
	nk.ready["leader"] = true
	nk.ready["member"] = true
	err := afterKickGroupUsersHook(
		refreshCtx("leader"), testLogger{}, nil, nk,
		&api.KickGroupUsersRequest{
			GroupId: "party-1", UserIds: []string{"member"}})
	if err != nil {
		t.Fatalf("kick hook: err = %v", err)
	}
	if nk.ready["member"] {
		t.Fatal("kicked user's ready row survived")
	}
	if !nk.ready["leader"] {
		t.Fatal("survivor's ready row was deleted")
	}
}
