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
## Once initialized, subsystem accessors are populated:
##
##     Platform.api          — generic HTTP client (PlatformApiClient).
##     Platform.token_store  — encrypted token persistence.
##
## Higher-level helpers (auth flows, friends, party, presence,
## settings, matchmaking, screens) are added in subsequent
## extraction passes.
extends Node


signal initialized

var is_initialized := false

var game_id: String = ""
var api_base_url: String = ""
var sdk_version: String = ""

# Subsystem references. Null until initialize() runs.
# Untyped to avoid Godot 4.6's class_name lookup ordering
# issue when the autoload script is parsed before its
# sibling addon files have registered their class_names.
var token_store
var api

# Future subsystems (populated incrementally during Phase 2):
# var auth: PlatformAuthClient
# var account: PlatformAccountClient
# var friends: PlatformFriendsClient
# var party: PlatformPartyManager
# var presence: PlatformPresenceClient
# var settings: PlatformSettingsManager
# var matchmaking: PlatformMatchmakingClient
# var screens: PlatformScreenManager


func initialize(config: Dictionary) -> void:
	assert(not is_initialized, "Platform.initialize() called twice")
	assert(config.has("game_id"), "Platform.initialize requires game_id")
	assert(
			config.has("api_base_url"),
			"Platform.initialize requires api_base_url")

	game_id = config.game_id
	api_base_url = config.api_base_url
	sdk_version = config.get("sdk_version", "unknown")

	# Encrypted JWT/refresh-token persistence. Stays in memory
	# until the consuming game wants to call save_tokens().
	# Per-game auth file path keeps multiple games' tokens
	# isolated (so a single device can be signed into Hop 'n Bop
	# AND another future game without overwriting either side).
	var auth_file_path: String = config.get(
		"auth_file_path",
		"user://%s_auth.cfg" % game_id,
	)
	# preload() resolves at script-load time and sidesteps the
	# class_name registration ordering issue with sibling addon
	# files. The class_name decls in those files are still
	# correct for external consumers.
	var TokenStoreScript := preload(
		"res://addons/snoringcat_platform_client"
		+ "/core/auth_token_store.gd")
	var ApiClientScript := preload(
		"res://addons/snoringcat_platform_client"
		+ "/core/api_client.gd")

	token_store = TokenStoreScript.new(auth_file_path)

	# Generic HTTP client. add_child() runs _ready, which sets
	# up the inner HTTPRequest node.
	api = ApiClientScript.new()
	api.name = "PlatformApiClient"
	api.base_url = api_base_url
	api.token_store = token_store
	add_child(api)

	is_initialized = true
	initialized.emit()
