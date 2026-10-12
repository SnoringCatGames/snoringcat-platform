extends GutTest
## export_player_data RPC: the GDPR / CCPA data export. Returns
## everything the platform stores about the caller
## (runtime/data_export.go). The privacy policy promises players it
## works on demand from in-game settings.
##
## Contract:
##   request:  empty
##   response: {format_version, generated_at, account,
##              account_deletion, last_ip_address, friends, groups,
##              leaderboard_records, storage_objects, notifications,
##              chat_messages, wallet_ledger}
##   - account holds the profile, any email, sign_in_ids (device
##     ids, custom id, every provider id), and the friend code.
##   - last_ip_address is null when no IP is stored.
##   - Every array is present, even when empty.
##   - A section that can't be read fails the whole export.
## Auth: Bearer session token.
##
## test_export_includes_every_category seeds one of each kind of
## data for a one-shot player (two more stand in for a friend and a
## blocked player) and checks that each turns up. It needs the
## console (to put an email on the account, which no client can) and
## a runtime with EDGEGAP_MOCK_DEPLOY=true (to play a match without
## allocating a server); local-smoke-test.ps1 provides both.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"
const _FORMAT_VERSION := 2
const _ENVELOPE_ARRAYS := [
	"friends", "groups", "leaderboard_records", "storage_objects",
	"notifications", "chat_messages", "wallet_ledger",
]
## Nakama's channel type for a group chat (ChannelJoin.ChannelType).
const _CHANNEL_TYPE_GROUP := 3
## Nakama's notification code for "your friend request was accepted".
const _NOTIFICATION_FRIEND_ACCEPT := -3
const _MATCH_TIMEOUT_SEC := 13.0

var _helper
var _sock_helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


func after_each() -> void:
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_export_returns_full_envelope_for_authed_user() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty())

	var result: Dictionary = await _helper.session_rpc(
		"export_player_data", token, null)
	assert_eq(
		result.status_code, 200,
		"export_player_data: status=%d" % result.status_code)
	assert_true(result.inner is Dictionary, "inner not a dict")
	if not (result.inner is Dictionary):
		return
	var export: Dictionary = result.inner

	assert_eq(int(export.get("format_version", 0)), _FORMAT_VERSION)
	assert_false(
		str(export.get("generated_at", "")).is_empty(),
		"generated_at is empty")
	for field in ["account", "account_deletion"]:
		assert_true(
			export.get(field) is Dictionary,
			"%s missing or not an object" % field)
	assert_true(
		export.has("last_ip_address"), "missing last_ip_address key")
	for field in _ENVELOPE_ARRAYS:
		assert_true(
			export.get(field) is Array,
			"%s missing or not an array" % field)


func test_export_account_block_has_user_id_when_present() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)

	# Pull the canonical user_id.
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	var canonical_id: String = str(
		account.body.get("user", {}).get("id", ""))
	assert_false(
		canonical_id.is_empty(), "no canonical user_id")

	var result: Dictionary = await _helper.session_rpc(
		"export_player_data", token, null)
	assert_eq(result.status_code, 200)
	var account_block: Variant = result.inner.get("account")
	assert_true(account_block is Dictionary, "account block missing")
	if account_block is Dictionary:
		assert_eq(
			str(account_block.get("user_id", "")), canonical_id,
			"export.account.user_id mismatch with /v2/account")


func test_export_includes_every_category() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return
	if not _helper.get_console_refusal().is_empty():
		pending(_helper.get_console_refusal())
		return
	if not await _helper.is_mock_deploy_mode():
		pending("needs a runtime with EDGEGAP_MOCK_DEPLOY=true")
		return

	_users = await _helper.multi_session_anon(3, "compliance-export")
	if _users.size() != 3:
		pending("multi_session_anon did not return three users")
		return
	var player: Dictionary = _users[0]
	var friend: Dictionary = _users[1]
	var blocked: Dictionary = _users[2]
	var token := str(player.token)
	var run := "%d%d" % [
		Time.get_unix_time_from_system() as int, randi() % 100000]

	# Profile, and what only an operator can set: an email (as a
	# Google sign-in stored one before the platform stopped) and a
	# custom id. Seeding through the console skips the sign-in
	# hooks, and the player doesn't sign in again before exporting.
	var display_name := "Export %s" % run
	var avatar_url := "https://example.invalid/%s.png" % run
	var email := "export-%s@example.invalid" % run
	var custom_id := "compliance-export-%s" % run
	var profile_update: Dictionary = await _helper.http_put(
		"/v2/account",
		{"display_name": display_name, "avatar_url": avatar_url},
		"bearer:" + token)
	assert_true(
		profile_update.status_code < 300, "profile update failed")
	assert_true(
		await _helper.console_update_account(
			str(player.user_id),
			{
				"email": email,
				"custom_id": custom_id,
				"location": "Compliance City",
				"timezone": "UTC",
			}),
		"console account update failed")

	var friend_code := await _friend_code(token)
	assert_false(friend_code.is_empty(), "no friend code")

	# A friendship (whose acceptance also leaves the player a
	# notification) and a block.
	await _helper.http_post(
		"/v2/friend?ids=" + str(friend.user_id), null, "bearer:" + token)
	await _helper.http_post(
		"/v2/friend?ids=" + str(player.user_id),
		null, "bearer:" + str(friend.token))
	var block: Dictionary = await _helper.session_rpc(
		"block_user", token, {"user_id": str(blocked.user_id)})
	assert_eq(block.status_code, 200, "block_user failed")

	# A party with a game mode.
	var party_name := "party-export-%s" % run
	var party: Dictionary = await _helper.http_post(
		"/v2/group",
		{"name": party_name, "open": false, "max_count": 4},
		"bearer:" + token)
	var party_id := ""
	if party.body is Dictionary:
		party_id = str(party.body.get("id", ""))
	assert_false(party_id.is_empty(), "party not created")
	var mode: Dictionary = await _helper.session_rpc(
		"party_set_mode", token,
		{"party_id": party_id, "mode_id": "ffa"})
	assert_eq(mode.status_code, 200, "party_set_mode failed")

	# Settings, presence, and the stored IP.
	var settings: Dictionary = await _helper.http_put(
		"/v2/storage",
		{"objects": [{
			"collection": "settings",
			"key": "global",
			"value": JSON.stringify({"compliance_run": run}),
			"permission_read": 1,
			"permission_write": 1,
		}]},
		"bearer:" + token)
	assert_true(settings.status_code < 300, "settings write failed")
	var presence: Dictionary = await _helper.session_rpc(
		"update_and_get_presence", token,
		{"rich_presence": "compliance-export", "status": "online"})
	assert_eq(presence.status_code, 200, "presence write failed")
	var ip: Dictionary = await _helper.session_rpc(
		"record_client_ip", token, {})
	assert_eq(ip.status_code, 200, "record_client_ip failed")

	# A party chat message, then a match with the friend: match
	# history, recent players, leaderboard records, and a stored
	# match_ready notification.
	var connection: Dictionary = await _sock_helper.connect_users(
		[player, friend])
	if not connection.ok:
		fail_test(str(connection.reason))
		return
	var sockets: Array = connection.sockets
	var chat_text := "compliance export %s" % run
	await _send_party_chat(sockets[0], party_id, chat_text)
	var request_id := await _play_match(sockets, [player, friend])
	for sock in sockets:
		sock.close()
	assert_false(request_id.is_empty(), "the match did not finish")

	var result: Dictionary = await _helper.session_rpc(
		"export_player_data", token, null)
	assert_eq(
		result.status_code, 200,
		"export_player_data: status=%d" % result.status_code)
	if not (result.inner is Dictionary):
		fail_test("export payload is not an object")
		return
	var export: Dictionary = result.inner

	# Account: profile, email, every sign-in id, friend code.
	var account: Dictionary = export.get("account", {})
	assert_eq(str(account.get("user_id", "")), str(player.user_id))
	assert_eq(str(account.get("display_name", "")), display_name)
	assert_eq(str(account.get("avatar_url", "")), avatar_url)
	assert_eq(
		str(account.get("email", "")), email, "export lacks the email")
	assert_eq(str(account.get("location", "")), "Compliance City")
	assert_eq(str(account.get("timezone", "")), "UTC")
	assert_eq(str(account.get("friend_code", "")), friend_code)
	assert_gt(int(account.get("created_at", 0)), 0, "no created_at")
	var sign_in: Dictionary = account.get("sign_in_ids", {})
	assert_has(
		sign_in.get("device_ids", []), str(player.device_id),
		"export lacks the device id")
	assert_eq(
		str(sign_in.get("custom_id", "")), custom_id,
		"export lacks the custom id")
	for provider in [
		"google", "facebook", "facebook_instant_game", "apple",
		"steam", "game_center",
	]:
		assert_true(
			sign_in.has(provider), "sign_in_ids lacks %s" % provider)

	# The stored IP. Its value is never printed.
	var last_ip: Variant = export.get("last_ip_address")
	assert_true(last_ip is Dictionary, "export lacks the stored IP")
	if last_ip is Dictionary:
		assert_false(
			str(last_ip.get("ip", "")).is_empty(), "stored IP is empty")
		assert_gt(int(last_ip.get("recorded_at", 0)), 0)

	# Friends and blocked players.
	assert_eq(
		_friend_state(export, str(friend.user_id)), "friend",
		"export lacks the friend")
	assert_eq(
		_friend_state(export, str(blocked.user_id)), "blocked",
		"export lacks the blocked player")

	# Party data.
	var group := _find(export.get("groups", []), "group_id", party_id)
	assert_false(group.is_empty(), "export lacks the party")
	if not group.is_empty():
		assert_true(bool(group.get("is_party", false)), "not a party")
		assert_true(bool(group.get("is_creator", false)), "not creator")
		assert_eq(str(group.get("party_game_mode", "")), "ffa")
		assert_eq(
			str(group.get("party_leader_id", "")), str(player.user_id))

	# Storage: settings, presence, the IP record, match history,
	# recent players.
	var game_id: String = _helper.game_id()
	for expected in [
		["settings", "global"],
		["presence", game_id + "/current"],
		["client_ip", "latest"],
		["match_history", request_id],
		["recent_players", str(friend.user_id)],
	]:
		assert_true(
			_has_storage(export, expected[0], expected[1]),
			"export lacks storage %s/%s" % [expected[0], expected[1]])

	# Leaderboard records: whatever Nakama's own API reports for the
	# player on the game's board must be in the export. (The local
	# stack has no board unless a runtime creates one; then both are
	# empty.)
	await _assert_leaderboard_records_exported(
		export, token, str(player.user_id), game_id + "_ffa")

	# Notifications: the friend acceptance and the match_ready.
	assert_true(
		_has_notification(export, "code", _NOTIFICATION_FRIEND_ACCEPT),
		"export lacks the friend-accept notification")
	assert_true(
		_has_notification(export, "subject", "match_ready"),
		"export lacks the match_ready notification")

	# The party chat message.
	assert_true(
		_has_chat_message(export, party_id, chat_text),
		"export lacks the party chat message")

	var deletion: Dictionary = export.get("account_deletion", {})
	assert_false(
		bool(deletion.get("pending", true)), "deletion reported pending")
	assert_true(
		export.get("wallet_ledger") is Array, "wallet_ledger missing")


## Returns the player's friend code, generating one ("" on failure).
func _friend_code(token: String) -> String:
	var result: Dictionary = await _helper.session_rpc(
		"get_friend_code", token, null)
	if result.status_code != 200 or not (result.inner is Dictionary):
		return ""
	return str(result.inner.get("friend_code", ""))


## Joins the party's group chat on `sock` and sends one message.
func _send_party_chat(sock: Variant, party_id: String, text: String) -> void:
	var channel: Variant = await sock.join_chat_async(
		party_id, _CHANNEL_TYPE_GROUP, true, false)
	if channel == null or channel.is_exception():
		fail_test("join party chat failed")
		return
	var ack: Variant = await sock.write_chat_message_async(
		channel.id, {"text": text})
	if ack == null or ack.is_exception():
		fail_test("send party chat failed")


## Matches the two users against each other on the mock allocator,
## then registers a server and ends the match with both of them, as
## a game server would. Returns the match's request_id, or "".
func _play_match(sockets: Array, users: Array) -> String:
	var captures: Array = []
	for sock in sockets:
		var capture := _SocketHelper.NotificationCapture.new()
		capture.attach(sock)
		captures.append(capture)
	var run_id := "export%dx%d" % [
		Time.get_unix_time_from_system() as int, randi() % 100000]
	var props := {
		"platform": "native",
		"player_count": "1",
		"game_id": _helper.game_id(),
		"client_protocol_version": _protocol_version(),
		"game_mode": "ffa",
		"compliance_run": run_id,
	}
	for sock in sockets:
		var ticket: Variant = await sock.add_matchmaker_async(
			"+properties.compliance_run:" + run_id, 2, 2, props, {})
		if ticket == null or ticket.is_exception():
			for capture in captures:
				capture.detach()
			return ""
	var is_settled := func() -> bool:
		for capture in captures:
			if not capture.match_failed.is_empty():
				return true
			if capture.match_ready.is_empty():
				return false
		return true
	await _sock_helper.wait_until(is_settled, _MATCH_TIMEOUT_SEC)
	for capture in captures:
		capture.detach()
	var request_id := str(captures[0].match_ready.get("request_id", ""))
	if request_id.is_empty():
		return ""
	var registered: Dictionary = await _helper.http_key_rpc(
		"register_server",
		{
			"request_id": request_id,
			"server_ip": "203.0.113.7",
			"server_port": 4433,
		})
	if registered.status_code != 200:
		return ""
	var players: Array = []
	for user in users:
		players.append({
			"user_id": str(user.user_id),
			"score": 3,
			"kills": 1,
			"bumps": 2,
		})
	var ended: Dictionary = await _helper.http_key_rpc(
		"match_end",
		{
			"request_id": request_id,
			"winner_id": str(users[0].user_id),
			"players": players,
		})
	if ended.status_code != 200:
		return ""
	return request_id


func _protocol_version() -> String:
	if not ProjectSettings.has_setting(
			"application/config/protocol_version"):
		return "0"
	return str(int(ProjectSettings.get_setting(
		"application/config/protocol_version")))


func _assert_leaderboard_records_exported(
	export: Dictionary,
	token: String,
	user_id: String,
	board: String,
) -> void:
	var exported: Array = export.get("leaderboard_records", [])
	assert_true(
		exported is Array, "leaderboard_records missing")
	var listing: Dictionary = await _helper.http_get(
		"/v2/leaderboard/%s?owner_ids=%s&limit=1" % [board, user_id],
		"bearer:" + token)
	if listing.status_code != 200 or not (listing.body is Dictionary):
		return
	for record in listing.body.get("owner_records", []):
		var found := _find(exported, "leaderboard_id", board)
		assert_false(
			found.is_empty(),
			"export lacks the record on %s" % board)
		if not found.is_empty():
			# Nakama's REST API sends int64 scores as strings; the
			# export sends a number, which Godot parses as a float.
			assert_eq(
				int(found.get("score", -1)), int(record.get("score", -2)),
				"exported score differs from Nakama's listing")


func _friend_state(export: Dictionary, user_id: String) -> String:
	var entry := _find(export.get("friends", []), "user_id", user_id)
	return str(entry.get("state_name", ""))


## True when a stored notification's `field` equals `value`.
func _has_notification(
	export: Dictionary,
	field: String,
	value: Variant,
) -> bool:
	for entry in export.get("notifications", []):
		if entry is Dictionary and entry.get(field) == value:
			return true
	return false


## True when the export holds `text` sent in the party's group chat.
func _has_chat_message(
	export: Dictionary,
	party_id: String,
	text: String,
) -> bool:
	for message in export.get("chat_messages", []):
		if not (message is Dictionary):
			continue
		var content: Variant = message.get("content")
		if (
			str(message.get("channel_type", "")) == "group"
			and str(message.get("channel_target", "")) == party_id
			and content is Dictionary
			and str(content.get("text", "")) == text
		):
			return true
	return false


func _has_storage(
	export: Dictionary,
	collection: String,
	key: String,
) -> bool:
	for obj in export.get("storage_objects", []):
		if (
			obj is Dictionary
			and str(obj.get("collection", "")) == collection
			and str(obj.get("key", "")) == key
		):
			return true
	return false


## The first dictionary in `entries` whose `field` equals `value`,
## or {}.
func _find(entries: Variant, field: String, value: String) -> Dictionary:
	if not (entries is Array):
		return {}
	for entry in entries:
		if entry is Dictionary and str(entry.get(field, "")) == value:
			return entry
	return {}
