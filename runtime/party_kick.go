package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Kick authorization (SP33). Kicks previously rode Nakama's
// native group perms with only an After hook for notices, so
// who could kick whom was whatever the group roles happened
// to be. The Before hook checks, in one place at kick time:
// the group is a party, the caller is the (override-aware)
// leader, every target is a current member, and the leader
// is never a target. Nakama performs the write only when the
// hook returns the request, so check and write cannot drift
// apart the way an After-only check could.

func registerPartyKickHook(
	initializer runtime.Initializer,
) error {
	return initializer.RegisterBeforeKickGroupUsers(
		beforeKickGroupUsersHook)
}

func beforeKickGroupUsersHook(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.KickGroupUsersRequest,
) (*api.KickGroupUsersRequest, error) {
	groups, err := nk.GroupsGetId(ctx, []string{in.GetGroupId()})
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		return nil, runtime.NewError("party not found", 5)
	}
	if !strings.HasPrefix(groups[0].GetName(), partyGroupPrefix) {
		return in, nil
	}
	callerID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	leaderID, err := resolvePartyLeader(
		ctx, nk, in.GetGroupId(), groups[0].GetCreatorId())
	if err != nil {
		logger.Warn(
			"kick resolve leader party=%s err=%v",
			in.GetGroupId(), err)
		return nil, err
	}
	if callerID == "" || callerID != leaderID {
		return nil, runtime.NewError(
			"only the party leader can kick members", 7)
	}
	members, _, err := nk.GroupUsersList(
		ctx, in.GetGroupId(), 100, nil, "")
	if err != nil {
		return nil, err
	}
	states := map[string]int32{}
	for _, m := range members {
		if m.User == nil || m.User.Id == "" {
			continue
		}
		state := int32(2)
		if m.State != nil {
			state = m.State.Value
		}
		states[m.User.Id] = state
	}
	for _, target := range in.GetUserIds() {
		if target == leaderID {
			return nil, runtime.NewError(
				"the party leader cannot be kicked", 7)
		}
		if _, ok := states[target]; !ok {
			return nil, runtime.NewError(
				"kick target is not a party member", 5)
		}
	}
	return in, nil
}
