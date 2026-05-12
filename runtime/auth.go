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

// gameIDFromVars pulls the per-game scope marker out of a session-
// vars map. Nakama exposes the auth-time vars on every RPC via
// RUNTIME_CTX_VARS — provided the client passed them on its
// authenticate_* call. Returns "" when missing.
func gameIDFromVars(vars map[string]string) string {
	if vars == nil {
		return ""
	}
	return vars["game_id"]
}

// requireGameID returns the game_id the caller's session was
// minted with, or an error when missing / unknown. Used by every
// stateful client-session RPC to scope reads and writes per game
// instead of trusting a client-passed game_id param.
//
// Bootstrap behavior: when the `games` table is empty (e.g.
// immediately after first deploy, before sync-game-config.ps1
// has run), the helper returns the raw vars value without
// validating against the cache — possibly the empty string. The
// caller can still gate state writes on a non-empty result; the
// transitional window ends as soon as the first game is
// registered.
//
// Returns INVALID_ARGUMENT (3) when game_id is missing once the
// `games` cache is populated, or when the provided game_id
// references a game that hasn't been registered.
func requireGameID(
	ctx context.Context,
	games *perGameConfig,
) (string, error) {
	vars, _ := ctx.Value(runtime.RUNTIME_CTX_VARS).(map[string]string)
	gameID := gameIDFromVars(vars)
	if len(games.GameIDs()) == 0 {
		// Pre-bootstrap: no games registered yet. Pass through
		// whatever the session has (including empty) so the
		// runtime stays usable before the first
		// `register_game` call lands.
		return gameID, nil
	}
	if gameID == "" {
		return "", runtime.NewError(
			"game_id required in session vars (re-authenticate"+
				" with a client that passes game_id in its"+
				" authenticate vars)", 3)
	}
	if _, ok := games.Get(gameID); !ok {
		return "", runtime.NewError(
			"unknown game_id in session: "+gameID, 3)
	}
	return gameID, nil
}

// validateGameIDInVars enforces the same rule as requireGameID,
// but pre-authentication. Called from each RegisterBeforeAuthenticate*
// hook with the inbound request's Vars map (the vars haven't been
// baked into a session token yet).
//
// Same bootstrap exemption as requireGameID: if the `games`
// table is empty, all authenticate calls pass through, even
// those that didn't supply game_id.
func validateGameIDInVars(
	vars map[string]string,
	games *perGameConfig,
) error {
	if len(games.GameIDs()) == 0 {
		return nil
	}
	gameID := gameIDFromVars(vars)
	if gameID == "" {
		return runtime.NewError(
			"game_id required in authenticate vars", 3)
	}
	if _, ok := games.Get(gameID); !ok {
		return runtime.NewError(
			"unknown game_id: "+gameID, 3)
	}
	return nil
}
