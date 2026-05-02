package main

import (
	"context"

	"github.com/heroiclabs/nakama-common/runtime"
)

// requireServerToServer rejects calls that arrived through a
// client session. Server-to-server callers (game server,
// internal probes, admin tools) authenticate via the runtime
// HTTP key — `?http_key=...` on the RPC URL — and Nakama
// leaves RUNTIME_CTX_USER_ID empty in that case. Client
// sessions populate it with the player's user_id.
//
// Use this for any RPC that mutates server-side state outside
// the calling player's own scope (leaderboard writes,
// registration, bulk import, runtime introspection).
//
// Returns a runtime.Error with gRPC code PERMISSION_DENIED (7)
// when called from a client session.
func requireServerToServer(ctx context.Context) error {
	userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if userID != "" {
		return runtime.NewError(
			"forbidden: server-to-server callers only", 7)
	}
	return nil
}

// requireClientSession returns the caller's user ID when the RPC
// arrived through an authenticated client session, or an error
// otherwise. Mirror of requireServerToServer for RPCs that act
// on the calling player's own scope (presence, stats, history,
// data export).
//
// Returns a runtime.Error with gRPC code UNAUTHENTICATED (16) on
// missing session.
func requireClientSession(ctx context.Context) (string, error) {
	userID, _ := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if userID == "" {
		return "", runtime.NewError("client session required", 16)
	}
	return userID, nil
}
