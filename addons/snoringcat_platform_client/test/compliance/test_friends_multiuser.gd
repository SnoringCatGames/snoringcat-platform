extends GutTest
## Two-user friend request → accept → mutual-friendship flow.
## First test landing the Stage 8.12 `multi_session_anon` helper
## end-to-end. Catches regressions in Nakama's friend-state
## semantics (state transitions across `POST /v2/friend?ids=X`
## and `GET /v2/friend`) that the single-user test_friends.gd
## can't reach.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
## Nakama's friend state enum, hard-coded here so the test
## doesn't depend on importing the SDK class.
##   FRIEND = 0 — mutual.
##   INVITE_SENT = 1 — caller initiated, awaiting other side.
##   INVITE_RECEIVED = 2 — other side initiated, awaiting caller.
const _STATE_FRIEND := 0
const _STATE_INVITE_SENT := 1
const _STATE_INVITE_RECEIVED := 2

var _helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	# Best-effort cleanup of the one-shot accounts. A failure
	# here is logged but doesn't fail the test — the next CI run
	# generates fresh ids and the leftover rows are grep-able by
	# their "compliance-multi-" prefix.
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_friend_request_then_accept_makes_mutual() -> void:
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

	# Step 1: A sends a friend request to B.
	var add: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		add.status_code >= 200 and add.status_code < 300,
		"A→B request: status=%d body=%s"
			% [add.status_code, add.text])

	# Step 2: A sees B with state=1 (INVITE_SENT).
	var a_state: int = await _fetch_friend_state(
		str(a.token), str(b.user_id))
	assert_eq(
		a_state, _STATE_INVITE_SENT,
		"A should see B as INVITE_SENT (1); got %d" % a_state)

	# Step 3: B sees A with state=2 (INVITE_RECEIVED).
	var b_state_before: int = await _fetch_friend_state(
		str(b.token), str(a.user_id))
	assert_eq(
		b_state_before, _STATE_INVITE_RECEIVED,
		"B should see A as INVITE_RECEIVED (2); got %d"
			% b_state_before)

	# Step 4: B accepts by sending the mirror request.
	var accept: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(a.user_id),
		null,
		"bearer:" + str(b.token))
	assert_true(
		accept.status_code >= 200 and accept.status_code < 300,
		"B→A accept: status=%d body=%s"
			% [accept.status_code, accept.text])

	# Step 5: both sides now see state=0 (FRIEND).
	var a_state_after: int = await _fetch_friend_state(
		str(a.token), str(b.user_id))
	var b_state_after: int = await _fetch_friend_state(
		str(b.token), str(a.user_id))
	assert_eq(
		a_state_after, _STATE_FRIEND,
		"A should see B as FRIEND (0) after accept; got %d"
			% a_state_after)
	assert_eq(
		b_state_after, _STATE_FRIEND,
		"B should see A as FRIEND (0) after accept; got %d"
			% b_state_after)


## Returns the Nakama friend-state int for `target_id` from the
## perspective of the session that owns `token`. Returns -1 when
## the target isn't in the friend list at all.
func _fetch_friend_state(token: String, target_id: String) -> int:
	var result: Dictionary = await _helper.http_get(
		"/v2/friend", "bearer:" + token)
	if result.status_code != 200:
		return -1
	if not (result.body is Dictionary):
		return -1
	var friends: Variant = result.body.get("friends", [])
	if not (friends is Array):
		return -1
	for entry in friends:
		if not (entry is Dictionary):
			continue
		var user: Variant = entry.get("user")
		if not (user is Dictionary):
			continue
		if str(user.get("id", "")) == target_id:
			return int(entry.get("state", -1))
	return -1
