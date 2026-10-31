class_name PlatformAuthTokenStore
extends RefCounted
## Persists authentication tokens to encrypted local storage.
##
## Stores JWT, refresh token, and player metadata in an
## encrypted ConfigFile at user://auth.cfg. In preview mode,
## secondary clients use separate files to avoid identity
## collisions during matchmaking. Files are encrypted with a
## per-install random key (SP44); pre-SP44 files used a
## device-derived passphrase and still load (see load_tokens).
##
## Anonymous players use a local-only identity. Their JWT is
## never saved to disk. A short-lived guest JWT is obtained
## on-demand before matchmaking and held only in memory for
## the duration of the session.
##
## The encryption is obfuscation, not secrecy (TS-1, accepted
## 2026-10-30): the install key sits in a file beside the auth
## file, so anyone who can read the player's user folder can
## read both. It keeps tokens out of casual view (a screenshot
## of the folder, a support bundle listing file contents) and
## stops one game's build reading another's by a shared
## constant; it does not protect a copied user folder.
##
## Extracted from hopnbop/src/core/auth_token_store.gd
## at tag pre-platform-extraction. Renamed AuthTokenStore →
## PlatformAuthTokenStore.
##
## Differences from the source:
## - Removed the LEGAL_VERSION constant; the consuming game
##   tracks its own legal version (different games may have
##   different legal documents).
## - Auth file path is configurable via the constructor so
##   games can choose `user://auth.cfg` or a per-game variant
##   like `user://hopnbop_auth.cfg`. The preview-mode
##   per-client filename suffix logic is preserved.

const _SECTION := "auth"
const _REFRESH_MARGIN_SEC := 3600

var _auth_file_path: String

var jwt_token := ""
var refresh_token := ""
var player_id := ""
var display_name := ""
var provider := ""
var is_anonymous := false
var local_player_id := ""
var expires_at := 0
var rating := 1500
var linked_providers: Array[String] = []
var consent_accepted_at := 0
var consent_legal_version := ""
var profile_image_url := ""
## True when an auth file exists but the last load could not
## read it (wrong key, damaged bytes). Callers must not mint
## a fresh identity over such a file: the account it holds
## would be orphaned (SP44).
var load_failed := false


func _init(base_file_path := "user://auth.cfg") -> void:
	_auth_file_path = base_file_path
	# In preview mode, each client uses a separate auth file so
	# they don't overwrite each other.
	var client_num := _get_preview_client_number()
	if client_num > 1:
		var dot := _auth_file_path.rfind(".")
		if dot > 0:
			_auth_file_path = (
				_auth_file_path.substr(0, dot)
				+ "_client%d" % client_num
				+ _auth_file_path.substr(dot)
			)
		else:
			_auth_file_path = (
				_auth_file_path
				+ "_client%d" % client_num
			)
	load_tokens()


static func _get_preview_client_number() -> int:
	if not OS.has_feature("editor"):
		return 0
	for arg in OS.get_cmdline_args():
		if arg.begins_with("--client="):
			return int(arg.substr(9))
	return 0


## Returns true when the player has a usable local identity
## without needing a backend JWT. Anonymous players can
## navigate to the lobby immediately.
func is_anonymous_ready() -> bool:
	return is_anonymous


## Returns true when a JWT exists and has not expired.
func is_token_valid() -> bool:
	if jwt_token.is_empty():
		return false
	var now := int(Time.get_unix_time_from_system())
	return now < expires_at


## Returns true when consent has been given for the specified
## legal version.
func has_valid_consent(
	current_version: String,
) -> bool:
	return (
		consent_accepted_at > 0
		and consent_legal_version
			== current_version
	)


## Returns true when the JWT is close to expiring but a
## refresh token is available. Anonymous users never have a
## refresh token, so this returns false.
func needs_refresh() -> bool:
	if is_anonymous:
		return false
	if refresh_token.is_empty():
		return false
	if jwt_token.is_empty():
		return false
	var now := int(Time.get_unix_time_from_system())
	return now >= (expires_at - _REFRESH_MARGIN_SEC)


## Populate fields from a backend auth response dict.
func store_from_response(data: Dictionary) -> void:
	jwt_token = data.get("jwt_token", "")
	refresh_token = data.get("refresh_token", "")
	player_id = data.get("player_id", "")
	display_name = data.get("display_name", "")
	is_anonymous = data.get("is_anonymous", false)
	expires_at = data.get("expires_at", 0)
	rating = data.get("rating", 1500)
	provider = data.get("provider", "")
	var server_consent: int = data.get(
		"consent_accepted_at", 0
	)
	if server_consent > 0:
		consent_accepted_at = server_consent
		consent_legal_version = data.get(
			"consent_legal_version", ""
		)
	profile_image_url = data.get(
		"profile_image_url", ""
	)
	# Copy the list before clearing ours, in case data holds this very
	# array.
	var lp: Array = data.get("linked_providers", [])
	lp = lp.duplicate()
	linked_providers.clear()
	for p in lp:
		linked_providers.append(str(p))
	save_tokens()


## Delete all stored auth state, consent included, and the auth file.
## For a sign-out the player asked for and for account deletion: whoever
## plays next on this device has to consent again.
func clear_tokens() -> void:
	jwt_token = ""
	refresh_token = ""
	player_id = ""
	display_name = ""
	provider = ""
	is_anonymous = false
	local_player_id = ""
	expires_at = 0
	rating = 1500
	linked_providers.clear()
	consent_accepted_at = 0
	consent_legal_version = ""
	profile_image_url = ""
	DirAccess.remove_absolute(
		ProjectSettings.globalize_path(_auth_file_path)
	)


## Delete the stored session and account but keep the player's consent
## (consent_accepted_at and consent_legal_version), for a sign-out the
## player did not ask for: Nakama rejected the refresh token, or the
## stored one cannot be used. The same player signs back in without
## consenting again, unless the legal version has changed since. The
## auth file is left holding the consent alone, or deleted when there
## is no consent to keep.
func clear_session() -> void:
	var kept_accepted_at := consent_accepted_at
	var kept_legal_version := consent_legal_version
	clear_tokens()
	if kept_accepted_at <= 0:
		return
	consent_accepted_at = kept_accepted_at
	consent_legal_version = kept_legal_version
	save_tokens()


## Whether an auth file exists at all. A file that exists
## but cannot be read must never be replaced by a fresh
## guest identity (SP44): the guest account would orphan.
func has_auth_file() -> bool:
	return FileAccess.file_exists(_auth_file_path)


## Moves an unreadable auth file aside (auth.cfg.unreadable)
## so a fresh guest can start without destroying the old
## file. Returns whether the file was moved.
func move_unreadable_file_aside() -> bool:
	var aside_path := _auth_file_path + ".unreadable"
	var err := DirAccess.rename_absolute(
		ProjectSettings.globalize_path(_auth_file_path),
		ProjectSettings.globalize_path(aside_path))
	if err != OK:
		return false
	load_failed = false
	return true


## Path of the per-install key file next to the auth file.
func _key_file_path() -> String:
	return _auth_file_path.get_basename() + "_key.cfg"


## The pre-SP44 passphrase, kept so old files still load.
func _legacy_passphrase() -> String:
	return OS.get_unique_id().sha256_text()


## The per-install random key, created on first save.
## Returns "" when none exists yet and create is false.
func _install_key(create: bool) -> String:
	var config := ConfigFile.new()
	if config.load(_key_file_path()) == OK:
		var existing := str(
			config.get_value(_SECTION, "install_key", ""))
		if not existing.is_empty():
			return existing
	if not create:
		return ""
	var key := Crypto.new().generate_random_bytes(32).hex_encode()
	config.set_value(_SECTION, "install_key", key)
	# The key lands before any auth file encrypted under it
	# (save_tokens asks for the passphrase first). If it
	# cannot be written, report no key: the caller saves
	# under the previous passphrase rather than encrypting
	# under a key that is not on disk.
	if config.save(_key_file_path()) != OK:
		return ""
	return key


## Persist current auth state to encrypted file.
##
## For anonymous players, JWT fields are intentionally omitted
## so the token is never written to disk. If a previous
## anonymous account had JWT fields (legacy), they are
## overwritten with empty values.
func save_tokens() -> void:
	var config := ConfigFile.new()
	config.set_value(
		_SECTION, "is_anonymous", is_anonymous
	)
	config.set_value(
		_SECTION, "display_name", display_name
	)
	config.set_value(
		_SECTION, "local_player_id", local_player_id
	)
	config.set_value(
		_SECTION, "consent_accepted_at",
		consent_accepted_at,
	)
	config.set_value(
		_SECTION, "consent_legal_version",
		consent_legal_version,
	)
	if not is_anonymous:
		config.set_value(
			_SECTION, "jwt_token", jwt_token
		)
		config.set_value(
			_SECTION, "refresh_token", refresh_token
		)
		config.set_value(
			_SECTION, "player_id", player_id
		)
		config.set_value(
			_SECTION, "provider", provider
		)
		config.set_value(
			_SECTION, "expires_at", expires_at
		)
		config.set_value(_SECTION, "rating", rating)
		config.set_value(
			_SECTION, "linked_providers",
			linked_providers,
		)
		config.set_value(
			_SECTION, "profile_image_url",
			profile_image_url,
		)
	config.save_encrypted_pass(
		_auth_file_path, _save_passphrase()
	)


## Load auth state from encrypted file.
func load_tokens() -> void:
	var config := ConfigFile.new()
	var err := config.load_encrypted_pass(
		_auth_file_path, _get_passphrase()
	)
	if err != OK and _get_passphrase() != _legacy_passphrase():
		# A pre-SP44 file: read it with the old passphrase.
		# The next save re-keys it under the install key.
		err = config.load_encrypted_pass(
			_auth_file_path, _legacy_passphrase()
		)
	if err != OK:
		# A missing file is a fresh install; a file that
		# exists but cannot be read is recorded, so no
		# caller mints a new identity over it.
		load_failed = FileAccess.file_exists(_auth_file_path)
		return
	load_failed = false
	is_anonymous = config.get_value(
		_SECTION, "is_anonymous", false
	)
	display_name = config.get_value(
		_SECTION, "display_name", ""
	)
	local_player_id = config.get_value(
		_SECTION, "local_player_id", ""
	)
	consent_accepted_at = config.get_value(
		_SECTION, "consent_accepted_at", 0
	)
	consent_legal_version = config.get_value(
		_SECTION, "consent_legal_version", ""
	)
	# JWT fields are only persisted for non-anonymous players.
	# Anonymous players' tokens are ephemeral and obtained
	# on-demand before matchmaking.
	if not is_anonymous:
		jwt_token = config.get_value(
			_SECTION, "jwt_token", ""
		)
		refresh_token = config.get_value(
			_SECTION, "refresh_token", ""
		)
		player_id = config.get_value(
			_SECTION, "player_id", ""
		)
		provider = config.get_value(
			_SECTION, "provider", ""
		)
		expires_at = config.get_value(
			_SECTION, "expires_at", 0
		)
		rating = config.get_value(
			_SECTION, "rating", 1500
		)
		linked_providers.clear()
		var lp: Array = config.get_value(
			_SECTION, "linked_providers", []
		)
		for p in lp:
			linked_providers.append(str(p))
		profile_image_url = config.get_value(
			_SECTION, "profile_image_url", ""
		)


func _get_passphrase() -> String:
	var key := _install_key(false)
	return key if not key.is_empty() else _legacy_passphrase()


## Passphrase for the next save: the install key, created
## on first save so new installs are random-keyed and old
## files re-key the first time they are saved again. When
## the key cannot be written, the previous (legacy)
## passphrase is used, so the file stays readable.
func _save_passphrase() -> String:
	var key := _install_key(true)
	return key if not key.is_empty() else _legacy_passphrase()
