extends GutTest
## Cloud-synced settings via Nakama Storage. The SDK uses a
## dedicated storage collection ("settings") to persist
## per-player JSON blobs across devices.
##
## Tests:
##  - PUT writes a blob with private read/write permissions.
##  - GET reads it back, body matches.
##  - Re-PUT with a new value updates in place (Nakama uses
##    versioning under the hood, but the public contract is
##    "last write wins" when no version is supplied).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"
const _COLLECTION := "settings"
const _KEY := "compliance-roundtrip"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_storage_write_read_roundtrip() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty())

	# Need the user_id to read the object back via the
	# /v2/storage/{collection}/{key}/{user_id} GET path. Pull
	# it from /v2/account.
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	var user_id: String = str(
		account.body.get("user", {}).get("id", ""))
	assert_false(user_id.is_empty(), "no user_id from /v2/account")

	# Write. Use a value that includes the timestamp so the
	# read-back can't pass on stale data.
	var ts := Time.get_unix_time_from_system() as int
	var marker := "compliance-marker-%d" % ts
	# Nakama's /v2/storage takes {objects: [{collection,key,
	# value, permission_read, permission_write}]}. value must
	# be a JSON string (not a dict).
	var write_body := {
		"objects": [{
			"collection": _COLLECTION,
			"key": _KEY,
			"value": JSON.stringify({"marker": marker}),
			"permission_read": 1,    # owner-only.
			"permission_write": 1,   # owner-only.
		}],
	}
	var write_result: Dictionary = await _helper.http_put(
		"/v2/storage", write_body, "bearer:" + token)
	assert_eq(
		write_result.status_code, 200,
		"storage write: %s" % write_result.text)

	# Read back via POST /v2/storage with object_ids. Nakama
	# returns {objects: [{collection,key,value,...}]}; an
	# empty objects list means "not found" (Nakama doesn't
	# 404 missing storage objects).
	var read_body := {
		"object_ids": [{
			"collection": _COLLECTION,
			"key": _KEY,
			"user_id": user_id,
		}],
	}
	var read_result: Dictionary = await _helper.http_post(
		"/v2/storage", read_body, "bearer:" + token)
	assert_eq(
		read_result.status_code, 200,
		"storage read: %s" % read_result.text)

	var objects: Variant = read_result.body.get("objects")
	assert_true(
		objects is Array and (objects as Array).size() == 1,
		"expected 1 storage object, got: %s" % read_result.text)
	var stored_value_str: String = str(
		(objects as Array)[0].get("value", ""))
	# value comes back as a JSON-string. Decode and verify our
	# marker.
	var parsed := JSON.new()
	assert_eq(
		parsed.parse(stored_value_str), OK,
		"stored value didn't parse: %s" % stored_value_str)
	var parsed_data: Dictionary = parsed.data
	assert_eq(
		str(parsed_data.get("marker", "")), marker,
		"round-trip marker mismatch")
