package main

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Friend-related abuse prevention. Two checks fire from a single
// BeforeAddFriends hook:
//
//   1. Stage 7.12 — max pending outgoing friend requests. A caller
//      with 50 unresolved outgoing requests can't issue new ones
//      until some are accepted, rejected, or cancelled. Protects
//      receivers from spam-add inbox floods.
//
//   2. Stage 7.13 — per-caller rate limit on add-by-username
//      ("friend code") calls. A caller can issue at most
//      _FRIEND_CODE_RATE_LIMIT_COUNT add-by-username calls in any
//      _FRIEND_CODE_RATE_LIMIT_WINDOW seconds. Protects against
//      brute-force friend-code enumeration: codes are 6 chars of
//      a 32-symbol alphabet (1B+ space), so a sustained sweep
//      would need >150 calls/minute to cover the space in a year.
//      The limit pegs an attacker to under that throughput while
//      leaving headroom for the very-rare "I want to add 10
//      friends right now" UX.
//
// The hook is the only enforcement point; the client SDK doesn't
// know about either limit and surfaces the runtime's error message
// via the existing `request_failed(error)` signal on
// PlatformFriendsApiClient.

const (
	// maxPendingOutgoingFriendRequests is the cap on state=1
	// (INVITE_SENT) entries a caller can hold open. Once at the
	// cap, BeforeAddFriends rejects new requests until some clear.
	// Picked to be high enough for normal social use (you almost
	// never want 50 outstanding invites) but low enough that a
	// spam vector can't blast a thousand-target inbox flood.
	maxPendingOutgoingFriendRequests = 50

	// friendCodeRateLimitWindow is the sliding window over which
	// add-by-username calls are counted. 60 seconds is short
	// enough that real users never hit the limit and long enough
	// that an enumeration attacker can't average around it by
	// pacing requests.
	friendCodeRateLimitWindow = 60 * time.Second

	// friendCodeRateLimitCount is the per-window cap on
	// add-by-username calls. 10 is well above the legitimate
	// "I'm adding several friends after a meet-up" peak while
	// being orders of magnitude below the throughput an
	// enumeration sweep would need to cover the code space in
	// any reasonable time.
	friendCodeRateLimitCount = 10
)

// friendsLimiter holds the sliding-window timestamps for each
// caller's add-by-username calls. The map is keyed by user_id;
// each entry is a slice of unix-nanos timestamps within the
// current window. Older timestamps are pruned on every lookup so
// the slice can't grow unbounded.
//
// State is in-memory only. A restart wipes the windows, which
// gives every user a free fresh budget. That's fine: the worst
// case is one extra burst per restart, and a restart-driven
// attacker has bigger problems than friend-code enumeration.
//
// sync.Map vs map+RWMutex: the access pattern is "look up by
// caller user_id, append a timestamp, write back". sync.Map's
// LoadOrStore + manual locking-per-entry would optimize the
// no-contention common case, but the work per call is dominated
// by the Nakama FriendsList round trip anyway. A plain map+mutex
// is the simpler model.
type friendsLimiter struct {
	mu     sync.Mutex
	windows map[string][]int64

	// now is a clock indirection for tests. Defaults to
	// time.Now in production; tests inject a fake.
	now func() time.Time
}

// newFriendsLimiter wires a fresh limiter against the wall clock.
func newFriendsLimiter() *friendsLimiter {
	return &friendsLimiter{
		windows: map[string][]int64{},
		now:     time.Now,
	}
}

// allowFriendCodeCall returns true when the caller is under the
// rate limit AND records the call (mutating state). Returns false
// without recording when the call would exceed the limit, so a
// rejected attempt doesn't burn a slot. The window slides on
// every call: stale timestamps drop off, then the current
// timestamp is appended (only on accept).
func (l *friendsLimiter) allowFriendCodeCall(userID string) bool {
	if userID == "" {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	nowNanos := l.now().UnixNano()
	cutoff := nowNanos - friendCodeRateLimitWindow.Nanoseconds()
	stamps := l.windows[userID]
	// Prune timestamps older than the window. The slice is
	// append-only and kept in arrival order, so an in-place
	// compaction scanning until the first non-stale entry is
	// O(prune_count) and never touches the rest.
	pruneTo := 0
	for pruneTo < len(stamps) && stamps[pruneTo] < cutoff {
		pruneTo++
	}
	if pruneTo > 0 {
		stamps = stamps[pruneTo:]
	}
	if len(stamps) >= friendCodeRateLimitCount {
		// Write the pruned slice back even on reject so the next
		// call starts from a clean slate; otherwise the cleanup
		// only fires on accepts.
		if len(stamps) == 0 {
			delete(l.windows, userID)
		} else {
			l.windows[userID] = stamps
		}
		return false
	}
	stamps = append(stamps, nowNanos)
	l.windows[userID] = stamps
	return true
}

// registerFriendsLimitHook installs the BeforeAddFriends hook.
// Wired from main.go's InitModule on every boot — there's no
// env flag, the protections are always on. A caller can sidestep
// neither limit; both apply to every API path that lands on
// AddFriends (POST /v2/friend, friendsAdd on the realtime socket,
// the SDK helpers all go through here).
func registerFriendsLimitHook(
	initializer runtime.Initializer,
	limiter *friendsLimiter,
) error {
	return initializer.RegisterBeforeAddFriends(
		func(
			ctx context.Context,
			_ runtime.Logger,
			_ *sql.DB,
			nk runtime.NakamaModule,
			in *api.AddFriendsRequest,
		) (*api.AddFriendsRequest, error) {
			userID, _ := ctx.Value(
				runtime.RUNTIME_CTX_USER_ID).(string)
			if userID == "" {
				// Server-to-server call (no session). Hook
				// guards user-driven abuse; runtime callers
				// pass through.
				return in, nil
			}

			// 7.13 rate-limit. Only fires when usernames are in
			// the request — Ids-only paths (e.g. accept-incoming,
			// recent-match add) don't expose codes to brute-
			// force. The state-1 cap below still applies to both.
			if len(in.GetUsernames()) > 0 {
				if !limiter.allowFriendCodeCall(userID) {
					return nil, runtime.NewError(
						"friend-code rate limit exceeded; wait a"+
							" minute and try again", 8)
				}
			}

			// 7.12 max pending. Count the caller's current
			// state=1 (INVITE_SENT) entries. Nakama's FriendsList
			// returns at most 100 per page; we cap at
			// (maxPendingOutgoingFriendRequests+1) pages so
			// "user already over cap with 5000 pending" still
			// rejects without spending 50 round trips. The +1
			// page gives us the "≥cap" answer with one extra
			// read.
			pending, err := countOutgoingPending(
				ctx, nk, userID,
				maxPendingOutgoingFriendRequests+1)
			if err != nil {
				return nil, runtime.NewError(
					"failed to enforce friend-request limit", 13)
			}
			// Each entry in the inbound request creates one new
			// pending row (assuming none auto-accept; that's
			// fine, the worst case overcount is the request
			// size). Count both Ids + Usernames against the cap.
			newCount := len(in.GetIds()) + len(in.GetUsernames())
			if pending+newCount > maxPendingOutgoingFriendRequests {
				return nil, runtime.NewError(
					"too many pending friend requests; resolve"+
						" some before sending more", 9)
			}

			return in, nil
		})
}

// countOutgoingPending returns the caller's INVITE_SENT (state=1)
// friend count, walking up to maxPages of 100-entry pages. The
// helper short-circuits as soon as a cursor-less page arrives or
// the page cap is hit; the cap exists so a pathologically large
// pending list can't trap the hook in an unbounded loop. Returns
// the capped count, not the literal total — that's what
// registerFriendsLimitHook actually wants for its
// "newCount + existing > limit" check.
func countOutgoingPending(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
	maxPages int,
) (int, error) {
	state := friendStateInviteSent
	cursor := ""
	count := 0
	for i := 0; i < maxPages; i++ {
		friends, next, err := nk.FriendsList(
			ctx, userID, 100, &state, cursor)
		if err != nil {
			return 0, err
		}
		count += len(friends)
		if next == "" {
			break
		}
		cursor = next
	}
	return count, nil
}

// friendStateInviteSent is the Nakama friend-state enum value for
// "you sent the invite; they haven't responded yet". Pulled into
// a typed const so test code can name it the same way production
// does.
const friendStateInviteSent = 1
