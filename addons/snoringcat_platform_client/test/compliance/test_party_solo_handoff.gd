extends GutTest
## A party holding only one player is not a bar to joining
## another party: the invite and the code join accept them,
## and accepting (or redeeming) disbands the party of one,
## including any invites it still has open. A party with
## other members is a bar, at invite, at code join and at
## accept alike.


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


func test_invite_to_solo_player_then_accept_disbands_solo() -> void:
	if not await _mint_users(2):
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var solo_id := await _make_party(a, b, "solo")
	var party_id := await _make_party(a, a, "")
	if solo_id.is_empty() or party_id.is_empty():
		return
	var invite: Dictionary = await _helper.session_rpc(
		"party_invite", str(a.token),
		{"party_id": party_id, "target_id": str(b.user_id)})
	assert_eq(
		invite.status_code, 200,
		"invite to a solo player: status=%d body=%s"
			% [invite.status_code, invite.text])

	var accept: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/join",
		null, "bearer:" + str(b.token))
	assert_true(
		_is_2xx(accept),
		"accept: status=%d body=%s"
			% [accept.status_code, accept.text])
	assert_eq(
		await _fetch_user_group_state(b, party_id), 2,
		"b is an active member of the new party")
	assert_eq(
		await _fetch_user_group_state(b, solo_id), -1,
		"the party of one is gone after the accept")


func test_code_join_from_solo_party_disbands_it() -> void:
	if not await _mint_users(2):
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var solo_id := await _make_party(a, b, "solo")
	var party_id := await _make_party(a, a, "")
	if solo_id.is_empty() or party_id.is_empty():
		return
	var code := await _invite_code(a, party_id)
	if code.is_empty():
		return
	var joined: Dictionary = await _helper.session_rpc(
		"party_join_by_code", str(b.token), {"code": code})
	assert_eq(
		joined.status_code, 200,
		"code join from a solo party: status=%d body=%s"
			% [joined.status_code, joined.text])
	assert_eq(
		await _fetch_user_group_state(b, party_id), 2,
		"b is an active member of the new party")
	assert_eq(
		await _fetch_user_group_state(b, solo_id), -1,
		"the party of one is gone after the redeem")


func test_code_join_refused_when_in_shared_party() -> void:
	if not await _mint_users(3):
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var c: Dictionary = _users[2]
	var shared_id := await _make_party(a, b, "solo")
	if shared_id.is_empty():
		return
	if not await _add_and_accept(b, c, shared_id):
		return
	var party_id := await _make_party(a, a, "")
	if party_id.is_empty():
		return
	var code := await _invite_code(a, party_id)
	if code.is_empty():
		return
	var joined: Dictionary = await _helper.session_rpc(
		"party_join_by_code", str(b.token), {"code": code})
	assert_ne(
		joined.status_code, 200,
		"code join from a shared party should be refused: %s"
			% joined.text)
	await _assert_active_member(b, shared_id,
		"b is still in their own party")


func test_accept_refused_when_solo_party_gained_a_member() -> void:
	if not await _mint_users(3):
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var c: Dictionary = _users[2]
	var solo_id := await _make_party(a, b, "solo")
	var party_id := await _make_party(a, a, "")
	if solo_id.is_empty() or party_id.is_empty():
		return
	var invite: Dictionary = await _helper.session_rpc(
		"party_invite", str(a.token),
		{"party_id": party_id, "target_id": str(b.user_id)})
	assert_eq(
		invite.status_code, 200,
		"invite to a solo player: status=%d body=%s"
			% [invite.status_code, invite.text])
	var invited_state := await _fetch_user_group_state(
		b, party_id)
	if not await _add_and_accept(b, c, solo_id):
		return
	var accept: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/join",
		null, "bearer:" + str(b.token))
	assert_false(
		_is_2xx(accept),
		"accept after the solo party gained a member should"
		+ " be refused: status=%d" % accept.status_code)
	await _assert_active_member(b, solo_id,
		"b is still in their own party")
	assert_eq(
		await _fetch_user_group_state(b, party_id),
		invited_state,
		"the refused accept changed nothing")


func _mint_users(count: int) -> bool:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return false
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return false
	_users = await _helper.multi_session_anon(
		count, "compliance-party-solo")
	if _users.size() != count:
		pending("multi_session_anon did not return the users")
		return false
	return true


## Creates a closed party as `creator`. With kind "solo" the
## party belongs to `b` and holds only them; otherwise it
## belongs to `a`. Returns "" after failing.
func _make_party(
	a: Dictionary, b: Dictionary, kind: String,
) -> String:
	var owner: Dictionary = b if kind == "solo" else a
	var group_name := (
		"party-compliance-"
		+ str(Time.get_unix_time_from_system() as int)
		+ "-"
		+ str(randi() % 100000))
	var create: Dictionary = await _helper.http_post(
		"/v2/group",
		{"name": group_name, "open": false, "max_count": 4},
		"bearer:" + str(owner.token))
	assert_true(
		_is_2xx(create),
		"create-group: status=%d body=%s"
			% [create.status_code, create.text])
	if not (create.body is Dictionary):
		fail_test("create-group body not a dict: %s" % create.text)
		return ""
	var group_id := str(create.body.get("id", ""))
	if group_id.is_empty():
		fail_test("create-group missing id: %s" % create.text)
	return group_id


## Adds `member` to the party as its leader and has them
## accept, making them an active member.
func _add_and_accept(
	leader: Dictionary, member: Dictionary, party_id: String,
) -> bool:
	var add: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/add?user_ids="
			+ str(member.user_id),
		null, "bearer:" + str(leader.token))
	assert_true(
		_is_2xx(add),
		"add member: status=%d body=%s"
			% [add.status_code, add.text])
	var join: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/join",
		null, "bearer:" + str(member.token))
	assert_true(
		_is_2xx(join),
		"member accept: status=%d body=%s"
			% [join.status_code, join.text])
	return _is_2xx(add) and _is_2xx(join)


func _invite_code(a: Dictionary, party_id: String) -> String:
	var result: Dictionary = await _helper.session_rpc(
		"party_get_invite_code", str(a.token),
		{"party_id": party_id})
	assert_eq(
		result.status_code, 200,
		"party_get_invite_code: status=%d body=%s"
			% [result.status_code, result.text])
	if not (result.inner is Dictionary):
		fail_test("invite code body not a dict: %s" % result.text)
		return ""
	var code := str(result.inner.get("code", ""))
	assert_false(code.is_empty(), "invite code missing")
	return code


## The user's edge state in the group (2 = active member,
## 3 = pending invite), or -1 when there is no edge.
func _fetch_user_group_state(
	user: Dictionary, group_id: String,
) -> int:
	var result: Dictionary = await _helper.http_get(
		"/v2/user/" + str(user.user_id) + "/group",
		"bearer:" + str(user.token))
	if result.status_code != 200:
		return -1
	if not (result.body is Dictionary):
		return -1
	var groups: Variant = result.body.get("user_groups", [])
	if not (groups is Array):
		return -1
	for entry in groups:
		if not (entry is Dictionary):
			continue
		var group: Variant = entry.get("group")
		if not (group is Dictionary):
			continue
		if str(group.get("id", "")) == group_id:
			return int(entry.get("state", -1))
	return -1



## Asserts the user is an active member of the group
## (leader or member edge, not a pending invite).
func _assert_active_member(
	user: Dictionary, group_id: String, message: String,
) -> void:
	var state := await _fetch_user_group_state(user, group_id)
	assert_true(
		state == 0 or state == 2,
		"%s (state=%d)" % [message, state])

func _is_2xx(result: Dictionary) -> bool:
	return (
		result.status_code >= 200 and result.status_code < 300)
