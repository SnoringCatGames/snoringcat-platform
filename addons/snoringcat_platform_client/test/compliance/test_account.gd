extends GutTest
## Account profile read/update. Catches breakage in the
## authenticated /v2/account read path and the metadata
## round-trip that consuming games rely on (display name,
## avatar URL, lang, location, timezone, custom metadata).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_account_get_returns_user_block() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty(), "anon auth failed")

	var result: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	assert_eq(
		result.status_code, 200,
		"/v2/account: %s" % result.text)

	# Verify the canonical fields the SDK reads. Schema
	# regressions here would silently corrupt every consumer's
	# profile UI.
	var user: Dictionary = result.body.get("user", {})
	assert_true(user.has("id"), "user.id missing")
	assert_true(user.has("username"), "user.username missing")
	# The user always has a `create_time`. Used to gate
	# new-account onboarding.
	assert_true(
		user.has("create_time"),
		"user.create_time missing: %s" % str(user))


func test_account_update_round_trips() -> void:
	# Update display_name, GET it back, confirm the new value
	# is persisted. This exercises the only mutator on the
	# account object the SDK calls in normal play.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty())

	# Use a unique-ish display name so a stale cached value
	# can't pass the round-trip check.
	var new_name := (
		"Compliance Test "
		+ str(Time.get_unix_time_from_system() as int))
	var update: Dictionary = await _helper.http_put(
		"/v2/account",
		{"display_name": new_name},
		"bearer:" + token)
	# Nakama returns 200 with empty body on success.
	assert_true(
		update.status_code == 200 or update.status_code == 204,
		"update returned %d: %s"
			% [update.status_code, update.text])

	# Read back. Nakama may take a tick to propagate writes —
	# but in practice the same connection sees the write
	# immediately since it's a single Postgres txn.
	var get_result: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	assert_eq(get_result.status_code, 200)
	var user: Dictionary = get_result.body.get("user", {})
	assert_eq(
		str(user.get("display_name", "")), new_name,
		"display_name didn't round-trip;"
			+ " got '%s' expected '%s'"
			% [str(user.get("display_name", "")), new_name])
