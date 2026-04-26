class_name PlatformScreenTransition
extends ColorRect
## Shader-based screen transition overlay.
##
## Provides tile-based screen transitions with configurable
## patterns. Add as a child of a CanvasLayer to overlay the
## entire screen.
##
## Extracted from hopnbop_private/src/ui/screen_transition.gd
## at the platform-extraction tag.
##
## Differences from the source:
## - Renamed ScreenTransition → PlatformScreenTransition so the
##   in-tree copy can stay until hopnbop migrates.
## - Removed `G.screen_transition = self` self-registration.
## - Removed `Netcode.is_server` early-out — consuming game is
##   responsible for not loading the transition on dedicated
##   server processes.
##
## The shader material is supplied via the .tscn (game-owned).
## Subclass or instantiate this script on a ColorRect that
## already has its ShaderMaterial assigned.


signal transition_started
signal transition_completed
@warning_ignore("unused_signal")
signal transition_midpoint


enum DelayPattern {
	RADIAL = 0,
	DIAGONAL_TOP_LEFT = 1,
	DIAGONAL_TOP_RIGHT = 2,
	HORIZONTAL = 3,
	VERTICAL = 4,
	RANDOM = 5,
}

enum TileStyle {
	SMOOTH_FADE = 0,
	SNAP = 1,
}


const DEFAULT_DURATION := 0.7
const DEFAULT_TILE_SIZE_PX := 10.0
const DEFAULT_PATTERN := DelayPattern.DIAGONAL_TOP_LEFT
const DEFAULT_TILE_STYLE := TileStyle.SNAP
const DEFAULT_DELAY_SPREAD := 0.3
const DEFAULT_PATTERN_RANDOMNESS := 0.3

var _tween: Tween
var _is_transitioning := false


func _enter_tree() -> void:
	# Run during pause so transitions work when game is paused.
	process_mode = Node.PROCESS_MODE_ALWAYS


func _ready() -> void:
	# Ensure visible.
	visible = true

	# Set base color to white (shader handles the color).
	color = Color.WHITE

	# Apply default shader parameters.
	set_tile_size(DEFAULT_TILE_SIZE_PX)
	set_delay_spread(DEFAULT_DELAY_SPREAD)
	set_pattern_randomness(DEFAULT_PATTERN_RANDOMNESS)

	# Initialize to fully transparent (progress = 0).
	_set_progress(0.0)


## Performs a transition out (covers the screen).
func transition_out(
	duration := DEFAULT_DURATION,
	pattern := DEFAULT_PATTERN,
	tile_style := DEFAULT_TILE_STYLE,
) -> void:
	randomize_seed()
	_configure_shader(pattern, tile_style, false)
	await _animate_progress(0.0, 1.0, duration)


## Performs a transition in (reveals the screen).
func transition_in(
	duration := DEFAULT_DURATION,
	pattern := DEFAULT_PATTERN,
	tile_style := DEFAULT_TILE_STYLE,
) -> void:
	randomize_seed()
	_configure_shader(pattern, tile_style, true)
	await _animate_progress(1.0, 0.0, duration)


## Performs a wipe transition: captures current screen, runs
## callback, fades out. More efficient than transition_full as
## it only animates once.
func transition_wipe(
	duration := DEFAULT_DURATION,
	pattern := DEFAULT_PATTERN,
	tile_style := DEFAULT_TILE_STYLE,
	switch_callback: Callable = Callable(),
) -> void:
	# Capture current screen.
	_capture_screen()

	# Switch content immediately.
	if switch_callback.is_valid():
		switch_callback.call()

	# Configure and animate.
	randomize_seed()
	_configure_shader(pattern, tile_style, false)
	_set_use_captured_screen(true)

	# Fade out the captured screen (progress 0→1 means alpha 1→0).
	await _animate_progress(0.0, 1.0, duration)

	# Clean up: reset to transparent state.
	_set_use_captured_screen(false)
	_clear_captured_screen()
	_set_progress(0.0)


## Legacy: Performs a full transition (out then in) with
## callback at midpoint.
func transition_full(
	duration := DEFAULT_DURATION * 2.0,
	pattern := DEFAULT_PATTERN,
	tile_style := DEFAULT_TILE_STYLE,
	midpoint_callback: Callable = Callable(),
) -> void:
	# Use the new wipe transition instead.
	await transition_wipe(
		duration / 2.0, pattern,
		tile_style, midpoint_callback)


## Sets the transition color.
func set_transition_color(col: Color) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("transition_color", col)


## Sets the tile size in pixels (tiles are always square).
func set_tile_size(size_px: float) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("tile_size", size_px)


## Sets the delay spread (0.0 to 1.0).
func set_delay_spread(spread: float) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("delay_spread", spread)


## Sets a random seed for the random pattern.
func set_random_seed(seed_value: float) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("random_seed", seed_value)


## Sets how much random jitter to add to the base pattern
## (0.0 to 1.0).
func set_pattern_randomness(amount: float) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("pattern_randomness", amount)


## Randomizes the seed for the random pattern.
func randomize_seed() -> void:
	set_random_seed(randf() * 1000.0)


## Returns whether a transition is currently in progress.
func is_transitioning() -> bool:
	return _is_transitioning


## Cancels any in-progress transition.
func cancel() -> void:
	if _tween and _tween.is_valid():
		_tween.kill()
	_is_transitioning = false


func _configure_shader(
	pattern: int,
	tile_style: int,
	reverse: bool,
) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("delay_pattern", pattern)
		mat.set_shader_parameter("tile_style", tile_style)
		mat.set_shader_parameter("reverse_direction", reverse)


func _capture_screen() -> void:
	var viewport := get_viewport()
	var img := viewport.get_texture().get_image()
	var tex := ImageTexture.create_from_image(img)
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("captured_screen", tex)


func _set_use_captured_screen(enabled: bool) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter(
			"use_captured_screen", enabled)


func _clear_captured_screen() -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("captured_screen", null)


func _animate_progress(
	from: float,
	to: float,
	duration: float,
) -> void:
	cancel()

	_is_transitioning = true
	transition_started.emit()

	print(
		"PlatformScreenTransition: Animating progress"
		+ " %s -> %s over %ss"
		% [from, to, duration])

	_set_progress(from)

	_tween = create_tween()
	_tween.set_pause_mode(Tween.TWEEN_PAUSE_PROCESS)
	_tween.tween_method(_set_progress, from, to, duration)

	await _tween.finished

	_is_transitioning = false
	transition_completed.emit()


func _set_progress(value: float) -> void:
	var mat := _get_shader_material()
	if mat:
		mat.set_shader_parameter("progress", value)


func _get_shader_material() -> ShaderMaterial:
	return material as ShaderMaterial
