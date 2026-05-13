package main

import (
	"testing"
	"time"
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
