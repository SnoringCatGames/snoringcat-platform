class_name PlatformToastOverlay
extends PanelContainer
## Displays temporary toast notifications. Toasts fade out
## automatically after a short delay.
##
## Extracted from hopnbop_private/src/ui/toast/toast_overlay.gd
## at the platform-extraction tag. Differences:
## - Renamed ToastOverlay → PlatformToastOverlay so the
##   in-tree copy can stay until hopnbop wires the addon.
## - Removed the `G.toast_overlay = self` self-registration —
##   the consuming game holds the reference itself.
## - Replaced `G.log.print(...)` with built-in `print(...)`.
## - Replaced `G.settings.icon_scale` with an exported
##   `icon_scale` property (default 1.0) so games configure it
##   in the .tscn instead of through a Settings autoload.


const _FADE_DELAY_SEC := 2.0
const _FADE_DURATION_SEC := 0.5
const _MAX_TOASTS := 5

enum Type {
	INFO,
	SUCCESS,
	ERROR,
}

@export var _toast_style: StyleBoxFlat
@export var _error_icon: Texture2D
## Multiplier applied to the error icon's natural size.
@export var icon_scale: float = 1.0

const _COLORS := {
	Type.INFO: Color.WHITE,
	Type.SUCCESS: Color(0.6, 1.0, 0.6),
	Type.ERROR: Color(1.0, 0.4, 0.4),
}


## Show a toast message with the given type.
func show_toast(
	message: String,
	type: Type = Type.INFO,
) -> void:
	var type_name: String = Type.keys()[type]
	print("[Toast:%s] %s" % [type_name, message])

	var panel := PanelContainer.new()
	panel.mouse_filter = Control.MOUSE_FILTER_IGNORE
	panel.size_flags_horizontal = (
		Control.SIZE_SHRINK_CENTER
	)

	panel.add_theme_stylebox_override(
		"panel", _toast_style,
	)

	var color: Color = _COLORS.get(
		type, Color.WHITE)

	if type == Type.ERROR and _error_icon != null:
		var hbox := HBoxContainer.new()
		hbox.mouse_filter = (
			Control.MOUSE_FILTER_IGNORE)
		hbox.alignment = (
			BoxContainer.ALIGNMENT_CENTER)
		hbox.add_theme_constant_override(
			"separation", 6)
		var icon := TextureRect.new()
		icon.texture = _error_icon
		icon.custom_minimum_size = (
			_error_icon.get_size() * icon_scale)
		icon.expand_mode = (
			TextureRect.EXPAND_IGNORE_SIZE)
		icon.stretch_mode = (
			TextureRect
				.STRETCH_KEEP_ASPECT_CENTERED)
		icon.mouse_filter = (
			Control.MOUSE_FILTER_IGNORE)
		icon.modulate = color
		hbox.add_child(icon)
		var label := Label.new()
		label.text = message
		label.modulate = color
		hbox.add_child(label)
		panel.add_child(hbox)
	else:
		var label := Label.new()
		label.text = message
		label.modulate = color
		label.horizontal_alignment = (
			HORIZONTAL_ALIGNMENT_CENTER)
		panel.add_child(label)

	%ToastContainer.add_child(panel)

	# Cap the number of visible toasts.
	while %ToastContainer.get_child_count() > _MAX_TOASTS:
		var oldest: Node = %ToastContainer.get_child(0)
		oldest.queue_free()

	# Fade out and remove after delay. Use a node-bound tween
	# so it inherits this node's PROCESS_MODE_ALWAYS and keeps
	# running even when the scene tree is paused.
	var tween := create_tween()
	tween.tween_property(
		panel, "modulate:a",
		0.0, _FADE_DURATION_SEC,
	).set_delay(_FADE_DELAY_SEC)
	tween.tween_callback(
		func() -> void:
			if is_instance_valid(panel):
				panel.queue_free()
	)
