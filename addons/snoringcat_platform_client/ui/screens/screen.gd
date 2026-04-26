class_name PlatformScreen
extends PanelContainer
## Base class for full-screen UI screens (auth, consent, lobby,
## game over, etc.).
##
## Extracted from hopnbop_private/src/ui/screens/screen.gd at
## the platform-extraction tag.
##
## Differences from the source:
## - Renamed Screen → PlatformScreen so the in-tree copy can
##   stay until hopnbop migrates.
## - Removed the `Netcode.is_server` early-out — the consuming
##   game is responsible for not loading screens on dedicated
##   server processes (typically by checking is_server before
##   instantiating the UI tree).
## - Removed the hard-coded `G.settings.default_theme` and
##   `G.settings.screen_style_box` references. The consuming
##   game subclass overrides _set_default_styling() (or sets
##   a Theme/StyleBox via the .tscn) to apply game-specific
##   look-and-feel.


func _enter_tree() -> void:
	process_mode = Node.PROCESS_MODE_ALWAYS
	_set_default_styling()


## Override in subclasses to apply game-specific styling.
## Default implementation just sets the anchors to fill the
## viewport — themes/stylebox come from the .tscn.
func _set_default_styling() -> void:
	set_anchors_preset(Control.PRESET_FULL_RECT)


## Called when the screen becomes active (e.g. after a
## transition completes). Override in subclasses for setup.
func on_open() -> void:
	pass


## Called when the screen is about to be closed (e.g. before a
## transition out). Override in subclasses for teardown.
func on_close() -> void:
	pass
