extends GutTest
## Account self-deletion (GDPR data deletion). Two surfaces:
##
##   1. The platform's soft-delete RPCs (Stage 1.4 / 1.5):
##      `delete_account` queues the account for hard deletion
##      after a 30-day grace window and anonymizes it,
##      `get_account_deletion_status` reports the queued state,
##      and `cancel_account_deletion` restores the profile. The
##      friends and storage cascades have their own files
##      (test_friends_account_delete_cascade.gd,
##      test_account_delete_cascade_surfaces.gd).
##   2. Nakama's built-in DELETE /v2/account, which deletes the
##      user's row hard.
##
## Every test uses a one-shot account, never the shared
## `compliance-anon-fixed-1`. delete_account would schedule that
## account for deletion and scrub its storage for every later run.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
## The runtime's grace window (accountDeletionGracePeriod in
## account.go).
const _GRACE_DAYS := 30
## Display name delete_account anonymizes to.
const _ANONYMIZED_DISPLAY_NAME := "[deleted]"

var _helper
## One-shot users to hard-delete in after_each.
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	# The soft-deleted user can still sign in (no ban since
	# Stage 1.5), so its token still authorizes the hard delete.
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func _one_shot_device_id() -> String:
	var ts: int = Time.get_unix_time_from_system() as int
	return "compliance-delete-%d-%d" % [ts, randi() % 10000]


func test_delete_endpoint_removes_account() -> void:
	# End-to-end: create a one-shot account, delete it via
	# DELETE /v2/account, verify subsequent reads no longer
	# resolve the user. Catches the regression where a refactor
	# accidentally turns delete into a no-op (data-deletion
	# requests stop being honored).
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var device_id := _one_shot_device_id()

	# Step 1: create.
	var auth: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=true",
		_helper.device_auth_body(device_id),
		"basic_server_key")
	assert_eq(
		auth.status_code, 200,
		"create one-shot account: %s"
			% _helper.redact_tokens(auth.text))
	var token: String = str(auth.body.get("token", ""))
	var user_id: String = ""
	# Pull the user_id while we still have the session.
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	if account.body is Dictionary:
		user_id = str(
			account.body.get("user", {}).get("id", ""))
	assert_false(user_id.is_empty(), "no user_id pre-delete")

	# Step 2: delete.
	var delete_result: Dictionary = await _helper.http_delete(
		"/v2/account", "bearer:" + token)
	# Nakama responds 204 on success. Some deployments gate the
	# endpoint behind a flag and return a 4xx. The contract we
	# need is "delete works OR is explicitly gated"; a 5xx is
	# the regression (handler crashed).
	assert_lt(
		delete_result.status_code, 500,
		"DELETE /v2/account 5xx'd: %s" % delete_result.text)
	if (delete_result.status_code < 200
			or delete_result.status_code >= 300):
		pending(
			"DELETE /v2/account returned %d (likely gated by"
			+ " server config); cannot complete deletion"
			+ " round-trip"
				% delete_result.status_code)
		return

	# Step 3: re-auth with the same device_id, create=false.
	# A 404 confirms the user is actually gone (creating with
	# create=true would mask the issue by silently making a
	# new user with the same device_id). Nakama returns 404
	# for "device not found" when create=false.
	var verify: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=false",
		_helper.device_auth_body(device_id),
		"basic_server_key")
	assert_ne(
		verify.status_code, 200,
		"device_id still resolves to a user post-delete"
			+ " — delete was a no-op")


func test_soft_delete_then_cancel_restores_display_name() -> void:
	# The full Stage 1.5 round trip for a player who set a display
	# name: delete_account queues and anonymizes, the status RPC
	# reports it, and cancel_account_deletion puts the name back
	# and clears the queue.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	var user: Dictionary = await _one_shot_user("compliance-softdel")
	if user.is_empty():
		pending("multi_session_anon did not return a user")
		return
	var token: String = str(user.token)
	var display_name := "Compliance %d" % (
		Time.get_unix_time_from_system() as int)
	var update: Dictionary = await _helper.http_put(
		"/v2/account", {"display_name": display_name},
		"bearer:" + token)
	assert_true(
		update.status_code >= 200 and update.status_code < 300,
		"display_name update: status=%d body=%s"
			% [update.status_code, _body(update)])

	var requested_at := Time.get_unix_time_from_system() as int
	var deleted: Dictionary = await _helper.session_rpc(
		"delete_account", token, null)
	assert_eq(
		deleted.status_code, 200,
		"delete_account: status=%d body=%s"
			% [deleted.status_code, _body(deleted)])
	if not (deleted.inner is Dictionary):
		fail_test("delete_account body not a dict: %s" % _body(deleted))
		return
	assert_true(
		bool(deleted.inner.get("ok", false)),
		"delete_account ok!=true: %s" % _body(deleted))
	assert_eq(
		str(deleted.inner.get("user_id", "")), str(user.user_id),
		"delete_account user_id mismatch: %s" % _body(deleted))
	assert_eq(
		int(deleted.inner.get("grace_days", 0)), _GRACE_DAYS,
		"delete_account grace_days: %s" % _body(deleted))
	# scheduled_for is a unix time one grace window out. Allow a
	# minute of clock skew between this machine and the server.
	var expected_at := requested_at + _GRACE_DAYS * 86400
	assert_almost_eq(
		int(deleted.inner.get("scheduled_for", 0)), expected_at, 60,
		"delete_account scheduled_for: %s" % _body(deleted))

	assert_eq(
		await _fetch_display_name(token), _ANONYMIZED_DISPLAY_NAME,
		"display_name should be anonymized after delete_account")

	var status: Dictionary = await _helper.session_rpc(
		"get_account_deletion_status", token, null)
	assert_eq(
		status.status_code, 200,
		"get_account_deletion_status: status=%d body=%s"
			% [status.status_code, _body(status)])
	if not (status.inner is Dictionary):
		fail_test("deletion status body not a dict: %s" % _body(status))
		return
	assert_true(
		bool(status.inner.get("pending", false)),
		"deletion status pending!=true: %s" % _body(status))
	assert_eq(
		str(status.inner.get("original_display_name", "")),
		display_name,
		"queue row must keep the original display_name: %s"
			% _body(status))

	var cancelled: Dictionary = await _helper.session_rpc(
		"cancel_account_deletion", token, null)
	assert_eq(
		cancelled.status_code, 200,
		"cancel_account_deletion: status=%d body=%s"
			% [cancelled.status_code, _body(cancelled)])
	if cancelled.inner is Dictionary:
		assert_true(
			bool(cancelled.inner.get("ok", false)),
			"cancel_account_deletion ok!=true: %s" % _body(cancelled))
	assert_eq(
		await _fetch_display_name(token), display_name,
		"cancel_account_deletion should restore the display_name")
	var status_after: Dictionary = await _helper.session_rpc(
		"get_account_deletion_status", token, null)
	assert_true(
		status_after.inner is Dictionary
			and not bool(status_after.inner.get("pending", true)),
		"deletion status should clear after cancel: %s"
			% _body(status_after))

	# Nothing is left to cancel; the runtime refuses rather than
	# pretending.
	var cancelled_again: Dictionary = await _helper.session_rpc(
		"cancel_account_deletion", token, null)
	assert_ne(
		cancelled_again.status_code, 200,
		"a second cancel_account_deletion should be refused: %s"
			% _body(cancelled_again))


func test_cancel_restores_empty_display_name() -> void:
	# Anonymous sign-in sets no display name, so this is the common
	# case. Cancelling must clear the "[deleted]" marker rather than
	# leave the player looking deleted to everyone.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	var user: Dictionary = await _one_shot_user("compliance-softdel-anon")
	if user.is_empty():
		pending("multi_session_anon did not return a user")
		return
	var token: String = str(user.token)
	assert_eq(
		await _fetch_display_name(token), "",
		"a fresh anonymous account should have no display_name")

	var deleted: Dictionary = await _helper.session_rpc(
		"delete_account", token, null)
	assert_eq(
		deleted.status_code, 200,
		"delete_account: status=%d body=%s"
			% [deleted.status_code, _body(deleted)])
	var cancelled: Dictionary = await _helper.session_rpc(
		"cancel_account_deletion", token, null)
	assert_eq(
		cancelled.status_code, 200,
		"cancel_account_deletion: status=%d body=%s"
			% [cancelled.status_code, _body(cancelled)])
	assert_eq(
		await _fetch_display_name(token), "",
		"cancel_account_deletion left the anonymized display_name"
		+ " on an account that had none before")


## Mints one one-shot anonymous user and registers it for cleanup.
## Returns {} on failure.
func _one_shot_user(prefix: String) -> Dictionary:
	var users: Array = await _helper.multi_session_anon(1, prefix)
	if users.size() != 1:
		return {}
	_users.append(users[0])
	return users[0]


## Returns the caller's display_name from /v2/account ("" when
## unset), or "<status N>" when the read fails.
func _fetch_display_name(token: String) -> String:
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	if account.status_code != 200 or not (account.body is Dictionary):
		return "<status %d>" % account.status_code
	var user: Variant = account.body.get("user", {})
	if not (user is Dictionary):
		return "<no user block>"
	return str(user.get("display_name", ""))

## A response body for an assertion message, with any session or
## refresh token redacted.
func _body(result: Dictionary) -> String:
	return _helper.redact_tokens(str(result.text))
