// Snoring Cat platform Nakama runtime modules.
//
// Built into a Go plugin via the heroiclabs/nakama-pluginbuilder
// image and mounted at /nakama/data/modules/snoringcat.so.
// Nakama loads the plugin at startup and calls InitModule.
//
// Hooks registered (when EDGEGAP_TOKEN is set, OR when
// EDGEGAP_MOCK_DEPLOY=true is set for compliance-test mode):
//   - MatchmakerMatched: allocates an Edgegap deployment for the
//     matched players and notifies them with connection info.
//     In EDGEGAP_MOCK_DEPLOY mode, the Edgegap allocation is
//     synthesized (no real container spin-up); match_ready
//     notifications carry `"mock": true` so the compliance tests
//     can sanity-check that they ran against the mock-enabled
//     runtime. See Stage 8.13 of MULTI_GAME_ROADMAP.md.
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
//                            join its realtime party, so the
//                            leader can submit one matchmaker
//                            ticket for the whole group.
//   - party_abort_matchmaking: leader tells members to stand down
//                            (e.g. someone never joined).
//   - party_invite:        invite a friend on behalf of ANY active
//                            member (server-side add as the leader/
//                            admin, since Nakama gates client adds to
//                            admins). Kick stays leader-only.
//   - party_set_ready:     toggle the caller's per-party ready
//                            flag (fans out party_state_changed).
//   - party_set_mode:      leader picks the matchmaker game mode
//                            for the whole party (Stage 5.7).
//   - party_get_invite_code: fetch / generate a 6-char invite
//                            code for the caller's party.
//   - party_join_by_code:   join a party using a previously-
//                            generated invite code.
//   - party_transfer_leadership: leader hands the party's lead
//                            role to another active member.
//   - delete_account:      soft-delete + cascade (GDPR / CCPA).
//   - get_account_deletion_status: check whether the caller has
//                            an active account_deletion_queue
//                            row (Stage 1.5). Client polls at
//                            auth_completed to prompt cancel.
//   - cancel_account_deletion: restore username + display name
//                            and drop the queue row before the
//                            cron fires (Stage 1.5).
//   - cancel_matchmaking_allocation: matched player aborts an
//                            in-flight Edgegap allocation
//                            mid-poll. Registered alongside the
//                            matchmaker hook; tears down the
//                            in-progress deploy (if any) and
//                            fans out match_failed
//                            reason=cancelled to all matched
//                            users (Stage 7.2).
//   - block_user:          add a user to the caller's BANNED
//                            list (Nakama state=3). Removes any
//                            existing friendship in either
//                            direction; future friend-add calls
//                            in either direction are rejected by
//                            Nakama (Stage 7.4).
//   - unblock_user:        remove a state=3 row from the caller's
//                            friends table (Stage 7.4).
//   - list_blocked_users:  list the caller's BANNED entries with
//                            display name + username (Stage 7.4).
//   - list_recent_players: read the caller's recent-opponents
//                            list. match_end seeds rows for every
//                            (owner, other) pair on a real match
//                            so the UI can offer "add as friend"
//                            for fellow players (Stage 7.6).
//   - get_game_config:     read a game's public per_game_config.
//   - get_friend_code:     fetch / generate the caller's stable
//                            8-char friend code. Account-level (not
//                            per-game); decoupled from the username.
//   - add_friend_by_code:  resolve a friend code to a user and send
//                            a friend request. Resolves server-side
//                            (with a username fallback for old
//                            clients) then FriendsAdd; re-enforces
//                            the BeforeAddFriends rate + pending caps
//                            since a runtime FriendsAdd skips them.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

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
	// Overlay the OS process env on top of Nakama's
	// runtime.env. Nakama parses runtime.env entries as
	// literal "KEY=VALUE" strings — no `${VAR}` expansion at
	// parse time — so an entry like `EDGEGAP_TOKEN=${EDGEGAP_TOKEN}`
	// reaches the runtime as the literal string `${EDGEGAP_TOKEN}`
	// and silently breaks downstream auth. docker-compose's
	// `environment:` block, by contrast, does expand `${VAR}`
	// at compose-up time, so the process env reflects real
	// substituted values. Letting process env win gives one
	// reliable channel for runtime config without rewriting
	// every reader to call os.Getenv individually.
	for _, kv := range os.Environ() {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		env[kv[:i]] = kv[i+1:]
	}

	edgegapToken := env["EDGEGAP_TOKEN"]
	// EDGEGAP_MOCK_DEPLOY=true short-circuits the matchmaker
	// hook's Edgegap allocation path so compliance tests don't
	// burn paid container-hours. Mock mode also enables the
	// matchmaker hook when EDGEGAP_TOKEN is unset (so a fresh
	// test Nakama can run end-to-end matchmaking flows without
	// a real Edgegap account). The runtime logs a loud warning at
	// boot and surfaces the flag via runtime_status so a prod
	// instance that flipped this on by accident is loudly
	// visible. Stage 8.13 of MULTI_GAME_ROADMAP.md.
	mockDeploy := env["EDGEGAP_MOCK_DEPLOY"] == "true" ||
		env["EDGEGAP_MOCK_DEPLOY"] == "1"
	// EDGEGAP_APP_NAME and EDGEGAP_APP_VERSION are required when
	// the matchmaker hook is enabled in real (non-mock) mode.
	// When the hook is disabled, both can be empty — the runtime
	// still loads and serves RPCs, just without fleet
	// allocation. This module is platform-shared (multiple games
	// can mount it), so there's no game-specific default. Mock
	// mode supplies dummy defaults below so a bare test Nakama
	// can boot with just EDGEGAP_MOCK_DEPLOY=true.
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
	matchmakerHookEnabled := edgegapToken != "" || mockDeploy
	registered := []string{}
	statusFn := statusRpcFactory(runtimeStatusConfig{
		EdgegapAppName:       appName,
		EdgegapAppVersion:    appVersion,
		EdgegapTokenSet:      edgegapToken != "",
		EdgegapMockDeploy:    mockDeploy,
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

	// Stage 7.12 + 7.13: BeforeAddFriends hook enforces a cap on
	// pending outgoing friend requests and a per-caller rate
	// limit on add-by-username ("friend code") calls. Always on;
	// no env flag. See runtime/friends_limits.go.
	//
	// The limiter instance is shared with the add_friend_by_code RPC
	// (registered below): both enforce the same per-caller add-by-code
	// rate limit, so they must count against one budget rather than
	// two independent ones.
	sharedFriendsLimiter := newFriendsLimiter()
	if err := registerFriendsLimitHook(
		initializer, sharedFriendsLimiter); err != nil {
		return err
	}

	// Shared Edgegap client used by both the matchmaker hook
	// (to allocate deployments) and matchLifecycle.MatchEndRpc
	// (to terminate them on match end). Stays nil if no token
	// is configured; matchLifecycle no-ops the stop call in
	// that case. Mock mode also leaves it nil — the allocator
	// branches on a.mockDeploy and never reads the client.
	var edgegap *edgegapClient
	if edgegapToken != "" {
		edgegap = &edgegapClient{token: edgegapToken}
	}

	// Optional LocalDockerAllocator for games with
	// allocator_mode "local" or "hybrid". Opt-in via
	// LOCAL_PUBLIC_IP env var (other LOCAL_* envs have
	// sensible defaults). Stays nil when the env isn't
	// set, in which case the matchmaker hook rejects any
	// local-mode game at allocate time (sendAllocationFailed
	// path). This keeps Edgegap-only deployments from needing
	// docker-socket access just to register the runtime.
	var localAllocator *LocalDockerAllocator
	if env["LOCAL_PUBLIC_IP"] != "" {
		la, err := newLocalDockerAllocator(env)
		if err != nil {
			return fmt.Errorf(
				"local allocator config: %w", err)
		}
		localAllocator = la
		logger.Info(
			"local docker allocator enabled (public_ip=%s)",
			env["LOCAL_PUBLIC_IP"])
	}

	if !matchmakerHookEnabled {
		logger.Warn(
			"EDGEGAP_TOKEN not set; matchmaker_matched hook is" +
				" not registered. Players will pair but never" +
				" receive match_ready notifications. Set the" +
				" env var on the Nakama host and restart the" +
				" container to recover. (Set" +
				" EDGEGAP_MOCK_DEPLOY=true to enable the hook" +
				" in compliance-test mode without a real" +
				" Edgegap account.)")
	} else {
		if mockDeploy {
			// Loud, repeated warning so a misconfigured prod
			// instance with mock mode flipped on is impossible
			// to miss in the daily prod-health-check digest.
			logger.Warn(
				"EDGEGAP_MOCK_DEPLOY=true: matchmaker hook will" +
					" synthesize deploy responses (no real" +
					" Edgegap allocations). This MUST be off in" +
					" production.")
			// Mock mode supplies reasonable defaults so a fresh
			// test Nakama can boot with just EDGEGAP_MOCK_DEPLOY
			// set. Real-mode defaults stay strict (fail-fast on
			// missing config).
			if appName == "" {
				appName = "mock-app"
			}
			if appVersion == "" {
				appVersion = "v0"
			}
		} else if appName == "" || appVersion == "" {
			// Real mode: EDGEGAP_TOKEN is set but the app
			// coordinates aren't. Fail loudly rather than
			// silently allocating against a wrong default —
			// this runtime is platform-shared and has no
			// game-specific fallback.
			return runtime.NewError(
				"EDGEGAP_TOKEN set but EDGEGAP_APP_NAME and/or"+
					" EDGEGAP_APP_VERSION missing. Both must be"+
					" set in the Nakama runtime env when the"+
					" matchmaker hook is enabled.", 3)
		}
		// Signaling URL config. Real mode: fail-fast on missing
		// env so prod never ships match_ready notifications with
		// a half-formed signaling URL. Mock mode: hand out
		// placeholder values so a fresh test Nakama can boot
		// without operator pre-config.
		signalingDomain := env["SIGNALING_DOMAIN"]
		signalingHmacSecret := env["SIGNALING_HMAC_SECRET"]
		if mockDeploy {
			if signalingDomain == "" {
				signalingDomain = "mock-signaling.test"
			}
			if signalingHmacSecret == "" {
				signalingHmacSecret = "mock-hmac-secret"
			}
		} else if signalingDomain == "" || signalingHmacSecret == "" {
			return runtime.NewError(
				"EDGEGAP_TOKEN set but SIGNALING_DOMAIN and/or"+
					" SIGNALING_HMAC_SECRET missing. Both must be"+
					" set in the Nakama runtime env when the"+
					" matchmaker hook is enabled.", 3)
		}
		alloc := &fleetAllocator{
			edgegap:             edgegap,
			local:               localAllocator,
			geo:                 newGeoIPClient(env),
			appName:             appName,
			appVersion:          appVersion,
			signalingDomain:     signalingDomain,
			signalingHmacSecret: []byte(signalingHmacSecret),
			games:               games,
			mockDeploy:          mockDeploy,
		}
		if err := initializer.RegisterMatchmakerMatched(
			alloc.OnMatchmakerMatched); err != nil {
			return err
		}
		// Stage 7.2: client-session RPC to abort an in-flight
		// Edgegap allocation. Registered alongside the matchmaker
		// hook because it operates on the same allocator's
		// in-flight tracker — registering it without the hook
		// would always return cancelled=false.
		if err := addRpc(
			"cancel_matchmaking_allocation",
			cancelAllocationRpcFactory(alloc, games)); err != nil {
			return err
		}
	}

	lifecycle := &matchLifecycle{
		edgegap: edgegap,
		local:   localAllocator,
	}
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
	// Friend codes: account-level, decoupled from the username. No
	// game scoping — friends are shared across every game on the
	// platform. add_friend_by_code reuses the shared friends limiter
	// for anti-enumeration.
	if err := addRpc("get_friend_code", getFriendCodeRpc); err != nil {
		return err
	}
	if err := addRpc(
		"add_friend_by_code",
		addFriendByCodeRpcFactory(sharedFriendsLimiter)); err != nil {
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
		"party_abort_matchmaking",
		partyAbortMatchmakingRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"party_invite",
		partyInviteRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"party_set_ready",
		partySetReadyRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"party_set_mode",
		partySetModeRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"party_get_invite_code",
		partyGetInviteCodeRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"party_join_by_code",
		partyJoinByCodeRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"party_transfer_leadership",
		partyTransferLeadershipRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"delete_account",
		deleteAccountRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"get_account_deletion_status",
		getAccountDeletionStatusRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"cancel_account_deletion",
		cancelAccountDeletionRpcFactory(games)); err != nil {
		return err
	}

	// Stage 7.4: friend block list. Uses Nakama's native state=3
	// (BANNED) friend state for storage, so add-friend rejection
	// is bidirectional out of the box.
	if err := addRpc(
		"block_user",
		blockUserRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"unblock_user",
		unblockUserRpcFactory(games)); err != nil {
		return err
	}
	if err := addRpc(
		"list_blocked_users",
		listBlockedUsersRpcFactory(games)); err != nil {
		return err
	}

	// Stage 7.6: recent-players list. match_end seeds rows; this
	// RPC reads them back for the post-match "add as friend" UI.
	if err := addRpc(
		"list_recent_players",
		listRecentPlayersRpcFactory(games)); err != nil {
		return err
	}

	// Per-game config RPCs.
	if err := addRpc("register_game", games.RegisterGameRpc); err != nil {
		return err
	}
	if err := addRpc("get_game_config", games.GetGameConfigRpc); err != nil {
		return err
	}

	// Stage 1.4 hard-delete cron. Scans account_deletion_queue
	// across all users every hour, calls nk.AccountDeleteId for
	// each row whose scheduled_for has elapsed, and drops the
	// queue row. See runtime/account_cron.go.
	startAccountCron(logger, nk)

	logger.Info(
		"snoringcat-platform runtime loaded (build=%s app=%s version=%s edgegap=%t mock_deploy=%t games=%v)",
		BuildID, appName, appVersion, matchmakerHookEnabled,
		mockDeploy, games.GameIDs())
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

