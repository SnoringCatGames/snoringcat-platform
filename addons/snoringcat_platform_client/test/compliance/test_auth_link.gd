extends GutTest
## Anon → permanent account linking. The contract is "the
## anonymous account a player creates on first launch can be
## upgraded to a permanent account in place — same user_id,
## same data". Tests the device-id link path as a proxy
## (Google/Facebook/Apple linking has the same shape but needs
## real OAuth tokens, deferred).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"
## A second device id used for the link test. Deleting this
## one between runs would also be safe; we don't depend on
## state from a previous run.
const _LINK_DEVICE_ID := "compliance-link-secondary-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_link_secondary_device_to_anon_account() -> void:
	# Flow:
	#   1. Auth as the primary device → session A.
	#   2. POST /v2/account/link/device with the secondary
	#      device id, using session A's bearer token.
	#   3. Auth as the secondary device. Should return a session
	#      whose user_id equals session A's user_id (same player,
	#      now linked to two devices).
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	# Step 1.
	var primary_token: String = await _helper.nakama_anon_session(
		_DEVICE_ID)
	assert_false(primary_token.is_empty(), "primary auth failed")

	var primary_account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + primary_token)
	assert_eq(
		primary_account.status_code, 200,
		"/v2/account fetch: %s" % primary_account.text)
	var primary_user_id: String = str(
		primary_account.body.get("user", {}).get("id", ""))
	assert_false(
		primary_user_id.is_empty(),
		"primary user_id missing")

	# Step 2 — link the secondary device.
	# Nakama returns 204 on success or 409 if already linked.
	# Either is fine (the test is idempotent against re-runs).
	var link_result: Dictionary = await _helper.http_post(
		"/v2/account/link/device",
		{"id": _LINK_DEVICE_ID},
		"bearer:" + primary_token)
	var status: int = link_result.status_code
	var ok_status: bool = (
		status == 204 or status == 200 or status == 409)
	assert_true(
		ok_status,
		"link returned %d: %s" % [status, link_result.text])

	# Step 3 — auth as the secondary device, confirm same user_id.
	# create=false on a properly linked device means "find the
	# existing user, don't make a new one". A 404 here would
	# mean the link silently no-op'd.
	var secondary_result: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=false",
		{"id": _LINK_DEVICE_ID},
		"basic_server_key")
	assert_eq(
		secondary_result.status_code, 200,
		"secondary auth (post-link) failed: %d body=%s"
			% [secondary_result.status_code,
			   secondary_result.text])
	var secondary_token: String = str(
		secondary_result.body.get("token", ""))
	var secondary_account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + secondary_token)
	var secondary_user_id: String = str(
		secondary_account.body.get("user", {}).get("id", ""))
	assert_eq(
		secondary_user_id, primary_user_id,
		"linked device should map to same user_id")


func test_unlink_does_not_delete_user() -> void:
	# Inverse contract: unlinking a device leaves the underlying
	# user account intact. This catches regressions where a
	# refactor accidentally cascades the unlink into a delete.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	# Ensure the secondary device is linked (test_link runs
	# first usually but tests are independent — be defensive).
	var primary_token: String = await _helper.nakama_anon_session(
		_DEVICE_ID)
	assert_false(primary_token.is_empty())
	var _ignored: Dictionary = await _helper.http_post(
		"/v2/account/link/device",
		{"id": _LINK_DEVICE_ID},
		"bearer:" + primary_token)

	var primary_account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + primary_token)
	var primary_user_id: String = str(
		primary_account.body.get("user", {}).get("id", ""))

	# Unlink the secondary.
	var unlink: Dictionary = await _helper.http_post(
		"/v2/account/unlink/device",
		{"id": _LINK_DEVICE_ID},
		"bearer:" + primary_token)
	# 204 OK or 400 "can't unlink the last device" depending on
	# state. Both leave the primary account intact.
	assert_true(
		unlink.status_code == 204
			or unlink.status_code == 200
			or unlink.status_code == 400,
		"unlink returned unexpected %d: %s"
			% [unlink.status_code, unlink.text])

	# Primary account still readable + same id.
	var post_unlink: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + primary_token)
	assert_eq(
		post_unlink.status_code, 200,
		"primary account should still exist after unlink")
	assert_eq(
		str(post_unlink.body.get("user", {}).get("id", "")),
		primary_user_id,
		"primary user_id changed (account got rebuilt?)")
