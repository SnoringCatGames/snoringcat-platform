class_name PlatformAuthScreen
extends PlatformScreen
## Reusable authentication screen for Snoring Cat games.
##
## Provides Google / Facebook / anonymous sign-in buttons with
## keyboard / controller navigation. Games subclass to plug in
## game-specific navigation and game-side cache clears via the
## signals below. Branding is supplied via the @export texture
## slots set on the inherited scene.
##
## Extracted from
## hopnbop_private/src/ui/screens/auth_screen.gd at the
## Stage 6.11b platform-extraction pass.
##
## Differences from the source:
## - Renamed AuthScreen → PlatformAuthScreen.
## - Removed direct writes to game-side autoload globals
##   (G.auth_screen, G.profile_image_cache,
##   G.friends_notification_poller, G.party_manager,
##   G.client_session). The screen now emits the
##   force_anonymous_state_reset_requested signal before any
##   force-anonymous path; the game-side wrapper listens and
##   clears its own caches.
## - Replaced direct G.screens navigation with the
##   lobby_navigation_requested signal so the addon does not
##   reference the game's ScreensMain.ScreenType enum.
## - Provider icons (Google / Facebook / anonymous) are
##   @export slots on the screen instead of reading
##   G.settings.anonymous_texture.
## - The focus navigator is constructed via the
##   _create_navigator() virtual so subclasses can inject a
##   navigator with an audio callback wired.
## - The Netcode-preview-secondary-client force-anonymous
##   gate is delegated to the _should_force_anonymous()
##   virtual so the addon doesn't reference the rollback-
##   netcode autoload. Default returns false.


@export_group("Branding")
@export var logo_texture: Texture2D

@export_group("Provider Icons")
@export var icon_google: Texture2D
@export var icon_facebook: Texture2D
@export var icon_anonymous: Texture2D

## Emitted when the screen is ready to leave for the lobby
## (auth completed, valid cached token, or anonymous-ready).
## Game-side listeners should call their screen-navigation
## helper (e.g. G.screens.client_open_screen(LOBBY)).
signal lobby_navigation_requested

## Emitted synchronously before forcing anonymous login (e.g.
## preview mode secondary clients, auth refresh failure).
## Listeners should clear any game-side caches that persist
## across logins so the new anonymous identity starts clean.
## The addon clears its own Platform.* caches after the emit
## returns.
signal force_anonymous_state_reset_requested

var _is_authenticating := false
var _navigator: PlatformScreenFocusNavigator


func _enter_tree() -> void:
	super._enter_tree()
	if _navigator == null:
		_navigator = _create_navigator()


func _ready() -> void:
	if logo_texture != null and has_node("%Logo"):
		(%Logo as TextureRect).texture = logo_texture


## Override in a subclass to supply a navigator with a
## game-specific focus-moved audio callback. Default
## navigator has no audio.
func _create_navigator() -> PlatformScreenFocusNavigator:
	return PlatformScreenFocusNavigator.new()


func on_open() -> void:
	super.on_open()

	%StatusLabel.text = ""
	%ErrorLabel.text = ""
	_show_buttons()

	# In preview mode, force secondary clients to use anonymous
	# login so each gets a unique player identity for matchmaking.
	# Clear any cached tokens first so they start fresh.
	if _should_force_anonymous():
		force_anonymous_state_reset_requested.emit()
		Platform.token_store.clear_tokens()
		Platform.friends.cached_friends.clear()
		Platform.friends.cached_sent_requests.clear()
		Platform.friends.cached_incoming_requests.clear()
		Platform.presence.cached_online_ids.clear()
		_start_login(
			PlatformAuthApiClient.Provider.ANONYMOUS)
		return

	# Anonymous players have a local-only identity.
	# No network call is needed to enter the lobby.
	if Platform.token_store.is_anonymous_ready():
		_emit_lobby_navigation()
		return

	# Check cached token.
	if Platform.token_store.is_token_valid():
		_emit_lobby_navigation()
		return

	# Try auto-refresh.
	if Platform.token_store.needs_refresh():
		_show_loading(tr("AUTH.RESUMING_SESSION"))
		Platform.auth.auth_completed.connect(
			_on_auto_refresh_completed,
			CONNECT_ONE_SHOT,
		)
		Platform.auth.refresh_token()
		return

	# On platforms with implied auth, auto-login.
	var platform_provider := (
		PlatformAuthApiClient.get_platform_provider()
	)
	if platform_provider >= 0:
		_start_login(
			platform_provider
			as PlatformAuthApiClient.Provider)
		return

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
		# routes to `on_back()` (no-op by default for screens
		# without a natural back action — auth is the entry
		# point, so Left does nothing here).
		if _navigator.last_activation_direction == -1:
			on_back()
		else:
			_activate_focused()


func _build_focusable_list() -> void:
	var items: Array[Control] = []
	if %GoogleButton.visible:
		items.append(%GoogleButton)
	if %FacebookButton.visible:
		items.append(%FacebookButton)
	if %AnonButton.visible:
		items.append(%AnonButton)
	_navigator.set_focusable_list(items)


func _activate_focused() -> void:
	var focused := _navigator.get_focused()
	if focused == null:
		return
	if focused == %GoogleButton:
		_on_google_pressed()
	elif focused == %FacebookButton:
		_on_facebook_pressed()
	elif focused == %AnonButton:
		_on_anonymous_pressed()


func _show_buttons() -> void:
	_is_authenticating = false
	%ButtonsContainer.visible = true
	%LoadingContainer.visible = false

	# Set button icons.
	_apply_button_icon(%GoogleButton, icon_google)
	_apply_button_icon(%FacebookButton, icon_facebook)
	_apply_button_icon(%AnonButton, icon_anonymous)

	# Hide buttons not relevant to this platform.
	var has_platform := (
		PlatformAuthApiClient.get_platform_provider() >= 0
	)
	%OAuthRow.visible = not has_platform
	%AnonButton.visible = not has_platform


func _show_loading(status: String) -> void:
	_is_authenticating = true
	%ButtonsContainer.visible = false
	%LoadingContainer.visible = true
	%StatusLabel.text = status
	%ErrorLabel.text = ""


func _show_error(message: String) -> void:
	_show_buttons()
	%ErrorLabel.text = message
	_build_focusable_list()
	_navigator.prime()


func _emit_lobby_navigation() -> void:
	lobby_navigation_requested.emit()


# --- Button handlers ---


func _on_google_pressed() -> void:
	_start_login(PlatformAuthApiClient.Provider.GOOGLE)


func _on_facebook_pressed() -> void:
	_start_login(
		PlatformAuthApiClient.Provider.FACEBOOK)


func _on_anonymous_pressed() -> void:
	_start_login(
		PlatformAuthApiClient.Provider.ANONYMOUS)


func _start_login(
	provider: PlatformAuthApiClient.Provider,
) -> void:
	if _is_authenticating:
		return

	_show_loading(tr("AUTH.SIGNING_IN"))

	Platform.auth.auth_completed.connect(
		_on_login_completed,
		CONNECT_ONE_SHOT,
	)
	Platform.auth.auth_status_changed.connect(
		_on_status_changed,
	)
	Platform.auth.login_with_provider(provider)


func _on_login_completed(
	success: bool,
	error: String,
) -> void:
	_disconnect_status_signal()
	if success:
		_emit_lobby_navigation()
	else:
		_show_error(error)


func _on_auto_refresh_completed(
	success: bool,
	_error: String,
) -> void:
	if success:
		_emit_lobby_navigation()
	else:
		# Refresh failed. Show login buttons.
		force_anonymous_state_reset_requested.emit()
		Platform.token_store.clear_tokens()
		Platform.friends.cached_friends.clear()
		Platform.friends.cached_sent_requests.clear()
		Platform.friends.cached_incoming_requests.clear()
		Platform.presence.cached_online_ids.clear()
		_show_buttons()
		_build_focusable_list()
		_navigator.prime()


func _on_status_changed(status: String) -> void:
	%StatusLabel.text = status


func _disconnect_signals() -> void:
	_disconnect_status_signal()
	if Platform.auth.auth_completed.is_connected(
		_on_login_completed,
	):
		Platform.auth.auth_completed.disconnect(
			_on_login_completed,
		)
	if Platform.auth.auth_completed.is_connected(
		_on_auto_refresh_completed,
	):
		Platform.auth.auth_completed.disconnect(
			_on_auto_refresh_completed,
		)


## Override in a subclass to surface a "force anonymous" path
## (e.g. preview mode secondary clients). Default is false so
## the addon does not reach into the rollback-netcode
## autoload.
func _should_force_anonymous() -> bool:
	return false


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
