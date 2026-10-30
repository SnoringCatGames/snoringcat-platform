package main

import (
	"context"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// fakeClock returns a controllable now() the tests can advance.
// Used to step the sliding window forward without sleeping.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter() (*friendsLimiter, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newFriendsLimiter()
	l.now = clk.now
	return l, clk
}

// allowFriendCodeCall rejects calls past the per-window cap and
// keeps rejecting until the window slides.
func TestAllowFriendCodeCallRespectsLimit(t *testing.T) {
	l, _ := newTestLimiter()
	const user = "u1"
	for i := 0; i < friendCodeRateLimitCount; i++ {
		if !l.allowFriendCodeCall(user) {
			t.Fatalf("call %d should be allowed (under cap)", i+1)
		}
	}
	if l.allowFriendCodeCall(user) {
		t.Fatalf(
			"call %d should be rejected (at cap)",
			friendCodeRateLimitCount+1)
	}
}

// After the window expires, the budget is fresh again.
func TestAllowFriendCodeCallSlidesWindow(t *testing.T) {
	l, clk := newTestLimiter()
	const user = "u1"
	// Burn the budget.
	for i := 0; i < friendCodeRateLimitCount; i++ {
		l.allowFriendCodeCall(user)
	}
	if l.allowFriendCodeCall(user) {
		t.Fatal("expected reject before window advances")
	}
	// Step past the window (with a small epsilon so the
	// boundary itself is unambiguous).
	clk.advance(friendCodeRateLimitWindow + time.Second)
	if !l.allowFriendCodeCall(user) {
		t.Fatal("expected allow after window slid")
	}
}

// Different callers don't share budgets.
func TestAllowFriendCodeCallIsPerUser(t *testing.T) {
	l, _ := newTestLimiter()
	for i := 0; i < friendCodeRateLimitCount; i++ {
		l.allowFriendCodeCall("u1")
	}
	if l.allowFriendCodeCall("u1") {
		t.Fatal("u1 should be rejected at cap")
	}
	if !l.allowFriendCodeCall("u2") {
		t.Fatal("u2's budget should be untouched")
	}
}

// Pruning runs incrementally as calls arrive: half the window's
// worth of old calls drop off while newer ones inside the window
// stay.
func TestAllowFriendCodeCallPrunesIncrementally(t *testing.T) {
	l, clk := newTestLimiter()
	const user = "u1"
	// Place half the cap inside the first half of the window.
	half := friendCodeRateLimitCount / 2
	if half == 0 {
		t.Skip("cap too small to exercise incremental prune")
	}
	for i := 0; i < half; i++ {
		l.allowFriendCodeCall(user)
	}
	// Step past the window so those calls become stale.
	clk.advance(friendCodeRateLimitWindow + time.Second)
	// Burn the cap again with fresh calls; this exercises the
	// in-place prune path before the append.
	for i := 0; i < friendCodeRateLimitCount; i++ {
		if !l.allowFriendCodeCall(user) {
			t.Fatalf(
				"call %d after prune should be allowed", i+1)
		}
	}
	if l.allowFriendCodeCall(user) {
		t.Fatal("expected reject after second cap burn")
	}
}

// Empty user_id is a no-op: server-to-server callers reach the
// hook with no session, and we don't want to lock them out.
func TestAllowFriendCodeCallEmptyUser(t *testing.T) {
	l, _ := newTestLimiter()
	// Burn more than the cap; all should pass through.
	for i := 0; i < friendCodeRateLimitCount*3; i++ {
		if !l.allowFriendCodeCall("") {
			t.Fatalf("empty user_id call %d unexpectedly rejected", i+1)
		}
	}
}

// Constants don't drift accidentally. The rate-limit constants
// have an audit trail in MULTI_GAME_ROADMAP.md; surface a loud
// failure if anyone bumps them without thinking through the
// implications.
func TestFriendsLimitsConstantsStable(t *testing.T) {
	if maxPendingOutgoingFriendRequests != 50 {
		t.Errorf(
			"maxPendingOutgoingFriendRequests changed; update"+
				" MULTI_GAME_ROADMAP.md to match (was 50,"+
				" got %d)",
			maxPendingOutgoingFriendRequests)
	}
	if friendCodeRateLimitCount != 10 {
		t.Errorf(
			"friendCodeRateLimitCount changed; update"+
				" MULTI_GAME_ROADMAP.md (was 10, got %d)",
			friendCodeRateLimitCount)
	}
	if friendCodeRateLimitWindow != 60*time.Second {
		t.Errorf(
			"friendCodeRateLimitWindow changed; update"+
				" MULTI_GAME_ROADMAP.md (was 60s, got %v)",
			friendCodeRateLimitWindow)
	}
}

// fakeAddFriendsNk answers FriendsList for the SP35 hook
// tests: outgoing holds that many state=1 rows, incoming the
// given state=2 requester ids. Any other method panics
// through the nil embedded interface.
type fakeAddFriendsNk struct {
	runtime.NakamaModule

	outgoing int
	incoming []string
}

func (f *fakeAddFriendsNk) FriendsList(
	_ context.Context, _ string, _ int, state *int, _ string,
) ([]*api.Friend, string, error) {
	if state != nil && *state == friendStateInviteReceived {
		out := []*api.Friend{}
		for _, id := range f.incoming {
			out = append(out, &api.Friend{
				User: &api.User{Id: id}})
		}
		return out, "", nil
	}
	out := []*api.Friend{}
	for i := 0; i < f.outgoing; i++ {
		out = append(out, &api.Friend{
			User: &api.User{Id: "pending"}})
	}
	return out, "", nil
}

func TestAcceptExemptFromPendingCap(t *testing.T) {
	nk := &fakeAddFriendsNk{
		outgoing: maxPendingOutgoingFriendRequests,
		incoming: []string{"u2"}}
	limiter, _ := newTestLimiter()
	in := &api.AddFriendsRequest{Ids: []string{"u2"}}
	if err := checkAddFriends(
		context.Background(), nk, limiter, "u1", in); err != nil {
		t.Fatalf("accept at cap: err = %v, want nil", err)
	}
}

func TestNewAddBlockedAtPendingCap(t *testing.T) {
	nk := &fakeAddFriendsNk{
		outgoing: maxPendingOutgoingFriendRequests}
	limiter, _ := newTestLimiter()
	in := &api.AddFriendsRequest{Ids: []string{"u3"}}
	err := checkAddFriends(
		context.Background(), nk, limiter, "u1", in)
	if errorCode(err) != 9 {
		t.Fatalf("new add at cap: err = %v, want code 9", err)
	}
}

func TestAcceptSkipsRateLimit(t *testing.T) {
	nk := &fakeAddFriendsNk{incoming: []string{"u2"}}
	limiter, _ := newTestLimiter()
	for i := 0; i < friendCodeRateLimitCount; i++ {
		limiter.allowFriendCodeCall("u1")
	}
	accept := &api.AddFriendsRequest{Ids: []string{"u2"}}
	if err := checkAddFriends(
		context.Background(), nk, limiter, "u1", accept); err != nil {
		t.Fatalf("accept with burned budget: err = %v", err)
	}
	newAdd := &api.AddFriendsRequest{Ids: []string{"u3"}}
	err := checkAddFriends(
		context.Background(), nk, limiter, "u1", newAdd)
	if errorCode(err) != 8 {
		t.Fatalf("new add with burned budget: err = %v, want 8", err)
	}
}

func TestNewFriendRequestCount(t *testing.T) {
	incoming := map[string]bool{"u2": true}
	cases := []struct {
		ids, usernames []string
		want           int
	}{
		{[]string{"u2"}, nil, 0},
		{[]string{"u2", "u3"}, nil, 1},
		{nil, []string{"code1"}, 1},
		{[]string{"u3"}, []string{"code1"}, 2},
	}
	for i, tc := range cases {
		if got := newFriendRequestCount(
			tc.ids, tc.usernames, incoming); got != tc.want {
			t.Errorf("case %d: got %d, want %d", i, got, tc.want)
		}
	}
}
