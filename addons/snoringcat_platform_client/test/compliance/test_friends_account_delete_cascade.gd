extends GutTest
## Friends cascade when a mutual-friend account is soft-deleted.
## Stage 8.16.
##
## The single-user test_account_delete.gd covers Nakama's
## built-in `DELETE /v2/account` and the existence of the
## custom `delete_account` RPC, but cannot reach the cascade
## paths that Stage 1.4 introduced. The multi-user shape
## verifies that:
##
##   1. `delete_account` returns a well-formed soft-delete
##      response (OK=true, scheduled_for, grace_days=30).
##   2. The deleter's friends list is cleared.
##   3. The surviving friend's list also loses the deleter
##      (Nakama's FriendsDelete is bidirectional — the
##      cascade scrubs both rows of every friendship).
##   4. The deleter's display name is anonymized to
##      "[deleted]" while username and account row survive
##      for the 30-day grace.
##   5. `get_account_deletion_status` reports `pending=true`
##      for the deleter (Stage 1.5 cancellation surface; the
##      deleter is intentionally NOT banned post-1.5).
##
## What this test guards:
##   - Stage 1.4: friends cascade clears bidirectionally on
##     `delete_account`; display-name anonymization runs;
##     account_deletion_queue row is written.
##   - Stage 1.5: the deleter retains sign-in capability so
##     `cancel_account_deletion` is reachable.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
## Nakama's friend-state enum. Mirrors the
## test_friends_multiuser.gd subset.
const _STATE_FRIEND := 0

var _helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	# Best-effort cleanup. The deleter (A) is soft-deleted but
	# not banned post-1.5, so its token still works for a
	# Nakama hard-delete — that strips the user shell AND the
	# audit-trail queue row in one shot. The surviving friend
	# (B) hard-deletes normally.
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_delete_account_removes_deleter_from_other_friend_lists() -> void:
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

	# Step 1: A↔B mutual friendship.
	var add: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		add.status_code >= 200 and add.status_code < 300,
		"A→B request: status=%d body=%s"
			% [add.status_code, add.text])
	var accept: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(a.user_id),
		null,
		"bearer:" + str(b.token))
	assert_true(
		accept.status_code >= 200 and accept.status_code < 300,
		"B→A accept: status=%d body=%s"
			% [accept.status_code, accept.text])

	# Step 2: confirm pre-cascade state — both see each other
	# as state=0 FRIEND. If this fails, the rest of the test
	# isn't exercising the cascade.
	var a_pre: int = await _fetch_friend_state(
		str(a.token), str(b.user_id))
	var b_pre: int = await _fetch_friend_state(
		str(b.token), str(a.user_id))
	assert_eq(
		a_pre, _STATE_FRIEND,
		"pre-cascade: A should see B as FRIEND; got %d" % a_pre)
	assert_eq(
		b_pre, _STATE_FRIEND,
		"pre-cascade: B should see A as FRIEND; got %d" % b_pre)

	# Step 3: A invokes the platform delete_account RPC.
	# Payload is ignored by the handler; the user is identified
	# by the session token.
	var delete_result: Dictionary = await _helper.session_rpc(
		"delete_account", str(a.token), null)
	assert_eq(
		delete_result.status_code, 200,
		"delete_account RPC: status=%d body=%s"
			% [delete_result.status_code, delete_result.text])
	var inner: Variant = delete_result.inner
	assert_true(
		inner is Dictionary,
		"delete_account response not a dict: %s"
			% delete_result.text)
	assert_true(
		bool(inner.get("ok", false)),
		"delete_account ok!=true: %s" % delete_result.text)
	assert_eq(
		str(inner.get("user_id", "")), str(a.user_id),
		"delete_account user_id mismatch: %s" % delete_result.text)
	assert_gt(
		int(inner.get("scheduled_for", 0)), 0,
		"delete_account missing scheduled_for: %s"
			% delete_result.text)
	assert_eq(
		int(inner.get("grace_days", 0)), 30,
		"delete_account grace_days != 30: %s" % delete_result.text)

	# Step 4: B's friends list no longer contains A. Nakama's
	# FriendsDelete (called by the cascade) deletes both sides
	# of the relationship, so B loses A even though B is the
	# party that didn't initiate.
	var b_post: int = await _fetch_friend_state(
		str(b.token), str(a.user_id))
	assert_eq(
		b_post, -1,
		(
			"post-cascade: B's friend list still contains A"
			+ " (state=%d); FriendsDelete should clear both"
			+ " sides bidirectionally."
		) % b_post)

	# Step 5: A's own friends list is also empty. The cascade
	# paginates A's full friend list and calls FriendsDelete on
	# the bulk; A side should be empty by the time the RPC
	# returns.
	var a_post: int = await _fetch_friend_state(
		str(a.token), str(b.user_id))
	assert_eq(
		a_post, -1,
		(
			"post-cascade: A's own friend list still contains B"
			+ " (state=%d); deleter's side wasn't scrubbed."
		) % a_post)

	# Step 6: A's display name is anonymized to "[deleted]"
	# while username + account survive (Stage 1.5: no ban, so
	# A retains sign-in capability for the cancellation
	# surface). A's session token from pre-delete still works.
	var a_account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + str(a.token))
	assert_eq(
		a_account.status_code, 200,
		(
			"post-cascade: A should still authenticate (not"
			+ " banned per 1.5); got status=%d body=%s"
		) % [a_account.status_code, a_account.text])
	if a_account.body is Dictionary:
		var user_obj: Variant = a_account.body.get("user", {})
		if user_obj is Dictionary:
			assert_eq(
				str(user_obj.get("display_name", "")),
				"[deleted]",
				(
					"post-cascade: A's display_name should be"
					+ " '[deleted]'; got %s"
				) % str(user_obj.get("display_name", "")))

	# Step 7: get_account_deletion_status reports pending=true
	# with the audit fields populated. The original_username
	# matches the device-id-derived Nakama username so a future
	# cancel_account_deletion can restore it.
	var status: Dictionary = await _helper.session_rpc(
		"get_account_deletion_status", str(a.token), null)
	assert_eq(
		status.status_code, 200,
		"get_account_deletion_status: status=%d body=%s"
			% [status.status_code, status.text])
	var status_inner: Variant = status.inner
	assert_true(
		status_inner is Dictionary,
		"get_account_deletion_status response not a dict: %s"
			% status.text)
	assert_true(
		bool(status_inner.get("pending", false)),
		"get_account_deletion_status pending!=true: %s"
			% status.text)
	assert_eq(
		str(status_inner.get("user_id", "")), str(a.user_id),
		"get_account_deletion_status user_id mismatch: %s"
			% status.text)
	assert_gt(
		int(status_inner.get("scheduled_for", 0)), 0,
		"get_account_deletion_status missing scheduled_for: %s"
			% status.text)


## Returns the Nakama friend-state int for `target_id` from the
## perspective of the session that owns `token`. Returns -1 when
## the target isn't in the friend list at all (the post-cascade
## expected state).
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
