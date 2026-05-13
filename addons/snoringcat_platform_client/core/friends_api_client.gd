class_name PlatformFriendsApiClient
extends Node
## Nakama-backed friends client. Wraps Nakama's friends API
## (list/add/delete) plus a few custom RPCs for friend-code
## lookup and notifications-mark-seen.
##
## Presence write/read lives in PlatformPresenceApiClient.
##
## Reads `Platform.nakama_client` and `Platform.token_store`.
## Both must be populated before any RPC method is called (the
## consuming game's bootstrap ensures this by eagerly invoking
## auth_client._get_nakama_client() before registering this
## subsystem).


signal friends_received(data: Dictionary)
signal friend_request_sent(data: Dictionary)
signal friend_request_accepted(data: Dictionary)
signal friend_request_rejected(data: Dictionary)
signal friend_request_cancelled(data: Dictionary)
signal friend_removed(data: Dictionary)
signal friend_search_result(data: Dictionary)
signal notifications_received(data: Dictionary)
signal friends_marked_seen(data: Dictionary)
signal request_failed(error: String)


# Nakama Friend states (from the SDK API):
#   0=Friend, 1=PendingInvite, 2=PendingApproval, 3=Banned
const _STATE_FRIEND := 0
const _STATE_PENDING_OUTGOING := 1
const _STATE_PENDING_INCOMING := 2
const _STATE_BANNED := 3

# Pagination caps for fetch_friends. Matches the runtime
# account.go cascade pattern (10 pages × 100 = 1000 entries) so a
# pathologically-large friend list stays bounded without an
# unbounded loop. Real player lists are << 100 in current
# deployment; this only matters for future high-cap accounts.
const _FRIENDS_PAGE_SIZE := 100
const _FRIENDS_PAGE_CAP := 10


var cached_friends: Array[Dictionary] = []
var cached_sent_requests: Array[Dictionary] = []
var cached_incoming_requests: Array[Dictionary] = []

var _is_busy := false
var _is_poll_busy := false


func fetch_friends() -> void:
	if _is_busy:
		return
	_is_busy = true
	var session := _ensure_session()
	if session == null:
		_is_busy = false
		return
	var next_friends: Array[Dictionary] = []
	var next_sent: Array[Dictionary] = []
	var next_incoming: Array[Dictionary] = []
	var cursor: String = ""
	for page in _FRIENDS_PAGE_CAP:
		var result = await Platform.nakama_client.list_friends_async(
			session, null, _FRIENDS_PAGE_SIZE, cursor)
		if result.is_exception():
			_is_busy = false
			request_failed.emit(_describe(result.get_exception()))
			return
		for f in result.friends:
			var entry := {
				"player_id": f.user.id,
				"display_name": f.user.display_name,
				"username": f.user.username,
				"avatar_url": f.user.avatar_url,
				"online": f.user.online,
			}
			match f.state:
				_STATE_FRIEND:
					next_friends.append(entry)
				_STATE_PENDING_OUTGOING:
					next_sent.append(entry)
				_STATE_PENDING_INCOMING:
					next_incoming.append(entry)
		cursor = result.cursor if result.cursor != null else ""
		if cursor.is_empty():
			break
	_is_busy = false
	cached_friends = next_friends
	cached_sent_requests = next_sent
	cached_incoming_requests = next_incoming
	friends_received.emit({
		"friends": cached_friends,
		"sent_requests": cached_sent_requests,
		"incoming_requests": cached_incoming_requests,
	})


func send_request_by_code(code: String) -> void:
	# Friend codes are stored as Nakama usernames in this
	# project (same uniqueness, simpler lookup).
	var session := _ensure_session()
	if session == null:
		return
	var result = await Platform.nakama_client.add_friends_async(
		session, null, [code])
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	friend_request_sent.emit({"code": code})


func send_request_by_player_id(
	player_id: String,
	source: String = "recent_match",
) -> void:
	# `source` was an analytics tag in the legacy AWS path
	# (recent_match, friend_code_search, etc.). Nakama's
	# add_friends_async doesn't carry metadata, so we just
	# log it for telemetry and pass through.
	var session := _ensure_session()
	if session == null:
		return
	var result = await Platform.nakama_client.add_friends_async(
		session, [player_id], null)
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	friend_request_sent.emit({
		"player_id": player_id,
		"source": source,
	})


func accept_request(player_id: String) -> void:
	# Nakama: re-issuing add_friends_async on a pending-incoming
	# accepts the friendship.
	var session := _ensure_session()
	if session == null:
		return
	var result = await Platform.nakama_client.add_friends_async(
		session, [player_id], null)
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	friend_request_accepted.emit({"player_id": player_id})


func reject_request(player_id: String) -> void:
	await _delete_friend(player_id, "rejected")


func cancel_request(player_id: String) -> void:
	await _delete_friend(player_id, "cancelled")


func remove_friend(player_id: String) -> void:
	await _delete_friend(player_id, "removed")


func search_friend_code(code: String) -> void:
	var session := _ensure_session()
	if session == null:
		return
	var result = await Platform.nakama_client.get_users_async(
		session, PackedStringArray(), [code], null)
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	if result.users.size() == 0:
		friend_search_result.emit({"code": code, "found": false})
		return
	var u = result.users[0]
	friend_search_result.emit({
		"code": code,
		"found": true,
		"player_id": u.id,
		"display_name": u.display_name,
		"avatar_url": u.avatar_url,
	})


func mark_seen() -> void:
	# Custom RPC on the runtime side. Bumps last_friends_seen_at.
	var session := _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.nakama_client.rpc_async(
		session, "mark_friends_seen", "{}")
	if rpc_result.is_exception():
		# RPC missing on older deploys: silent fail.
		friends_marked_seen.emit({"ok": false})
		return
	friends_marked_seen.emit({"ok": true})


func fetch_notifications(
	limit: int = 50,
	cacheable_cursor: String = "",
) -> void:
	if _is_poll_busy:
		return
	_is_poll_busy = true
	var session := _ensure_session()
	if session == null:
		_is_poll_busy = false
		return
	var result = await Platform.nakama_client.list_notifications_async(
		session, limit, cacheable_cursor)
	_is_poll_busy = false
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	var entries := []
	for n in result.notifications:
		entries.append({
			"id": n.id,
			"subject": n.subject,
			"content": JSON.parse_string(n.content) \
				if not n.content.is_empty() else {},
			"sender_id": n.sender_id,
			"create_time": n.create_time,
			"persistent": n.persistent,
		})
	notifications_received.emit({
		"notifications": entries,
		"cacheable_cursor": result.cacheable_cursor,
	})


# --------------------------------------------------------------
# Status
# --------------------------------------------------------------

func is_busy() -> bool: return _is_busy
func is_poll_busy() -> bool: return _is_poll_busy


func is_friend(player_id: String) -> bool:
	for f in cached_friends:
		if f.get("player_id", "") == player_id:
			return true
	return false


func has_sent_request(player_id: String) -> bool:
	for f in cached_sent_requests:
		if f.get("player_id", "") == player_id:
			return true
	return false


func has_incoming_request(player_id: String) -> bool:
	for f in cached_incoming_requests:
		if f.get("player_id", "") == player_id:
			return true
	return false


# --------------------------------------------------------------
# Internals
# --------------------------------------------------------------

func _delete_friend(player_id: String, kind: String) -> void:
	var session := _ensure_session()
	if session == null:
		return
	var result = await Platform.nakama_client.delete_friends_async(
		session, [player_id], null)
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	match kind:
		"rejected":
			friend_request_rejected.emit({"player_id": player_id})
		"cancelled":
			friend_request_cancelled.emit({"player_id": player_id})
		"removed":
			friend_removed.emit({"player_id": player_id})


func _ensure_session() -> NakamaSession:
	var s := Platform.build_session_from_store()
	if s == null:
		request_failed.emit("Not authenticated")
		return null
	return s


func _describe(ex: NakamaException) -> String:
	if ex == null:
		return "Unknown Nakama error"
	return "%s (status=%d)" % [ex.message, ex.status_code]
