extends GutTest
## export_player_data RPC: GDPR data export. Returns the
## caller's account, storage objects, leaderboard records, and
## friends list. The legal/data_deletion website page promises
## users this works on demand.
##
## Contract (from runtime/player_data.go):
##   request:  empty
##   response: {generated_at, account, storage_objects,
##              leaderboard_records, friends}
##   - account is null if AccountGetId failed (logged as warn,
##     not an error to the client).
##   - All array fields are always present (never absent), even
##     when empty.
## Auth: Bearer session token.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_export_returns_full_envelope_for_authed_user() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty())

	var result: Dictionary = await _helper.session_rpc(
		"export_player_data", token, null)
	assert_eq(
		result.status_code, 200,
		"export_player_data: %s" % result.text)
	assert_true(
		result.inner is Dictionary,
		"inner not a dict: %s" % result.text)

	var export: Dictionary = result.inner

	# generated_at: required envelope field. The legal page
	# promises a timestamp users can reference.
	assert_true(
		export.has("generated_at"),
		"missing generated_at: %s" % str(export.keys()))
	assert_false(
		str(export.get("generated_at", "")).is_empty(),
		"generated_at is empty")

	# account: present (null is acceptable per runtime; absent
	# is not — the field must be in the envelope so consumer
	# code can null-check rather than KeyError).
	assert_true(
		export.has("account"),
		"missing account key: %s" % str(export.keys()))

	# All three arrays are required and non-null. Runtime
	# explicitly initializes them as `[]` even on errors so the
	# consumer never sees null.
	for field in ["storage_objects", "leaderboard_records",
				   "friends"]:
		var name: String = field
		assert_true(
			export.has(name),
			"missing %s in envelope" % name)
		assert_true(
			export.get(name) is Array,
			"%s not an array: %s" % [name, str(export.get(name))])


func test_export_account_block_has_user_id_when_present() -> void:
	# The compliance account always has an account block (we
	# just authed). Verify the user_id is the canonical id from
	# /v2/account, not some other field.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)

	# Pull the canonical user_id.
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	var canonical_id: String = str(
		account.body.get("user", {}).get("id", ""))
	assert_false(
		canonical_id.is_empty(), "no canonical user_id")

	var result: Dictionary = await _helper.session_rpc(
		"export_player_data", token, null)
	assert_eq(result.status_code, 200)

	var account_block: Variant = result.inner.get("account")
	# account_block CAN legitimately be null on transient
	# read errors (the runtime logs and continues). If it's
	# present, validate it.
	if account_block != null:
		assert_true(
			account_block is Dictionary,
			"account block not a dict: %s" % str(account_block))
		var account_dict: Dictionary = account_block
		assert_eq(
			str(account_dict.get("user_id", "")), canonical_id,
			"export.account.user_id mismatch with /v2/account")
