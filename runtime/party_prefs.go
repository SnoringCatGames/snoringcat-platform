package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// party_set_prefs (H55): the leader's mode, level prefs and
// cheat prefs were three separate RPCs, so a sync wrote them
// one at a time and a start in between saw a mix. This RPC
// writes whichever prefs are present in one StorageWrite and
// fans out one notification. The three single-pref RPCs stay
// for shipped clients.

type partySetPrefsArgs struct {
	PartyID          string          `json:"party_id"`
	ModeID           *string         `json:"mode_id"`
	Prefs            json.RawMessage `json:"prefs"`
	AreCheatsEnabled *bool           `json:"are_cheats_enabled"`
	NetworkedCheats  []string        `json:"networked_cheats"`
}

// The party prefs rows are server-owned, so the storage hook
// never sees these writes. The caps below bound what one call
// can store: the client's prefs blob is a small dictionary,
// a mode id names one of the game's modes, and the cheat list
// holds short cheat names.
const (
	partyPrefsMaxBytes           = 4096
	partyModeIDMaxBytes          = 64
	partyNetworkedCheatsMaxCount = 64
	partyNetworkedCheatMaxBytes  = 64
)

func validatePartyPrefsBlob(prefs json.RawMessage) error {
	if len(prefs) > partyPrefsMaxBytes {
		return runtime.NewError("prefs too large", 3)
	}
	return nil
}

func validatePartyModeID(modeID string) error {
	if len(modeID) > partyModeIDMaxBytes {
		return runtime.NewError("mode_id too long", 3)
	}
	return nil
}

func validatePartyNetworkedCheats(cheats []string) error {
	if len(cheats) > partyNetworkedCheatsMaxCount {
		return runtime.NewError("too many networked_cheats", 3)
	}
	for _, cheat := range cheats {
		if len(cheat) > partyNetworkedCheatMaxBytes {
			return runtime.NewError("networked_cheat too long", 3)
		}
	}
	return nil
}

func partySetPrefsRpcFactory(
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
		return partySetPrefsRpc(ctx, logger, nk, games, payload)
	}
}

func partySetPrefsRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
	payload string,
) (string, error) {
	var args partySetPrefsArgs
	if err := json.Unmarshal([]byte(payload), &args); err != nil {
		return "", runtime.NewError(
			"malformed payload (expected JSON object)", 3)
	}
	userID, err := requirePartyLeader(ctx, nk, games, args.PartyID)
	if err != nil {
		return "", err
	}
	if args.ModeID != nil {
		if err := validatePartyModeID(*args.ModeID); err != nil {
			return "", err
		}
	}
	if err := validatePartyPrefsBlob(args.Prefs); err != nil {
		return "", err
	}
	if err := validatePartyNetworkedCheats(
		args.NetworkedCheats); err != nil {
		return "", err
	}

	now := nowUnix()
	writes := []*runtime.StorageWrite{}
	if args.ModeID != nil {
		if *args.ModeID == "" {
			return "", runtime.NewError("mode_id empty", 3)
		}
		value, _ := json.Marshal(map[string]any{
			"mode_id": *args.ModeID,
			"set_by":  userID,
			"set_at":  now,
		})
		writes = append(writes, &runtime.StorageWrite{
			Collection:      partyModeCollection,
			Key:             args.PartyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		})
	}
	if len(args.Prefs) > 0 {
		value, _ := json.Marshal(map[string]any{
			"prefs":  args.Prefs,
			"set_by": userID,
			"set_at": now,
		})
		writes = append(writes, &runtime.StorageWrite{
			Collection:      partyLevelPrefsCollection,
			Key:             args.PartyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		})
	}
	if args.AreCheatsEnabled != nil {
		cheats := args.NetworkedCheats
		if cheats == nil {
			cheats = []string{}
		}
		value, _ := json.Marshal(map[string]any{
			"are_cheats_enabled": *args.AreCheatsEnabled,
			"networked_cheats":   cheats,
			"set_by":             userID,
			"set_at":             now,
		})
		writes = append(writes, &runtime.StorageWrite{
			Collection:      partyCheatPrefsCollection,
			Key:             args.PartyID,
			UserID:          "",
			Value:           string(value),
			PermissionRead:  2,
			PermissionWrite: 0,
		})
	}
	if len(writes) == 0 {
		return "", runtime.NewError("no prefs to set", 3)
	}
	if _, err := nk.StorageWrite(ctx, writes); err != nil {
		logger.Error(
			"party_set_prefs write party=%s err=%v",
			args.PartyID, err)
		return "", err
	}
	notifyPartyMembers(
		ctx, logger, nk, args.PartyID, partyEventModeChanged, nil)

	out, _ := json.Marshal(map[string]any{
		"party_id": args.PartyID,
		"written":  len(writes),
	})
	return string(out), nil
}
