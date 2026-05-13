package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Stage 7.4: friend block list.
//
// Nakama natively tracks blocked relationships as state=3 (BANNED)
// in the friends table. Calling `nk.FriendsBlock` writes the row,
// removes any pre-existing friendship/pending request between the
// two users, and (server-side) rejects any future
// `nk.FriendsAdd(callerID, blockedID, ...)` call where caller is
// in the target's BANNED list. The blocked-add rejection is
// bidirectional in Nakama: if A blocked B, neither A nor B can
// send a friend request to the other (the read uses the same
// state row from either direction).
//
// We layer a few thin RPCs on top of this:
//
//   - block_user:        call FriendsBlock; capture display name
//                        for the list-blocked UI.
//   - unblock_user:      call FriendsDelete on the state=3 row.
//   - list_blocked_users: list state=3 entries with display name.
//
// Game-side filtering hooks:
//
//   - presence.go's `update_and_get_presence` already filters
//     friend presence reads to state=0 (MUTUAL) only, so blocked
//     users are naturally excluded from the friend presence list.
//     No change needed there.
//
//   - fleet_allocator.go's `OnMatchmakerMatched` walks every
//     matched user's BANNED list. When any pair has blocked the
//     other, the match aborts before Edgegap allocation and the
//     affected users see LOADING.BLOCKED_PAIR (a recoverable
//     "you and someone in this match have blocked each other"
//     message with a retry button). Implemented in fleet_allocator
//     under `abortBlockedPair`.

const (
	// friendStateBanned is the Nakama friend-state enum value for
	// "this user is blocked by the row owner". Pulled into a
	// typed const so the matchmaker filter and the list RPC name
	// it the same way.
	friendStateBanned = 3

	// blockListPageSize is the Nakama-imposed cap on FriendsList
	// page size. The list RPC paginates up to blockListPageCap
	// pages to drain a pathologically large block list without
	// burning every page in a single round trip.
	blockListPageSize = 100

	// blockListPageCap bounds the number of pages
	// list_blocked_users walks before truncating. 1000 entries is
	// far more than any normal user will ever block; an account
	// with more is almost certainly malicious or compromised, and
	// the truncated view is still useful to surface the most-
	// recent entries for an unblock pass.
	blockListPageCap = 10
)

// blockUserArgs is the wire shape for block_user. Either UserID
// or Username can identify the target (matches the symmetry of
// nk.FriendsBlock's `ids` / `usernames` arrays).
type blockUserArgs struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}

type blockUserResp struct {
	OK          bool   `json:"ok"`
	UserID      string `json:"user_id"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type unblockUserArgs struct {
	UserID string `json:"user_id"`
}

type unblockUserResp struct {
	OK     bool   `json:"ok"`
	UserID string `json:"user_id"`
}

// blockedUserEntry is one row in the list_blocked_users response.
// Mirrors the friends_received entries the client SDK already
// renders so the UI can re-use existing row templates.
type blockedUserEntry struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

type listBlockedUsersResp struct {
	BlockedUsers []blockedUserEntry `json:"blocked_users"`
	// Truncated is set when the caller has at least
	// blockListPageSize * blockListPageCap entries and the
	// pagination cut off before reaching the end. Surfaced for
	// telemetry; the UI doesn't currently render a "load more"
	// affordance because no real user is expected to hit the cap.
	Truncated bool `json:"truncated,omitempty"`
}

// blockUserRpcFactory wires the per-game config store through so
// the standard requireGameID session-identity check fires.
func blockUserRpcFactory(
	games *perGameConfig,
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
		return blockUserRpc(ctx, logger, nk, games, payload)
	}
}

func blockUserRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	callerID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	args := blockUserArgs{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return "", runtime.NewError(
				"invalid payload: "+err.Error(), 3)
		}
	}
	if args.UserID == "" && args.Username == "" {
		return "", runtime.NewError(
			"user_id or username required", 3)
	}
	if args.UserID == callerID {
		return "", runtime.NewError(
			"cannot block yourself", 3)
	}

	// Capture the caller's own username for the FriendsBlock call.
	// Nakama uses the caller's username for the audit fields on
	// the resulting friend row; an empty string is accepted but
	// loses the audit value.
	var callerUsername string
	if acc, accErr := nk.AccountGetId(ctx, callerID); accErr == nil && acc != nil {
		callerUsername = acc.User.Username
	}

	var ids []string
	var usernames []string
	if args.UserID != "" {
		ids = []string{args.UserID}
	}
	if args.Username != "" {
		usernames = []string{args.Username}
	}

	if err := nk.FriendsBlock(
		ctx, callerID, callerUsername, ids, usernames,
	); err != nil {
		logger.Warn(
			"block_user FriendsBlock failed: caller=%s target_id=%q target_username=%q err=%v",
			callerID, args.UserID, args.Username, err)
		return "", runtime.NewError(
			"failed to block user: "+err.Error(), 13)
	}

	// Resolve the target's display info so the response carries a
	// renderable label. Skip silently on failure — the block went
	// through, the UI just won't have a fresh display name.
	resp := blockUserResp{
		OK:       true,
		UserID:   args.UserID,
		Username: args.Username,
	}
	if args.UserID != "" {
		if accs, accErr := nk.UsersGetId(
			ctx, []string{args.UserID}, nil,
		); accErr == nil && len(accs) > 0 {
			resp.UserID = accs[0].Id
			resp.Username = accs[0].Username
			resp.DisplayName = accs[0].DisplayName
		}
	} else if args.Username != "" {
		if accs, accErr := nk.UsersGetUsername(
			ctx, []string{args.Username},
		); accErr == nil && len(accs) > 0 {
			resp.UserID = accs[0].Id
			resp.Username = accs[0].Username
			resp.DisplayName = accs[0].DisplayName
		}
	}

	logger.Info(
		"block_user: caller=%s blocked=%s",
		callerID, resp.UserID)

	out, _ := json.Marshal(resp)
	return string(out), nil
}

// unblockUserRpcFactory threads per-game config for the session
// game_id check.
func unblockUserRpcFactory(
	games *perGameConfig,
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
		return unblockUserRpc(ctx, logger, nk, games, payload)
	}
}

func unblockUserRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	callerID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	args := unblockUserArgs{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return "", runtime.NewError(
				"invalid payload: "+err.Error(), 3)
		}
	}
	if args.UserID == "" {
		return "", runtime.NewError(
			"user_id required", 3)
	}

	// FriendsDelete removes the state=3 row. We don't validate
	// that the row actually exists before deleting — Nakama
	// no-ops a delete against a missing friendship, which is
	// the right semantics for an idempotent unblock call.
	if err := nk.FriendsDelete(
		ctx, callerID, "", []string{args.UserID}, nil,
	); err != nil {
		logger.Warn(
			"unblock_user FriendsDelete failed: caller=%s target=%s err=%v",
			callerID, args.UserID, err)
		return "", runtime.NewError(
			"failed to unblock user: "+err.Error(), 13)
	}

	logger.Info(
		"unblock_user: caller=%s unblocked=%s",
		callerID, args.UserID)

	out, _ := json.Marshal(unblockUserResp{
		OK:     true,
		UserID: args.UserID,
	})
	return string(out), nil
}

// listBlockedUsersRpcFactory threads per-game config for the
// session game_id check.
func listBlockedUsersRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		_ *sql.DB,
		nk runtime.NakamaModule,
		_ string,
	) (string, error) {
		return listBlockedUsersRpc(ctx, logger, nk, games)
	}
}

func listBlockedUsersRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
) (string, error) {
	callerID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}

	entries, truncated, err := listBlockedUserIDs(ctx, nk, callerID)
	if err != nil {
		logger.Error(
			"list_blocked_users FriendsList: caller=%s err=%v",
			callerID, err)
		return "", runtime.NewError(
			"failed to list blocked users", 13)
	}

	out, _ := json.Marshal(listBlockedUsersResp{
		BlockedUsers: entries,
		Truncated:    truncated,
	})
	return string(out), nil
}

// listBlockedUserIDs returns the caller's state=3 (BANNED) friend
// entries paginated up to blockListPageCap pages. Shared with the
// matchmaker filter so both code paths read the same view.
//
// Returns (entries, truncated, err). `truncated=true` means the
// list hit the page cap; the entries returned are the first
// blockListPageSize * blockListPageCap rows in Nakama's insertion
// order.
func listBlockedUserIDs(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) ([]blockedUserEntry, bool, error) {
	state := friendStateBanned
	cursor := ""
	entries := []blockedUserEntry{}
	truncated := false
	for i := 0; i < blockListPageCap; i++ {
		friends, next, err := nk.FriendsList(
			ctx, userID, blockListPageSize, &state, cursor)
		if err != nil {
			return nil, false, err
		}
		for _, f := range friends {
			if f.User == nil || f.User.Id == "" {
				continue
			}
			entries = append(entries, blockedUserEntry{
				UserID:      f.User.Id,
				Username:    f.User.Username,
				DisplayName: f.User.DisplayName,
				AvatarURL:   f.User.AvatarUrl,
			})
		}
		if next == "" {
			break
		}
		cursor = next
		if i == blockListPageCap-1 {
			truncated = true
		}
	}
	return entries, truncated, nil
}

// blockedUserIDSet returns the caller's outgoing BANNED user IDs
// as a set, dropping the display-name overhead. Used by the
// matchmaker hook's blocked-pair check, where the only question
// is "is user X in user Y's BANNED list".
func blockedUserIDSet(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) (map[string]struct{}, error) {
	entries, _, err := listBlockedUserIDs(ctx, nk, userID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		set[e.UserID] = struct{}{}
	}
	return set, nil
}
