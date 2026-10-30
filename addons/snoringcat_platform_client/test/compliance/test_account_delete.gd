extends GutTest
## Account self-deletion (GDPR data deletion). Two surfaces:
##
##   1. The platform's soft-delete RPCs (Stage 1.4 / 1.5):
##      `delete_account` queues the account for hard deletion
##      after a 30-day grace window and clears everything a
##      cancellation doesn't need at once,
##      `get_account_deletion_status` reports the queued state,
##      and `cancel_account_deletion` keeps the account, without
##      restoring what was cleared. The friends and storage
##      cascades have their own files
##      (test_friends_account_delete_cascade.gd,
##      test_account_delete_cascade_surfaces.gd).
##   2. Nakama's built-in DELETE /v2/account, which deletes the
##      user's row hard.
##
## Every test uses a one-shot account, never the shared
## `compliance-anon-fixed-1`. delete_account would schedule that
## account for deletion and scrub its storage for every later run.
##
## test_delete_clears_identifying_data_at_once reads the deleted
## account back through the console and through the player's own
## data export, which lists everything stored about them
## (test_data_export.gd checks that it does).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
## The runtime's grace window (accountDeletionGracePeriod in
## account.go).
const _GRACE_DAYS := 30
## Display name delete_account anonymizes to.
const _ANONYMIZED_DISPLAY_NAME := "[deleted]"
## Nakama's channel type for a group chat (ChannelJoin.ChannelType).
const _CHANNEL_TYPE_GROUP := 3

var _helper
var _sock_helper
## One-shot users to hard-delete in after_each.
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


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
			(
				"DELETE /v2/account returned %d (likely gated by "
				+ "server config); cannot complete deletion round-trip"
			) % delete_result.status_code)
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


func test_soft_delete_then_cancel_keeps_account_without_profile() -> void:
	# The full Stage 1.5 round trip for a player who set a display
	# name: delete_account queues and anonymizes, the status RPC
	# reports it, and cancel_account_deletion clears the queue and
	# the "[deleted]" marker. The name doesn't come back: the queue
	# record keeps no copy of it.
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
	for key in ["original_display_name", "original_username"]:
		assert_false(
			status.inner.has(key),
			"the deletion keeps a copy of the profile (%s): %s"
				% [key, _body(status)])

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
		await _fetch_display_name(token), "",
		"cancel_account_deletion should clear the [deleted] marker"
		+ " and restore nothing")
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


func test_cancel_clears_marker_on_account_without_display_name() -> void:
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


func test_delete_clears_identifying_data_at_once() -> void:
	# A deletion request clears everything a cancellation doesn't
	# need right away, not at the 30-day hard delete: the profile
	# (avatar URL, email, location, time zone), the stored IP, the
	# friend code, friends, the player's party and chat messages,
	# notifications, and the rows naming them in other players'
	# recent-players lists. What stays is how they sign back in:
	# the device id and the custom id. Signing in while the
	# deletion is pending doesn't bring the profile back, and
	# cancelling keeps the account without restoring any of it.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if not _helper.get_console_refusal().is_empty():
		pending(_helper.get_console_refusal())
		return
	var users: Array = await _helper.multi_session_anon(
		2, "compliance-scrub")
	if users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	_users.append_array(users)
	var player: Dictionary = users[0]
	var other: Dictionary = users[1]
	var token := str(player.token)
	var user_id := str(player.user_id)
	var run := "%d%d" % [
		Time.get_unix_time_from_system() as int, randi() % 100000]
	var custom_id := "compliance-scrub-%s" % run

	# Seed one of each kind of identifying data.
	var profile: Dictionary = await _helper.http_put(
		"/v2/account",
		{
			"display_name": "Scrub %s" % run,
			"avatar_url": "https://lh3.googleusercontent.com/%s.png" % run,
		},
		"bearer:" + token)
	assert_true(profile.status_code < 300, "profile update failed")
	assert_true(
		await _helper.console_update_account(
			user_id,
			{
				"email": "scrub-%s@example.invalid" % run,
				"custom_id": custom_id,
				"location": "Compliance City",
				"timezone": "UTC",
			}),
		"console account update failed")
	var ip: Dictionary = await _helper.session_rpc(
		"record_client_ip", token, {})
	assert_eq(ip.status_code, 200, "record_client_ip failed")
	var code: Dictionary = await _helper.session_rpc(
		"get_friend_code", token, null)
	assert_eq(code.status_code, 200, "get_friend_code failed")
	await _helper.http_post(
		"/v2/friend?ids=" + str(other.user_id), null, "bearer:" + token)
	await _helper.http_post(
		"/v2/friend?ids=" + user_id, null, "bearer:" + str(other.token))
	# The row match_end would write in the other player's list.
	var recent: Dictionary = await _helper.http_put(
		"/v2/storage",
		{"objects": [{
			"collection": "recent_players",
			"key": user_id,
			"value": JSON.stringify({
				"user_id": user_id,
				"username": str(player.username),
				"display_name": "Scrub %s" % run,
				"matched_at": Time.get_unix_time_from_system() as int,
			}),
			"permission_read": 1,
			"permission_write": 1,
		}]},
		"bearer:" + str(other.token))
	assert_true(recent.status_code < 300, "recent_players seed failed")
	var party: Dictionary = await _helper.http_post(
		"/v2/group",
		{"name": "party-scrub-%s" % run, "open": false, "max_count": 4},
		"bearer:" + token)
	var party_id := ""
	if party.body is Dictionary:
		party_id = str(party.body.get("id", ""))
	assert_false(party_id.is_empty(), "party not created")
	await _send_party_chat(player, party_id, "scrub %s" % run)

	var before := await _export(token)
	var before_account: Dictionary = before.get("account", {})
	assert_false(
		str(before_account.get("email", "")).is_empty(),
		"seeding: no email")
	assert_false(
		str(before_account.get("friend_code", "")).is_empty(),
		"seeding: no friend code")
	assert_true(
		before.get("last_ip_address") is Dictionary, "seeding: no IP")
	assert_false(
		(before.get("notifications", []) as Array).is_empty(),
		"seeding: no notification")
	assert_false(
		(before.get("chat_messages", []) as Array).is_empty(),
		"seeding: no chat message")
	var inbox_before: Dictionary = await _helper.http_get(
		"/v2/notification?limit=100", "bearer:" + str(other.token))
	assert_true(
		_has_notification_from(inbox_before.body, user_id),
		"seeding: the friend request isn't in the other's inbox")

	var deleted: Dictionary = await _helper.session_rpc(
		"delete_account", token, null)
	assert_eq(
		deleted.status_code, 200,
		"delete_account: status=%d body=%s"
			% [deleted.status_code, _body(deleted)])

	var after := await _export(token)
	var account: Dictionary = after.get("account", {})
	assert_eq(
		str(account.get("display_name", "")), _ANONYMIZED_DISPLAY_NAME)
	for field in [
		"avatar_url", "email", "location", "timezone", "friend_code",
	]:
		assert_eq(
			str(account.get(field, "")), "",
			"delete_account left %s" % field)
	var sign_in: Dictionary = account.get("sign_in_ids", {})
	assert_has(
		sign_in.get("device_ids", []), str(player.device_id),
		"delete_account removed the device id a cancellation needs")
	assert_eq(
		str(sign_in.get("custom_id", "")), custom_id,
		"delete_account removed the custom id")
	assert_null(
		after.get("last_ip_address"), "delete_account left the stored IP")
	for section in [
		"friends", "groups", "leaderboard_records", "notifications",
		"chat_messages",
	]:
		assert_eq(
			(after.get(section, []) as Array).size(), 0,
			"delete_account left %s" % section)
	var storage: Array = after.get("storage_objects", [])
	assert_eq(storage.size(), 1, "delete_account left storage rows")
	if storage.size() == 1:
		assert_eq(
			str(storage[0].get("collection", "")),
			"account_deletion_queue")
	assert_true(
		bool(after.get("account_deletion", {}).get("pending", false)),
		"deletion not pending")
	var others_recent: Dictionary = await _helper.session_rpc(
		"list_recent_players", str(other.token), null)
	assert_false(
		_lists_user(others_recent.inner, user_id),
		"delete_account left the player in another's recent players")
	# The friend request the player sent sat in the other's inbox.
	var others_inbox: Dictionary = await _helper.http_get(
		"/v2/notification?limit=100", "bearer:" + str(other.token))
	assert_eq(others_inbox.status_code, 200, "list notifications")
	assert_false(
		_has_notification_from(others_inbox.body, user_id),
		"delete_account left the player's friend request in an inbox")

	# A Google sign-in refills an empty avatar from the provider.
	# Put one back that way, sign in, and it must be gone again.
	assert_true(
		await _helper.console_update_account(
			user_id, {"avatar_url": "https://lh3.googleusercontent.com/refill.png"}),
		"console avatar refill failed")
	assert_eq(
		await _device_sign_in(str(player.device_id)), 200,
		"a pending deletion should still sign in")
	var pending_account: Dictionary = await _helper.console_get_account(
		user_id)
	assert_eq(
		str(pending_account.get("user", {}).get("avatar_url", "")), "",
		"signing in during the grace window restored the avatar")

	var cancelled: Dictionary = await _helper.session_rpc(
		"cancel_account_deletion", token, null)
	assert_eq(
		cancelled.status_code, 200,
		"cancel_account_deletion: status=%d body=%s"
			% [cancelled.status_code, _body(cancelled)])
	var kept := await _export(token)
	var kept_account: Dictionary = kept.get("account", {})
	assert_false(
		bool(kept.get("account_deletion", {}).get("pending", true)),
		"still pending after cancel")
	for field in ["display_name", "avatar_url", "email"]:
		assert_eq(
			str(kept_account.get(field, "")), "",
			"cancel restored %s" % field)
	assert_eq(
		str(kept_account.get("sign_in_ids", {}).get("custom_id", "")),
		custom_id, "cancel lost the custom id")
	assert_eq(
		await _device_sign_in(str(player.device_id)), 200,
		"the kept account should still sign in")


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


## The caller's data export, or {} (after failing the test).
func _export(token: String) -> Dictionary:
	var result: Dictionary = await _helper.session_rpc(
		"export_player_data", token, null)
	if result.status_code != 200 or not (result.inner is Dictionary):
		fail_test("export_player_data: status=%d" % result.status_code)
		return {}
	return result.inner


## Signs in with an existing device id, without creating an
## account. Returns the HTTP status.
func _device_sign_in(device_id: String) -> int:
	var result: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=false",
		_helper.device_auth_body(device_id),
		"basic_server_key")
	return int(result.status_code)


## Sends one message in the party's group chat as `user`.
func _send_party_chat(user: Dictionary, party_id: String, text: String) -> void:
	var connection: Dictionary = await _sock_helper.connect_users([user])
	if not connection.ok:
		fail_test(str(connection.reason))
		return
	var sock: Variant = connection.sockets[0]
	var channel: Variant = await sock.join_chat_async(
		party_id, _CHANNEL_TYPE_GROUP, true, false)
	if channel == null or channel.is_exception():
		fail_test("join party chat failed")
	else:
		var ack: Variant = await sock.write_chat_message_async(
			channel.id, {"text": text})
		if ack == null or ack.is_exception():
			fail_test("send party chat failed")
	sock.close()


## True when a /v2/notification listing holds one sent by
## `sender_id`.
func _has_notification_from(listing: Variant, sender_id: String) -> bool:
	if not (listing is Dictionary):
		return false
	for entry in listing.get("notifications", []):
		if entry is Dictionary and str(entry.get("sender_id", "")) == sender_id:
			return true
	return false


## True when a list_recent_players response names `user_id`.
func _lists_user(response: Variant, user_id: String) -> bool:
	if not (response is Dictionary):
		return false
	for entry in response.get("recent_players", []):
		if entry is Dictionary and str(entry.get("user_id", "")) == user_id:
			return true
	return false
