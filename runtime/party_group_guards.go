package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Membership and metadata changes on party groups go through
// the party RPCs and hooks, which enforce the party rules
// (who leads, who may be added, what a party is called). The
// Before hooks below refuse the raw group calls for `party-`
// groups (a leader's add is held to the invite RPC's checks
// instead), so those rules cannot be stepped around with the
// group API directly. Non-party groups pass through
// untouched. The runtime's own module calls are not API
// calls, so the party RPCs keep working.

func registerPartyGroupGuards(
	initializer runtime.Initializer,
) error {
	if err := initializer.RegisterBeforeAddGroupUsers(
		beforeAddGroupUsersPartyGuard); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeBanGroupUsers(
		beforeBanGroupUsersPartyGuard); err != nil {
		return err
	}
	if err := initializer.RegisterBeforePromoteGroupUsers(
		beforePromoteGroupUsersPartyGuard); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeDemoteGroupUsers(
		beforeDemoteGroupUsersPartyGuard); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeUpdateGroup(
		beforeUpdateGroupPartyGuard); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeDeleteGroup(
		beforeDeleteGroupPartyGuard); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeJoinGroup(
		beforeJoinGroupPartyGuard); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeCreateGroup(
		beforeCreateGroupPartyGuard); err != nil {
		return err
	}
	return nil
}

// A player holds one live party they created. Creating a
// `party-` group ends any party the caller created earlier
// (the full disband, so its members hear about it and its
// rows go with it) instead of letting stale parties pile up.
// Parties the caller only belongs to are left alone.
func beforeCreateGroupPartyGuard(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.CreateGroupRequest,
) (*api.CreateGroupRequest, error) {
	if !strings.HasPrefix(in.GetName(), partyGroupPrefix) {
		return in, nil
	}
	userID, _ := ctx.Value(
		runtime.RUNTIME_CTX_USER_ID).(string)
	if userID == "" {
		return in, nil
	}
	memberships, err := partyMemberships(ctx, nk, userID, "")
	if err != nil {
		return nil, err
	}
	for partyID := range memberships {
		groups, err := nk.GroupsGetId(ctx, []string{partyID})
		if err != nil {
			return nil, err
		}
		if len(groups) == 0 ||
			groups[0].GetCreatorId() != userID {
			continue
		}
		if err := disbandPartyGroup(
			ctx, logger, nk, partyID); err != nil {
			return nil, err
		}
	}
	return in, nil
}
// partyGroupGuard refuses the call when the group is a party
// group. A group that does not resolve passes through, and
// Nakama reports the bad id itself.
func partyGroupGuard(
	ctx context.Context,
	nk runtime.NakamaModule,
	groupID string,
) error {
	groups, err := nk.GroupsGetId(ctx, []string{groupID})
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}
	if !strings.HasPrefix(
		groups[0].GetName(), partyGroupPrefix) {
		return nil
	}
	return runtime.NewError(
		"party groups are managed by the party system", 7)
}

// A raw add to a party group is the one raw call with a
// legitimate shape: the party leader adding a player,
// which the clients' own invite flows and the party_invite
// RPC both perform. It is held to the invite RPC's checks
// (caller leads the party, no block either way, target not
// already in a party); every other raw add is refused.
func beforeAddGroupUsersPartyGuard(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.AddGroupUsersRequest,
) (*api.AddGroupUsersRequest, error) {
	groups, err := nk.GroupsGetId(ctx, []string{in.GetGroupId()})
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 || !strings.HasPrefix(
		groups[0].GetName(), partyGroupPrefix) {
		return in, nil
	}
	callerID, _ := ctx.Value(
		runtime.RUNTIME_CTX_USER_ID).(string)
	leaderID, err := resolvePartyLeader(
		ctx, nk, in.GetGroupId(), groups[0].GetCreatorId())
	if err != nil {
		logger.Warn(
			"add guard resolve leader party=%s err=%v",
			in.GetGroupId(), err)
		return nil, err
	}
	if callerID == "" || callerID != leaderID {
		return nil, runtime.NewError(
			"only the party leader can add members", 7)
	}
	for _, targetID := range in.GetUserIds() {
		blocked, err := usersBlockEachOther(
			ctx, nk, callerID, targetID)
		if err != nil {
			return nil, err
		}
		if blocked {
			return nil, runtime.NewError(
				"this invite is unavailable", 7)
		}
		targetParties, err := partyMemberships(
			ctx, nk, targetID, in.GetGroupId())
		if err != nil {
			return nil, err
		}
		if sharedParty(targetParties) != "" {
			return nil, runtime.NewError(
				"that player is already in a party", 6)
		}
	}
	return in, nil
}

func beforeBanGroupUsersPartyGuard(
	ctx context.Context,
	_ runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.BanGroupUsersRequest,
) (*api.BanGroupUsersRequest, error) {
	if err := partyGroupGuard(
		ctx, nk, in.GetGroupId()); err != nil {
		return nil, err
	}
	return in, nil
}

func beforePromoteGroupUsersPartyGuard(
	ctx context.Context,
	_ runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.PromoteGroupUsersRequest,
) (*api.PromoteGroupUsersRequest, error) {
	if err := partyGroupGuard(
		ctx, nk, in.GetGroupId()); err != nil {
		return nil, err
	}
	return in, nil
}

func beforeDemoteGroupUsersPartyGuard(
	ctx context.Context,
	_ runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.DemoteGroupUsersRequest,
) (*api.DemoteGroupUsersRequest, error) {
	if err := partyGroupGuard(
		ctx, nk, in.GetGroupId()); err != nil {
		return nil, err
	}
	return in, nil
}

func beforeUpdateGroupPartyGuard(
	ctx context.Context,
	_ runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.UpdateGroupRequest,
) (*api.UpdateGroupRequest, error) {
	if err := partyGroupGuard(
		ctx, nk, in.GetGroupId()); err != nil {
		return nil, err
	}
	return in, nil
}

func beforeDeleteGroupPartyGuard(
	ctx context.Context,
	_ runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.DeleteGroupRequest,
) (*api.DeleteGroupRequest, error) {
	if err := partyGroupGuard(
		ctx, nk, in.GetGroupId()); err != nil {
		return nil, err
	}
	return in, nil
}

// Joining a party (accepting an invite) moves the joiner
// into it. A joiner already in a party with other members
// stays there; a party holding only the joiner does not
// block the move, and the after-join hook disbands it.
func beforeJoinGroupPartyGuard(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	in *api.JoinGroupRequest,
) (*api.JoinGroupRequest, error) {
	groups, err := nk.GroupsGetId(ctx, []string{in.GetGroupId()})
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 || !strings.HasPrefix(
		groups[0].GetName(), partyGroupPrefix) {
		return in, nil
	}
	joiner, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if joiner == "" {
		return in, nil
	}
	parties, err := partyMemberships(
		ctx, nk, joiner, in.GetGroupId())
	if err != nil {
		logger.Warn(
			"join guard memberships party=%s err=%v",
			in.GetGroupId(), err)
		return nil, err
	}
	if sharedParty(parties) != "" {
		return nil, runtime.NewError(
			"that player is already in a party", 6)
	}
	return in, nil
}
