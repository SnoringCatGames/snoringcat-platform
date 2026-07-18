package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Friend codes are short, per-account, human-shareable identifiers a
// player hands to someone so they can send a friend request. They are
// deliberately NOT the Nakama username: usernames are mixed-case,
// case-sensitive, and (historically) doubled as the "friend code",
// which made the code impossible to type reliably and leaked the
// username into a shareable surface. A generated code decouples the
// two.
//
// The design mirrors the party-invite-code system in party.go (same
// alphabet family, crypto/rand generation, forward/reverse storage
// rows, collision-retry). The differences:
//   - 8 chars, not 6. Friend codes are permanent and public-ish, so
//     the extra entropy (32^8 ≈ 1.1e12 vs 32^6 ≈ 1.07e9) is cheap
//     insurance against enumeration and makes friend/party codes read
//     as visibly distinct to a player who has seen both.
//   - Keyed by user, not party, and reused for the account's lifetime.
//
// Storage layout (collection friendCodeCollection, system-owned rows
// with UserID="" so a forward lookup works without already knowing the
// owner, exactly like the party invite codes):
//   - forward  code:<CODE>     -> {"user_id": "<id>"}
//   - reverse  user:<user_id>  -> {"code": "<CODE>"}

const friendCodeCollection = "friend_codes"

// friendCodeAlphabet matches the party-invite-code alphabet: 32
// chars, deliberately omitting I, O, 0, and 1 so a code read aloud or
// off a screenshot is unambiguous. Uppercase-only, which is why the
// client can safely upper-case whatever the user types.
const friendCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// friendCodeLength is fixed at 8. The client's friend-code textbox
// auto-advances at exactly this many characters, so this value and
// _FRIEND_CODE_LENGTH in add_friend_panel.gd must stay in lockstep
// (the same source-of-truth duplication the party code already
// accepts between partyInviteCodeLength and join_by_code_panel.gd's
// _CODE_LENGTH).
const friendCodeLength = 8

// friendCodeMaxAttempts bounds the collision-retry loop. 32^8 is
// enormous relative to any realistic account count, so a collision is
// astronomically unlikely; a small cap is plenty.
const friendCodeMaxAttempts = 5

func friendCodeForwardKey(code string) string {
	return "code:" + code
}

func friendCodeReverseKey(userID string) string {
	return "user:" + userID
}

// getOrCreateFriendCode returns the caller's stable friend code,
// generating and persisting one on first request. The reverse lookup
// (user -> code) makes the code idempotent for a given account: every
// later call returns the same code.
func getOrCreateFriendCode(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	userID string,
) (string, error) {
	reverseReads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: friendCodeCollection,
			Key:        friendCodeReverseKey(userID),
			UserID:     "",
		}},
	)
	if err != nil {
		logger.Error("friend code reverse read: %v", err)
		return "", err
	}
	for _, obj := range reverseReads {
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(obj.Value), &parsed); err != nil {
			continue
		}
		if code, ok := parsed["code"].(string); ok && code != "" {
			return code, nil
		}
	}

	// No code yet. Generate a unique one. The forward write uses
	// Version="*" so Nakama refuses to overwrite an existing row,
	// which would otherwise steal another account's code on collision.
	for attempt := 0; attempt < friendCodeMaxAttempts; attempt++ {
		code, genErr := generateFriendCode()
		if genErr != nil {
			return "", genErr
		}
		forward, _ := json.Marshal(map[string]any{
			"user_id":    userID,
			"created_at": nowUnix(),
		})
		_, wErr := nk.StorageWrite(
			ctx,
			[]*runtime.StorageWrite{{
				Collection:      friendCodeCollection,
				Key:             friendCodeForwardKey(code),
				UserID:          "",
				Value:           string(forward),
				Version:         "*",
				PermissionRead:  0,
				PermissionWrite: 0,
			}},
		)
		if wErr != nil {
			// "version check failed" means the forward row already
			// exists for a different account — retry with a new code.
			if strings.Contains(wErr.Error(), "version check failed") {
				continue
			}
			logger.Error("friend code forward write: %v", wErr)
			return "", wErr
		}

		reverse, _ := json.Marshal(map[string]any{
			"code":       code,
			"created_at": nowUnix(),
		})
		if _, rErr := nk.StorageWrite(
			ctx,
			[]*runtime.StorageWrite{{
				Collection:      friendCodeCollection,
				Key:             friendCodeReverseKey(userID),
				UserID:          "",
				Value:           string(reverse),
				PermissionRead:  0,
				PermissionWrite: 0,
			}},
		); rErr != nil {
			logger.Error("friend code reverse write: %v", rErr)
			return "", rErr
		}
		return code, nil
	}
	return "", runtime.NewError(
		"failed to generate a unique friend code", 13)
}

// resolveFriendCode maps a friend code to a user_id. On a miss in the
// friend_codes collection it falls back to a username lookup: during
// the rollout window a client on an old build still shares its Nakama
// username as its "friend code", and a new client must be able to add
// it. Returns "" (with no error) when neither path resolves, which the
// caller surfaces to the player as "no player with that code".
func resolveFriendCode(
	ctx context.Context,
	nk runtime.NakamaModule,
	code string,
) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if normalized == "" {
		return "", nil
	}
	forwardReads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: friendCodeCollection,
			Key:        friendCodeForwardKey(normalized),
			UserID:     "",
		}},
	)
	if err != nil {
		return "", err
	}
	for _, obj := range forwardReads {
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(obj.Value), &parsed); err != nil {
			continue
		}
		if uid, ok := parsed["user_id"].(string); ok && uid != "" {
			return uid, nil
		}
	}

	// Backward-compat: treat the code as a Nakama username (the old
	// friend-code scheme). Usernames are case-sensitive, so look up the
	// original trimmed string, not the upper-cased one.
	trimmed := strings.TrimSpace(code)
	users, err := nk.UsersGetUsername(ctx, []string{trimmed})
	if err != nil {
		return "", err
	}
	if len(users) > 0 && users[0] != nil && users[0].Id != "" {
		return users[0].Id, nil
	}
	return "", nil
}

// deleteFriendCode removes both storage rows for a user's friend code.
// Called from the account-deletion cascade so a deleted account's code
// stops resolving. Best-effort: a failure is logged, not fatal — a
// dangling forward row just resolves to a now-deleted user, whose
// FriendsAdd then fails harmlessly.
func deleteFriendCode(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	userID string,
) {
	reverseReads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: friendCodeCollection,
			Key:        friendCodeReverseKey(userID),
			UserID:     "",
		}},
	)
	if err != nil {
		logger.Warn("friend code cleanup reverse read: %v", err)
		return
	}
	deletes := []*runtime.StorageDelete{{
		Collection: friendCodeCollection,
		Key:        friendCodeReverseKey(userID),
		UserID:     "",
	}}
	for _, obj := range reverseReads {
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(obj.Value), &parsed); err != nil {
			continue
		}
		if code, ok := parsed["code"].(string); ok && code != "" {
			deletes = append(deletes, &runtime.StorageDelete{
				Collection: friendCodeCollection,
				Key:        friendCodeForwardKey(code),
				UserID:     "",
			})
		}
	}
	if err := nk.StorageDelete(ctx, deletes); err != nil {
		logger.Warn("friend code cleanup delete: %v", err)
	}
}

// generateFriendCode draws friendCodeLength random bytes and maps each
// into friendCodeAlphabet. Mirror of generatePartyInviteCode.
func generateFriendCode() (string, error) {
	buf := make([]byte, friendCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, friendCodeLength)
	for i, b := range buf {
		out[i] = friendCodeAlphabet[int(b)%len(friendCodeAlphabet)]
	}
	return string(out), nil
}

// --------------------------------------------------------------------
// RPCs
// --------------------------------------------------------------------

type friendCodeResp struct {
	FriendCode string `json:"friend_code"`
}

// getFriendCodeRpc returns (creating on first call) the caller's
// stable friend code. Client-session only; no game scoping because
// friends and their codes are account-level, shared across every game
// on the platform.
func getFriendCodeRpc(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	_ string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	code, err := getOrCreateFriendCode(ctx, logger, nk, userID)
	if err != nil {
		return "", runtime.NewError("failed to get friend code", 13)
	}
	out, _ := json.Marshal(friendCodeResp{FriendCode: code})
	return string(out), nil
}

type addFriendByCodeArgs struct {
	Code string `json:"code"`
}

type addFriendByCodeResp struct {
	Result string `json:"result"`
	UserID string `json:"user_id,omitempty"`
}

// addFriendByCodeRpcFactory builds the add-by-friend-code RPC. It
// resolves the code server-side and adds by user ID, which the
// BeforeAddFriends hook (friends_limits.go) never sees — runtime-
// initiated FriendsAdd calls don't fire client hooks. So this RPC has
// to re-enforce BOTH of that hook's protections itself, or adding by
// code would be a rate-limit- and pending-cap-free bypass:
//   - the per-caller add-by-code rate limit (anti-enumeration), via
//     the SHARED limiter instance passed in from main.go; and
//   - the max-pending-outgoing cap.
func addFriendByCodeRpcFactory(
	limiter *friendsLimiter,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		_ *sql.DB,
		nk runtime.NakamaModule,
		payload string,
	) (string, error) {
		callerID, err := requireClientSession(ctx)
		if err != nil {
			return "", err
		}
		var args addFriendByCodeArgs
		if payload != "" {
			if err := json.Unmarshal(
				[]byte(payload), &args); err != nil {
				return "", runtime.NewError("invalid payload", 3)
			}
		}
		if strings.TrimSpace(args.Code) == "" {
			return "", runtime.NewError("code required", 3)
		}

		// Rate-limit BEFORE resolving: each resolve attempt is the
		// enumerable operation, so a rejected code must still burn a
		// slot. Mirrors the BeforeAddFriends add-by-username limit.
		if !limiter.allowFriendCodeCall(callerID) {
			return "", runtime.NewError(
				"friend-code rate limit exceeded; wait a minute and"+
					" try again", 8)
		}

		targetID, err := resolveFriendCode(ctx, nk, args.Code)
		if err != nil {
			logger.Error("resolve friend code: %v", err)
			return "", runtime.NewError("failed to resolve code", 13)
		}
		if targetID == "" {
			out, _ := json.Marshal(
				addFriendByCodeResp{Result: "not_found"})
			return string(out), nil
		}
		if targetID == callerID {
			return "", runtime.NewError("cannot add yourself", 3)
		}

		// Mirror the BeforeAddFriends max-pending cap (7.12).
		pending, err := countOutgoingPending(
			ctx, nk, callerID, maxPendingOutgoingFriendRequests+1)
		if err != nil {
			return "", runtime.NewError(
				"failed to enforce friend-request limit", 13)
		}
		if pending+1 > maxPendingOutgoingFriendRequests {
			return "", runtime.NewError(
				"too many pending friend requests; resolve some"+
					" before sending more", 9)
		}

		// Nakama stamps the caller's username onto the friend-row
		// audit fields; an empty string is accepted but loses that
		// value.
		var callerUsername string
		if acc, accErr := nk.AccountGetId(
			ctx, callerID); accErr == nil && acc != nil {
			callerUsername = acc.User.Username
		}
		if err := nk.FriendsAdd(
			ctx, callerID, callerUsername,
			[]string{targetID}, nil,
		); err != nil {
			logger.Warn(
				"add_friend_by_code FriendsAdd failed:"+
					" caller=%s target=%s err=%v",
				callerID, targetID, err)
			return "", runtime.NewError(
				"failed to add friend: "+err.Error(), 13)
		}

		// FriendsAdd reports only success/failure, not whether the
		// edge auto-accepted a mutual pending request or was already
		// a friendship. "request_sent" is the safe generic the client
		// maps to a toast; the finer-grained results in the client's
		// switch are reachable only via the legacy add-by-username
		// path, which does surface them.
		out, _ := json.Marshal(addFriendByCodeResp{
			Result: "request_sent",
			UserID: targetID,
		})
		return string(out), nil
	}
}
