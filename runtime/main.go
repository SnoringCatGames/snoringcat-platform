// Snoring Cat platform Nakama runtime modules.
//
// Built into a Go plugin via the heroiclabs/nakama-pluginbuilder
// image and mounted at /nakama/data/modules/snoringcat.so.
// Nakama loads the plugin at startup and calls InitModule.
//
// Hooks registered (when EDGEGAP_TOKEN is set):
//   - MatchmakerMatched: allocates an Edgegap deployment for the
//     matched players and notifies them with connection info.
//
// RPCs registered:
//   Server-to-server (HTTP-key gated):
//   - register_server:     game server checks in after boot.
//   - match_end:           game server posts match results.
//   - bulk_import:         Phase E migration RPC.
//   - runtime_status:      read-only probe of build + config.
//   - record_client_ip:    pre-matchmaking IP recorder.
//   Client session:
//   - version_check:       client/server compatibility check.
//   - update_and_get_presence: write own presence + read friends'.
//   - get_player_stats:    rating + match count.
//   - get_match_history:   recent matches for the caller.
//   - export_player_data:  GDPR data export.
package main

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/heroiclabs/nakama-common/runtime"
)

// InitModule is the entry point Nakama calls when loading the
// plugin.
func InitModule(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	initializer runtime.Initializer,
) error {
	env, _ := ctx.Value(runtime.RUNTIME_CTX_ENV).(map[string]string)
	if env == nil {
		env = map[string]string{}
	}

	edgegapToken := env["EDGEGAP_TOKEN"]
	// EDGEGAP_APP_NAME and EDGEGAP_APP_VERSION are required when
	// the matchmaker hook is enabled (i.e. when EDGEGAP_TOKEN is
	// set). When the hook is disabled, both can be empty — the
	// runtime still loads and serves RPCs, just without fleet
	// allocation. This module is platform-shared (multiple games
	// can mount it), so there's no game-specific default.
	appName := env["EDGEGAP_APP_NAME"]
	appVersion := env["EDGEGAP_APP_VERSION"]

	// Register the status probe first so the runtime is
	// diagnosable even if a downstream init step fails or is
	// skipped because of missing config. The status RPC reads
	// `registered` at call time (it captures &registered, not a
	// value), so subsequent registrations below show up
	// automatically.
	matchmakerHookEnabled := edgegapToken != ""
	registered := []string{}
	statusFn := statusRpcFactory(runtimeStatusConfig{
		EdgegapAppName:       appName,
		EdgegapAppVersion:    appVersion,
		EdgegapTokenSet:      edgegapToken != "",
		MatchmakerHookActive: matchmakerHookEnabled,
		RegisteredRpcs:       &registered,
	})
	if err := initializer.RegisterRpc("runtime_status", statusFn); err != nil {
		return err
	}
	registered = append(registered, "runtime_status")
	// Helper that registers an RPC and tracks the name so
	// runtime_status reflects reality.
	addRpc := func(name string, fn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, string,
	) (string, error)) error {
		if err := initializer.RegisterRpc(name, fn); err != nil {
			return err
		}
		registered = append(registered, name)
		return nil
	}

	// Shared Edgegap client used by both the matchmaker hook
	// (to allocate deployments) and matchLifecycle.MatchEndRpc
	// (to terminate them on match end). Stays nil if no token
	// is configured; matchLifecycle no-ops the stop call in
	// that case.
	var edgegap *edgegapClient
	if matchmakerHookEnabled {
		edgegap = &edgegapClient{token: edgegapToken}
	}

	if !matchmakerHookEnabled {
		logger.Warn(
			"EDGEGAP_TOKEN not set; matchmaker_matched hook is" +
				" not registered. Players will pair but never" +
				" receive match_ready notifications. Set the" +
				" env var on the Nakama host and restart the" +
				" container to recover.")
	} else if appName == "" || appVersion == "" {
		// EDGEGAP_TOKEN is set but the app coordinates aren't.
		// Fail loudly rather than silently allocating against
		// a wrong default — this runtime is platform-shared and
		// has no game-specific fallback.
		return runtime.NewError(
			"EDGEGAP_TOKEN set but EDGEGAP_APP_NAME and/or"+
				" EDGEGAP_APP_VERSION missing. Both must be"+
				" set in the Nakama runtime env when the"+
				" matchmaker hook is enabled.", 3)
	} else {
		alloc := &fleetAllocator{
			edgegap:             edgegap,
			appName:             appName,
			appVersion:          appVersion,
			serverDNSBase:       env["SERVER_DNS_BASE"],
			cloudflareDNSToken:  env["CLOUDFLARE_DNS_TOKEN"],
			cloudflareDNSZoneID: env["CLOUDFLARE_DNS_ZONE_ID"],
		}
		if err := initializer.RegisterMatchmakerMatched(
			alloc.OnMatchmakerMatched); err != nil {
			return err
		}
	}

	lifecycle := &matchLifecycle{edgegap: edgegap}
	if err := addRpc("register_server", lifecycle.RegisterServerRpc); err != nil {
		return err
	}
	if err := addRpc("match_end", lifecycle.MatchEndRpc); err != nil {
		return err
	}
	// Phase E migration RPC. Gated behind BULK_IMPORT_ENABLED
	// because the HTTP key is now in client builds, and this
	// RPC is a write-anywhere primitive that would let any
	// attacker forge storage records / leaderboard entries.
	// Set the env var on the Nakama host only while running the
	// migration script; unset + restart afterwards.
	if env["BULK_IMPORT_ENABLED"] == "true" {
		if err := addRpc("bulk_import", bulkImportRpc); err != nil {
			return err
		}
		logger.Warn(
			"bulk_import RPC is REGISTERED — open write access" +
				" to anyone with NAKAMA_HTTP_KEY. Unset" +
				" BULK_IMPORT_ENABLED and restart when the" +
				" migration is done.")
	}
	// Pre-matchmaking client IP recorder. The client calls this
	// right before joining the matchmaker so fleet_allocator can
	// pull each matched user's public IP and feed it to
	// Edgegap's ip_list. See client_ip.go.
	if err := addRpc("record_client_ip", recordClientIPRpc); err != nil {
		return err
	}

	// Client-session RPCs. The client surfaces these in the
	// lobby UI; the AWS-era backend served them as REST endpoints.
	verCfg := versionConfig{
		GameVersion:     env["NAKAMA_GAME_VERSION"],
		ProtocolVersion: parseEnvInt(env, "NAKAMA_PROTOCOL_VERSION", 0),
	}
	if err := addRpc("version_check", versionCheckRpcFactory(verCfg)); err != nil {
		return err
	}
	if err := addRpc("update_and_get_presence", updateAndGetPresenceRpc); err != nil {
		return err
	}
	if err := addRpc("get_player_stats", getPlayerStatsRpc); err != nil {
		return err
	}
	if err := addRpc("get_match_history", getMatchHistoryRpc); err != nil {
		return err
	}
	if err := addRpc("export_player_data", exportPlayerDataRpc); err != nil {
		return err
	}
	if err := addRpc("transport_select", transportSelectRpc); err != nil {
		return err
	}

	logger.Info(
		"snoringcat-platform runtime loaded (build=%s app=%s version=%s edgegap=%t)",
		BuildID, appName, appVersion, matchmakerHookEnabled)
	return nil
}

// parseEnvInt reads an int env var with a fallback default.
// Empty or unparseable values use the default rather than failing
// init.
func parseEnvInt(env map[string]string, key string, def int) int {
	raw := env[key]
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}
