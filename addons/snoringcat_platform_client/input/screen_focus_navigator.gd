class_name PlatformScreenFocusNavigator
extends RefCounted
## Shared focus navigation for screens that use
## PlatformAnyDeviceInputPoller with a list of focusable Button
## controls. Encapsulates polling, focus movement, and focus
## visual state.
##
## Extracted from
## hopnbop/src/ui/screen_focus_navigator.gd at tag
## pre-platform-extraction.
##
## Differences from the source:
## - Renamed ScreenFocusNavigator → PlatformScreenFocusNavigator.
## - Uses PlatformAnyDeviceInputPoller (the renamed addon class).
## - Audio is now optional. The original called
##   `G.audio.play_sound("focus")` directly. The platform addon
##   does not own audio, so the consumer can pass a `Callable`
##   that receives no args and plays the focus sound (or omit
##   it for silent navigation).


var _poller := PlatformAnyDeviceInputPoller.new()
var _focusable: Array[Control] = []
var _focused_index := 0
## Optional callable invoked when focus moves. Use this to play
## a focus sound (e.g. `Callable(audio, "play_focus_sound")`).
var _on_focus_moved: Callable

## Set when poll() returns true. -1 = left, 1 = right, 0 = trigger.
var last_activation_direction := 0


## Wires an optional sound-on-focus callback. Pass an empty
## `Callable()` (the default) to disable sound.
func set_focus_moved_callback(cb: Callable) -> void:
	_on_focus_moved = cb


## Replaces the focusable list, resets focus to the first item,
## and updates visuals.
func set_focusable_list(
	items: Array[Control],
) -> void:
	_focusable = items
	_focused_index = 0
	_update_focus()


## Primes the input poller to avoid phantom "just pressed"
## detections.
func prime() -> void:
	_poller.prime()


## Returns the currently focused control, or null if the list
## is empty.
func get_focused() -> Control:
	if _focusable.is_empty():
		return null
	return _focusable[_focused_index]


## Polls input and returns true if the activate action was
## triggered (left, right, or trigger). Automatically handles
## up/down focus movement.
func poll(delta: float) -> bool:
	if _focusable.is_empty():
		return false

	_poller.poll(delta)

	if _poller.up_just:
		_move_focus(-1)
	elif _poller.down_just:
		_move_focus(1)
	elif _poller.left_just:
		last_activation_direction = -1
		return true
	elif _poller.right_just:
		last_activation_direction = 1
		return true
	elif _poller.trigger_just:
		last_activation_direction = 0
		return true

	return false


## Sets focus to the item at the given index without playing
## the focus sound.
func focus_index(index: int) -> void:
	if _focusable.is_empty():
		return
	_focused_index = clampi(
		index, 0, _focusable.size() - 1)
	_update_focus()


func _move_focus(direction: int) -> void:
	if _focusable.is_empty():
		return
	_focused_index = (
		(_focused_index + direction)
		% _focusable.size())
	if _focused_index < 0:
		_focused_index += _focusable.size()
	_update_focus()
	if _on_focus_moved.is_valid():
		_on_focus_moved.call()


func _update_focus() -> void:
	for i in _focusable.size():
		if i == _focused_index:
			_focusable[i].grab_focus()
		else:
			_focusable[i].release_focus()
