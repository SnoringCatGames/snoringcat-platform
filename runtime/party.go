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

// partyMatchmakingAbortSubject tells followers to stand down after
// the leader gave up on a party matchmaking attempt — most often
// because a member never got their realtime socket into the party
// (see partyAbortMatchmakingRpc).
//
// Sent transient: an abort is only meaningful to a client that is
// currently sitting in the "waiting to be matched" state. A client
// that was offline for the whole attempt has nothing to stand down
// from, and replaying the abort on their next login would drop
// them out of an unrelated later attempt.
const partyMatchmakingAbortSubject = "party_matchmaking_abort"

// partyMatchmakingAbortCode pairs with the subject above. 100 is
// match_ready / party_matchmaking_start, 101 is
// party_state_changed.
const partyMatchmakingAbortCode = 102

// partyEvent describes which membership operation triggered a
// party_state_changed notification. Clients today refresh state on
// any event, but the field is included so a future UI can render
// "Alice joined the party" / "Bob left" without a second roundtrip.
type partyEvent string

const (
	partyEventInvited       partyEvent = "invited"
	partyEventJoined        partyEvent = "joined"
	partyEventLeft          partyEvent = "left"
	partyEventKicked        partyEvent = "kicked"
	partyEventReadyChanged  partyEvent = "ready_changed"
	partyEventLeaderChanged partyEvent = "leader_changed"
	partyEventModeChanged   partyEvent = "mode_changed"
)

// partyLeaderCollection holds the optional "current leader"
// override for a party. Nakama groups have an immutable
// `creator_id` field, so we can't reassign leadership by mutating
// the group itself; instead, the runtime stores an override row
// here whenever `party_transfer_leadership` runs. Schema:
//
//	(partyLeaderCollection, partyID, "") → {user_id, transferred_at}
//
// The row is server-owned (UserID="") with PermissionRead=2 so
// any party member can fold the override into their local view
// of leader_id, but PermissionWrite=0 keeps the transfer RPC as
// the sole way to mutate it. `resolvePartyLeader` reads this row
// first and falls back to `group.CreatorId` when absent, keeping
// pre-transfer parties working without a migration.
const partyLeaderCollection = "party_leader"

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

// partyModeCollection holds the optional "current matchmaker
// game mode" override for a party (Stage 5.7). Server-owned
// (UserID="") with PermissionRead=2 / PermissionWrite=0; the
// `party_set_mode` RPC is the sole way to mutate the row. Schema:
//
//	(partyModeCollection, partyID, "") → {mode_id, set_by, set_at}
//
// The mode propagates to followers via party_state_changed
// fan-out (event=mode_changed) and is folded into the response of
// fetch_party_status. partyStartMatchmaking reads it as the
// game_mode default when the caller doesn't override on the RPC.
const partyModeCollection = "party_mode"

// partyLevelPrefsCollection holds the leader's level preferences for
// the party (leader-authoritative match settings). Same server-owned
// shape as partyModeCollection. The stored `prefs` blob is the
// client's LevelPreferences.to_dict() ({inclusion, exclusion,
// preferred}); the runtime treats it as opaque and forwards it to the
// game server via the SELECTED_LEVEL_PREFS deploy env var, which
// resolves the concrete level using its own level registry.
//
//	(partyLevelPrefsCollection, partyID, "") → {prefs, set_by, set_at}
const partyLevelPrefsCollection = "party_level_prefs"

// partyCheatPrefsCollection holds the leader's gameplay-cheat prefs
// for the party. Only the gameplay-affecting (networked) cheats are
// leader-authoritative; aesthetic (local) cheats stay per-client.
// Forwarded to the game server via MATCH_CHEAT_PREFS.
//
//	(partyCheatPrefsCollection, partyID, "") →
//		{are_cheats_enabled, networked_cheats:[...], set_by, set_at}
const partyCheatPrefsCollection = "party_cheat_prefs"

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
	// RtPartyID is the Nakama *realtime* party the leader created
	// on its socket before calling this RPC. Distinct from PartyID,
	// which is the persistent group backing the social party.
	//
	// Followers join this realtime party so the leader can submit a
	// single matchmaker ticket for the whole group via
	// PartyMatchmakerAdd. That is what actually guarantees the
	// party lands in one match: before this existed, every member
	// submitted an independent ticket carrying party_id as an inert
	// property, and Nakama was free to split them across matches
	// whenever other players were queued at the same time.
	RtPartyID string `json:"rt_party_id"`
}

type partyStartMatchmakingResp struct {
	OK        bool     `json:"ok"`
	PartyID   string   `json:"party_id"`
	RtPartyID string   `json:"rt_party_id"`
	GameMode  string   `json:"game_mode"`
	LeaderID  string   `json:"leader_id"`
	MemberIDs []string `json:"member_ids"`
	// MatchmakerProperties is the property bag attached to the
	// party's matchmaker ticket. party_id rides along so
	// fleet_allocator can confirm the matched players shared a
	// party; co-location itself is now guaranteed by the realtime
	// party, not by this property.
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
// for the whole party" request. Nakama's server runtime can't add
// matchmaker tickets on behalf of users (that needs their live
// socket), so the ticket is still driven from a client — but from
// the LEADER's client only, as a single party ticket, rather than
// from every member independently.
//
// This RPC:
//   1. Validates the caller is the party group's leader (creator).
//   2. Confirms the group is actually a party (name prefix).
//   3. Enumerates members.
//   4. Sends a transient `party_matchmaking_start` notification to
//      each member carrying the leader's `rt_party_id`, so each
//      client can join that realtime party on its own socket.
//   5. Returns the same info to the caller (the leader), which
//      waits for the members' presences to appear and then submits
//      one PartyMatchmakerAdd for the whole party.
//
// The caller's own client SHOULD treat the RPC response as the
// authoritative kickoff (skip waiting for the leader's own
// notification) — the notification is for the followers.
//
// Why the realtime party at all: matched-together-ness cannot be
// expressed as a matchmaker property. The previous design attached
// `party_id` to N independent tickets and hoped; Nakama matches on
// the query, which never referenced party_id, so a party queuing
// alongside solo players could be split across matches. Nor can a
// query fix it — pinning `+properties.party_id:X` would stop a
// 3-person party from ever filling a 4-player match. A single
// party ticket is the only construct that says "these N players,
// together, plus fill".
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
	// rt_party_id is deliberately NOT required.
	//
	// Clients predating the realtime-party flow don't send one, and
	// the runtime auto-deploys on every push to main while the web
	// client ships on its own cadence — so there is always a window
	// where the live client is older than the runtime. Rejecting
	// the call would take party matchmaking from "works, but can
	// split the party under load" to "errors out", which is a
	// strictly worse trade for the people playing right now.
	//
	// Old callers therefore keep the legacy behavior: they get a
	// notification with no rt_party_id and go on submitting their
	// own solo tickets, exactly as before. New callers get
	// co-location. Once the old client is off the field this can
	// become a hard error — the client-side guard in
	// PartyManager._start_party_matchmaking already refuses to
	// silently degrade, so a new client can never take this path.
	if args.RtPartyID == "" {
		logger.Warn(
			"party_start_matchmaking without rt_party_id"+
				" (pre-realtime-party client): party=%s user=%s;"+
				" members will submit independent tickets and may"+
				" be split across matches",
			args.PartyID, userID)
	}
	if args.GameMode == "" {
		// Stage 5.7: prefer the leader-persisted mode for the
		// party. Empty (no party_set_mode yet) falls back to the
		// hard-coded ffa default below.
		if persistedMode, ok, modeErr := loadPartyMode(
			ctx, nk, args.PartyID); modeErr == nil && ok {
			args.GameMode = persistedMode
		}
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
	// Any ACTIVE member can start matchmaking, not just the leader
	// (verified against the roster enumerated below). The
	// leader-persisted game mode resolved above — and the leader's
	// level / cheat prefs — stay authoritative regardless of who
	// starts, so the match still reflects the leader's choices.
	leaderID, err := resolvePartyLeader(
		ctx, nk, args.PartyID, group.CreatorId)
	if err != nil {
		logger.Error(
			"resolvePartyLeader(%s): %v", args.PartyID, err)
		return "", err
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

	// Enumerate active members (state 3 = pending invite, not yet a
	// real member) and confirm the caller is one of them BEFORE
	// sending anything — otherwise a non-member could spam a party's
	// members with start notifications.
	memberIDs := make([]string, 0, len(members))
	callerIsMember := false
	for _, m := range members {
		if m.User == nil || m.User.Id == "" {
			continue
		}
		if m.State != nil && m.State.Value == 3 {
			continue
		}
		memberIDs = append(memberIDs, m.User.Id)
		if m.User.Id == userID {
			callerIsMember = true
		}
	}
	if !callerIsMember {
		return "", runtime.NewError(
			"only an active party member can start matchmaking", 7)
	}

	// Dispatch the start notification to every OTHER active member.
	//
	// `leader_id` carries the resolved party leader (informational;
	// followers don't act on it — they join the rt_party_id above).
	//
	// Transient (persistent=false). This used to be persistent, on
	// the theory that a disconnected member should pick the request
	// up on reconnect. That reasoning doesn't survive the realtime
	// party: the starter waits a bounded number of seconds for
	// members to join and then aborts, so a notification replayed
	// minutes later refers to a matchmaking attempt that is long
	// dead — and acting on it would drag the member into a match
	// nobody asked for. A member who missed the window is expected
	// to miss the match.
	notification := map[string]any{
		"party_id":              args.PartyID,
		"rt_party_id":           args.RtPartyID,
		"game_mode":             args.GameMode,
		"leader_id":             leaderID,
		"matchmaker_properties": matchmakerProperties,
	}
	for _, id := range memberIDs {
		// The starter doesn't need a notification — they're acting
		// on the RPC response directly.
		if id == userID {
			continue
		}
		if err := nk.NotificationSend(
			ctx,
			id,
			partyMatchmakingStartSubject,
			notification,
			100,
			"",
			false,
		); err != nil {
			logger.Warn(
				"notify party member %s: %v", id, err)
		}
	}

	logger.Info(
		"party_matchmaking_start dispatched: party=%s rt_party=%s"+
			" leader=%s members=%d game_mode=%s",
		args.PartyID, args.RtPartyID, userID, len(memberIDs),
		args.GameMode)

	resp := partyStartMatchmakingResp{
		OK:                   true,
		PartyID:              args.PartyID,
		RtPartyID:            args.RtPartyID,
		GameMode:             args.GameMode,
		LeaderID:             userID,
		MemberIDs:            memberIDs,
		MatchmakerProperties: matchmakerProperties,
	}
	out, _ := json.Marshal(resp)
	return string(out), nil
}

// partyAbortMatchmakingArgs is the leader's "stand down" payload.
type partyAbortMatchmakingArgs struct {
	PartyID string `json:"party_id"`
	// Reason is an opaque short tag echoed to members so their UI
	// can explain itself ("members_never_joined", "cancelled").
	// Not validated — it's display/telemetry only.
	Reason string `json:"reason"`
}

type partyAbortMatchmakingResp struct {
	OK      bool   `json:"ok"`
	PartyID string `json:"party_id"`
	Reason  string `json:"reason"`
}

func partyAbortMatchmakingRpcFactory(
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
		return partyAbortMatchmakingRpc(
			ctx, logger, nk, games, payload)
	}
}

// partyAbortMatchmakingRpc fans out `party_matchmaking_abort` so
// every member drops out of the "waiting to be matched" state.
//
// The leader calls this when it gives up on an attempt — chiefly
// when a member never joined the realtime party within the client's
// wait window. Aborting is deliberate: the alternative (submit the
// ticket with whoever showed up) silently starts a match the party
// didn't agree to, and the missing member is left staring at a
// lobby while their friends play.
//
// Note this does NOT touch the realtime party or the matchmaker
// ticket — both live on the starter's socket and are that client's
// to tear down. This RPC only carries word to the others, which is
// the part a client can't do for itself.
//
// Authorization: caller must be an ACTIVE member. It used to be
// leader-only, but any member can now start matchmaking (becoming the
// rt-party leader), so the starter — not necessarily the party leader
// — must be able to abort it. Aborting only drops members out of the
// waiting state (it doesn't disband the party), so opening it to any
// active member is low-stakes: the worst case is a member cancelling
// an attempt everyone can simply restart.
func partyAbortMatchmakingRpc(
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
	args := partyAbortMatchmakingArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if args.PartyID == "" {
		return "", runtime.NewError("party_id required", 3)
	}
	if args.Reason == "" {
		args.Reason = "aborted"
	}

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
			"group "+args.PartyID+" is not a party", 3)
	}
	// Confirm the caller is an active member before fanning anything
	// out (a non-member could otherwise spam the party with aborts).
	members, _, err := nk.GroupUsersList(
		ctx, args.PartyID, 100, nil, "")
	if err != nil {
		logger.Error("GroupUsersList(%s): %v", args.PartyID, err)
		return "", err
	}
	memberIDs := make([]string, 0, len(members))
	callerIsMember := false
	for _, m := range members {
		if m.User == nil || m.User.Id == "" {
			continue
		}
		if m.State != nil && m.State.Value == 3 {
			continue
		}
		memberIDs = append(memberIDs, m.User.Id)
		if m.User.Id == userID {
			callerIsMember = true
		}
	}
	if !callerIsMember {
		return "", runtime.NewError(
			"only an active party member can abort matchmaking", 7)
	}

	content := map[string]any{
		"party_id": args.PartyID,
		"reason":   args.Reason,
	}
	for _, id := range memberIDs {
		// The caller is acting on this RPC's response directly and
		// doesn't need to hear its own abort.
		if id == userID {
			continue
		}
		if err := nk.NotificationSend(
			ctx,
			id,
			partyMatchmakingAbortSubject,
			content,
			partyMatchmakingAbortCode,
			"",
			false,
		); err != nil {
			logger.Warn("notify abort to %s: %v", id, err)
		}
	}

	logger.Info(
		"party_matchmaking_abort: party=%s leader=%s reason=%s",
		args.PartyID, userID, args.Reason)

	out, _ := json.Marshal(partyAbortMatchmakingResp{
		OK:      true,
		PartyID: args.PartyID,
		Reason:  args.Reason,
	})
	return string(out), nil
}

// partyInviteArgs is the client → runtime payload for inviting a
// friend to a party on behalf of a (possibly non-leader) member.
type partyInviteArgs struct {
	PartyID  string `json:"party_id"`
	TargetID string `json:"target_id"`
}

func partyInviteRpcFactory(
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
		return partyInviteRpc(ctx, logger, nk, games, payload)
	}
}

// partyInviteRpc lets ANY active party member invite a friend, not
// just the leader. Nakama's client AddGroupUsers requires the caller
// to be a group admin, and only the leader is admin — so a non-leader
// invite fails client-side. This RPC verifies the caller is an active
// member, then performs the add server-side acting as the party leader
// (an admin). For a closed group (parties are created closed) an
// admin-initiated add produces a state-3 invitation the target accepts
// through the normal join flow, exactly like a leader's invite.
//
// Only invite is opened up this way; kick / set-mode / transfer stay
// leader-only on their existing admin-gated paths.
func partyInviteRpc(
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
	args := partyInviteArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if args.PartyID == "" || args.TargetID == "" {
		return "", runtime.NewError(
			"party_id and target_id required", 3)
	}
	if args.TargetID == userID {
		return "", runtime.NewError("cannot invite yourself", 3)
	}

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
			"group "+args.PartyID+" is not a party", 3)
	}
	if group.EdgeCount >= group.MaxCount {
		return "", runtime.NewError("party is full", 9)
	}

	// The caller must be an ACTIVE member (state != 3) to invite.
	members, _, err := nk.GroupUsersList(
		ctx, args.PartyID, 100, nil, "")
	if err != nil {
		logger.Error("GroupUsersList(%s): %v", args.PartyID, err)
		return "", err
	}
	callerIsMember := false
	for _, m := range members {
		if m.User == nil || m.User.Id != userID {
			continue
		}
		// State 3 = pending invite: not yet a real member.
		if m.State == nil || m.State.Value != 3 {
			callerIsMember = true
		}
		break
	}
	if !callerIsMember {
		return "", runtime.NewError(
			"only an active party member can invite", 7)
	}

	// Add as the leader (an admin). resolvePartyLeader honors a
	// transfer override, and the auto-transfer path promotes new
	// leaders to Nakama admin so this add succeeds for them too.
	leaderID, err := resolvePartyLeader(
		ctx, nk, args.PartyID, group.CreatorId)
	if err != nil {
		logger.Error(
			"resolvePartyLeader(%s): %v", args.PartyID, err)
		return "", err
	}
	if err := nk.GroupUsersAdd(
		ctx, leaderID, args.PartyID, []string{args.TargetID},
	); err != nil {
		logger.Warn(
			"party_invite GroupUsersAdd(party=%s target=%s"+
				" as leader=%s): %v",
			args.PartyID, args.TargetID, leaderID, err)
		return "", runtime.NewError(
			"failed to invite: "+err.Error(), 13)
	}

	// Notify the invitee + refresh the party. Mirrors
	// party_join_by_code's explicit fan-out; harmless if the
	// AfterAddGroupUsers hook also fires (the client dedups by id).
	notifyPartyMembers(
		ctx, logger, nk, args.PartyID,
		partyEventInvited, []string{args.TargetID},
	)

	logger.Info(
		"party_invite: party=%s inviter=%s target=%s",
		args.PartyID, userID, args.TargetID)

	out, _ := json.Marshal(map[string]any{
		"ok":        true,
		"party_id":  args.PartyID,
		"target_id": args.TargetID,
	})
	return string(out), nil
}

// loadPartyLeaderOverride reads the (partyLeaderCollection,
// partyID, "") storage row if present. Returns ("", false, nil)
// when the row doesn't exist (caller should fall back to the
// group's creator_id). Errors propagate so the caller can surface
// them rather than silently masking a leadership change.
func loadPartyLeaderOverride(
	ctx context.Context,
	nk runtime.NakamaModule,
	partyID string,
) (string, bool, error) {
	reads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: partyLeaderCollection,
			Key:        partyID,
			UserID:     "",
		}},
	)
	if err != nil {
		return "", false, err
	}
	if len(reads) == 0 {
		return "", false, nil
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(
		[]byte(reads[0].Value), &parsed); err != nil {
		return "", false, nil
	}
	uid, _ := parsed["user_id"].(string)
	if uid == "" {
		return "", false, nil
	}
	return uid, true, nil
}

// resolvePartyLeader returns the current leader's user_id for the
// party, honoring any transfer override stored in
// partyLeaderCollection and falling back to the group's immutable
// creator_id when no override exists. The override is dropped
// automatically by the partyEventLeft handler when the prior leader
// leaves, so a stale row pointing at someone no longer in the
// party shouldn't be possible in steady state. As defense in depth,
// resolvePartyLeader does NOT validate the resolved id against the
// active roster; callers that care (e.g. authorization checks)
// should re-verify membership separately.
func resolvePartyLeader(
	ctx context.Context,
	nk runtime.NakamaModule,
	partyID string,
	fallbackCreatorID string,
) (string, error) {
	uid, ok, err := loadPartyLeaderOverride(ctx, nk, partyID)
	if err != nil {
		return "", err
	}
	if ok {
		return uid, nil
	}
	return fallbackCreatorID, nil
}

// partyTransferLeadershipArgs is the client → runtime payload for
// transferring leadership to another active member.
type partyTransferLeadershipArgs struct {
	PartyID      string `json:"party_id"`
	TargetUserID string `json:"target_user_id"`
}

// partyTransferLeadershipResp echoes the resolved {party, leader}
// pair so the caller's UI can flip leader-only affordances off
// without waiting for the next fetch_party_status round-trip.
type partyTransferLeadershipResp struct {
	OK       bool   `json:"ok"`
	PartyID  string `json:"party_id"`
	LeaderID string `json:"leader_id"`
}

func partyTransferLeadershipRpcFactory(
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
		return partyTransferLeadershipRpc(
			ctx, logger, nk, games, payload)
	}
}

// partyTransferLeadershipRpc reassigns the leader of a party.
// Nakama groups have an immutable creator_id, so the actual
// reassignment is implemented as a storage-row override in
// partyLeaderCollection that resolvePartyLeader consults. The
// override is preserved across the original creator leaving (so
// the second transferee can keep leading), and is implicitly
// dropped along with the group when the party disbands.
//
// Authorization: caller must be the current leader. Returns
// PERMISSION_DENIED (7) otherwise. The target must be an active
// member (state 0/1/2) of the same party. Pending invitees
// (state=3) and non-members are rejected with INVALID_ARGUMENT
// (3) so the failure message points at the data, not the auth
// state.
func partyTransferLeadershipRpc(
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
	args := partyTransferLeadershipArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if args.PartyID == "" {
		return "", runtime.NewError("party_id required", 3)
	}
	if args.TargetUserID == "" {
		return "", runtime.NewError("target_user_id required", 3)
	}
	if args.TargetUserID == userID {
		return "", runtime.NewError(
			"target_user_id is the caller; nothing to transfer", 3)
	}

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
			"group "+args.PartyID+" is not a party", 3)
	}

	currentLeader, err := resolvePartyLeader(
		ctx, nk, args.PartyID, group.CreatorId)
	if err != nil {
		logger.Error(
			"resolvePartyLeader(%s): %v", args.PartyID, err)
		return "", err
	}
	if currentLeader != userID {
		logger.Info(
			"party_transfer_leadership refused: caller=%s"+
				" current_leader=%s party=%s",
			userID, currentLeader, args.PartyID)
		return "", runtime.NewError(
			"only the party leader can transfer leadership", 7)
	}

	// Verify target is an active member. Walking the full roster
	// rather than a single-member lookup also gives us the chance
	// to confirm the caller is still on it — a defense against
	// stale storage overrides surviving a missed AfterLeaveGroup
	// hook.
	members, _, err := nk.GroupUsersList(
		ctx, args.PartyID, 100, nil, "")
	if err != nil {
		logger.Error(
			"GroupUsersList(%s): %v", args.PartyID, err)
		return "", err
	}
	callerActive := false
	targetActive := false
	for _, m := range members {
		if m.User == nil {
			continue
		}
		state := int32(2)
		if m.State != nil {
			state = m.State.Value
		}
		if state == 3 {
			continue
		}
		if m.User.Id == userID {
			callerActive = true
		}
		if m.User.Id == args.TargetUserID {
			targetActive = true
		}
	}
	if !callerActive {
		return "", runtime.NewError(
			"caller is not an active member of party "+
				args.PartyID, 7)
	}
	if !targetActive {
		return "", runtime.NewError(
			"target is not an active member of party "+
				args.PartyID, 3)
	}

	value, _ := json.Marshal(map[string]any{
		"user_id":        args.TargetUserID,
		"transferred_at": nowUnix(),
		"transferred_by": userID,
	})
	if _, err := nk.StorageWrite(
		ctx,
		[]*runtime.StorageWrite{{
			Collection:      partyLeaderCollection,
			Key:             args.PartyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		}},
	); err != nil {
		logger.Error(
			"party_leader write party=%s new=%s: %v",
			args.PartyID, args.TargetUserID, err)
		return "", err
	}
	// Elevate the new leader to admin so their client can call the
	// standard Nakama group endpoints for kick / invite. No-op (and
	// nil error) when the target is already admin or superadmin.
	// We don't demote the previous leader — Nakama doesn't allow
	// demoting the creator, and ad-hoc demotion of a non-creator
	// previous leader would prevent them ever taking leadership
	// back via another transfer.
	if err := nk.GroupUsersPromote(
		ctx, "", args.PartyID, []string{args.TargetUserID},
	); err != nil {
		logger.Warn(
			"GroupUsersPromote party=%s to=%s: %v",
			args.PartyID, args.TargetUserID, err)
	}

	// Fan out so every member's UI refreshes and the new leader's
	// affordances light up immediately. Reuses partyStateChanged
	// so existing clients don't need a new subject filter.
	notifyPartyMembers(
		ctx, logger, nk,
		args.PartyID,
		partyEventLeaderChanged,
		nil,
	)

	logger.Info(
		"party_transfer_leadership: party=%s from=%s to=%s",
		args.PartyID, userID, args.TargetUserID)

	out, _ := json.Marshal(partyTransferLeadershipResp{
		OK:       true,
		PartyID:  args.PartyID,
		LeaderID: args.TargetUserID,
	})
	return string(out), nil
}

// autoTransferIfLeaderDeparted promotes a new leader when the
// current leader has left (or been kicked from) the party. The
// pick is "first active member returned by GroupUsersList" — not
// principled, but deterministic relative to a given roster state,
// which is sufficient to keep the party functional. The new
// leader is also promoted to Nakama admin via GroupUsersPromote
// so the standard kick / invite client paths (which call Nakama
// directly with the caller's session) succeed.
//
// No-op when the departing user wasn't the leader, when there
// are no remaining active members (the party is effectively
// over), or when promote / storage write fails (best-effort:
// resolvePartyLeader on the next caller will fall back to the
// group's creator_id, which keeps the original-creator-still-in-
// party case working).
func autoTransferIfLeaderDeparted(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	partyID string,
	departed string,
) {
	if partyID == "" || departed == "" {
		return
	}
	groups, err := nk.GroupsGetId(ctx, []string{partyID})
	if err != nil || len(groups) == 0 {
		return
	}
	if !strings.HasPrefix(groups[0].Name, partyGroupPrefix) {
		return
	}
	leader, err := resolvePartyLeader(
		ctx, nk, partyID, groups[0].CreatorId)
	if err != nil {
		logger.Warn(
			"auto-transfer resolve party=%s err=%v",
			partyID, err)
		return
	}
	if leader != departed {
		return
	}
	members, _, err := nk.GroupUsersList(
		ctx, partyID, 100, nil, "")
	if err != nil {
		logger.Warn(
			"auto-transfer roster party=%s err=%v",
			partyID, err)
		return
	}
	var next string
	for _, m := range members {
		if m.User == nil || m.User.Id == "" {
			continue
		}
		if m.User.Id == departed {
			continue
		}
		state := int32(2)
		if m.State != nil {
			state = m.State.Value
		}
		if state == 3 {
			continue
		}
		next = m.User.Id
		break
	}
	if next == "" {
		// No remaining active members. Drop the override so the
		// row doesn't leak into a future reuse of this party
		// (Nakama can recycle the group when the last member
		// leaves; in our flow that's already an empty party).
		clearPartyLeaderOverride(ctx, logger, nk, partyID)
		return
	}
	value, _ := json.Marshal(map[string]any{
		"user_id":        next,
		"transferred_at": nowUnix(),
		"transferred_by": "auto:" + departed,
	})
	if _, err := nk.StorageWrite(
		ctx,
		[]*runtime.StorageWrite{{
			Collection:      partyLeaderCollection,
			Key:             partyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		}},
	); err != nil {
		logger.Warn(
			"auto-transfer write party=%s to=%s err=%v",
			partyID, next, err)
		return
	}
	if err := nk.GroupUsersPromote(
		ctx, "", partyID, []string{next},
	); err != nil {
		// Promote failure is recoverable — the override is the
		// app-level source of truth; the only loss is that some
		// Nakama-SDK-direct operations (invite, kick) might fail
		// for the new leader until they're promoted manually.
		logger.Warn(
			"auto-transfer promote party=%s to=%s err=%v",
			partyID, next, err)
	}
	logger.Info(
		"party auto-transfer: party=%s from=%s to=%s",
		partyID, departed, next)
}

// clearPartyLeaderOverride removes the partyLeaderCollection row
// for a party. Best-effort: a missing row (the common case for
// parties that never transferred leadership) is silent.
func clearPartyLeaderOverride(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	partyID string,
) {
	if partyID == "" {
		return
	}
	if err := nk.StorageDelete(
		ctx,
		[]*runtime.StorageDelete{{
			Collection: partyLeaderCollection,
			Key:        partyID,
			UserID:     "",
		}},
	); err != nil {
		// "object not found" surfaces as an error from
		// StorageDelete for missing rows; demote to debug-level
		// noise.
		logger.Debug(
			"clear party leader override party=%s err=%v",
			partyID, err)
	}
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
	// If the leaver was the current (override-aware) leader, auto-
	// transfer to a remaining active member so the party isn't
	// stranded with no leader. This also handles the case where the
	// original creator left long ago and a transferred leader is now
	// leaving — `resolvePartyLeader` would otherwise return the long-
	// gone creator on subsequent calls.
	autoTransferIfLeaderDeparted(
		ctx, logger, nk, in.GetGroupId(), leaver)
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
	// If any kicked user was the current leader, auto-transfer to
	// keep the surviving party functional. The "leader kicks self"
	// path isn't reachable via the client UI (kick_member checks
	// is_leader + skips self), but a malicious / racy direct
	// Nakama API call could exit through here; defense in depth.
	for _, uid := range in.GetUserIds() {
		autoTransferIfLeaderDeparted(
			ctx, logger, nk, in.GetGroupId(), uid)
	}
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

// loadPartyMode reads the (partyModeCollection, partyID, "")
// storage row. Returns ("", false, nil) when the party has no
// mode override yet — callers fall back to the matchmaker's top-
// level default (`ffa` for hopnbop). Errors propagate so a
// transient Postgres blip doesn't silently swap modes mid-flight.
// Stage 5.7.
func loadPartyMode(
	ctx context.Context,
	nk runtime.NakamaModule,
	partyID string,
) (string, bool, error) {
	reads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: partyModeCollection,
			Key:        partyID,
			UserID:     "",
		}},
	)
	if err != nil {
		return "", false, err
	}
	if len(reads) == 0 {
		return "", false, nil
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(
		[]byte(reads[0].Value), &parsed); err != nil {
		return "", false, nil
	}
	mode, _ := parsed["mode_id"].(string)
	if mode == "" {
		return "", false, nil
	}
	return mode, true, nil
}

// partySetModeArgs is the client → runtime payload for the
// leader's mode-selection.
type partySetModeArgs struct {
	PartyID string `json:"party_id"`
	ModeID  string `json:"mode_id"`
}

// partySetModeResp echoes the resolved {party, mode} pair so the
// caller's UI can skip waiting for the next fetch_party_status
// round-trip.
type partySetModeResp struct {
	OK      bool   `json:"ok"`
	PartyID string `json:"party_id"`
	ModeID  string `json:"mode_id"`
}

func partySetModeRpcFactory(
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
		return partySetModeRpc(
			ctx, logger, nk, games, payload)
	}
}

// partySetModeRpc lets the party leader pick a matchmaker mode
// for the whole party. The runtime persists it as a server-owned
// storage row and fans out a `party_state_changed` notification
// with event=mode_changed so followers' UIs update without
// waiting on the catch-up poll. Stage 5.7.
//
// Authorization: caller must be the current leader (resolved via
// partyLeaderCollection override, falling back to group's
// creator_id). PermissionRead=2 on the row keeps the data
// readable by any party member; PermissionWrite=0 keeps this RPC
// as the sole mutation entry point.
//
// The new mode_id is not validated against `game.yaml.matchmaker_
// rules.modes` here — the runtime intentionally trusts the
// caller's pick and validates at allocation time when the
// matched ticket's `game_mode` property reaches fleet_allocator.
// Validation here would double-bind the runtime to the games
// table at write time and reject in the (briefly possible)
// rollout window where a new mode was deployed in game.yaml but
// not yet synced to the games table.
func partySetModeRpc(
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
	args := partySetModeArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if args.PartyID == "" {
		return "", runtime.NewError("party_id required", 3)
	}
	if args.ModeID == "" {
		return "", runtime.NewError("mode_id required", 3)
	}

	// Confirm the group is a party (name prefix) and the caller
	// is the current leader. resolvePartyLeader honors an existing
	// transfer override so the post-transfer leader can still
	// pick the mode.
	groups, err := nk.GroupsGetId(ctx, []string{args.PartyID})
	if err != nil {
		logger.Error(
			"GroupsGetId(%s): %v", args.PartyID, err)
		return "", err
	}
	if len(groups) == 0 {
		return "", runtime.NewError("party not found", 5)
	}
	if !strings.HasPrefix(
		groups[0].Name, partyGroupPrefix) {
		return "", runtime.NewError(
			"group "+args.PartyID+" is not a party", 3)
	}
	leaderID, err := resolvePartyLeader(
		ctx, nk, args.PartyID, groups[0].CreatorId)
	if err != nil {
		logger.Error(
			"resolvePartyLeader(%s): %v", args.PartyID, err)
		return "", err
	}
	if leaderID != userID {
		return "", runtime.NewError(
			"only the party leader can change the game mode", 7)
	}

	value, _ := json.Marshal(map[string]any{
		"mode_id": args.ModeID,
		"set_by":  userID,
		"set_at":  nowUnix(),
	})
	if _, err := nk.StorageWrite(
		ctx,
		[]*runtime.StorageWrite{{
			Collection:      partyModeCollection,
			Key:             args.PartyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		}},
	); err != nil {
		logger.Error(
			"party_set_mode write party=%s mode=%s: %v",
			args.PartyID, args.ModeID, err)
		return "", err
	}

	// Followers refetch on any party_state_changed event, so the
	// new mode propagates without inlining it in the payload.
	// Matches the partyEventReadyChanged shape (no extras).
	notifyPartyMembers(
		ctx, logger, nk,
		args.PartyID,
		partyEventModeChanged,
		nil,
	)

	resp := partySetModeResp{
		OK:      true,
		PartyID: args.PartyID,
		ModeID:  args.ModeID,
	}
	out, _ := json.Marshal(resp)
	return string(out), nil
}

// --- Leader-authoritative level + gameplay-cheat prefs -----------
//
// Both mirror party_mode: a leader-only RPC writes a server-owned
// (party_..._prefs, partyID, "") row, and fleet_allocator reads it at
// allocation and forwards it to the game server via a deploy env var,
// which applies it with the game's own logic. Any active member can
// START matchmaking, but only the leader can SET these, so the match
// reflects the leader's choices regardless of who starts. Unlike
// mode, these aren't shown to followers, so there's no
// party_state_changed fan-out.

// requirePartyLeader confirms the caller's client session, that the
// group is a party, and that the caller is its current leader
// (honoring a transfer override). Returns the caller's userID.
func requirePartyLeader(
	ctx context.Context,
	nk runtime.NakamaModule,
	games *perGameConfig,
	partyID string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	if partyID == "" {
		return "", runtime.NewError("party_id required", 3)
	}
	groups, err := nk.GroupsGetId(ctx, []string{partyID})
	if err != nil {
		return "", err
	}
	if len(groups) == 0 {
		return "", runtime.NewError("party not found", 5)
	}
	if !strings.HasPrefix(groups[0].Name, partyGroupPrefix) {
		return "", runtime.NewError(
			"group "+partyID+" is not a party", 3)
	}
	leaderID, err := resolvePartyLeader(
		ctx, nk, partyID, groups[0].CreatorId)
	if err != nil {
		return "", err
	}
	if leaderID != userID {
		return "", runtime.NewError(
			"only the party leader can change this setting", 7)
	}
	return userID, nil
}

type partySetLevelPrefsArgs struct {
	PartyID string          `json:"party_id"`
	Prefs   json.RawMessage `json:"prefs"`
}

func partySetLevelPrefsRpcFactory(
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
		return partySetLevelPrefsRpc(ctx, logger, nk, games, payload)
	}
}

// partySetLevelPrefsRpc persists the leader's level preferences for
// the party. The `prefs` blob is opaque to the runtime (it's the
// client's LevelPreferences.to_dict); the game server interprets it.
func partySetLevelPrefsRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	args := partySetLevelPrefsArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	userID, err := requirePartyLeader(ctx, nk, games, args.PartyID)
	if err != nil {
		return "", err
	}
	prefs := json.RawMessage("{}")
	if len(args.Prefs) > 0 {
		prefs = args.Prefs
	}
	value, _ := json.Marshal(map[string]any{
		"prefs":  prefs,
		"set_by": userID,
		"set_at": nowUnix(),
	})
	if _, err := nk.StorageWrite(
		ctx,
		[]*runtime.StorageWrite{{
			Collection:      partyLevelPrefsCollection,
			Key:             args.PartyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		}},
	); err != nil {
		logger.Error(
			"party_set_level_prefs write party=%s: %v",
			args.PartyID, err)
		return "", err
	}
	out, _ := json.Marshal(map[string]any{
		"ok": true, "party_id": args.PartyID})
	return string(out), nil
}

type partySetCheatPrefsArgs struct {
	PartyID          string   `json:"party_id"`
	AreCheatsEnabled bool     `json:"are_cheats_enabled"`
	NetworkedCheats  []string `json:"networked_cheats"`
}

func partySetCheatPrefsRpcFactory(
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
		return partySetCheatPrefsRpc(ctx, logger, nk, games, payload)
	}
}

// partySetCheatPrefsRpc persists the leader's gameplay-cheat prefs
// (which networked cheats are on + the master enable). Aesthetic
// (local) cheats are never party-scoped.
func partySetCheatPrefsRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	args := partySetCheatPrefsArgs{}
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	userID, err := requirePartyLeader(ctx, nk, games, args.PartyID)
	if err != nil {
		return "", err
	}
	if args.NetworkedCheats == nil {
		args.NetworkedCheats = []string{}
	}
	value, _ := json.Marshal(map[string]any{
		"are_cheats_enabled": args.AreCheatsEnabled,
		"networked_cheats":   args.NetworkedCheats,
		"set_by":             userID,
		"set_at":             nowUnix(),
	})
	if _, err := nk.StorageWrite(
		ctx,
		[]*runtime.StorageWrite{{
			Collection:      partyCheatPrefsCollection,
			Key:             args.PartyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		}},
	); err != nil {
		logger.Error(
			"party_set_cheat_prefs write party=%s: %v",
			args.PartyID, err)
		return "", err
	}
	out, _ := json.Marshal(map[string]any{
		"ok": true, "party_id": args.PartyID})
	return string(out), nil
}

// loadPartyLevelPrefs returns the leader's level-prefs blob (the
// client's LevelPreferences.to_dict JSON) for the party, or
// ("", false, nil) when none is set.
func loadPartyLevelPrefs(
	ctx context.Context,
	nk runtime.NakamaModule,
	partyID string,
) (string, bool, error) {
	reads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: partyLevelPrefsCollection,
			Key:        partyID,
			UserID:     "",
		}},
	)
	if err != nil {
		return "", false, err
	}
	if len(reads) == 0 {
		return "", false, nil
	}
	parsed := map[string]json.RawMessage{}
	if err := json.Unmarshal(
		[]byte(reads[0].Value), &parsed); err != nil {
		return "", false, nil
	}
	prefs, ok := parsed["prefs"]
	if !ok || len(prefs) == 0 {
		return "", false, nil
	}
	return string(prefs), true, nil
}

// loadPartyCheatPrefs returns the leader's gameplay-cheat prefs blob
// ({are_cheats_enabled, networked_cheats}) for the party, or
// ("", false, nil) when none is set.
func loadPartyCheatPrefs(
	ctx context.Context,
	nk runtime.NakamaModule,
	partyID string,
) (string, bool, error) {
	reads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: partyCheatPrefsCollection,
			Key:        partyID,
			UserID:     "",
		}},
	)
	if err != nil {
		return "", false, err
	}
	if len(reads) == 0 {
		return "", false, nil
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(
		[]byte(reads[0].Value), &parsed); err != nil {
		return "", false, nil
	}
	out, _ := json.Marshal(map[string]any{
		"are_cheats_enabled": parsed["are_cheats_enabled"],
		"networked_cheats":   parsed["networked_cheats"],
	})
	return string(out), true, nil
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
