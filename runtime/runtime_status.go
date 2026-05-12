package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// BuildID is injected at link time via ldflags. CI sets it to the
// git commit SHA so a `runtime_status` response unambiguously
// identifies which build is running. Default value is what shows
// up for local builds without ldflags.
var BuildID = "dev"

// BuildTime is also injected at link time via ldflags (RFC3339).
// Lets us tell how stale a deployed plugin is without consulting
// CI history.
var BuildTime = "unknown"

// runtimeStatusResponse is the shape returned from the
// `runtime_status` RPC. Adding fields is fine; renaming or
// removing them is a breaking change for the desktop probe.
type runtimeStatusResponse struct {
	BuildID            string   `json:"build_id"`
	BuildTime          string   `json:"build_time"`
	ServerUnixTime     int64    `json:"server_unix_time"`
	EdgegapAppName     string   `json:"edgegap_app_name"`
	EdgegapAppVersion  string   `json:"edgegap_app_version"`
	EdgegapTokenSet    bool     `json:"edgegap_token_set"`
	RegisteredRpcs     []string `json:"registered_rpcs"`
	RegisteredHooks    []string `json:"registered_hooks"`
	RegisteredGames    []string `json:"registered_games"`
}

// runtimeStatusConfig captures the values main.go decided at
// init time so the RPC can echo them back without re-reading
// env vars (which would always be set inside the Nakama
// container regardless of what main saw).
//
// RegisteredRpcs is a pointer to the slice main.go appends into
// as it registers each RPC. The factory closure dereferences at
// call time, so the response always reflects the actual
// registered set (notably: bulk_import only appears when
// BULK_IMPORT_ENABLED gated it on).
//
// Games is a pointer-to-pointer to the perGameConfig store.
// The status RPC is registered before newPerGameConfig runs
// (so runtime_status stays diagnosable even when DDL/cache-warm
// fails); main.go assigns `*Games` once init succeeds, and the
// RPC handler dereferences at call time.
type runtimeStatusConfig struct {
	EdgegapAppName       string
	EdgegapAppVersion    string
	EdgegapTokenSet      bool
	MatchmakerHookActive bool
	RegisteredRpcs       *[]string
	Games                **perGameConfig
}

// statusRpcFactory binds the snapshot of configuration into a
// runtime.RpcFunction the initializer can register.
//
// Gated to server-to-server callers (HTTP key) so the response
// fields, which describe internal config and the registered RPC
// surface, are not enumerable by random Nakama session holders.
func statusRpcFactory(cfg runtimeStatusConfig) func(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	return func(
		ctx context.Context,
		_ runtime.Logger,
		_ *sql.DB,
		_ runtime.NakamaModule,
		_ string,
	) (string, error) {
		if err := requireServerToServer(ctx); err != nil {
			return "", err
		}
		hooks := []string{}
		if cfg.MatchmakerHookActive {
			hooks = append(hooks, "matchmaker_matched")
		}
		// Snapshot the registered set at call time, not init
		// time — main.go appends as RPCs register and bulk_import
		// is env-gated.
		rpcs := []string{}
		if cfg.RegisteredRpcs != nil {
			rpcs = append(rpcs, (*cfg.RegisteredRpcs)...)
		}
		// Same logic for games: snapshot at call time so a
		// runtime_status hit after a register_game upsert
		// reflects the new row.
		games := []string{}
		if cfg.Games != nil && *cfg.Games != nil {
			games = (*cfg.Games).GameIDs()
		}
		resp := runtimeStatusResponse{
			BuildID:           BuildID,
			BuildTime:         BuildTime,
			ServerUnixTime:    time.Now().Unix(),
			EdgegapAppName:    cfg.EdgegapAppName,
			EdgegapAppVersion: cfg.EdgegapAppVersion,
			EdgegapTokenSet:   cfg.EdgegapTokenSet,
			RegisteredRpcs:    rpcs,
			RegisteredHooks:   hooks,
			RegisteredGames:   games,
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}
