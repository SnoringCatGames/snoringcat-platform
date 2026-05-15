class_name PlatformConsentScreen
extends PlatformScreen
## Reusable age-gate / legal-consent screen for Snoring Cat
## games. Requires the player to confirm they are 13+ and
## accept Terms of Service + Privacy Policy before proceeding
## past auth.
##
## Games subclass `PlatformConsentScreen` and listen to its
## navigation signals to wire the post-consent flow, Language
## / Terms / Privacy screen openings, and game-specific audio.
## Branding (icons, checkbox textures, styleboxes, logo) is
## supplied via the @export slots on the inherited scene.
##
## Extracted from
## hopnbop_private/src/ui/screens/consent_screen.gd at the
## Stage 6.11b platform-extraction pass.
##
## Differences from the source:
## - Renamed ConsentScreen → PlatformConsentScreen.
## - Removed direct writes to G.consent_screen and direct
##   G.screens navigation. Emits `consent_accepted` /
##   `language_picker_requested` / `terms_link_requested` /
##   `privacy_link_requested` signals instead so the addon
##   does not reference the game's ScreensMain.ScreenType
##   enum or G.language_screen.set_return_screen.
## - The current legal version is fetched via the
##   `_get_current_legal_version()` virtual so the addon does
##   not reach into game-side `LegalVersion.get_current()`.
## - The Netcode-preview-secondary-client auto-consent gate
##   is delegated to the `_should_auto_consent()` virtual.
## - Icon scaling reads `_get_icon_scale()` and
##   `_get_icon_padding()` virtuals instead of
##   G.settings.icon_scale / G.settings.icon_padding.
## - Focus / select audio call `_play_focus_sound()` /
##   `_play_select_sound()` virtuals (no-op default).
## - Chevron icon is an @export slot; the original read
##   G.settings.chevron_icon directly.


@export_group("Branding")
@export var logo_texture: Texture2D

@export_group("Row Icons")
@export var icon_language: Texture2D
@export var icon_terms: Texture2D
@export var icon_privacy: Texture2D
@export var chevron_icon: Texture2D

@export_group("Checked Textures")
@export var tex_normal_checked: Texture2D
@export var tex_hovered_checked: Texture2D
@export var tex_pressed_checked: Texture2D

@export_group("Unchecked Textures")
@export var tex_normal_unchecked: Texture2D
@export var tex_hovered_unchecked: Texture2D
@export var tex_pressed_unchecked: Texture2D

@export_group("Styling")
@export var focus_style: StyleBox
@export var unfocused_style: StyleBox


## Emitted after the user accepts age + terms and the consent
## timestamp/version are persisted to the platform token store.
## Game-side listeners route to whatever screen comes next
## (typically auth or — if the game is configured to skip
## auth — lobby).
signal consent_accepted

## Emitted when the user activates the Language row. Listeners
## should open the game's language picker.
signal language_picker_requested

## Emitted when the user activates the Terms link. Listeners
## should open the game's terms-of-service screen.
signal terms_link_requested

## Emitted when the user activates the Privacy link. Listeners
## should open the game's privacy-policy screen.
signal privacy_link_requested

var _age_checked := false
var _terms_checked := false

var _poller: PlatformAnyDeviceInputPoller
var _focusable: Array[Control] = []
var _focused_index := 0

var _language_arrow: TextureRect
var _terms_arrow: TextureRect
var _privacy_arrow: TextureRect


func _enter_tree() -> void:
	super._enter_tree()
	if _poller == null:
		_poller = _create_input_poller()


func _ready() -> void:
	if logo_texture != null and has_node("%Logo"):
		(%Logo as TextureRect).texture = logo_texture

	%AgeCheckBox.pressed.connect(
		_on_age_pressed)
	%TermsCheckBox.pressed.connect(
		_on_terms_pressed)
	%ContinueButton.pressed.connect(
		_on_continue_pressed)

	%AgeCheckBox.mouse_entered.connect(
		_update_checkbox_textures)
	%AgeCheckBox.mouse_exited.connect(
		_update_checkbox_textures)
	%TermsCheckBox.mouse_entered.connect(
		_update_checkbox_textures)
	%TermsCheckBox.mouse_exited.connect(
		_update_checkbox_textures)

	# Scale checkboxes 2x.
	if tex_normal_checked != null:
		var cb_size := (
			tex_normal_checked.get_size() * 2)
		%AgeCheckBox.custom_minimum_size = cb_size
		%TermsCheckBox.custom_minimum_size = cb_size
	%AgeCheckBox.stretch_mode = (
		TextureButton.STRETCH_KEEP_ASPECT_CENTERED)
	%TermsCheckBox.stretch_mode = (
		TextureButton.STRETCH_KEEP_ASPECT_CENTERED)

	# Set row icons.
	_setup_row_icon(
		%LanguageRow.get_node(
			"HBoxContainer/Icon"),
		icon_language)
	_setup_row_icon(
		%TermsLinkRow.get_node(
			"HBoxContainer/Icon"),
		icon_terms)
	_setup_row_icon(
		%PrivacyLinkRow.get_node(
			"HBoxContainer/Icon"),
		icon_privacy)

	# Store arrow refs before _setup_chevron wraps them in a
	# MarginContainer via padding.
	_language_arrow = %LanguageRow.get_node(
		"HBoxContainer/Arrow")
	_terms_arrow = %TermsLinkRow.get_node(
		"HBoxContainer/Arrow")
	_privacy_arrow = %PrivacyLinkRow.get_node(
		"HBoxContainer/Arrow")

	# Set up chevron icons on arrow rows.
	_setup_chevron(_language_arrow)
	_setup_chevron(_terms_arrow)
	_setup_chevron(_privacy_arrow)

	# Connect mouse interactions for focusable PanelContainer
	# rows.
	_connect_row_mouse(%LanguageRow, 0)
	_connect_row_mouse(%TermsLinkRow, 1)
	_connect_row_mouse(%PrivacyLinkRow, 2)
	_connect_row_mouse(%AgeRow, 3)
	_connect_row_mouse(%TermsRow, 4)


## Override in a subclass to supply an input poller with a
## game-specific binding set. Default poller uses the
## platform default keyboard partitions.
func _create_input_poller() -> PlatformAnyDeviceInputPoller:
	return PlatformAnyDeviceInputPoller.new()


## Override in a subclass to surface the auto-consent path
## (typically preview-mode secondary clients). Default
## returns false so the addon does not reach into
## rollback-netcode.
func _should_auto_consent() -> bool:
	return false


## Override in a subclass to supply the current legal-version
## string. The consent screen passes the value to
## Platform.token_store.has_valid_consent() and persists it
## on accept. Default is "1.0".
func _get_current_legal_version() -> String:
	return "1.0"


## Override in a subclass to provide a game-specific icon
## scale (uniform multiplier applied to row-icon
## custom_minimum_size). Default returns 1.0.
func _get_icon_scale() -> float:
	return 1.0


## Override in a subclass to provide a game-specific icon
## padding (added as MarginContainer wrapper around icons).
## Default returns 0 (no wrapper).
func _get_icon_padding() -> int:
	return 0


## Override in a subclass to play a focus-moved sound.
## Default is no-op.
func _play_focus_sound() -> void:
	pass


## Override in a subclass to play a selection / toggle sound.
## Default is no-op.
func _play_select_sound() -> void:
	pass


func on_open() -> void:
	super.on_open()

	if _should_auto_consent():
		_auto_consent_and_skip()
		return

	# Already consented for current legal version.
	if Platform.token_store.has_valid_consent(
		_get_current_legal_version(),
	):
		consent_accepted.emit()
		return

	# Show consent UI.
	_age_checked = false
	_terms_checked = false
	%ContinueButton.disabled = true
	_update_checkbox_textures()
	_update_rtl_arrows()

	_build_focusable_list()
	_poller.prime()


func _process(_delta: float) -> void:
	if not visible:
		return
	if _focusable.is_empty():
		return

	_poller.poll(_delta)

	if _poller.up_just:
		_move_focus(-1)
	elif _poller.down_just:
		_move_focus(1)
	elif (_poller.left_just
			or _poller.right_just
			or _poller.trigger_just):
		_activate_focused()


func _build_focusable_list() -> void:
	_focusable.clear()
	_focusable.append(%LanguageRow)
	_focusable.append(%TermsLinkRow)
	_focusable.append(%PrivacyLinkRow)
	_focusable.append(%AgeRow)
	_focusable.append(%TermsRow)
	_focusable.append(%ContinueButton)
	_focused_index = 0
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
	_play_focus_sound()


func _update_focus() -> void:
	for i in _focusable.size():
		var ctrl: Control = _focusable[i]
		if ctrl is Button:
			if i == _focused_index:
				ctrl.grab_focus()
			else:
				ctrl.release_focus()
		elif ctrl is PanelContainer:
			if i == _focused_index:
				ctrl.add_theme_stylebox_override(
					"panel", focus_style)
			else:
				ctrl.add_theme_stylebox_override(
					"panel", unfocused_style)


func _activate_focused() -> void:
	if _focusable.is_empty():
		return
	var focused: Control = (
		_focusable[_focused_index])
	if focused == %LanguageRow:
		language_picker_requested.emit()
	elif focused == %TermsLinkRow:
		terms_link_requested.emit()
	elif focused == %PrivacyLinkRow:
		privacy_link_requested.emit()
	elif focused == %AgeRow:
		_on_age_pressed()
	elif focused == %TermsRow:
		_on_terms_pressed()
	elif focused == %ContinueButton:
		if not %ContinueButton.disabled:
			_on_continue_pressed()


func _connect_row_mouse(
	row: PanelContainer,
	focus_index: int,
) -> void:
	row.gui_input.connect(
		func(event: InputEvent) -> void:
			if event is InputEventMouseButton:
				var mb: InputEventMouseButton = (
					event)
				if (mb.pressed
						and mb.button_index
						== MOUSE_BUTTON_LEFT):
					_focused_index = focus_index
					_update_focus()
					_activate_focused())
	row.mouse_entered.connect(
		func() -> void:
			_focused_index = focus_index
			_update_focus())


func _setup_row_icon(
	icon_rect: TextureRect,
	tex: Texture2D,
) -> void:
	if tex != null:
		icon_rect.texture = tex
		icon_rect.custom_minimum_size = (
			tex.get_size()
			* _get_icon_scale())
		_wrap_icon_with_padding(icon_rect)
		icon_rect.show()
	else:
		icon_rect.hide()


func _setup_chevron(rect: TextureRect) -> void:
	if chevron_icon == null:
		return
	rect.texture = chevron_icon
	rect.expand_mode = (
		TextureRect.EXPAND_IGNORE_SIZE)
	rect.stretch_mode = (
		TextureRect.STRETCH_KEEP_ASPECT_CENTERED)
	var icon_size := (
		chevron_icon.get_size()
		* _get_icon_scale())
	rect.custom_minimum_size = icon_size
	_wrap_icon_with_padding(rect)
	rect.scale.x = 1.0
	if is_layout_rtl():
		rect.pivot_offset = icon_size / 2.0
		rect.scale.x = -1.0


func _wrap_icon_with_padding(
	icon_rect: TextureRect,
) -> void:
	var pad := _get_icon_padding()
	if pad <= 0:
		return
	var parent := icon_rect.get_parent()
	var mc: MarginContainer
	if parent is MarginContainer:
		mc = parent as MarginContainer
	else:
		mc = MarginContainer.new()
		mc.mouse_filter = (
			Control.MOUSE_FILTER_IGNORE)
		parent.add_child(mc)
		parent.move_child(
			mc, icon_rect.get_index())
		icon_rect.reparent(mc)
	mc.add_theme_constant_override(
		"margin_left", pad)
	mc.add_theme_constant_override(
		"margin_right", pad)
	mc.add_theme_constant_override(
		"margin_top", pad)
	mc.add_theme_constant_override(
		"margin_bottom", pad)


func _update_rtl_arrows() -> void:
	_setup_chevron(_language_arrow)
	_setup_chevron(_terms_arrow)
	_setup_chevron(_privacy_arrow)


func _on_age_pressed() -> void:
	_age_checked = not _age_checked
	_update_state()


func _on_terms_pressed() -> void:
	_terms_checked = not _terms_checked
	_update_state()


func _update_state() -> void:
	_update_checkbox_textures()
	%ContinueButton.disabled = not (
		_age_checked and _terms_checked)
	_play_select_sound()


func _update_checkbox_textures() -> void:
	_apply_texture(
		%AgeCheckBox, _age_checked)
	_apply_texture(
		%TermsCheckBox, _terms_checked)


func _apply_texture(
	button: TextureButton,
	is_checked: bool,
) -> void:
	var is_hovered := (
		button.get_global_rect().has_point(
			button.get_global_mouse_position()))
	if is_checked:
		button.texture_normal = (
			tex_hovered_checked
			if is_hovered
			else tex_normal_checked)
	else:
		button.texture_normal = (
			tex_hovered_unchecked
			if is_hovered
			else tex_normal_unchecked)


func _on_continue_pressed() -> void:
	Platform.token_store.consent_accepted_at = (
		int(Time.get_unix_time_from_system()))
	Platform.token_store.consent_legal_version = (
		_get_current_legal_version())
	Platform.token_store.save_tokens()
	consent_accepted.emit()


func _auto_consent_and_skip() -> void:
	Platform.token_store.consent_accepted_at = (
		int(Time.get_unix_time_from_system()))
	Platform.token_store.consent_legal_version = (
		_get_current_legal_version())
	Platform.token_store.save_tokens()
	consent_accepted.emit()
