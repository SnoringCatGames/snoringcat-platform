package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// party_disband (SP34): the leader ends the party for everyone.
// Leaving alone only hands leadership on, so a party the leader
// wants gone needs this explicit end. Leader-only (the
// override-aware check, so a transferred leader can disband);
// deletes the group and every party-scoped row.

type partyDisbandArgs struct {
	PartyID string `json:"party_id"`
}

func partyDisbandRpcFactory(
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
		return partyDisbandRpc(ctx, logger, nk, games, payload)
	}
}

func partyDisbandRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	var args partyDisbandArgs
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"malformed payload (expected JSON object)", 3)
	}
	if _, err := requirePartyLeader(
		ctx, nk, games, args.PartyID); err != nil {
		return "", err
	}

	// Collect members before the delete: they get the
	// notification, and their ready rows are party state too.
	members, _, err := nk.GroupUsersList(
		ctx, args.PartyID, 100, nil, "")
	if err != nil {
		return "", err
	}
	memberIDs := []string{}
	for _, m := range members {
		if m.User != nil && m.User.Id != "" {
			memberIDs = append(memberIDs, m.User.Id)
		}
	}
	notifyPartyMembers(
		ctx, logger, nk, args.PartyID,
		partyEventDisbanded, memberIDs)

	if err := nk.GroupDelete(ctx, args.PartyID); err != nil {
		logger.Error(
			"party_disband delete party=%s err=%v",
			args.PartyID, err)
		return "", err
	}

	deletes := []*runtime.StorageDelete{}
	for _, uid := range memberIDs {
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: partyReadyCollection,
			Key:        args.PartyID,
			UserID:     uid,
		})
	}
	for _, collection := range []string{
		partyModeCollection,
		partyLevelPrefsCollection,
		partyCheatPrefsCollection,
		partyLeaderCollection,
	} {
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: collection,
			Key:        args.PartyID,
			UserID:     "",
		})
	}
	if err := nk.StorageDelete(ctx, deletes); err != nil {
		logger.Warn(
			"party_disband row cleanup party=%s err=%v",
			args.PartyID, err)
	}
	// Clear the invite code pair when one exists.
	reads, _ := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: partyInviteCodeCollection,
			Key:        partyInviteCodeReverseKey(args.PartyID),
			UserID:     "",
		}},
	)
	if len(reads) > 0 {
		var codeRow map[string]any
		if json.Unmarshal([]byte(reads[0].Value), &codeRow) == nil {
			if code, _ := codeRow["code"].(string); code != "" {
				deleteStalePartyInviteCode(
					ctx, logger, nk, code, args.PartyID)
			}
		}
	}

	out, _ := json.Marshal(map[string]any{
		"party_id":  args.PartyID,
		"disbanded": true,
	})
	return string(out), nil
}
