extends GutTest
## Ready state belongs to one matchmaking attempt: a
## start needs every active member ready, spends that
## readiness when it fires, and a member who leaves takes
## their ready row with them.


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


func test_start_needs_every_member_ready_and_spends_it() -> void:
	if not await _mint_users(2):
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var party_id := await _make_party_with_member(a, b)
	if party_id.is_empty():
		return

	await _set_ready(a, party_id, true)
	var early: Dictionary = await _helper.session_rpc(
		"party_start_matchmaking", str(a.token),
		{"party_id": party_id})
	assert_ne(
		early.status_code, 200,
		"start with an unready member should be refused: %s"
			% early.text)

	await _set_ready(b, party_id, true)
	var started: Dictionary = await _helper.session_rpc(
		"party_start_matchmaking", str(a.token),
		{"party_id": party_id})
	assert_eq(
		started.status_code, 200,
		"start with every member ready: status=%d body=%s"
			% [started.status_code, started.text])

	# The start spent the party's readiness, so a second
	# start without readying again is refused.
	var again: Dictionary = await _helper.session_rpc(
		"party_start_matchmaking", str(a.token),
		{"party_id": party_id})
	assert_ne(
		again.status_code, 200,
		"second start without re-readying should be refused: %s"
			% again.text)


func _mint_users(count: int) -> bool:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return false
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return false
	_users = await _helper.multi_session_anon(
		count, "compliance-party-ready")
	if _users.size() != count:
		pending("multi_session_anon did not return the users")
		return false
	return true


## Creates a closed party as `a` with `b` as an active
## member, through the same calls the client makes.
func _make_party_with_member(
	a: Dictionary, b: Dictionary,
) -> String:
	var group_name := (
		"party-compliance-"
		+ str(Time.get_unix_time_from_system() as int)
		+ "-"
		+ str(randi() % 100000))
	var create: Dictionary = await _helper.http_post(
		"/v2/group",
		{"name": group_name, "open": false, "max_count": 4},
		"bearer:" + str(a.token))
	assert_true(
		create.status_code >= 200 and create.status_code < 300,
		"create-group: status=%d body=%s"
			% [create.status_code, create.text])
	if not (create.body is Dictionary):
		fail_test("create-group body not a dict: %s" % create.text)
		return ""
	var party_id := str(create.body.get("id", ""))
	if party_id.is_empty():
		fail_test("create-group missing id: %s" % create.text)
		return ""
	var add: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/add?user_ids="
			+ str(b.user_id),
		null, "bearer:" + str(a.token))
	assert_true(
		add.status_code >= 200 and add.status_code < 300,
		"add member: status=%d body=%s"
			% [add.status_code, add.text])
	var join: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/join",
		null, "bearer:" + str(b.token))
	assert_true(
		join.status_code >= 200 and join.status_code < 300,
		"member accept: status=%d body=%s"
			% [join.status_code, join.text])
	return party_id


func _set_ready(
	user: Dictionary, party_id: String, ready: bool,
) -> void:
	var result: Dictionary = await _helper.session_rpc(
		"party_set_ready", str(user.token),
		{"party_id": party_id, "ready": ready})
	assert_eq(
		result.status_code, 200,
		"party_set_ready: status=%d body=%s"
			% [result.status_code, result.text])
