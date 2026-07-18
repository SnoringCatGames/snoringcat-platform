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
signal notifications_received(data: Dictionary)
signal friends_marked_seen(data: Dictionary)
signal request_failed(error: String)
signal user_blocked(data: Dictionary)
signal user_unblocked(data: Dictionary)
signal blocked_users_received(data: Dictionary)
signal recent_players_received(data: Dictionary)


# Nakama built-in notification codes. Nakama reserves negative
# codes for its own notifications and leaves non-negative ones to
# the application (our runtime uses 100 for match_ready /
# party_matchmaking_start and 101 for party_state_changed).
#
# These two are the entire friend surface Nakama emits. Note there
# is deliberately no "request rejected" code: Nakama models a
# rejection as a silent row delete, so a rejected requester is
# never told. Don't add a REJECTED constant expecting it to fire.
const CODE_FRIEND_REQUEST_RECEIVED := -2
const CODE_FRIEND_REQUEST_ACCEPTED := -3

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
# Stage 7.4: state=3 (BANNED) entries. Populated by
# fetch_blocked_users via the list_blocked_users RPC. Nakama
# rejects friend-add calls in either direction when the target
# is in the caller's BANNED list (or vice versa), so the field
# also acts as a local "should I show the Block button as
# Unblock?" cache for FriendDetailsPanel.
var cached_blocked_users: Array[Dictionary] = []
# Stage 7.6: recent opponents from real (non-synthetic) matches.
# Populated by fetch_recent_players via the list_recent_players
# RPC. The "Add Friend" surface in the UI iterates this list and
# filters out anyone already in cached_friends / pending request
# caches.
var cached_recent_players: Array[Dictionary] = []

var _is_busy := false
var _is_poll_busy := false
var _is_blocked_users_busy := false
var _is_recent_players_busy := false


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


## Send a friend request by friend code.
##
## Friend codes are generated per-account and are NOT the Nakama
## username (runtime/friend_code.go). Resolution happens server-side in
## the `add_friend_by_code` RPC, which rate-limits, resolves the code
## (falling back to a username lookup for old clients still sharing
## username-as-code), and adds by user id in one round trip.
##
## Emits `friend_request_sent` carrying the RPC result dict on success
## AND on a code that resolves to nobody (`result == "not_found"`); the
## consuming UI switches on `result` to show the right toast and decide
## whether to close. Only transport / rate-limit failures take the
## `request_failed` path.
func send_request_by_code(code: String) -> void:
	var session := _ensure_session()
	if session == null:
		return
	var result = await Platform.nakama_client.rpc_async(
		session, "add_friend_by_code",
		JSON.stringify({"code": code}))
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	var data: Variant = JSON.parse_string(result.payload)
	if data is Dictionary:
		friend_request_sent.emit(data)
	else:
		request_failed.emit("Unexpected response")


## Fetch (creating on first call) this account's stable friend code.
## Returns the 8-char code, or "" when the runtime predates the RPC or
## the call fails. Callers must treat "" as "unavailable" and not show
## a bogus code.
func get_my_friend_code() -> String:
	var session := _ensure_session()
	if session == null:
		return ""
	var result = await Platform.nakama_client.rpc_async(
		session, "get_friend_code", "{}")
	if result.is_exception():
		return ""
	var data: Variant = JSON.parse_string(result.payload)
	if data is Dictionary:
		return str(data.get("friend_code", ""))
	return ""


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


## Signal that the user has looked at their friends list.
##
## The `mark_friends_seen` RPC this calls is NOT currently
## registered by the runtime (main.go registers no such name), so
## in practice this always takes the exception path and emits
## ok=false. That is survivable because unseen-badge state is
## computed client-side from the pending-request list; the RPC
## exists for a future server-authoritative, cross-device
## "last seen" timestamp.
##
## The emit fires on both paths precisely so consumers can treat
## this as "user acknowledged the list" regardless of whether the
## server half exists yet. Don't make consumers branch on `ok`
## without also implementing the RPC.
func mark_seen() -> void:
	var session := _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.nakama_client.rpc_async(
		session, "mark_friends_seen", "{}")
	if rpc_result.is_exception():
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
			# `code` is the only stable discriminator for Nakama's
			# built-in notifications: their `subject` is a
			# human-readable sentence ("X wants to add you as a
			# friend") that is localized/reworded at Nakama's
			# discretion. Negative codes are Nakama's own (see
			# CODE_FRIEND_* below); positive codes are ours.
			"code": int(n.code),
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
# Block list (Stage 7.4)
# --------------------------------------------------------------
# Nakama stores blocked relationships as state=3 (BANNED) entries
# in the friends table. The runtime's block_user / unblock_user /
# list_blocked_users RPCs wrap the underlying nk.FriendsBlock /
# nk.FriendsDelete / nk.FriendsList(state=3) calls; we go through
# the RPCs (not the SDK's direct list_friends_async with a state
# filter) so the runtime can layer a consistent response shape
# and capture display names server-side.


func block_user(player_id: String, username: String = "") -> void:
	var session := _ensure_session()
	if session == null:
		return
	var payload := {}
	if not player_id.is_empty():
		payload["user_id"] = player_id
	if not username.is_empty():
		payload["username"] = username
	if payload.is_empty():
		request_failed.emit("block_user requires player_id or username")
		return
	var result = await Platform.nakama_client.rpc_async(
		session, "block_user", JSON.stringify(payload))
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	var data: Dictionary = JSON.parse_string(result.payload)
	# Drop any cached friend / pending rows for this user — the
	# server-side FriendsBlock deleted the relationship.
	if data.has("user_id") and not data["user_id"].is_empty():
		var blocked_id: String = data["user_id"]
		cached_friends = cached_friends.filter(
			func(f): return f.get("player_id", "") != blocked_id)
		cached_sent_requests = cached_sent_requests.filter(
			func(f): return f.get("player_id", "") != blocked_id)
		cached_incoming_requests = cached_incoming_requests.filter(
			func(f): return f.get("player_id", "") != blocked_id)
		# Insert into the cached blocked list if not already
		# present, so the UI updates without a round trip.
		var already := false
		for b in cached_blocked_users:
			if b.get("player_id", "") == blocked_id:
				already = true
				break
		if not already:
			cached_blocked_users.append({
				"player_id": blocked_id,
				"username": data.get("username", ""),
				"display_name": data.get("display_name", ""),
				"avatar_url": "",
			})
	user_blocked.emit(data)


func unblock_user(player_id: String) -> void:
	if player_id.is_empty():
		request_failed.emit("unblock_user requires player_id")
		return
	var session := _ensure_session()
	if session == null:
		return
	var result = await Platform.nakama_client.rpc_async(
		session, "unblock_user",
		JSON.stringify({"user_id": player_id}))
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	cached_blocked_users = cached_blocked_users.filter(
		func(b): return b.get("player_id", "") != player_id)
	user_unblocked.emit({"player_id": player_id})


func fetch_blocked_users() -> void:
	if _is_blocked_users_busy:
		return
	_is_blocked_users_busy = true
	var session := _ensure_session()
	if session == null:
		_is_blocked_users_busy = false
		return
	var result = await Platform.nakama_client.rpc_async(
		session, "list_blocked_users", "")
	_is_blocked_users_busy = false
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	var data: Dictionary = JSON.parse_string(result.payload)
	var raw: Array = data.get("blocked_users", [])
	var next: Array[Dictionary] = []
	for entry in raw:
		next.append({
			"player_id": entry.get("user_id", ""),
			"username": entry.get("username", ""),
			"display_name": entry.get("display_name", ""),
			"avatar_url": entry.get("avatar_url", ""),
		})
	cached_blocked_users = next
	blocked_users_received.emit({
		"blocked_users": cached_blocked_users,
		"truncated": data.get("truncated", false),
	})


# --------------------------------------------------------------
# Recent players (Stage 7.6)
# --------------------------------------------------------------

## Fetches the caller's recent-opponents list from the runtime
## (capped at the server's `recentPlayersCap`, currently 50).
## On success, populates `cached_recent_players` and emits
## `recent_players_received`. Each entry has shape
## `{player_id, username, display_name, matched_at}` where
## `matched_at` is the unix-second timestamp of the most recent
## match the user shared with the caller.
##
## Idempotent across concurrent calls — the second caller during
## an in-flight fetch is a no-op (mirrors fetch_blocked_users).
func fetch_recent_players() -> void:
	if _is_recent_players_busy:
		return
	_is_recent_players_busy = true
	var session := _ensure_session()
	if session == null:
		_is_recent_players_busy = false
		return
	var result = await Platform.nakama_client.rpc_async(
		session, "list_recent_players", "")
	_is_recent_players_busy = false
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	var data: Dictionary = JSON.parse_string(result.payload)
	var raw: Array = data.get("recent_players", [])
	var next: Array[Dictionary] = []
	for entry in raw:
		next.append({
			"player_id": entry.get("user_id", ""),
			"username": entry.get("username", ""),
			"display_name": entry.get("display_name", ""),
			"matched_at": int(entry.get("matched_at", 0)),
		})
	cached_recent_players = next
	recent_players_received.emit({
		"recent_players": cached_recent_players,
	})


# --------------------------------------------------------------
# Status
# --------------------------------------------------------------

func is_busy() -> bool: return _is_busy
func is_poll_busy() -> bool: return _is_poll_busy
func is_blocked_users_busy() -> bool: return _is_blocked_users_busy
func is_recent_players_busy() -> bool: return _is_recent_players_busy


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


func is_blocked(player_id: String) -> bool:
	for b in cached_blocked_users:
		if b.get("player_id", "") == player_id:
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
