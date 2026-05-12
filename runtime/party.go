package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// partyGroupPrefix mirrors the client's _PARTY_GROUP_PREFIX
// (src/core/party_api_client.gd:18). Nakama groups double as
// long-lived parties; the prefix distinguishes a party group from
// an arbitrary user-created group so this RPC can't be used to
// blast notifications to non-party groups.
const partyGroupPrefix = "party-"

// partyMatchmakingStartSubject is the notification subject each
// party member receives when the leader starts matchmaking. The
// client's notification listener uses this to know that it should
// drop its own matchmaker ticket with the matching party_id
// property so the whole party lands in one match.
const partyMatchmakingStartSubject = "party_matchmaking_start"

// partyStateChangedSubject fires on every party membership change
// (invite, join, leave, kick). Clients listening on a Nakama
// realtime socket refresh their local party state when they see it,
// replacing the previous 3 s / 10 s polling cadence in
// `party_manager.gd`. Sent transient (persistent=false) so the
// notifications don't accumulate in Nakama's inbox; clients always
// refetch on socket reconnect, so a missed event between a drop and
// reconnect is self-healing.
const partyStateChangedSubject = "party_state_changed"

// partyStateChangedCode is the application-defined notification
// code paired with partyStateChangedSubject. 100 is reserved for
// match_ready / party_matchmaking_start; pick a distinct value so
// downstream filters can route on either field.
const partyStateChangedCode = 101

// partyEvent describes which membership operation triggered a
// party_state_changed notification. Clients today refresh state on
// any event, but the field is included so a future UI can render
// "Alice joined the party" / "Bob left" without a second roundtrip.
type partyEvent string

const (
	partyEventInvited      partyEvent = "invited"
	partyEventJoined       partyEvent = "joined"
	partyEventLeft         partyEvent = "left"
	partyEventKicked       partyEvent = "kicked"
	partyEventReadyChanged partyEvent = "ready_changed"
)

// partyReadyCollection holds per-member ready-state rows. Each
// member owns their own row at (partyReadyCollection, partyID,
// memberID) with PermissionRead=2 / PermissionWrite=0 — clients
// can read everyone's row but only the runtime can write, so
// `party_set_ready` is the sole entry point and the fan-out
// notification can't be bypassed.
//
// Rows are cleared whenever the active roster changes (join /
// leave / kick) so a member rejoining doesn't carry forward a
// stale ready flag from a previous session.
const partyReadyCollection = "party_ready"

// partyInviteCodeCollection holds the bidirectional mapping
// between a 6-character invite code and the party group it
// belongs to. Two rows per active code:
//
//	(partyInviteCodeCollection, "code:" + CODE,    "") → {party_id}
//	(partyInviteCodeCollection, "party:" + partyID, "") → {code}
//
// Both rows are server-owned (UserID="") and server-only
// readable / writable (Permissions 0/0); the RPCs below are the
// only access path. The forward row by code lets join-by-code
// resolve a code to a party in O(1); the reverse by party lets
// repeat callers of party_get_invite_code skip generation.
//
// Stale rows are cleaned lazily: party_join_by_code deletes the
// pair when it discovers the underlying group no longer exists.
// account.go's bulk per-user scrub doesn't touch these because
// they aren't user-owned, which is the right call (the code
// belongs to the party, not the person who generated it).
const partyInviteCodeCollection = "party_invite_codes"

// partyInviteCodeAlphabet is the character set used to build
// invite codes. 32 chars, deliberately omitting I, O, 0, and 1
// to reduce visual ambiguity when reading a code aloud or off a
// screenshot. 32^6 ≈ 1.07B combinations.
const partyInviteCodeAlphabet = (
	"23456789ABCDEFGHJKLMNPQRSTUVWXYZ")

// partyInviteCodeLength is fixed at 6. Short enough to type
// quickly on a gamepad's on-screen keyboard, long enough that
// brute-forcing the code namespace is uninteresting.
const partyInviteCodeLength = 6

// partyInviteCodeMaxAttempts bounds the collision-retry loop in
// generatePartyInviteCode. With 32^6 codes and typical active-
// party counts in the low thousands, the expected collision rate
// is well under 0.001%, so a small cap is fine.
const partyInviteCodeMaxAttempts = 5

type partyStartMatchmakingArgs struct {
	PartyID  string `json:"party_id"`
	GameMode string `json:"game_mode"`
}

type partyStartMatchmakingResp struct {
	OK         bool     `json:"ok"`
	PartyID    string   `json:"party_id"`
	GameMode   string   `json:"game_mode"`
	LeaderID   string   `json:"leader_id"`
	MemberIDs  []string `json:"member_ids"`
	// MatchmakerProperties is the property bag the client should
	// attach to its matchmaker ticket. Carrying party_id as a
	// string property lets fleet_allocator confirm the matched
	// players actually shared a party.
	MatchmakerProperties map[string]string `json:"matchmaker_properties"`
}

// partyStartMatchmakingRpcFactory threads the per-game config
// store through so the handler can enforce game_id on the
// session. The party group's own game_id scoping (so two games
// can have isolated parties) is deferred to Stage 3 — today the
// group prefix is the only namespace separator.
func partyStartMatchmakingRpcFactory(
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
		return partyStartMatchmakingRpc(
			ctx, logger, nk, games, payload)
	}
}

// partyStartMatchmakingRpc handles the leader's "start matchmaking
// for the whole party" request. The actual matchmaker ticketing is
// driven from each member's client (Nakama's server runtime can't
// add matchmaker tickets on behalf of users without their active
// session/presence). This RPC:
//   1. Validates the caller is the party group's leader (creator).
//   2. Confirms the group is actually a party (name prefix).
//   3. Enumerates members.
//   4. Sends a persistent `party_matchmaking_start` notification
//      to each member with the shared party_id, so each client can
//      enqueue its own ticket with a matching `party_id` property.
//   5. Returns the same info to the caller (the leader), so it can
//      enqueue itself without waiting for its own notification to
//      round-trip.
//
// The caller's own client SHOULD treat the RPC response as the
// authoritative kickoff (skip waiting for the leader's own
// notification) — the notification is for the followers.
//
// Authorization: caller must be the party's leader. Returns
// PERMISSION_DENIED (7) otherwise.
func partyStartMatchmakingRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	args := partyStartMatchmakingArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if args.PartyID == "" {
		return "", runtime.NewError("party_id required", 3)
	}
	if args.GameMode == "" {
		args.GameMode = "ffa"
	}

	// Look up the group. Reject if it doesn't exist or isn't a
	// party (name prefix mismatch).
	groups, err := nk.GroupsGetId(ctx, []string{args.PartyID})
	if err != nil {
		logger.Error("GroupsGetId(%s): %v", args.PartyID, err)
		return "", err
	}
	if len(groups) == 0 {
		return "", runtime.NewError("party not found", 5)
	}
	group := groups[0]
	if !strings.HasPrefix(group.Name, partyGroupPrefix) {
		return "", runtime.NewError(
			"group "+args.PartyID+" is not a party"+
				" (name does not start with \""+
				partyGroupPrefix+"\")",
			3)
	}
	if group.CreatorId != userID {
		logger.Info(
			"party_start_matchmaking refused for non-leader:"+
				" user=%s party=%s leader=%s",
			userID, args.PartyID, group.CreatorId)
		return "", runtime.NewError(
			"only the party leader can start matchmaking", 7)
	}

	// Enumerate party members. Nakama caps the page size; parties
	// are capped at 4 members today (see party_api_client.gd
	// create_party() max_count argument), so a single page is
	// enough. If party sizes ever exceed the cap, paginate here.
	members, _, err := nk.GroupUsersList(
		ctx, args.PartyID, 100, nil, "")
	if err != nil {
		logger.Error("GroupUsersList(%s): %v", args.PartyID, err)
		return "", err
	}

	matchmakerProperties := map[string]string{
		"party_id":  args.PartyID,
		"game_mode": args.GameMode,
	}

	// Dispatch the start notification to each non-leader member.
	// Persistent=true so the notification survives a disconnected
	// client (Nakama will replay it when the client reconnects);
	// the matchmaker timeout on the leader side is the bound on
	// how long we wait before declaring the party-block dead.
	notification := map[string]any{
		"party_id":              args.PartyID,
		"game_mode":             args.GameMode,
		"leader_id":             userID,
		"matchmaker_properties": matchmakerProperties,
	}
	memberIDs := make([]string, 0, len(members))
	for _, m := range members {
		if m.User == nil || m.User.Id == "" {
			continue
		}
		// State 3 = JOIN_REQUEST (pending invite the user has
		// not yet accepted). Skip — they aren't truly in the
		// party until they accept.
		if m.State != nil && m.State.Value == 3 {
			continue
		}
		memberIDs = append(memberIDs, m.User.Id)
		// The leader doesn't need a notification — they're acting
		// on the RPC response directly.
		if m.User.Id == userID {
			continue
		}
		if err := nk.NotificationSend(
			ctx,
			m.User.Id,
			partyMatchmakingStartSubject,
			notification,
			100,
			"",
			true,
		); err != nil {
			logger.Warn(
				"notify party member %s: %v",
				m.User.Id, err)
		}
	}

	logger.Info(
		"party_matchmaking_start dispatched: party=%s leader=%s"+
			" members=%d game_mode=%s",
		args.PartyID, userID, len(memberIDs), args.GameMode)

	resp := partyStartMatchmakingResp{
		OK:                   true,
		PartyID:              args.PartyID,
		GameMode:             args.GameMode,
		LeaderID:             userID,
		MemberIDs:            memberIDs,
		MatchmakerProperties: matchmakerProperties,
	}
	out, _ := json.Marshal(resp)
	return string(out), nil
}

// registerPartyGroupHooks wires Nakama's AfterAddGroupUsers /
// AfterJoinGroup / AfterLeaveGroup / AfterKickGroupUsers hooks to
// fan out party_state_changed notifications. Each hook gates on the
// target group's name starting with partyGroupPrefix; non-party
// groups don't generate notifications.
//
// Caller authentication is enforced by Nakama before the hook runs
// — for the add/kick paths the actor is the auth'd session user;
// for join/leave it's the auth'd user joining/leaving themselves.
// We don't need to validate authority again here.
//
// The notifications are transient (persistent=false). Clients
// always refetch party state on socket reconnect, so a missed event
// between a socket drop and reconnect is recovered automatically
// without burning a row in the recipient's persistent notification
// inbox.
func registerPartyGroupHooks(
	initializer runtime.Initializer,
) error {
	if err := initializer.RegisterAfterAddGroupUsers(
		afterAddGroupUsersHook,
	); err != nil {
		return err
	}
	if err := initializer.RegisterAfterJoinGroup(
		afterJoinGroupHook,
	); err != nil {
		return err
	}
	if err := initializer.RegisterAfterLeaveGroup(
		afterLeaveGroupHook,
	); err != nil {
		return err
	}
	if err := initializer.RegisterAfterKickGroupUsers(
		afterKickGroupUsersHook,
	); err != nil {
		return err
	}
	return nil
}

func afterAddGroupUsersHook(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.AddGroupUsersRequest,
) error {
	// in.UserIds are the freshly-invited users. Nakama hasn't
	// returned them through GroupUsersList as state=3 yet by the
	// time the after-hook fires (write ordering varies), so include
	// them as extra recipients explicitly.
	notifyPartyMembers(
		ctx, logger, nk,
		in.GetGroupId(),
		partyEventInvited,
		in.GetUserIds(),
	)
	return nil
}

func afterJoinGroupHook(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.JoinGroupRequest,
) error {
	// The joiner is the auth'd user and is now a real member;
	// GroupUsersList will include them. No extras needed.
	clearPartyReadyRows(ctx, logger, nk, in.GetGroupId(), nil)
	notifyPartyMembers(
		ctx, logger, nk,
		in.GetGroupId(),
		partyEventJoined,
		nil,
	)
	return nil
}

func afterLeaveGroupHook(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.LeaveGroupRequest,
) error {
	// The leaver is no longer in GroupUsersList. Pull their user_id
	// from the request context so they also get the notification
	// (their UI flips off the in-party view immediately rather than
	// waiting for the local optimistic clear).
	leaver, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	extras := []string{}
	if leaver != "" {
		extras = append(extras, leaver)
	}
	clearPartyReadyRows(
		ctx, logger, nk, in.GetGroupId(), extras)
	notifyPartyMembers(
		ctx, logger, nk,
		in.GetGroupId(),
		partyEventLeft,
		extras,
	)
	return nil
}

func afterKickGroupUsersHook(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.KickGroupUsersRequest,
) error {
	// Kicked users are gone from GroupUsersList; include them
	// explicitly so their UI sees the kick.
	clearPartyReadyRows(
		ctx, logger, nk, in.GetGroupId(), in.GetUserIds())
	notifyPartyMembers(
		ctx, logger, nk,
		in.GetGroupId(),
		partyEventKicked,
		in.GetUserIds(),
	)
	return nil
}

// notifyPartyMembers fans out a party_state_changed notification to
// every current member of the group plus any extra recipients
// (users who were just added or kicked and may not appear in
// GroupUsersList anymore). Non-party groups (name prefix mismatch)
// are silently skipped.
//
// Best-effort by design: a NotificationSend failure is logged at
// warn level but doesn't bubble up. The slow catch-up poll on the
// client absorbs any missed deliveries.
func notifyPartyMembers(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	groupID string,
	event partyEvent,
	extras []string,
) {
	if groupID == "" {
		return
	}
	groups, err := nk.GroupsGetId(ctx, []string{groupID})
	if err != nil {
		logger.Warn(
			"party hook: GroupsGetId(%s) err=%v",
			groupID, err)
		return
	}
	if len(groups) == 0 {
		return
	}
	if !strings.HasPrefix(groups[0].Name, partyGroupPrefix) {
		return
	}
	members, _, err := nk.GroupUsersList(
		ctx, groupID, 100, nil, "")
	if err != nil {
		logger.Warn(
			"party hook: GroupUsersList(%s) err=%v",
			groupID, err)
		return
	}
	seen := map[string]bool{}
	recipients := make([]string, 0, len(members)+len(extras))
	for _, m := range members {
		if m.User == nil || m.User.Id == "" {
			continue
		}
		if seen[m.User.Id] {
			continue
		}
		seen[m.User.Id] = true
		recipients = append(recipients, m.User.Id)
	}
	for _, uid := range extras {
		if uid == "" || seen[uid] {
			continue
		}
		seen[uid] = true
		recipients = append(recipients, uid)
	}
	content := map[string]any{
		"party_id": groupID,
		"event":    string(event),
	}
	for _, uid := range recipients {
		if err := nk.NotificationSend(
			ctx, uid,
			partyStateChangedSubject,
			content,
			partyStateChangedCode,
			"",
			false,
		); err != nil {
			logger.Warn(
				"party hook: notify uid=%s event=%s err=%v",
				uid, event, err)
		}
	}
}

// partySetReadyArgs is the client → runtime payload for the ready
// toggle.
type partySetReadyArgs struct {
	PartyID string `json:"party_id"`
	Ready   bool   `json:"ready"`
}

// partySetReadyResp echoes back the new state so the client UI can
// stop showing a pending spinner without waiting for the next
// fetch_party_status round-trip.
type partySetReadyResp struct {
	OK      bool   `json:"ok"`
	PartyID string `json:"party_id"`
	UserID  string `json:"user_id"`
	Ready   bool   `json:"ready"`
}

// partySetReadyRpcFactory wires the per-game config so the handler
// can enforce the game_id-in-vars invariant established by Stage
// 2.6. Pattern mirrors partyStartMatchmakingRpcFactory.
func partySetReadyRpcFactory(
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
		return partySetReadyRpc(
			ctx, logger, nk, games, payload)
	}
}

// partySetReadyRpc toggles the caller's per-party ready flag. The
// row is owned by the caller so a future "list everyone's readies"
// read can use a single batched StorageRead keyed on the active
// member ids the client already has.
//
// Authorization: caller must be an active member of the party
// (state 0/1/2). Pending invitees (state 3) cannot mark themselves
// ready — they have to accept the invite first.
func partySetReadyRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	args := partySetReadyArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if args.PartyID == "" {
		return "", runtime.NewError("party_id required", 3)
	}

	// Confirm the group is a party (name prefix) and the caller is
	// an active member. Pending invitees (state=3) are rejected.
	groups, err := nk.GroupsGetId(ctx, []string{args.PartyID})
	if err != nil {
		logger.Error("GroupsGetId(%s): %v", args.PartyID, err)
		return "", err
	}
	if len(groups) == 0 {
		return "", runtime.NewError("party not found", 5)
	}
	if !strings.HasPrefix(groups[0].Name, partyGroupPrefix) {
		return "", runtime.NewError(
			"group "+args.PartyID+" is not a party", 3)
	}
	members, _, err := nk.GroupUsersList(
		ctx, args.PartyID, 100, nil, "")
	if err != nil {
		logger.Error(
			"GroupUsersList(%s): %v", args.PartyID, err)
		return "", err
	}
	memberFound := false
	for _, m := range members {
		if m.User == nil || m.User.Id != userID {
			continue
		}
		if m.State != nil && m.State.Value == 3 {
			return "", runtime.NewError(
				"accept the party invite before marking"+
					" yourself ready", 9)
		}
		memberFound = true
		break
	}
	if !memberFound {
		return "", runtime.NewError(
			"caller is not a member of party "+args.PartyID, 7)
	}

	if args.Ready {
		value, _ := json.Marshal(map[string]any{
			"ready":      true,
			"updated_at": nowUnix(),
		})
		if _, err := nk.StorageWrite(
			ctx,
			[]*runtime.StorageWrite{{
				Collection:      partyReadyCollection,
				Key:             args.PartyID,
				UserID:          userID,
				Value:           string(value),
				PermissionRead:  2,
				PermissionWrite: 0,
			}},
		); err != nil {
			logger.Error(
				"party_set_ready write uid=%s party=%s: %v",
				userID, args.PartyID, err)
			return "", err
		}
	} else if err := nk.StorageDelete(
		ctx,
		[]*runtime.StorageDelete{{
			Collection: partyReadyCollection,
			Key:        args.PartyID,
			UserID:     userID,
		}},
	); err != nil {
		// Delete failure is logged but not fatal — the most
		// common cause is "row already absent", which is the
		// state the caller asked for anyway.
		logger.Warn(
			"party_set_ready delete uid=%s party=%s: %v",
			userID, args.PartyID, err)
	}

	// Fan out party_state_changed so every member's UI reflects
	// the new ready state without waiting on the catch-up poll.
	notifyPartyMembers(
		ctx, logger, nk,
		args.PartyID,
		partyEventReadyChanged,
		nil,
	)

	resp := partySetReadyResp{
		OK:      true,
		PartyID: args.PartyID,
		UserID:  userID,
		Ready:   args.Ready,
	}
	out, _ := json.Marshal(resp)
	return string(out), nil
}

// partyGetInviteCodeArgs is the client → runtime payload for
// fetching (or generating) an invite code for a party.
type partyGetInviteCodeArgs struct {
	PartyID string `json:"party_id"`
}

// partyGetInviteCodeResp echoes the resolved code so the caller's
// UI can render it without a second roundtrip.
type partyGetInviteCodeResp struct {
	OK      bool   `json:"ok"`
	PartyID string `json:"party_id"`
	Code    string `json:"code"`
}

func partyGetInviteCodeRpcFactory(
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
		return partyGetInviteCodeRpc(
			ctx, logger, nk, games, payload)
	}
}

// partyGetInviteCodeRpc returns the party's shareable invite
// code. Any active member (state 0/1/2) of the party may fetch
// it; pending invitees and non-members are rejected. The code is
// generated lazily on first request and reused on subsequent
// calls, so re-sharing doesn't churn through the alphabet.
//
// Authorization: caller must be an active member of the party.
// Returns PERMISSION_DENIED (7) otherwise.
func partyGetInviteCodeRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	args := partyGetInviteCodeArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if args.PartyID == "" {
		return "", runtime.NewError("party_id required", 3)
	}

	// Validate the group exists, is a party, and the caller is an
	// active member. Pending invitees can't share the code — that
	// would leak it to strangers via the invitee's own UI.
	groups, err := nk.GroupsGetId(ctx, []string{args.PartyID})
	if err != nil {
		logger.Error("GroupsGetId(%s): %v", args.PartyID, err)
		return "", err
	}
	if len(groups) == 0 {
		return "", runtime.NewError("party not found", 5)
	}
	if !strings.HasPrefix(groups[0].Name, partyGroupPrefix) {
		return "", runtime.NewError(
			"group "+args.PartyID+" is not a party", 3)
	}
	members, _, err := nk.GroupUsersList(
		ctx, args.PartyID, 100, nil, "")
	if err != nil {
		logger.Error("GroupUsersList(%s): %v", args.PartyID, err)
		return "", err
	}
	memberFound := false
	for _, m := range members {
		if m.User == nil || m.User.Id != userID {
			continue
		}
		if m.State != nil && m.State.Value == 3 {
			return "", runtime.NewError(
				"accept the party invite before sharing"+
					" its code", 9)
		}
		memberFound = true
		break
	}
	if !memberFound {
		return "", runtime.NewError(
			"caller is not a member of party "+args.PartyID, 7)
	}

	code, err := resolveOrCreatePartyInviteCode(
		ctx, logger, nk, args.PartyID)
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(partyGetInviteCodeResp{
		OK:      true,
		PartyID: args.PartyID,
		Code:    code,
	})
	return string(out), nil
}

// resolveOrCreatePartyInviteCode reads the existing code for the
// party from storage, returning it verbatim if present. On miss,
// generates a fresh code, writes both forward/reverse rows, and
// returns the new code. Collision retries are bounded by
// partyInviteCodeMaxAttempts.
func resolveOrCreatePartyInviteCode(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	partyID string,
) (string, error) {
	// Reverse lookup: party → code. Hit means we've seen this
	// party before; reuse the code.
	reverseReads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: partyInviteCodeCollection,
			Key:        partyInviteCodeReverseKey(partyID),
			UserID:     "",
		}},
	)
	if err != nil {
		logger.Error("invite code reverse read: %v", err)
		return "", err
	}
	for _, obj := range reverseReads {
		parsed := map[string]any{}
		if err := json.Unmarshal(
			[]byte(obj.Value), &parsed); err != nil {
			continue
		}
		if code, ok := parsed["code"].(string); ok && code != "" {
			return code, nil
		}
	}

	// Generate a new code. The forward write uses Version="*" so
	// Nakama refuses to overwrite an existing row, which would
	// otherwise steal another party's code on collision.
	for attempt := 0; attempt < partyInviteCodeMaxAttempts; attempt++ {
		code, err := generatePartyInviteCode()
		if err != nil {
			return "", err
		}
		forward, _ := json.Marshal(map[string]any{
			"party_id":   partyID,
			"created_at": nowUnix(),
		})
		_, err = nk.StorageWrite(
			ctx,
			[]*runtime.StorageWrite{{
				Collection:      partyInviteCodeCollection,
				Key:             partyInviteCodeForwardKey(code),
				UserID:          "",
				Value:           string(forward),
				Version:         "*",
				PermissionRead:  0,
				PermissionWrite: 0,
			}},
		)
		if err != nil {
			// "Storage write failed: version check failed"
			// indicates the row already exists for a different
			// party — retry with a new code.
			if strings.Contains(
				err.Error(), "version check failed") {
				continue
			}
			logger.Error(
				"invite code forward write: %v", err)
			return "", err
		}

		reverse, _ := json.Marshal(map[string]any{
			"code":       code,
			"created_at": nowUnix(),
		})
		if _, err := nk.StorageWrite(
			ctx,
			[]*runtime.StorageWrite{{
				Collection:      partyInviteCodeCollection,
				Key:             partyInviteCodeReverseKey(partyID),
				UserID:          "",
				Value:           string(reverse),
				PermissionRead:  0,
				PermissionWrite: 0,
			}},
		); err != nil {
			logger.Error(
				"invite code reverse write: %v", err)
			return "", err
		}
		return code, nil
	}
	return "", runtime.NewError(
		"failed to generate a unique invite code", 13)
}

// generatePartyInviteCode draws partyInviteCodeLength random
// bytes and maps each into partyInviteCodeAlphabet. crypto/rand
// is used so codes can't be predicted from a known starting
// state — though the namespace is small enough that an attacker
// trying random codes will hit a real party occasionally
// regardless. The brute-force vector is bounded by party
// turnover, not RNG strength.
func generatePartyInviteCode() (string, error) {
	buf := make([]byte, partyInviteCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, partyInviteCodeLength)
	for i, b := range buf {
		out[i] = partyInviteCodeAlphabet[int(b)%len(partyInviteCodeAlphabet)]
	}
	return string(out), nil
}

func partyInviteCodeForwardKey(code string) string {
	return "code:" + code
}

func partyInviteCodeReverseKey(partyID string) string {
	return "party:" + partyID
}

// partyJoinByCodeArgs is the client → runtime payload for the
// join-by-code path.
type partyJoinByCodeArgs struct {
	Code string `json:"code"`
}

type partyJoinByCodeResp struct {
	OK      bool   `json:"ok"`
	PartyID string `json:"party_id"`
	Code    string `json:"code"`
}

func partyJoinByCodeRpcFactory(
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
		return partyJoinByCodeRpc(
			ctx, logger, nk, games, payload)
	}
}

// partyJoinByCodeRpc looks the code up in storage, validates the
// underlying party still exists and has room, and adds the
// caller as an active member (state=2) using server authority so
// the closed-group rule that forces invitees to state=3 is
// bypassed.
//
// Stale rows (party deleted out from under the code) are removed
// inline; the caller sees NOT_FOUND in that case rather than a
// confusing partial-success.
func partyJoinByCodeRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	args := partyJoinByCodeArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	// Normalize: strip whitespace, uppercase, then validate
	// alphabet membership. Rejecting unknown chars up front saves
	// a storage read on obviously bad input.
	code := strings.ToUpper(strings.TrimSpace(args.Code))
	if code == "" {
		return "", runtime.NewError("code required", 3)
	}
	if len(code) != partyInviteCodeLength {
		return "", runtime.NewError(
			"invalid code length", 3)
	}
	for _, c := range code {
		if !strings.ContainsRune(
			partyInviteCodeAlphabet, c) {
			return "", runtime.NewError(
				"invalid character in code", 3)
		}
	}

	// Forward lookup: code → party_id.
	forwardReads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: partyInviteCodeCollection,
			Key:        partyInviteCodeForwardKey(code),
			UserID:     "",
		}},
	)
	if err != nil {
		logger.Error("invite code forward read: %v", err)
		return "", err
	}
	if len(forwardReads) == 0 {
		return "", runtime.NewError("code not found", 5)
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(
		[]byte(forwardReads[0].Value), &parsed); err != nil {
		return "", runtime.NewError("code row malformed", 13)
	}
	partyID, _ := parsed["party_id"].(string)
	if partyID == "" {
		return "", runtime.NewError("code row missing party_id", 13)
	}

	// Validate the party still exists and has room. If the group
	// is gone (party disbanded since the code was issued), delete
	// both rows so the same code can be reissued cleanly when a
	// new party next requests one.
	groups, err := nk.GroupsGetId(ctx, []string{partyID})
	if err != nil {
		logger.Error("GroupsGetId(%s): %v", partyID, err)
		return "", err
	}
	if len(groups) == 0 {
		deleteStalePartyInviteCode(
			ctx, logger, nk, code, partyID)
		return "", runtime.NewError(
			"party no longer exists", 5)
	}
	g := groups[0]
	if !strings.HasPrefix(g.Name, partyGroupPrefix) {
		// The code points at a non-party group; treat as not-found
		// rather than letting a malicious code RPC drop a user
		// into an unrelated group. Clean up the stale row too.
		deleteStalePartyInviteCode(
			ctx, logger, nk, code, partyID)
		return "", runtime.NewError("code not found", 5)
	}
	if g.EdgeCount >= g.MaxCount {
		return "", runtime.NewError("party is full", 9)
	}

	// Bail early if the caller is already in the party in any
	// state — GroupUsersAdd would reject duplicate adds anyway,
	// and the error surface is friendlier this way.
	members, _, err := nk.GroupUsersList(
		ctx, partyID, 100, nil, "")
	if err != nil {
		logger.Error("GroupUsersList(%s): %v", partyID, err)
		return "", err
	}
	for _, m := range members {
		if m.User != nil && m.User.Id == userID {
			// Active member: idempotent success. Pending
			// invitee (state=3): treat as "already on the
			// invite list" — they can accept normally.
			out, _ := json.Marshal(partyJoinByCodeResp{
				OK:      true,
				PartyID: partyID,
				Code:    code,
			})
			return string(out), nil
		}
	}

	// Add the caller directly as state=2 (Member). Empty callerID
	// invokes server authority, which bypasses the closed-group
	// invite-and-accept dance — exactly what we want for join-by-
	// code, since holding the code IS the invitation.
	if err := nk.GroupUsersAdd(
		ctx, "", partyID, []string{userID},
	); err != nil {
		logger.Error(
			"GroupUsersAdd(party=%s user=%s): %v",
			partyID, userID, err)
		return "", err
	}

	// Fan out the same notification the AfterJoinGroup hook would
	// have sent for a self-initiated join, so the rest of the
	// party's UI refreshes. AfterAddGroupUsers will fire for the
	// nk.GroupUsersAdd call above and emit `invited`, but the
	// joiner is state=2 (not state=3 pending), so the semantics
	// are closer to `joined` from the party's perspective. Wire
	// it explicitly here and accept the duplicate fan-out (the
	// client dedups by notification id).
	clearPartyReadyRows(ctx, logger, nk, partyID, nil)
	notifyPartyMembers(
		ctx, logger, nk, partyID, partyEventJoined,
		[]string{userID},
	)

	logger.Info(
		"party_join_by_code: party=%s user=%s code=%s",
		partyID, userID, code)

	out, _ := json.Marshal(partyJoinByCodeResp{
		OK:      true,
		PartyID: partyID,
		Code:    code,
	})
	return string(out), nil
}

// deleteStalePartyInviteCode removes both mapping rows for a
// code whose underlying party has been deleted. Best-effort; a
// failure here doesn't change the caller-visible outcome.
func deleteStalePartyInviteCode(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	code string,
	partyID string,
) {
	if err := nk.StorageDelete(
		ctx,
		[]*runtime.StorageDelete{
			{
				Collection: partyInviteCodeCollection,
				Key:        partyInviteCodeForwardKey(code),
				UserID:     "",
			},
			{
				Collection: partyInviteCodeCollection,
				Key:        partyInviteCodeReverseKey(partyID),
				UserID:     "",
			},
		},
	); err != nil {
		logger.Warn(
			"delete stale invite code (party=%s code=%s): %v",
			partyID, code, err)
	}
}

// clearPartyReadyRows deletes every member's ready row for the
// given party, including any extras (e.g. the user who just left
// or was kicked and is no longer in GroupUsersList). Best-effort:
// failures log and continue, mirroring notifyPartyMembers.
func clearPartyReadyRows(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	partyID string,
	extras []string,
) {
	if partyID == "" {
		return
	}
	members, _, err := nk.GroupUsersList(
		ctx, partyID, 100, nil, "")
	if err != nil {
		logger.Warn(
			"clear ready: GroupUsersList(%s) err=%v",
			partyID, err)
		return
	}
	seen := map[string]bool{}
	deletes := []*runtime.StorageDelete{}
	add := func(uid string) {
		if uid == "" || seen[uid] {
			return
		}
		seen[uid] = true
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: partyReadyCollection,
			Key:        partyID,
			UserID:     uid,
		})
	}
	for _, m := range members {
		if m.User == nil {
			continue
		}
		add(m.User.Id)
	}
	for _, uid := range extras {
		add(uid)
	}
	if len(deletes) == 0 {
		return
	}
	if err := nk.StorageDelete(ctx, deletes); err != nil {
		logger.Warn(
			"clear ready: StorageDelete party=%s err=%v",
			partyID, err)
	}
}
