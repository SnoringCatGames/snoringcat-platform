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
}

// runtimeStatusConfig captures the values main.go decided at
// init time so the RPC can echo them back without re-reading
// env vars (which would always be set inside the Nakama
// container regardless of what main saw).
type runtimeStatusConfig struct {
	EdgegapAppName       string
	EdgegapAppVersion    string
	EdgegapTokenSet      bool
	MatchmakerHookActive bool
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
		resp := runtimeStatusResponse{
			BuildID:           BuildID,
			BuildTime:         BuildTime,
			ServerUnixTime:    time.Now().Unix(),
			EdgegapAppName:    cfg.EdgegapAppName,
			EdgegapAppVersion: cfg.EdgegapAppVersion,
			EdgegapTokenSet:   cfg.EdgegapTokenSet,
			RegisteredRpcs: []string{
				"register_server",
				"match_end",
				"bulk_import",
				"runtime_status",
				"record_client_ip",
				"version_check",
				"update_and_get_presence",
				"get_player_stats",
				"get_match_history",
				"export_player_data",
			},
			RegisteredHooks: hooks,
		}
		out, err := json.Marshal(resp)
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}
