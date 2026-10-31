extends GutTest
## Client storage writes are allowlisted (SP59, keys and
## count SP87): only the settings collection, only under
## its keys, one value within the size cap, and a bounded
## number of objects per write. Everything else is
## refused, so a dropped hook registration fails here.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper
var _user: Dictionary = {}


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	if not _user.is_empty():
		await _helper.delete_one_shot_account(_user)
	_user = {}


func test_client_storage_writes_are_allowlisted() -> void:
	if not await _mint_user():
		return
	var token := str(_user.token)

	# The one writable shape: settings under an allowed
	# key, small value.
	var ok: Dictionary = await _write(token, [{
		"collection": "settings",
		"key": "global",
		"value": "{\"volume\": 3}",
		"permission_read": 1,
		"permission_write": 1,
	}])
	assert_true(
		_is_2xx(ok),
		"settings/global write: status=%d body=%s"
			% [ok.status_code, ok.text])

	# Any other collection is refused.
	var presence: Dictionary = await _write(token, [{
		"collection": "presence",
		"key": "current",
		"value": "{}",
		"permission_read": 1,
		"permission_write": 1,
	}])
	assert_false(
		_is_2xx(presence),
		"presence write should be refused: status=%d"
			% presence.status_code)

	# A settings key outside the allowed set is refused.
	var junk: Dictionary = await _write(token, [{
		"collection": "settings",
		"key": "junk-00000000",
		"value": "{}",
		"permission_read": 1,
		"permission_write": 1,
	}])
	assert_false(
		_is_2xx(junk),
		"junk key write should be refused: status=%d"
			% junk.status_code)

	# recent_players is server-owned, not client-writable.
	var recent: Dictionary = await _write(token, [{
		"collection": "recent_players",
		"key": str(_user.user_id),
		"value": "{}",
		"permission_read": 1,
		"permission_write": 1,
	}])
	assert_false(
		_is_2xx(recent),
		"recent_players write should be refused: status=%d"
			% recent.status_code)

	# A value over the size cap is refused.
	var big: Dictionary = await _write(token, [{
		"collection": "settings",
		"key": "global",
		"value": "x".repeat(32769),
		"permission_read": 1,
		"permission_write": 1,
	}])
	assert_false(
		_is_2xx(big),
		"oversized value should be refused: status=%d"
			% big.status_code)

	# More objects than the per-write cap is refused.
	var objects: Array = []
	for i in 17:
		objects.append({
			"collection": "settings",
			"key": "global",
			"value": "{}",
			"permission_read": 1,
			"permission_write": 1,
		})
	var many: Dictionary = await _write(token, objects)
	assert_false(
		_is_2xx(many),
		"17-object write should be refused: status=%d"
			% many.status_code)


func _mint_user() -> bool:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return false
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return false
	var users: Array = await _helper.multi_session_anon(
		1, "compliance-storage-allowlist")
	if users.size() != 1:
		pending("multi_session_anon did not return the user")
		return false
	_user = users[0]
	return true


func _write(token: String, objects: Array) -> Dictionary:
	return await _helper.http_put(
		"/v2/storage", {"objects": objects},
		"bearer:" + token)


func _is_2xx(result: Dictionary) -> bool:
	return (
		result.status_code >= 200 and result.status_code < 300)
