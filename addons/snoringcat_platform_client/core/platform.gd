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
## All subsystems (auth, friends, party, presence, settings, etc.)
## hang off this autoload as child references. Subsystems are NOT
## populated yet. Phase 2 of the platform extraction wires them
## up as files migrate over from hopnbop_private.
extends Node


signal initialized

var is_initialized := false

var game_id: String = ""
var api_base_url: String = ""
var sdk_version: String = ""

# Subsystem references (populated incrementally during Phase 2).
# var auth: AuthClient
# var account: AccountClient
# var friends: FriendsClient
# var party: PartyManager
# var presence: PresenceClient
# var settings: SettingsManager
# var matchmaking: MatchmakingClient
# var screens: ScreenManager


func initialize(config: Dictionary) -> void:
	assert(not is_initialized, "Platform.initialize() called twice")
	assert(config.has("game_id"), "Platform.initialize requires game_id")
	assert(
			config.has("api_base_url"),
			"Platform.initialize requires api_base_url")

	game_id = config.game_id
	api_base_url = config.api_base_url
	sdk_version = config.get("sdk_version", "unknown")

	# TODO Phase 2: instantiate and wire subsystems here.

	is_initialized = true
	initialized.emit()
