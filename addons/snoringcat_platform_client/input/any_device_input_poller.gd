class_name PlatformAnyDeviceInputPoller
extends RefCounted
## Polls ALL keyboard partitions and ALL connected gamepads for
## directional and trigger input. Used by screens where no
## specific device config is assigned yet (auth, consent).
##
## Extracted from
## hopnbop_private/src/ui/any_device_input_poller.gd at tag
## pre-platform-extraction. Renamed AnyDeviceInputPoller →
## PlatformAnyDeviceInputPoller. Reads keyboard partition
## bindings from PlatformInputDeviceManager (the renamed addon
## class) instead of InputDeviceManager.
##
## Action names (`move_up`, `move_down`, etc.) are still expected
## to be registered in the consuming game's InputMap. The keyboard
## partition fallbacks polled directly via physical key codes do
## not require InputMap entries.


const _INPUT_INITIAL_DELAY := 0.3
const _INPUT_REPEAT_RATE := 0.1

var up_just := false
var down_just := false
var left_just := false
var right_just := false
var trigger_just := false

var _prev_up := false
var _prev_down := false
var _prev_left := false
var _prev_right := false
var _prev_trigger := false

# Input repeat for up/down only.
var _held_direction := ""
var _hold_timer := 0.0


## Prime previous-input state so inputs already held when the
## poller starts are not detected as "just pressed" on the first
## frame.
func prime() -> void:
	_prev_up = _is_up_pressed()
	_prev_down = _is_down_pressed()
	_prev_left = _is_left_pressed()
	_prev_right = _is_right_pressed()
	_prev_trigger = _is_trigger_pressed()
	_held_direction = ""
	_hold_timer = 0.0


func poll(delta: float) -> void:
	var up := _is_up_pressed()
	var down := _is_down_pressed()
	var left := _is_left_pressed()
	var right := _is_right_pressed()
	var trigger := _is_trigger_pressed()

	up_just = up and not _prev_up
	down_just = down and not _prev_down
	left_just = left and not _prev_left
	right_just = right and not _prev_right
	trigger_just = trigger and not _prev_trigger

	_prev_up = up
	_prev_down = down
	_prev_left = left
	_prev_right = right
	_prev_trigger = trigger

	# Input repeat for up/down only.
	var current_dir := ""
	if up:
		current_dir = "up"
	elif down:
		current_dir = "down"

	if (current_dir != ""
			and current_dir == _held_direction):
		_hold_timer += delta
		if _hold_timer >= _INPUT_INITIAL_DELAY:
			var time_past_delay := (
				_hold_timer - _INPUT_INITIAL_DELAY)
			var repeat_count := int(
				time_past_delay
				/ _INPUT_REPEAT_RATE)
			var prev_time := (
				_hold_timer - delta
				- _INPUT_INITIAL_DELAY)
			var prev_count := int(
				max(0, prev_time)
				/ _INPUT_REPEAT_RATE)
			if repeat_count > prev_count:
				if current_dir == "up":
					up_just = true
				elif current_dir == "down":
					down_just = true
	else:
		_held_direction = current_dir
		_hold_timer = 0.0


func _is_up_pressed() -> bool:
	for bindings in (
			PlatformInputDeviceManager
				.KEYBOARD_PARTITION_BINDINGS):
		if Input.is_physical_key_pressed(
				bindings["move_up"]):
			return true
	return Input.is_action_pressed(&"move_up")


func _is_down_pressed() -> bool:
	for bindings in (
			PlatformInputDeviceManager
				.KEYBOARD_PARTITION_BINDINGS):
		if Input.is_physical_key_pressed(
				bindings["move_down"]):
			return true
	return Input.is_action_pressed(&"move_down")


func _is_left_pressed() -> bool:
	for bindings in (
			PlatformInputDeviceManager
				.KEYBOARD_PARTITION_BINDINGS):
		if Input.is_physical_key_pressed(
				bindings["move_left"]):
			return true
	return Input.is_action_pressed(&"move_left")


func _is_right_pressed() -> bool:
	for bindings in (
			PlatformInputDeviceManager
				.KEYBOARD_PARTITION_BINDINGS):
		if Input.is_physical_key_pressed(
				bindings["move_right"]):
			return true
	return Input.is_action_pressed(&"move_right")


func _is_trigger_pressed() -> bool:
	if (Input.is_physical_key_pressed(KEY_ENTER)
			or Input.is_physical_key_pressed(
				KEY_SPACE)):
		return true
	if Input.is_action_pressed(&"trigger_ui"):
		return true
	return false
