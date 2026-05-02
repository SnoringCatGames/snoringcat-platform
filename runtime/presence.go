package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// updateAndGetPresence is the AWS-era "publish my presence, get
// my friends' presences back" round trip, ported to Nakama. The
// caller's presence row is written to a per-user storage object;
// the response includes every friend whose status is "online".
//
// Storage layout:
//   collection = "presence", key = "current"
//   value      = { rich_presence, status, updated_at }
//   read perm  = 2 (public read so friends can see)
//   write perm = 1 (owner write only)

// rich_presence is a free-form opaque blob the client uses to
// describe what the player is doing ("In Lobby", "In Match", etc).
// The client treats it as a string; we store and forward it
// verbatim without inspecting the contents.
type presenceArgs struct {
	RichPresence string `json:"rich_presence"`
	Status       string `json:"status"`
}

type presenceRecord struct {
	RichPresence string `json:"rich_presence"`
	Status       string `json:"status"`
	UpdatedAt    int64  `json:"updated_at"`
}

type presenceResponse struct {
	OnlineIDs     []string                  `json:"online_ids"`
	OnlineFriends map[string]presenceRecord `json:"online_friends"`
}

// updateAndGetPresenceRpc writes the caller's presence and
// returns every online friend's presence in one round trip.
func updateAndGetPresenceRpc(
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
	args := presenceArgs{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return "", runtime.NewError(
				"invalid payload: "+err.Error(), 3)
		}
	}
	if args.Status == "" {
		args.Status = "online"
	}

	// Write the caller's presence row.
	rec := presenceRecord{
		RichPresence: args.RichPresence,
		Status:       args.Status,
		UpdatedAt:    nowUnix(),
	}
	value, _ := json.Marshal(rec)
	if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
		Collection:      "presence",
		Key:             "current",
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

	// Read each friend's presence row in a single batched call.
	reads := make([]*runtime.StorageRead, 0, len(friends))
	friendIDs := make([]string, 0, len(friends))
	for _, f := range friends {
		if f.User == nil || f.User.Id == "" {
			continue
		}
		// State 0 = MUTUAL friend in Nakama; only mutual friends
		// see each other's presence.
		if f.State != nil && f.State.Value != 0 {
			continue
		}
		friendIDs = append(friendIDs, f.User.Id)
		reads = append(reads, &runtime.StorageRead{
			Collection: "presence",
			Key:        "current",
			UserID:     f.User.Id,
		})
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
	for _, obj := range objects {
		var rec presenceRecord
		if err := json.Unmarshal([]byte(obj.Value), &rec); err != nil {
			continue
		}
		if rec.Status != "online" {
			continue
		}
		resp.OnlineIDs = append(resp.OnlineIDs, obj.UserId)
		resp.OnlineFriends[obj.UserId] = rec
	}

	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
