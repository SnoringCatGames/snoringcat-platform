package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// updateAndGetPresence is the AWS-era "publish my presence, get
// my friends' presences back" round trip, ported to Nakama. The
// caller's presence row is written to a per-(user, game) storage
// object; the response includes every friend who is currently
// visible (not `offline`, heartbeat still fresh) in the same game,
// or in any game when include_other_games is set.
//
// Each returned record carries both `status` (machine-readable:
// online / in_match) and `game_id`, so a friends UI can render
// "in a match" distinctly from "in the lobby", and "in another
// game" distinctly from either. `rich_presence` is the localized
// string the friend's own client authored for display.
//
// Note the response deliberately keeps the `online_ids` /
// `online_friends` field names even though the set now includes
// in-match players: renaming them would break every deployed
// client for a cosmetic gain. Read "online" as "visible".
//
// Storage layout (Stage 3 scoping):
//   collection = "presence"
//   key        = "{game_id}/current"  (e.g. "hopnbop/current")
//   value      = { game_id, rich_presence, status, updated_at }
//   read perm  = 2 (public read so friends can see)
//   write perm = 1 (owner write only)
//
// Pre-Stage-3 (bootstrap) rows used key="current" and no
// game_id field. Those rows are overwritten on the next
// presence ping; no migration needed.

const (
	presenceCollection = "presence"
	presenceKeySuffix  = "/current"
	// presenceKeyLegacy is the pre-Stage-3 key. Kept as a
	// constant so account.go's cascade scrub can clean it up
	// for users whose last presence ping predates this change.
	presenceKeyLegacy = "current"
)

// Presence status values. The set is closed: any status the
// runtime doesn't recognize is treated as `presenceStatusOnline`
// so a newer client shipping an additional state degrades to
// "present, activity unknown" rather than vanishing from friends'
// lists.
//
// `offline` is the only value that suppresses a record. It exists
// so a client can announce a clean exit instead of waiting out the
// staleness TTL below.
const (
	presenceStatusOffline = "offline"
	presenceStatusOnline  = "online"
	presenceStatusInMatch = "in_match"
)

// presenceStaleAfterSeconds bounds how long a presence row is
// trusted without a refresh. Clients heartbeat every 30 s
// (FriendsNotificationPoller._PRESENCE_POLL_INTERVAL_SEC), so this
// tolerates three consecutive missed beats before a player drops
// off their friends' lists.
//
// Without this, a crash or a force-quit leaves the last-written row
// in storage forever and the player reads as permanently online —
// there is no disconnect hook to write `offline` on their behalf.
const presenceStaleAfterSeconds int64 = 90

// isPresenceVisible reports whether a stored record should surface
// to a friend. A record is visible when the player hasn't announced
// `offline` and the row is fresh enough to believe.
//
// Records with UpdatedAt == 0 predate the field being written and
// are treated as stale: there's no way to know how old they are,
// and the next heartbeat from a live client repairs it within 30 s.
func isPresenceVisible(rec presenceRecord, now int64) bool {
	if rec.Status == presenceStatusOffline {
		return false
	}
	if rec.UpdatedAt <= 0 {
		return false
	}
	return now-rec.UpdatedAt <= presenceStaleAfterSeconds
}

// normalizePresenceStatus maps a client-supplied status onto the
// closed set above. Empty defaults to `online` (the common case:
// a heartbeat from a client sitting in menus). Unknown values also
// resolve to `online` per the forward-compatibility note above.
func normalizePresenceStatus(status string) string {
	switch status {
	case presenceStatusOffline:
		return presenceStatusOffline
	case presenceStatusInMatch:
		return presenceStatusInMatch
	default:
		return presenceStatusOnline
	}
}

// presenceKey is the storage key for a user's presence row in a
// given game. Empty gameID falls back to the legacy
// (pre-Stage-3) key so a runtime that's still in bootstrap mode
// (no games registered) stays operational.
func presenceKey(gameID string) string {
	if gameID == "" {
		return presenceKeyLegacy
	}
	return gameID + presenceKeySuffix
}

// rich_presence is a free-form opaque blob the client uses to
// describe what the player is doing ("In Lobby", "In Match", etc).
// The client treats it as a string; we store and forward it
// verbatim without inspecting the contents.
//
// `status` is the machine-readable counterpart and IS inspected:
// it drives visibility (see isPresenceVisible) and lets a friend's
// UI branch on activity without parsing a localized string. Clients
// send one of presenceStatus{Offline,Online,InMatch}.
type presenceArgs struct {
	RichPresence string `json:"rich_presence"`
	Status       string `json:"status"`
	// IncludeOtherGames, when true, returns presence records for
	// friends across every registered game (each labeled with its
	// own game_id). Default false: only friends in the caller's
	// own game are returned.
	IncludeOtherGames bool `json:"include_other_games"`
}

type presenceRecord struct {
	GameID       string `json:"game_id"`
	RichPresence string `json:"rich_presence"`
	Status       string `json:"status"`
	UpdatedAt    int64  `json:"updated_at"`
}

type presenceResponse struct {
	OnlineIDs     []string                  `json:"online_ids"`
	OnlineFriends map[string]presenceRecord `json:"online_friends"`
}

// updateAndGetPresenceRpcFactory wires the RPC to the per-game
// config store so the handler can validate that the caller's
// session was minted with a known game_id and read+write the
// game-scoped presence row.
func updateAndGetPresenceRpcFactory(
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
		return updateAndGetPresenceRpc(
			ctx, logger, nk, games, payload)
	}
}

// updateAndGetPresenceRpc writes the caller's presence and
// returns every online friend's presence in one round trip.
func updateAndGetPresenceRpc(
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
	gameID, err := requireGameID(ctx, games)
	if err != nil {
		return "", err
	}
	args := presenceArgs{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return "", runtime.NewError(
				"invalid payload: "+err.Error(), 3)
		}
	}
	args.Status = normalizePresenceStatus(args.Status)

	// Write the caller's presence row, scoped to the caller's
	// game_id. Two games sharing one Nakama instance write to
	// two distinct rows (collection=presence, distinct keys).
	rec := presenceRecord{
		GameID:       gameID,
		RichPresence: args.RichPresence,
		Status:       args.Status,
		UpdatedAt:    nowUnix(),
	}
	value, _ := json.Marshal(rec)
	if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
		Collection:      presenceCollection,
		Key:             presenceKey(gameID),
		UserID:          userID,
		Value:           string(value),
		PermissionRead:  2,
		PermissionWrite: 1,
	}}); err != nil {
		logger.Error("presence write: %v", err)
		return "", err
	}

	// Pull the friends list. Nakama caps page size at 100; for
	// the friend counts we expect this is one round trip.
	friends, _, err := nk.FriendsList(
		ctx, userID, 100, nil, "")
	if err != nil {
		logger.Error("friends list: %v", err)
		return "", err
	}

	// Build the batched read set. Default: each friend's row in
	// the caller's own game. include_other_games: each friend's
	// row in every registered game (one batched read covers all
	// permutations; missing rows are silently absent).
	gameIDsToScan := []string{gameID}
	if args.IncludeOtherGames {
		gameIDsToScan = games.GameIDs()
		if len(gameIDsToScan) == 0 {
			// Bootstrap: no games registered yet. Still scan
			// the caller's effective game (possibly empty
			// during the pre-bootstrap window).
			gameIDsToScan = []string{gameID}
		}
	}
	reads := make(
		[]*runtime.StorageRead, 0,
		len(friends)*len(gameIDsToScan))
	for _, f := range friends {
		if f.User == nil || f.User.Id == "" {
			continue
		}
		// State 0 = MUTUAL friend in Nakama; only mutual friends
		// see each other's presence.
		if f.State != nil && f.State.Value != 0 {
			continue
		}
		for _, gid := range gameIDsToScan {
			reads = append(reads, &runtime.StorageRead{
				Collection: presenceCollection,
				Key:        presenceKey(gid),
				UserID:     f.User.Id,
			})
		}
	}

	resp := presenceResponse{
		OnlineIDs:     []string{},
		OnlineFriends: map[string]presenceRecord{},
	}
	if len(reads) == 0 {
		out, _ := json.Marshal(resp)
		return string(out), nil
	}

	objects, err := nk.StorageRead(ctx, reads)
	if err != nil {
		logger.Error("presence batched read: %v", err)
		return "", err
	}
	now := nowUnix()
	for _, obj := range objects {
		var rec presenceRecord
		if err := json.Unmarshal([]byte(obj.Value), &rec); err != nil {
			continue
		}
		// Suppress players who announced `offline` and players
		// whose last heartbeat is too old to trust. Everything
		// else surfaces with its status intact, so a friend in a
		// match reads as present-and-playing rather than dropping
		// off the list entirely.
		if !isPresenceVisible(rec, now) {
			continue
		}
		// When the stored row predates Stage 3 (no game_id
		// field), default to the read's source key so the
		// response always labels the game. Same-game default
		// path produces gameID; include_other_games path
		// produces whichever bucket the row came from.
		if rec.GameID == "" {
			rec.GameID = gameIDFromKey(obj.Key)
		}
		// Most-recent write wins when a friend has fresh rows in
		// several games (they played two titles inside the
		// staleness window). Picking the newest makes the answer
		// deterministic and gives the UI the game they're
		// actually in right now; iteration order over the batched
		// read isn't guaranteed, so "first wins" would have
		// flip-flopped between games across polls.
		prev, seen := resp.OnlineFriends[obj.UserId]
		if seen && prev.UpdatedAt >= rec.UpdatedAt {
			continue
		}
		if !seen {
			resp.OnlineIDs = append(resp.OnlineIDs, obj.UserId)
		}
		resp.OnlineFriends[obj.UserId] = rec
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// gameIDFromKey extracts the game_id segment from a presence
// storage key. Used for pre-Stage-3 rows that lack an explicit
// game_id field — the key's prefix is the next-best signal.
// Returns "" for the legacy "current" key.
func gameIDFromKey(key string) string {
	if key == presenceKeyLegacy {
		return ""
	}
	// Expected shape: "{game_id}/current". Anything else falls
	// back to empty rather than mis-attributing.
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i]
		}
	}
	return ""
}
