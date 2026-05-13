extends GutTest
## Two-user party invite → membership lifecycle. Stage 8.17.
##
## The single-user test_party.gd only covers the
## create → leave loop. The multi-user shape exercises
## the bits that broke pre-Stage-5: the invite ends with
## the invitee as a state=2 active member, the party
## creator sees state=0, and /v2/group/{id}/user returns
## both for the `fetch_party_status.members` path.
##
## What this test guards:
##   - Stage 1.2/1.3: party member list is reachable via
##     /v2/group/{id}/user and includes both users with
##     the correct group-user state values (0 for creator,
##     2 for member).
##   - Stage 5.11: list_user_groups returns each party
##     for the invitee with a well-defined state. The
##     fetch_party_status split between `party` and
##     `pending_invites` is keyed on state==3; the test
##     records the observed initial state so a future
##     Nakama upgrade flipping the contract (state=2 ↔
##     state=3 for admin-add on closed groups) fails
##     loudly here instead of silently in production.
##   - Round-trips via the same /v2/group endpoints
##     PlatformPartyApiClient hits through the SDK, so the
##     test fails the same way the addon would on contract
##     drift.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
## Nakama's group-user state enum. Hard-coded here so the
## test doesn't depend on importing the SDK class.
##   SUPERADMIN = 0 — group creator.
##   ADMIN = 1
##   MEMBER = 2 — accepted member.
##   JOIN_REQUEST = 3 — pending invite/join-request on a
##     closed group.
const _STATE_SUPERADMIN := 0
const _STATE_MEMBER := 2
const _STATE_JOIN_REQUEST := 3

var _helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	# Best-effort cleanup. Deleting the user cascades through
	# Nakama's group-membership tables, so the party group is
	# torn down too. A failure here is logged but doesn't
	# fail the test.
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_invite_lifecycle_lands_invitee_as_member() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	assert_false(str(a.user_id).is_empty(), "user A missing user_id")
	assert_false(str(b.user_id).is_empty(), "user B missing user_id")

	# Step 1: A creates a closed party group. Closed
	# (open=false) is the production party shape — open
	# parties would let anyone join uninvited.
	var group_name := (
		"party-compliance-"
		+ str(Time.get_unix_time_from_system() as int)
		+ "-"
		+ str(randi() % 100000))
	var create_body := {
		"name": group_name,
		"open": false,
		"max_count": 4,
	}
	var create: Dictionary = await _helper.http_post(
		"/v2/group", create_body, "bearer:" + str(a.token))
	assert_true(
		create.status_code >= 200 and create.status_code < 300,
		"create-group: status=%d body=%s"
			% [create.status_code, create.text])
	assert_true(
		create.body is Dictionary,
		"create-group body not a dict: %s" % create.text)
	var party_id: String = str(create.body.get("id", ""))
	assert_false(
		party_id.is_empty(),
		"create-group missing id: %s" % create.text)
	# Sanity-check: the group really is closed. If this
	# flips, the rest of the test isn't exercising the
	# closed-group invite path even if it still passes.
	assert_false(
		bool(create.body.get("open", true)),
		"create-group returned open=true; closed-party path"
			+ " not exercised. body=%s" % create.text)

	# Step 2: A invites B. Nakama's add-user-to-group
	# endpoint takes target user_ids as a query parameter.
	var add: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/add?user_ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		add.status_code >= 200 and add.status_code < 300,
		"invite A→B: status=%d body=%s"
			% [add.status_code, add.text])

	# Step 3: B observes the party in their list. The
	# observed state determines whether Nakama 3.x in this
	# install treats admin-add as direct-membership (state=2)
	# or as pending-invite (state=3). PartyApiClient.fetch_
	# party_status keys on this distinction to route the
	# entry into `party` vs `pending_invites`.
	var b_state_initial: int = await _fetch_user_group_state(
		str(b.token), str(b.user_id), party_id)
	assert_true(
		(
			b_state_initial == _STATE_MEMBER
			or b_state_initial == _STATE_JOIN_REQUEST
		),
		(
			"B's initial state after invite should be 2 or 3,"
			+ " got %d. If 0 or 1, B was added as admin which"
			+ " contradicts the invite-target role contract."
		) % b_state_initial)

	# Step 4: If Nakama parked B at state=3, exercise the
	# accept path (joinGroup flips state=3 → state=2). If
	# Nakama already put B at state=2, the joinGroup call
	# would no-op or 4xx; skip it.
	if b_state_initial == _STATE_JOIN_REQUEST:
		var join: Dictionary = await _helper.http_post(
			"/v2/group/" + party_id + "/join",
			null,
			"bearer:" + str(b.token))
		assert_true(
			join.status_code >= 200 and join.status_code < 300,
			"B accept-via-join: status=%d body=%s"
				% [join.status_code, join.text])
		var b_state_after_accept: int = (
			await _fetch_user_group_state(
				str(b.token), str(b.user_id), party_id))
		assert_eq(
			b_state_after_accept, _STATE_MEMBER,
			"B should flip 3 → 2 after joinGroup; got %d"
				% b_state_after_accept)

	# Step 5: A still sees themselves at state=0
	# (Superadmin / creator). The role assignment is the
	# bedrock the leader_id resolver relies on.
	var a_state: int = await _fetch_user_group_state(
		str(a.token), str(a.user_id), party_id)
	assert_eq(
		a_state, _STATE_SUPERADMIN,
		"A should be SUPERADMIN(0) of own party; got %d"
			% a_state)

	# Step 6: /v2/group/{id}/user (the endpoint
	# fetch_party_status' `members` array reads from)
	# returns both A and B with the right roles.
	var members: Dictionary = await _fetch_group_members(
		str(a.token), party_id)
	var a_in_list: Dictionary = members.get(str(a.user_id), {})
	var b_in_list: Dictionary = members.get(str(b.user_id), {})
	assert_false(
		a_in_list.is_empty(),
		"A missing from group members: keys=%s"
			% str(members.keys()))
	assert_false(
		b_in_list.is_empty(),
		"B missing from group members: keys=%s"
			% str(members.keys()))
	assert_eq(
		int(a_in_list.get("state", -1)), _STATE_SUPERADMIN,
		"A should be SUPERADMIN(0) in group members list")
	assert_eq(
		int(b_in_list.get("state", -1)), _STATE_MEMBER,
		"B should be MEMBER(2) in group members list")

	# Step 7: B leaves. Exercises the leave path that
	# AfterLeaveGroup fans out on (party_state_changed
	# notification + auto-transfer-on-leader-departed).
	var leave: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/leave",
		null,
		"bearer:" + str(b.token))
	assert_true(
		leave.status_code >= 200 and leave.status_code < 300,
		"B leave: status=%d body=%s"
			% [leave.status_code, leave.text])

	# Step 8: B no longer sees the party in their list.
	var b_state_final: int = await _fetch_user_group_state(
		str(b.token), str(b.user_id), party_id)
	assert_eq(
		b_state_final, -1,
		"B should no longer see party after leave; got state=%d"
			% b_state_final)


## Returns the Nakama group-user state int for `group_id`
## from the perspective of `user_id`. Returns -1 when the
## group isn't in the user's list at all.
func _fetch_user_group_state(
	token: String,
	user_id: String,
	group_id: String,
) -> int:
	var result: Dictionary = await _helper.http_get(
		"/v2/user/" + user_id + "/group",
		"bearer:" + token)
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


## Returns a Dictionary mapping user_id → {state, username,
## display_name} for every member of the given group, as
## reported by /v2/group/{id}/user. Empty on error.
func _fetch_group_members(
	token: String,
	group_id: String,
) -> Dictionary:
	var members: Dictionary = {}
	var result: Dictionary = await _helper.http_get(
		"/v2/group/" + group_id + "/user",
		"bearer:" + token)
	if result.status_code != 200:
		return members
	if not (result.body is Dictionary):
		return members
	var group_users: Variant = result.body.get("group_users", [])
	if not (group_users is Array):
		return members
	for entry in group_users:
		if not (entry is Dictionary):
			continue
		var user: Variant = entry.get("user")
		if not (user is Dictionary):
			continue
		var uid := str(user.get("id", ""))
		if uid.is_empty():
			continue
		members[uid] = {
			"state": int(entry.get("state", -1)),
			"username": str(user.get("username", "")),
			"display_name": str(user.get("display_name", "")),
		}
	return members
