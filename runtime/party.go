package main

import (
	"context"
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
	partyEventInvited partyEvent = "invited"
	partyEventJoined  partyEvent = "joined"
	partyEventLeft    partyEvent = "left"
	partyEventKicked  partyEvent = "kicked"
)

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
