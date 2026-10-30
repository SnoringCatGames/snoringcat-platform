## The single autoload entry point for the Snoring Cat platform SDK.
##
## Game projects initialize this from their own bootstrap with:
##
##     Platform.initialize({
##         "game_id": "hopnbop",
##         "api_base_url": "https://api.snoringcat.games/v1",
##         "sdk_version": "0.1.0",
##     })
##
## Once initialized:
##     Platform.token_store  — encrypted token persistence
##
## Subsystem references (populated incrementally as Stage 6 of the
## multi-game extraction lands each one):
##     Platform.auth                — sign-in, sign-out, token refresh
##     Platform.account             — profile, link/unlink, delete
##     Platform.friends             — list, add by code, accept/decline
##     Platform.party               — create, invite, leave, chat
##     Platform.presence            — set / read presence
##     Platform.notification_socket — long-lived Nakama realtime
##                                    socket: persistent + transient
##                                    notifications, channel messages
##     Platform.settings            — local + cloud sync
##     Platform.matchmaking         — start, poll, cancel
##     Platform.session             — connect / disconnect (delegates
##                                    to game-side session-provider)
##     Platform.screens             — reusable auth / consent screens
##
## Game code assigns its own implementations into these slots (or
## the addon does, once a subsystem is extracted). Today all
## subsystem properties default to null and the consuming game
## populates whichever ones have been extracted via
## Platform.register_subsystem(name, value). Slots that have not
## yet been extracted stay null; consumers fall back to G.* in the
## meantime.
##
## Implementation note: subsystem scripts are resolved via
## runtime `load()` rather than `preload()`. Godot 4.6 has a
## parser-cache bug where preloading sibling addon files at
## autoload-init time pulls stale parsed content. The runtime
## load path reads the current file content correctly.
extends Node


signal initialized

## Log level for nakama-godot's shared logger, which
## Nakama.create_client() sets. Its default there is DEBUG, and at
## DEBUG nakama-godot prints every HTTP request's headers and body
## and the realtime socket's connect URI: session tokens, refresh
## tokens, the server key, and device ids. That output goes to
## stdout and to user://logs/godot.log on players' machines.
## WARNING keeps nakama-godot's socket warnings and errors (server
## error replies, connection errors, unparseable or unhandled
## messages) and logs no request headers or bodies. Don't raise it
## to DEBUG, even in debug builds: those logs end up in bug
## reports and pasted transcripts.
const _NAKAMA_LOG_LEVEL := NakamaLogger.LOG_LEVEL.WARNING

## Path of the runtime's unauthenticated version-check route
## (runtime/version.go). It answers like the version_check RPC but
## needs no session and no key, so a client can call it before
## sign-in. See get_version_check_url().
const VERSION_CHECK_PATH := "/snoringcat/version_check"

## Path of the runtime's public leaderboard route
## (runtime/leaderboard_public.go): a game's board as the game website
## shows it, with no session and no key. See
## get_public_leaderboard_url().
const PUBLIC_LEADERBOARD_PATH := "/snoringcat/leaderboard"

## The port each URL scheme implies, which a URL leaves out.
const _DEFAULT_PORTS := {"https": 443, "http": 80}

## Config key that Platform.initialize refuses. Nakama's runtime HTTP
## key authenticates game servers and operator tools as trusted
## callers of every runtime RPC, so a client that ships it hands that
## power to anyone who unpacks the build. Clients up to SDK 0.5.2
## took it for the pre-sign-in version check, which now uses
## VERSION_CHECK_PATH. Dedicated game servers read the key from their
## environment (NAKAMA_HTTP_KEY) at runtime, never from this config.
const _REJECTED_HTTP_KEY_CONFIG := "nakama_http_key"

var is_initialized := false

var game_id: String = ""
var api_base_url: String = ""
var sdk_version: String = ""

# Nakama connection (Snoring Cat platform infrastructure, not
# game-specific). Populated from Platform.initialize config. The
# server key is public by design: it only lets a client call the
# authenticate endpoints.
var nakama_host: String = ""
var nakama_port: int = 443
var nakama_scheme: String = "https"
var nakama_server_key: String = ""
## Deprecated and always empty: Platform.initialize refuses the
## runtime HTTP key (see _REJECTED_HTTP_KEY_CONFIG). Kept only so
## game code that still reads it compiles until it moves to
## get_version_check_url(). Never assign it.
var nakama_http_key: String = ""

# OAuth surface (game-specific URLs / IDs). Populated from
# Platform.initialize config. The auth subsystem reads these
# instead of reaching back into game-side settings.
var oauth_callback_url: String = ""
var google_token_broker_url: String = ""
var google_oauth_client_id: String = ""
var facebook_oauth_client_id: String = ""

# Core references populated by initialize(). Untyped to dodge the
# same parser-cache bug that prevents class_name imports here.
var token_store

# Shared NakamaClient singleton. Lazily created by
# get_nakama_client() on first access using the nakama_* fields
# above. All addon subsystems (friends, presence, party,
# matchmaking, auth, ...) read this field rather than reaching
# into game code so the dependency direction is addon→Platform,
# not addon→game.
var nakama_client

# Subsystem slots. Assigned by the consuming game during its
# bootstrap, after Platform.initialize() runs, via:
#     Platform.register_subsystem("friends", friends_api_client)
#
# Each slot stays null until the corresponding Stage 6 extraction
# lands and the consuming game wires the implementation in. Code
# that reads a subsystem must tolerate null (typical pattern:
# `if Platform.friends != null: Platform.friends.foo(...)`) until
# the extraction completes.
var auth
var account
var friends
var party
var presence
var notification_socket
var settings
var matchmaking
var session
var screens


func initialize(config: Dictionary) -> void:
	assert(not is_initialized, "Platform.initialize() called twice")
	assert(config.has("game_id"), "Platform.initialize requires game_id")
	assert(
			config.has("api_base_url"),
			"Platform.initialize requires api_base_url")

	game_id = config.game_id
	api_base_url = config.api_base_url
	sdk_version = config.get("sdk_version", "unknown")

	nakama_host = config.get("nakama_host", "")
	nakama_port = int(config.get("nakama_port", 443))
	nakama_scheme = config.get("nakama_scheme", "https")
	nakama_server_key = config.get("nakama_server_key", "")
	if config.has(_REJECTED_HTTP_KEY_CONFIG):
		# Ignored, loudly. Don't add it back: it would have to be in
		# the shipped build to be read here.
		push_error(
			(
				"Platform.initialize: '%s' is a server credential"
				+ " and must not ship in a client. It is ignored."
				+ " Remove it from the config."
			)
			% _REJECTED_HTTP_KEY_CONFIG
		)

	oauth_callback_url = config.get("oauth_callback_url", "")
	google_token_broker_url = config.get(
		"google_token_broker_url", "")
	google_oauth_client_id = config.get(
		"google_oauth_client_id", "")
	facebook_oauth_client_id = config.get(
		"facebook_oauth_client_id", "")

	var auth_file_path: String = config.get(
		"auth_file_path",
		"user://%s_auth.cfg" % game_id,
	)
	var TokenStoreScript: GDScript = load(
		"res://addons/snoringcat_platform_client"
		+ "/core/auth_token_store.gd")
	token_store = TokenStoreScript.new(auth_file_path)

	is_initialized = true
	initialized.emit()


## Constructs a NakamaSession from the persisted JWT + refresh
## token in token_store. Returns null when the store has no valid
## session. Subsystems use this to attach a session to authenticated
## Nakama SDK calls.
func build_session_from_store() -> NakamaSession:
	if token_store == null or token_store.jwt_token.is_empty():
		return null
	return NakamaSession.new(
		token_store.jwt_token,
		false,
		token_store.refresh_token,
		null)


## Returns the public-facing Nakama base URL (e.g.
## "https://nakama.snoringcat.games"). Callers that need to hit
## raw HTTP endpoints (instead of the SDK) use this to build URLs.
## A port other than the scheme's default is kept
## ("http://127.0.0.1:7350" for a local stack); before SDK 0.7.0 it
## was dropped, so such URLs went to port 80.
func get_nakama_base_url() -> String:
	if nakama_port <= 0 or nakama_port == _DEFAULT_PORTS.get(nakama_scheme):
		return "%s://%s" % [nakama_scheme, nakama_host]
	return "%s://%s:%d" % [nakama_scheme, nakama_host, nakama_port]


## Returns the URL of the runtime's unauthenticated version check
## (VERSION_CHECK_PATH). GET it with game_id,
## client_protocol_version and client_game_version query
## parameters, or POST the version_check RPC's JSON body. The reply
## is the RPC's JSON, unwrapped.
func get_version_check_url() -> String:
	return get_nakama_base_url() + VERSION_CHECK_PATH


## Returns the URL of this game's board `leaderboard_id` on the
## runtime's public leaderboard route (PUBLIC_LEADERBOARD_PATH),
## asking for its top `limit` players: the same list the game website
## shows. GET it with no credential. The reply is
## {version, game_id, leaderboard, sort_order, operator,
## next_reset_at, generated_at, entries: [{rank, display_name,
## score}]}; see PLATFORM_ARCHITECTURE.md "Leaderboards".
func get_public_leaderboard_url(leaderboard_id: String, limit: int) -> String:
	return "%s%s?game_id=%s&leaderboard=%s&limit=%d" % [
		get_nakama_base_url(),
		PUBLIC_LEADERBOARD_PATH,
		game_id.uri_encode(),
		leaderboard_id.uri_encode(),
		limit,
	]


## Returns the shared NakamaClient, lazily creating it on first
## access. All addon subsystems use this (or the cached
## Platform.nakama_client field, which the same call populates) so
## the client is a singleton across the SDK.
func get_nakama_client() -> NakamaClient:
	if nakama_client == null:
		assert(
			not nakama_host.is_empty(),
			"Platform.initialize must supply nakama_host")
		nakama_client = Nakama.create_client(
			nakama_server_key,
			nakama_host,
			nakama_port,
			nakama_scheme,
			Nakama.DEFAULT_TIMEOUT,
			_NAKAMA_LOG_LEVEL,
		)
	return nakama_client


## Assign a subsystem implementation. The consuming game calls this
## during bootstrap to wire its own *_api_client / manager objects
## into the Platform.<name> surface, e.g.:
##
##     Platform.register_subsystem("friends", friends_api_client)
##
## Asserts on unknown names so typos surface at first call rather
## than turning into silent nulls. Re-registration is allowed (the
## last call wins) so a test harness can swap an implementation
## without restarting.
func register_subsystem(subsystem_name: String, value) -> void:
	match subsystem_name:
		"auth": auth = value
		"account": account = value
		"friends": friends = value
		"party": party = value
		"presence": presence = value
		"notification_socket": notification_socket = value
		"settings": settings = value
		"matchmaking": matchmaking = value
		"session": session = value
		"screens": screens = value
		_:
			assert(
				false,
				"Platform.register_subsystem: unknown name '%s'"
						% subsystem_name)
