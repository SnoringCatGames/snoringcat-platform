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

# Subsystem references. Untyped to dodge the same parser-cache bug.
var token_store
var api


func initialize(config: Dictionary) -> void:
	assert(not is_initialized, "Platform.initialize() called twice")
	assert(config.has("game_id"), "Platform.initialize requires game_id")
	assert(
			config.has("api_base_url"),
			"Platform.initialize requires api_base_url")

	game_id = config.game_id
	api_base_url = config.api_base_url
	sdk_version = config.get("sdk_version", "unknown")

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
