class_name PlatformPixelViewportManager
extends Node
## Manages integer-scaled pixel-perfect rendering for a game's
## SubViewport.
##
## Sizes the GameViewportContainer to fill the window at the
## nearest integer pixel scale. Adjusts the active camera's zoom
## so the base viewport area is always visible. Provides
## coordinate mapping from game world space to root viewport
## screen space.
##
## Extracted from hopnbop_private/src/core/pixel_viewport_manager.gd
## at tag pre-platform-extraction. Renamed PixelViewportManager →
## PlatformPixelViewportManager so the original can stay in
## hopnbop_private until the game is migrated to consume this
## copy. Remove the prefix once the game switches over.
##
## Differences from the source:
## - Removed `G.pixel_viewport_manager = self` autoload
##   registration. The consuming game holds a reference if needed.
## - Replaced `G.log.print(...)` with built-in `print(...)`.
## - Replaced `Netcode.is_headless` with
##   `DisplayServer.get_name() == "headless"` so this addon does
##   not depend on the rollback_netcode addon.


const _OVERFLOW_TOLERANCE := 0.05

## The SubViewportContainer holding the game viewport.
var container: SubViewportContainer
## The game SubViewport.
var sub_viewport: SubViewport
var _base_resolution: Vector2i

## Current integer scale factor.
var current_scale: int = 1

## Zoom multiplier applied to the active camera.
var _zoom_scale := 1.0

## Tracks each camera's original zoom before we modify it.
## Dictionary<Camera2D, Vector2>.
var _base_zooms := {}

## Last camera we applied zoom to, for detecting switches.
var _last_camera: Camera2D

## When true, overrides camera base zoom to Vector2(1, 1) for 1:1
## pixel rendering.
var is_thumbnail_snapshot_mode := false


static func _is_headless() -> bool:
	return DisplayServer.get_name() == "headless"


func _enter_tree() -> void:
	process_mode = Node.PROCESS_MODE_ALWAYS


func _ready() -> void:
	if _is_headless():
		return

	_base_resolution = Vector2i(
		ProjectSettings.get_setting(
			"display/window/size/viewport_width",
			1152),
		ProjectSettings.get_setting(
			"display/window/size/viewport_height",
			648),
	)

	container = get_parent().get_node(
		"GameViewportContainer")
	sub_viewport = get_parent().get_node(
		"GameViewportContainer/GameViewport")

	# Ensure stretch/resize propagation works while the tree is
	# paused (e.g. during match-start countdown).
	container.process_mode = (
		Node.PROCESS_MODE_ALWAYS)

	get_tree().root.size_changed.connect(
		_on_window_resized)
	# Apply initial sizing immediately.
	_on_window_resized()


func _process(_delta: float) -> void:
	if _is_headless():
		return
	_update_camera_zoom()


func _on_window_resized() -> void:
	if not is_instance_valid(container):
		return

	var window_size := DisplayServer.window_get_size()

	print(
		"[PVM] Resize: window=%s svp_before=%s"
		% [str(window_size),
			str(sub_viewport.size
				if is_instance_valid(sub_viewport)
				else "null")])

	if is_thumbnail_snapshot_mode:
		# Force exact pixel dimensions regardless of actual window
		# size to avoid sub-pixel stretching from DPI or OS rounding.
		current_scale = 1
		container.stretch_shrink = 1
		container.size = Vector2(_base_resolution)
		container.position = Vector2(
			(window_size.x
				- _base_resolution.x) / 2.0,
			(window_size.y
				- _base_resolution.y) / 2.0,
		)
		if is_instance_valid(sub_viewport):
			sub_viewport.size = _base_resolution
		_zoom_scale = 1.0
		_update_camera_zoom()
		return

	current_scale = _compute_integer_scale(
		window_size, _base_resolution)

	# Fill the window, aligned to integer scale.
	@warning_ignore("integer_division")
	var container_w := (
		(window_size.x / current_scale)
		* current_scale)
	@warning_ignore("integer_division")
	var container_h := (
		(window_size.y / current_scale)
		* current_scale)

	container.stretch_shrink = current_scale
	container.size = Vector2(container_w, container_h)

	# Center the remaining margin (0 to N-1 pixels per side).
	container.position = Vector2(
		(window_size.x - container_w) / 2.0,
		(window_size.y - container_h) / 2.0,
	)

	# Compute the SubViewport resolution and force it immediately.
	# Relying on SubViewportContainer stretch propagation alone can
	# lag by a frame when the tree is paused.
	@warning_ignore("integer_division")
	var svp_w := container_w / current_scale
	@warning_ignore("integer_division")
	var svp_h := container_h / current_scale
	if is_instance_valid(sub_viewport):
		sub_viewport.size = Vector2i(svp_w, svp_h)
	_zoom_scale = minf(
		float(svp_w) / float(_base_resolution.x),
		float(svp_h) / float(_base_resolution.y),
	)

	# Apply immediately to the active camera.
	_update_camera_zoom()


static func _compute_integer_scale(
	window_size: Vector2i,
	base_resolution: Vector2i,
) -> int:
	var max_scale_x := int(
		float(window_size.x)
		* (1.0 + _OVERFLOW_TOLERANCE)
		/ float(base_resolution.x)
	)
	var max_scale_y := int(
		float(window_size.y)
		* (1.0 + _OVERFLOW_TOLERANCE)
		/ float(base_resolution.y)
	)
	var scale := mini(max_scale_x, max_scale_y)
	return maxi(scale, 1)


func _update_camera_zoom() -> void:
	if not is_instance_valid(sub_viewport):
		return

	var camera := sub_viewport.get_camera_2d()
	if not is_instance_valid(camera):
		_last_camera = null
		return

	var is_camera_change := camera != _last_camera

	# Force a resize recalculation when the active camera changes
	# (e.g. level transitions). The window size_changed signal can
	# miss resizes that happen during scene teardown/setup.
	# Set _last_camera before the call to prevent mutual recursion
	# with _on_window_resized.
	if is_camera_change:
		_last_camera = camera
		_on_window_resized()

	# Record original zoom when we first see a camera.
	if not _base_zooms.has(camera):
		_base_zooms[camera] = camera.zoom
		# Camera2D must process during pause to propagate zoom
		# changes to the viewport's canvas_transform.
		camera.process_mode = (
			Node.PROCESS_MODE_ALWAYS)
		# DRAG_CENTER distributes extra viewport space equally on
		# all sides when the viewport aspect ratio differs from the
		# base resolution.
		camera.anchor_mode = (
			Camera2D.ANCHOR_MODE_DRAG_CENTER)

	var base_zoom: Vector2 = _base_zooms[camera]

	# In thumbnail snapshot mode, force base zoom to 1x for 1:1
	# pixel rendering.
	if is_thumbnail_snapshot_mode:
		base_zoom = Vector2.ONE

	camera.zoom = base_zoom * _zoom_scale

	if is_camera_change:
		var svp_size := sub_viewport.size
		print(
			"[PVM] Camera switched: %s"
			% camera.name
			+ " anchor=%d" % camera.anchor_mode
			+ " pos=%s" % str(camera.global_position)
			+ " zoom=%s" % str(camera.zoom)
			+ " base_zoom=%s" % str(base_zoom)
			+ " svp=%s" % str(svp_size)
			+ " parent=%s"
			% (camera.get_parent().name
				if camera.get_parent() else "?"))


## Configures thumbnail snapshot mode. Overrides _base_resolution
## so PVM's normal resize logic produces the correct SubViewport
## size and zoom. The container still fills the window (no
## margins). Once the async window resize to level_pixel_size
## completes, integer_scale = 1 and each game pixel = 1 screen
## pixel.
func configure_thumbnail_snapshot(
	level_pixel_size: Vector2i,
) -> void:
	is_thumbnail_snapshot_mode = true
	_base_resolution = level_pixel_size
	# Re-run resize logic with the new _base_resolution so the
	# SubViewport and zoom update immediately.
	_on_window_resized()


## Builds a Transform2D mapping world coordinates to root viewport
## screen coordinates.
func get_world_to_screen_transform() -> Transform2D:
	if not is_instance_valid(sub_viewport):
		return Transform2D.IDENTITY

	var cx := sub_viewport.get_canvas_transform()
	var sf := float(current_scale)
	var cp := container.position

	# Combine: canvas_transform * scale * offset.
	return Transform2D(
		Vector2(cx.x.x * sf, cx.x.y * sf),
		Vector2(cx.y.x * sf, cx.y.y * sf),
		Vector2(
			cx.origin.x * sf + cp.x,
			cx.origin.y * sf + cp.y),
	)


## Converts a world position to root viewport screen coordinates.
func world_to_screen(world_pos: Vector2) -> Vector2:
	if not is_instance_valid(sub_viewport):
		return world_pos

	var canvas_pos := (
		sub_viewport.get_canvas_transform()
		* world_pos
	)
	return (
		canvas_pos * float(current_scale)
		+ container.position
	)
