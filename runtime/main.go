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
// Hooks registered (always):
//   - BeforeAuthenticate{Device,Google,Facebook,Apple,Steam}:
//     enforces that every authenticate call carries a known
//     game_id in its `vars` map. The vars get baked into the
//     session token and are exposed to subsequent RPCs via
//     RUNTIME_CTX_VARS.
//   - BeforeSessionRefresh: same rule on token refresh.
//   - AfterAddGroupUsers / AfterJoinGroup / AfterLeaveGroup /
//     AfterKickGroupUsers: fan out a party_state_changed
//     notification on every membership change to a `party-`
//     group. Drives the client's real-time party UI.
//
// RPCs registered:
//   Server-to-server (HTTP-key gated):
//   - register_server:     game server checks in after boot.
//   - match_end:           game server posts match results.
//   - bulk_import:         Phase E migration RPC.
//   - runtime_status:      read-only probe of build + config.
//   - record_client_ip:    pre-matchmaking IP recorder.
//   - register_game:       upsert a game's per_game_config row.
//   Client session:
//   - version_check:       client/server compatibility check.
//   - update_and_get_presence: write own presence + read friends'.
//   - get_player_stats:    rating + match count.
//   - get_match_history:   recent matches for the caller.
//   - export_player_data:  GDPR data export.
//   - transport_select:    pick ENet vs WebRTC vs WebSocket.
//   - party_start_matchmaking: leader notifies party members to
//                            enqueue with a shared party_id.
//   - party_set_ready:     toggle the caller's per-party ready
//                            flag (fans out party_state_changed).
//   - delete_account:      soft-delete + cascade (GDPR / CCPA).
//   - get_game_config:     read a game's public per_game_config.
package main

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/heroiclabs/nakama-common/api"
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

	// Per-game config store. Created first so both the auth hooks
	// and every stateful RPC can validate game_id against it. The
	// DDL is idempotent (CREATE TABLE IF NOT EXISTS) and the cache
	// warm is fast on an empty table, so this is cheap to run at
	// every plugin reload. Fatal on failure — every game-scoped
	// RPC below assumes the store is readable.
	games, err := newPerGameConfig(ctx, db)
	if err != nil {
		return err
	}

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
		Games:                &games,
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

	// Register the BeforeAuthenticate* hooks. Each enforces that
	// the inbound request carries a known game_id in its vars
	// map. The vars propagate into the session token; the same
	// game_id is then visible to every RPC via RUNTIME_CTX_VARS.
	//
	// Bootstrap exemption: while the `games` table is empty
	// (e.g. immediately after first deploy, before
	// sync-game-config.ps1 has run), all auths pass through.
	// See validateGameIDInVars for the rule.
	if err := registerAuthHooks(initializer, games); err != nil {
		return err
	}

	// AfterAddGroupUsers / AfterJoinGroup / AfterLeaveGroup /
	// AfterKickGroupUsers hooks fan out party_state_changed
	// notifications when a `party-` group's membership changes.
	// Clients subscribed via a Nakama realtime socket refresh
	// their local party state on receipt, replacing the legacy
	// polling cadence in party_manager.gd.
	if err := registerPartyGroupHooks(initializer); err != nil {
		return err
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
	} else if env["SIGNALING_DOMAIN"] == "" ||
		env["SIGNALING_HMAC_SECRET"] == "" {
		// Fail fast rather than allocate deploys whose
		// match_ready notifications would carry an empty
		// signaling_url. Both vars must be wired on the
		// Nakama runtime env (config.yml's runtime.env
		// block) before the matchmaker hook is useful.
		return runtime.NewError(
			"EDGEGAP_TOKEN set but SIGNALING_DOMAIN and/or"+
				" SIGNALING_HMAC_SECRET missing. Both must be"+
				" set in the Nakama runtime env when the"+
				" matchmaker hook is enabled.", 3)
	} else {
		alloc := &fleetAllocator{
			edgegap:             edgegap,
			appName:             appName,
			appVersion:          appVersion,
			signalingDomain:     env["SIGNALING_DOMAIN"],
			signalingHmacSecret: []byte(env["SIGNALING_HMAC_SECRET"]),
			games:               games,
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
	if err := addRpc("match_cancel", lifecycle.MatchCancelRpc); err != nil {
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
	if err := addRpc(
		"version_check",
		versionCheckRpcFactory(verCfg, games)); err != nil {
		return err
	}
	// Every client-session RPC below reads game_id from the
	// session vars (set by the BeforeAuthenticate* hooks above)
	// and rejects when missing once `games` is populated. The
	// game_id value isn't fully wired into reads/writes yet —
	// Stage 3 of MULTI_GAME_ROADMAP.md applies the per-game
	// scoping to presence storage, leaderboards, party groups,
	// and Edgegap allocation.
	if err := addRpc(
		"update_and_get_presence",
		updateAndGetPresenceRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"get_player_stats",
		getPlayerStatsRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"get_match_history",
		getMatchHistoryRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"export_player_data",
		exportPlayerDataRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc("transport_select", transportSelectRpc); err != nil {
		return err
	}
	if err := addRpc(
		"party_start_matchmaking",
		partyStartMatchmakingRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"party_set_ready",
		partySetReadyRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"delete_account",
		deleteAccountRpcFactory(games)); err != nil {
		return err
	}

	// Per-game config RPCs.
	if err := addRpc("register_game", games.RegisterGameRpc); err != nil {
		return err
	}
	if err := addRpc("get_game_config", games.GetGameConfigRpc); err != nil {
		return err
	}

	logger.Info(
		"snoringcat-platform runtime loaded (build=%s app=%s version=%s edgegap=%t games=%v)",
		BuildID, appName, appVersion, matchmakerHookEnabled,
		games.GameIDs())
	return nil
}

// registerAuthHooks wires the BeforeAuthenticate* and
// BeforeSessionRefresh hooks that enforce the game_id-in-vars
// invariant. Each hook is a thin wrapper around
// validateGameIDInVars that pulls the vars off the provider-
// specific request shape.
func registerAuthHooks(
	initializer runtime.Initializer,
	games *perGameConfig,
) error {
	if err := initializer.RegisterBeforeAuthenticateDevice(
		func(
			_ context.Context, _ runtime.Logger, _ *sql.DB,
			_ runtime.NakamaModule,
			in *api.AuthenticateDeviceRequest,
		) (*api.AuthenticateDeviceRequest, error) {
			var vars map[string]string
			if in.GetAccount() != nil {
				vars = in.GetAccount().GetVars()
			}
			if err := validateGameIDInVars(vars, games); err != nil {
				return nil, err
			}
			return in, nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeAuthenticateGoogle(
		func(
			_ context.Context, _ runtime.Logger, _ *sql.DB,
			_ runtime.NakamaModule,
			in *api.AuthenticateGoogleRequest,
		) (*api.AuthenticateGoogleRequest, error) {
			var vars map[string]string
			if in.GetAccount() != nil {
				vars = in.GetAccount().GetVars()
			}
			if err := validateGameIDInVars(vars, games); err != nil {
				return nil, err
			}
			return in, nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeAuthenticateFacebook(
		func(
			_ context.Context, _ runtime.Logger, _ *sql.DB,
			_ runtime.NakamaModule,
			in *api.AuthenticateFacebookRequest,
		) (*api.AuthenticateFacebookRequest, error) {
			var vars map[string]string
			if in.GetAccount() != nil {
				vars = in.GetAccount().GetVars()
			}
			if err := validateGameIDInVars(vars, games); err != nil {
				return nil, err
			}
			return in, nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeAuthenticateApple(
		func(
			_ context.Context, _ runtime.Logger, _ *sql.DB,
			_ runtime.NakamaModule,
			in *api.AuthenticateAppleRequest,
		) (*api.AuthenticateAppleRequest, error) {
			var vars map[string]string
			if in.GetAccount() != nil {
				vars = in.GetAccount().GetVars()
			}
			if err := validateGameIDInVars(vars, games); err != nil {
				return nil, err
			}
			return in, nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeAuthenticateSteam(
		func(
			_ context.Context, _ runtime.Logger, _ *sql.DB,
			_ runtime.NakamaModule,
			in *api.AuthenticateSteamRequest,
		) (*api.AuthenticateSteamRequest, error) {
			var vars map[string]string
			if in.GetAccount() != nil {
				vars = in.GetAccount().GetVars()
			}
			if err := validateGameIDInVars(vars, games); err != nil {
				return nil, err
			}
			return in, nil
		}); err != nil {
		return err
	}
	if err := initializer.RegisterBeforeSessionRefresh(
		func(
			_ context.Context, _ runtime.Logger, _ *sql.DB,
			_ runtime.NakamaModule,
			in *api.SessionRefreshRequest,
		) (*api.SessionRefreshRequest, error) {
			if err := validateGameIDInVars(
				in.GetVars(), games); err != nil {
				return nil, err
			}
			return in, nil
		}); err != nil {
		return err
	}
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
