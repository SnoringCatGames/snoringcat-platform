package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Storage layout for client IPs. Each user owns a single record
// at (collection=clientIPCollection, key=clientIPKey, user_id=<self>).
// PermissionRead=1 ("owner read") + PermissionWrite=0 ("server-
// only write") means only the runtime can mutate it; the user
// can read their own.
const (
	clientIPCollection = "client_ip"
	clientIPKey        = "latest"
	// readClientIP discards records older than this. If a player
	// authenticated days ago, the IP from then is unlikely to
	// reflect their current location well enough to feed Edgegap.
	clientIPMaxAgeSec = 3600
)

// clientIPRecord is the on-disk shape inside Nakama Storage.
type clientIPRecord struct {
	IP        string `json:"ip"`
	Timestamp int64  `json:"ts"`
}

// recordClientIPRpc is invoked by the client right before it
// joins the matchmaker. The runtime sees the connection's
// public IP via RUNTIME_CTX_CLIENT_IP and persists it so
// fleet_allocator's MatchmakerMatched hook can feed Edgegap a
// usable `ip_list` for region selection.
//
// Calling pattern: requires a user session (RUNTIME_CTX_USER_ID
// must be set). Server-to-server calls have no caller-scoped
// IP and are rejected.
func recordClientIPRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if userID == "" {
		return "", runtime.NewError(
			"record_client_ip requires a user session", 16)
	}
	clientIP, _ := ctx.Value(runtime.RUNTIME_CTX_CLIENT_IP).(string)
	if clientIP == "" {
		return "", runtime.NewError(
			"no client IP in request context", 13)
	}

	rec := clientIPRecord{IP: clientIP, Timestamp: time.Now().Unix()}
	value, _ := json.Marshal(rec)
	if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
		Collection:      clientIPCollection,
		Key:             clientIPKey,
		UserID:          userID,
		Value:           string(value),
		PermissionRead:  1, // Owner-readable.
		PermissionWrite: 0, // Server-only writes.
	}}); err != nil {
		logger.Error("record_client_ip storage write: %v", err)
		return "", err
	}

	resp, _ := json.Marshal(map[string]any{"ip": clientIP})
	return string(resp), nil
}

// readClientIP returns the most recently recorded IP for a
// user, or an empty string if no record exists or it is older
// than clientIPMaxAgeSec. This is the read path the matchmaker
// hook uses to assemble Edgegap's ip_list.
func readClientIP(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) (string, error) {
	objs, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: clientIPCollection,
		Key:        clientIPKey,
		UserID:     userID,
	}})
	if err != nil {
		return "", fmt.Errorf("storage read: %w", err)
	}
	if len(objs) == 0 {
		return "", nil
	}
	rec := clientIPRecord{}
	if err := json.Unmarshal([]byte(objs[0].Value), &rec); err != nil {
		return "", fmt.Errorf("decode record: %w", err)
	}
	if rec.IP == "" {
		return "", nil
	}
	if time.Now().Unix()-rec.Timestamp > clientIPMaxAgeSec {
		return "", nil
	}
	return rec.IP, nil
}
