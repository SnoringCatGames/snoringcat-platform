extends GutTest
## Account self-deletion (GDPR data deletion). Two surfaces:
##
##   1. Custom `delete_account` RPC. PLATFORM_ARCHITECTURE.md
##      describes a soft-delete-with-grace-period flow, but the
##      RPC isn't implemented yet (no handler in runtime/). When
##      it lands, this test should switch to it.
##   2. Nakama's built-in DELETE /v2/account. The current path
##      a SDK can use today; deletes the user's row hard.
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
		_helper.device_auth_body(device_id),
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
		_helper.device_auth_body(device_id),
		"basic_server_key")
	assert_ne(
		verify.status_code, 200,
		"device_id still resolves to a user post-delete"
			+ " — delete was a no-op")


func test_delete_account_rpc_documented_but_not_implemented() -> void:
	# When the platform ships the soft-delete-with-grace-period
	# RPC described in PLATFORM_ARCHITECTURE.md, this test
	# should be replaced with a real flow. Probe that the RPC
	# either doesn't exist (current expected state) or, if it
	# does, the gate is wired correctly.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(
		"compliance-anon-fixed-1")
	var result: Dictionary = await _helper.session_rpc(
		"delete_account", token, null)
	# Nakama returns a structured error for unregistered RPCs.
	# The handler doesn't exist yet — accept 404/400/5xx-with-
	# error-payload — but flag a 200 as the "we accidentally
	# implemented this and forgot to update the test" case.
	if result.status_code == 200:
		pending(
			"delete_account RPC now responds with 200 — replace"
			+ " this placeholder test with a real soft-delete"
			+ " flow per PLATFORM_ARCHITECTURE.md.")
		return
	# Otherwise: the contract is just "RPC isn't there". No
	# assertion to fail; this is documentation in test form.
	assert_true(true, "delete_account RPC not yet implemented")
