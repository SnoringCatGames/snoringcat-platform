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
##     Platform.api          — generic HTTP client
##     Platform.token_store  — encrypted token persistence
##
## Subsystem references (populated incrementally as Stage 6 of the
## multi-game extraction lands each one):
##     Platform.auth          — sign-in, sign-out, token refresh
##     Platform.account       — profile, link/unlink, delete
##     Platform.friends       — list, add by code, accept/decline
##     Platform.party         — create, invite, leave, chat
##     Platform.presence      — set / read presence
##     Platform.settings      — local + cloud sync (global / per-game)
##     Platform.matchmaking   — start, poll, cancel
##     Platform.session       — connect / disconnect (delegates to
##                              game-side session-provider)
##     Platform.screens       — reusable auth / consent screens
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

var is_initialized := false

var game_id: String = ""
var api_base_url: String = ""
var sdk_version: String = ""

# Nakama connection (Snoring Cat platform infrastructure, not
# game-specific). Populated from Platform.initialize config.
var nakama_host: String = ""
var nakama_port: int = 443
var nakama_scheme: String = "https"
var nakama_server_key: String = ""
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
var api

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
	nakama_http_key = config.get("nakama_http_key", "")

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
	var ApiClientScript: GDScript = load(
		"res://addons/snoringcat_platform_client"
		+ "/core/api_client.gd")

	token_store = TokenStoreScript.new(auth_file_path)
	api = ApiClientScript.new()
	api.name = "PlatformApiClient"
	api.base_url = api_base_url
	api.token_store = token_store
	add_child(api)

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
func get_nakama_base_url() -> String:
	return "%s://%s" % [nakama_scheme, nakama_host]


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
			nakama_scheme)
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
		"settings": settings = value
		"matchmaking": matchmaking = value
		"session": session = value
		"screens": screens = value
		_:
			assert(
				false,
				"Platform.register_subsystem: unknown name '%s'"
						% subsystem_name)
