package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

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
	_ *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
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
