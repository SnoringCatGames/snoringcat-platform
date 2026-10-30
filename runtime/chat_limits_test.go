package main

import (
	"context"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeChatNk answers the reads checkPartyChat makes. The party
// has two active members, sender u1 and peer u2; blocked holds
// the senders u2 has blocked. Any other method panics through
// the nil embedded interface.
type fakeChatNk struct {
	runtime.NakamaModule

	blocked map[string]bool
}

func (f *fakeChatNk) GroupsGetId(
	_ context.Context, ids []string,
) ([]*api.Group, error) {
	for _, id := range ids {
		if id == "11111111-1111-1111-1111-111111111111" {
			return []*api.Group{
				{Id: "11111111-1111-1111-1111-111111111111", Name: "party-hopnbop"},
			}, nil
		}
	}
	return nil, nil
}

func (f *fakeChatNk) GroupUsersList(
	_ context.Context, _ string, _ int, _ *int, _ string,
) ([]*api.GroupUserList_GroupUser, string, error) {
	return []*api.GroupUserList_GroupUser{
		{User: &api.User{Id: "u1"}, State: wrapperspb.Int32(2)},
		{User: &api.User{Id: "u2"}, State: wrapperspb.Int32(2)},
	}, "", nil
}

func (f *fakeChatNk) FriendsList(
	_ context.Context, userID string, _ int, _ *int, _ string,
) ([]*api.Friend, string, error) {
	if userID == "u2" && f.blocked["u1"] {
		return []*api.Friend{
			{User: &api.User{Id: "u1"}},
		}, "", nil
	}
	return nil, "", nil
}

func TestChatAllowsShortMessage(t *testing.T) {
	nk := &fakeChatNk{blocked: map[string]bool{}}
	limiter, _ := newTestLimiter()
	err := checkPartyChat(
		context.Background(), nk, limiter, "u1",
		"11111111-1111-1111-1111-111111111111", `{"text":"hello party"}`)
	if err != nil {
		t.Fatalf("short message: err = %v, want nil", err)
	}
}

func TestChatRejectsOverlongText(t *testing.T) {
	nk := &fakeChatNk{blocked: map[string]bool{}}
	limiter, _ := newTestLimiter()
	long := strings.Repeat("a", partyChatMaxRunes+1)
	err := checkPartyChat(
		context.Background(), nk, limiter, "u1",
		"11111111-1111-1111-1111-111111111111", `{"text":"`+long+`"}`)
	if errorCode(err) != 3 {
		t.Fatalf("overlong text: err = %v, want code 3", err)
	}
}

func TestChatRateLimited(t *testing.T) {
	nk := &fakeChatNk{blocked: map[string]bool{}}
	limiter := burnedLimiter("u1")
	err := checkPartyChat(
		context.Background(), nk, limiter, "u1",
		"11111111-1111-1111-1111-111111111111", `{"text":"hello"}`)
	if errorCode(err) != 8 {
		t.Fatalf("over rate limit: err = %v, want code 8", err)
	}
}

func TestChatRefusedWhenMemberBlockedSender(t *testing.T) {
	nk := &fakeChatNk{blocked: map[string]bool{"u1": true}}
	limiter, _ := newTestLimiter()
	err := checkPartyChat(
		context.Background(), nk, limiter, "u1",
		"11111111-1111-1111-1111-111111111111", `{"text":"hello"}`)
	if errorCode(err) != 7 {
		t.Fatalf("blocked sender: err = %v, want code 7", err)
	}
}

func TestChatIgnoresNonPartyChannels(t *testing.T) {
	nk := &fakeChatNk{blocked: map[string]bool{"u1": true}}
	limiter := burnedLimiter("u1")
	long := strings.Repeat("a", partyChatMaxRunes+10)
	err := checkPartyChat(
		context.Background(), nk, limiter, "u1",
		"room-lobby", `{"text":"`+long+`"}`)
	if err != nil {
		t.Fatalf("room message: err = %v, want nil", err)
	}
}
