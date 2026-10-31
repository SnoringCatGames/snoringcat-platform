extends GutTest
## Party membership and metadata changes go through the
## party RPCs, which enforce the party rules. These tests
## hold the live stack to that shape: an invite or a
## join-by-code stops at a block, and the raw group calls
## on a party group are refused while other groups keep
## working.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_invite_refuses_a_blocked_target() -> void:
	if not await _mint_users(2):
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var party_id := await _make_party(a, "party-")
	if party_id.is_empty():
		return
	var blocked: Dictionary = await _helper.session_rpc(
		"block_user", str(b.token), {"user_id": str(a.user_id)})
	assert_eq(
		blocked.status_code, 200,
		"block_user: status=%d body=%s"
			% [blocked.status_code, blocked.text])

	var invite: Dictionary = await _helper.session_rpc(
		"party_invite", str(a.token),
		{"party_id": party_id, "target_id": str(b.user_id)})
	assert_ne(
		invite.status_code, 200,
		"invite across a block should be refused: %s"
			% invite.text)


func test_join_by_code_refuses_a_blocked_member() -> void:
	if not await _mint_users(2):
		return
	var a: Dictionary = _users[0]
	var d: Dictionary = _users[1]
	var party_id := await _make_party(a, "party-")
	if party_id.is_empty():
		return
	var code_resp: Dictionary = await _helper.session_rpc(
		"party_get_invite_code", str(a.token),
		{"party_id": party_id})
	assert_eq(
		code_resp.status_code, 200,
		"party_get_invite_code: status=%d body=%s"
			% [code_resp.status_code, code_resp.text])
	if not (code_resp.inner is Dictionary):
		fail_test("invite code body not a dict: %s" % code_resp.text)
		return
	var code := str(code_resp.inner.get("code", ""))
	assert_false(code.is_empty(), "invite code missing")

	var blocked: Dictionary = await _helper.session_rpc(
		"block_user", str(a.token), {"user_id": str(d.user_id)})
	assert_eq(
		blocked.status_code, 200,
		"block_user: status=%d body=%s"
			% [blocked.status_code, blocked.text])

	var joined: Dictionary = await _helper.session_rpc(
		"party_join_by_code", str(d.token), {"code": code})
	assert_ne(
		joined.status_code, 200,
		"join across a block should be refused: %s"
			% joined.text)


func test_raw_group_calls_on_a_party_are_refused() -> void:
	if not await _mint_users(2):
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var party_id := await _make_party(a, "party-")
	if party_id.is_empty():
		return

	var renamed: Dictionary = await _helper.http_put(
		"/v2/group/" + party_id, {"name": "party-renamed"},
		"bearer:" + str(a.token))
	assert_false(
		_is_2xx(renamed),
		"raw rename of a party should be refused: status=%d"
			% renamed.status_code)

	var banned: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/ban?user_ids="
			+ str(b.user_id),
		null, "bearer:" + str(a.token))
	assert_false(
		_is_2xx(banned),
		"raw ban in a party should be refused: status=%d"
			% banned.status_code)

	var deleted: Dictionary = await _helper.http_delete(
		"/v2/group/" + party_id, "bearer:" + str(a.token))
	assert_false(
		_is_2xx(deleted),
		"raw delete of a party should be refused: status=%d"
			% deleted.status_code)

	# The same calls keep working on a group that is not a
	# party.
	var clan_id := await _make_party(a, "clan-")
	if clan_id.is_empty():
		return
	var clan_renamed: Dictionary = await _helper.http_put(
		"/v2/group/" + clan_id, {"name": "clan-renamed"},
		"bearer:" + str(a.token))
	assert_true(
		_is_2xx(clan_renamed),
		"rename of a non-party group: status=%d body=%s"
			% [clan_renamed.status_code, clan_renamed.text])
	var clan_deleted: Dictionary = await _helper.http_delete(
		"/v2/group/" + clan_id, "bearer:" + str(a.token))
	assert_true(
		_is_2xx(clan_deleted),
		"delete of a non-party group: status=%d body=%s"
			% [clan_deleted.status_code, clan_deleted.text])


func _mint_users(count: int) -> bool:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return false
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return false
	_users = await _helper.multi_session_anon(
		count, "compliance-party-guards")
	if _users.size() != count:
		pending("multi_session_anon did not return the users")
		return false
	return true


## Creates a closed group named `<prefix>compliance-<run>`
## as the user and returns its id ("" after failing).
func _make_party(a: Dictionary, prefix: String) -> String:
	var group_name := (
		prefix + "compliance-"
		+ str(Time.get_unix_time_from_system() as int)
		+ "-"
		+ str(randi() % 100000))
	var create: Dictionary = await _helper.http_post(
		"/v2/group",
		{"name": group_name, "open": false, "max_count": 4},
		"bearer:" + str(a.token))
	assert_true(
		_is_2xx(create),
		"create-group: status=%d body=%s"
			% [create.status_code, create.text])
	if not (create.body is Dictionary):
		fail_test("create-group body not a dict: %s" % create.text)
		return ""
	var group_id := str(create.body.get("id", ""))
	assert_false(
		group_id.is_empty(),
		"create-group missing id: %s" % create.text)
	return group_id


func _is_2xx(result: Dictionary) -> bool:
	return (
		result.status_code >= 200 and result.status_code < 300)
