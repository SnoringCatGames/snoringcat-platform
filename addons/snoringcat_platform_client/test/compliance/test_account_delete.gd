extends GutTest
## Account self-deletion (GDPR data deletion). Two surfaces:
##
##   1. Custom `delete_account` RPC. Soft-delete-with-grace-period
##      per PLATFORM_ARCHITECTURE.md §"Account deletion": queues a
##      `account_deletion_queue` record, anonymizes the user's
##      display name, cascade-clears friends/groups/presence/
##      leaderboards/storage, and bans the user so the existing
##      session can no longer authenticate. The hard-delete cron
##      is not yet implemented; the soft-delete is the user-
##      facing fact today.
##   2. Nakama's built-in DELETE /v2/account. Hard-deletes the
##      user's row; useful as a fallback and exercised here to
##      keep the contract that "delete really deletes" green.
##
## Tests use a one-shot device_id (timestamped) so each run
## creates + deletes a fresh account. No leaking state.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


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
		{"id": device_id},
		"basic_server_key")
	assert_eq(
		auth.status_code, 200,
		"create one-shot account: %s" % auth.text)
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
		{"id": device_id},
		"basic_server_key")
	assert_ne(
		verify.status_code, 200,
		"device_id still resolves to a user post-delete"
			+ " — delete was a no-op")


func test_delete_account_rpc_soft_deletes_and_bans() -> void:
	# End-to-end: create a one-shot device account, call the
	# `delete_account` RPC, verify (a) the response carries the
	# soft-delete payload, and (b) the existing session token can
	# no longer authenticate against the user (the ban took
	# effect). The hard-delete cron is not yet implemented; this
	# test covers the soft-delete + ban surface that
	# PLATFORM_ARCHITECTURE.md §"Account deletion" describes.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var device_id := _one_shot_device_id()

	# Step 1: create a one-shot account.
	var auth: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=true",
		{"id": device_id},
		"basic_server_key")
	assert_eq(
		auth.status_code, 200,
		"create one-shot account: %s" % auth.text)
	var token: String = str(auth.body.get("token", ""))
	assert_false(token.is_empty(), "no session token after create")

	# Step 2: call delete_account.
	var result: Dictionary = await _helper.session_rpc(
		"delete_account", token, null)
	assert_eq(
		result.status_code, 200,
		"delete_account RPC failed: %s" % result.text)
	assert_true(
		result.body is Dictionary,
		"delete_account body not a Dictionary: %s" % result.text)
	if result.body is Dictionary:
		assert_true(
			bool(result.body.get("ok", false)),
			"delete_account response not ok: %s" % result.text)
		assert_gt(
			int(result.body.get("scheduled_for", 0)),
			0,
			"delete_account missing scheduled_for")
		assert_gt(
			int(result.body.get("grace_days", 0)),
			0,
			"delete_account missing grace_days")

	# Step 3: verify the session token can no longer read the
	# account (the ban took effect, even though the JWT itself
	# is cryptographically valid until expiry).
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	assert_ne(
		account.status_code, 200,
		"banned session still reads /v2/account — ban did not"
		+ " take effect: %s" % account.text)
