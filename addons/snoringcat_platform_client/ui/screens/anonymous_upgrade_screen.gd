class_name PlatformAnonymousUpgradeScreen
extends PlatformScreen
## Reusable "upgrade your anonymous account" screen for
## Snoring Cat games. Surfaces Google / Facebook provider
## buttons that link the anonymous account to a permanent
## identity via `Platform.auth.login_with_provider()` (the
## same call drives the initial sign-in for non-anonymous
## users). Provides a "Maybe Later" dismiss row so the screen
## is non-blocking.
##
## Use this for the "force the player through this gate
## before proceeding" pattern. Hopnbop's `UpgradeAccountPanel`
## (SidePanel-shaped, Stage 7.9) is the non-blocking
## equivalent — pick the right shape per game. The screen
## stays mounted while the auth round-trip is in flight and
## emits `upgrade_completed` once the platform reports a
## successful link.
##
## Stage 6.11b addition. Greenfield — no in-tree consumer
## yet; surface is set up so a future game can render the
## scene + wire the dismiss / completion signals.


@export_group("Branding")
@export var logo_texture: Texture2D

@export_group("Provider Icons")
@export var icon_google: Texture2D
@export var icon_facebook: Texture2D


## Emitted after the platform reports a successful upgrade
## (`Platform.auth.auth_completed` fires with success=true).
## The screen is non-disposing; the listener decides what
## happens next (typically navigate to the lobby or the
## screen that pushed this one).
signal upgrade_completed

## Emitted when the user picks "Maybe Later". Listener
## should pop/close the screen and return to wherever it
## was opened from.
signal dismiss_requested

var _is_upgrading := false
var _navigator: PlatformScreenFocusNavigator


func _enter_tree() -> void:
	super._enter_tree()
	if _navigator == null:
		_navigator = _create_navigator()


func _ready() -> void:
	if logo_texture != null and has_node("%Logo"):
		(%Logo as TextureRect).texture = logo_texture


## Override in a subclass to supply a navigator with a
## game-specific focus-moved audio callback.
func _create_navigator() -> PlatformScreenFocusNavigator:
	return PlatformScreenFocusNavigator.new()


func on_open() -> void:
	super.on_open()

	%StatusLabel.text = ""
	%ErrorLabel.text = ""
	_show_buttons()
	_build_focusable_list()
	_navigator.prime()


func on_close() -> void:
	super.on_close()
	_disconnect_signals()


func _process(_delta: float) -> void:
	if (not visible
			or not %ButtonsContainer.visible):
		return

	if _navigator.poll(_delta):
		# Right + Trigger activate the focused button; Left
		# routes to `on_back()` which dismisses the screen
		# (same as the "Maybe Later" button).
		if _navigator.last_activation_direction == -1:
			on_back()
		else:
			_activate_focused()


## Dismiss the upgrade screen (same as pressing
## "Maybe Later"). Triggered by Left input or by
## close_menu (Escape / B-button).
func on_back() -> void:
	_on_maybe_later_pressed()


func _build_focusable_list() -> void:
	var items: Array[Control] = []
	if %GoogleButton.visible:
		items.append(%GoogleButton)
	if %FacebookButton.visible:
		items.append(%FacebookButton)
	if %MaybeLaterButton.visible:
		items.append(%MaybeLaterButton)
	_navigator.set_focusable_list(items)


func _activate_focused() -> void:
	var focused := _navigator.get_focused()
	if focused == null:
		return
	if focused == %GoogleButton:
		_on_google_pressed()
	elif focused == %FacebookButton:
		_on_facebook_pressed()
	elif focused == %MaybeLaterButton:
		_on_maybe_later_pressed()


func _show_buttons() -> void:
	_is_upgrading = false
	%ButtonsContainer.visible = true
	%LoadingContainer.visible = false

	_apply_button_icon(%GoogleButton, icon_google)
	_apply_button_icon(%FacebookButton, icon_facebook)


func _show_loading(status: String) -> void:
	_is_upgrading = true
	%ButtonsContainer.visible = false
	%LoadingContainer.visible = true
	%StatusLabel.text = status
	%ErrorLabel.text = ""


func _show_error(message: String) -> void:
	_show_buttons()
	%ErrorLabel.text = message
	_build_focusable_list()
	_navigator.prime()


# --- Button handlers ---


func _on_google_pressed() -> void:
	_start_upgrade(
		PlatformAuthApiClient.Provider.GOOGLE)


func _on_facebook_pressed() -> void:
	_start_upgrade(
		PlatformAuthApiClient.Provider.FACEBOOK)


func _on_maybe_later_pressed() -> void:
	if _is_upgrading:
		return
	dismiss_requested.emit()


func _start_upgrade(
	provider: PlatformAuthApiClient.Provider,
) -> void:
	if _is_upgrading:
		return

	_show_loading(tr("AUTH.SIGNING_IN"))

	Platform.auth.auth_completed.connect(
		_on_upgrade_completed,
		CONNECT_ONE_SHOT,
	)
	Platform.auth.auth_status_changed.connect(
		_on_status_changed,
	)
	Platform.auth.login_with_provider(provider)


func _on_upgrade_completed(
	success: bool,
	error: String,
) -> void:
	_disconnect_status_signal()
	if success:
		upgrade_completed.emit()
	else:
		_show_error(error)


func _on_status_changed(status: String) -> void:
	%StatusLabel.text = status


func _disconnect_signals() -> void:
	_disconnect_status_signal()
	if Platform.auth.auth_completed.is_connected(
		_on_upgrade_completed,
	):
		Platform.auth.auth_completed.disconnect(
			_on_upgrade_completed,
		)


func _disconnect_status_signal() -> void:
	if Platform.auth.auth_status_changed.is_connected(
		_on_status_changed,
	):
		Platform.auth.auth_status_changed.disconnect(
			_on_status_changed,
		)


func _apply_button_icon(
	button: Button,
	tex: Texture2D,
) -> void:
	if tex == null:
		return
	button.icon = tex
	button.expand_icon = true
