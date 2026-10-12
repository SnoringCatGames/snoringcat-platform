package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/url"
	"strings"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Operator-only RPCs need more than the runtime HTTP key.
//
// The HTTP key (`?http_key=`, Nakama's --runtime.http_key) is what
// game servers authenticate with, and it shipped inside every
// hopnbop client build from 2026-05-02 until the release after
// 0.47.0, committed to a public repo. Anyone can extract it, so it
// proves nothing about the caller. That is tolerable for register_server
// and match_end, which carry their own integrity checks, but not for
// RPCs that rewrite platform-wide state.
//
// register_game (replaces a game's whole per-game config) and
// bulk_import (writes storage and leaderboard records for any user)
// therefore also require GAME_ADMIN_KEY: a second secret that only
// the operator's deploy tooling holds (credentials.env and the
// GAME_ADMIN_KEY GitHub secret). It never ships in a client or a
// game server. Callers pass it beside the HTTP key:
//
//	POST /v2/rpc/register_game?http_key=<k>&admin_key=<k>&unwrap=true
//
// Fail closed: while GAME_ADMIN_KEY is unset, every call to a gated
// RPC is refused, and the rest of the runtime loads and serves as
// usual.

// adminKeyEnvVar names the runtime env var holding the expected key.
const adminKeyEnvVar = "GAME_ADMIN_KEY"

// adminKeyQueryParam is the RPC query parameter callers put it in.
const adminKeyQueryParam = "admin_key"

// adminGate checks the admin key on operator-only RPCs. It keeps
// only a SHA-256 digest of the configured key: comparing digests
// makes the check constant-time and length-independent, and the key
// itself never sits in a struct that might get logged or marshaled.
type adminGate struct {
	digest     [sha256.Size]byte
	configured bool
}

// newAdminGate builds the gate from the GAME_ADMIN_KEY value.
// Surrounding whitespace is dropped (a stray newline from a pasted
// secret would otherwise make every call fail). An empty value
// yields a gate that refuses everything.
func newAdminGate(key string) *adminGate {
	key = strings.TrimSpace(key)
	if key == "" {
		return &adminGate{}
	}
	return &adminGate{
		digest:     sha256.Sum256([]byte(key)),
		configured: true,
	}
}

// isConfigured reports whether GAME_ADMIN_KEY was set. A nil gate
// counts as unconfigured, so a caller that forgot to wire one in
// fails closed.
func (g *adminGate) isConfigured() bool {
	return g != nil && g.configured
}

// authorize returns nil when the call came over the HTTP-key path
// and carries the right admin_key. Otherwise it returns the error
// to hand back to the caller. The error never echoes the key.
//
// Codes: PERMISSION_DENIED (7) for a client-session caller,
// FAILED_PRECONDITION (9) when GAME_ADMIN_KEY is unset, and
// UNAUTHENTICATED (16) for a missing or wrong admin_key. Nakama maps
// them to HTTP 403, 400 and 401.
func (g *adminGate) authorize(ctx context.Context, rpcName string) error {
	if err := requireServerToServer(ctx); err != nil {
		return err
	}
	if !g.isConfigured() {
		return runtime.NewError(
			rpcName+" is disabled: "+adminKeyEnvVar+
				" is not set on this runtime", 9)
	}
	provided := adminKeyFromContext(ctx)
	if provided == "" {
		return runtime.NewError(
			rpcName+" requires the "+adminKeyQueryParam+
				" query parameter", 16)
	}
	sum := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(sum[:], g.digest[:]) != 1 {
		return runtime.NewError("invalid "+adminKeyQueryParam, 16)
	}
	return nil
}

// adminKeyFromContext reads the admin_key query parameter of the
// HTTP RPC request. Nakama hands RPCs the request's query string as
// RUNTIME_CTX_QUERY_PARAMS. RPCs that arrive over the realtime
// socket carry none, so they can never pass the gate.
func adminKeyFromContext(ctx context.Context) string {
	var values []string
	switch params := ctx.Value(runtime.RUNTIME_CTX_QUERY_PARAMS).(type) {
	case map[string][]string:
		values = params[adminKeyQueryParam]
	case url.Values:
		values = params[adminKeyQueryParam]
	}
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
