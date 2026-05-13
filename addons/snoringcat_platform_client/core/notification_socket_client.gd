class_name PlatformNotificationSocketClient
extends Node
## Long-lived Nakama realtime socket for receiving persistent and
## transient notifications. Opens on authentication (non-anonymous)
## and stays connected for the life of the session; reconnects with
## exponential backoff if dropped. Consumers connect to
## `notification_received` and filter by subject — there's no
## subscription registry to keep in sync.
##
## Reads `Platform.auth`, `Platform.token_store`,
## `Platform.build_session_from_store()`, and
## `Platform.get_nakama_client()`. Game code reads this subsystem
## via `Platform.notification_socket`.


## Fires once per notification delivered over the socket. Content is
## the parsed JSON body; an empty dict means the payload wasn't a
## JSON object (in which case consumers should ignore it).
signal notification_received(
	subject: String,
	content: Dictionary,
	notification_id: String,
)

## Fires when the socket transitions to connected (initial connect
## or successful reconnect). Consumers that maintain local state
## from persistent server state should refetch on this so they
## catch up on any events missed during the down window.
signal socket_connected

## Fires when the socket closes for any reason. The reconnect logic
## kicks in automatically afterward when start() was called and
## the auth token is valid.
signal socket_disconnected

## Fires once per chat message delivered over the socket. The
## payload is a flat dict shaped from ApiChannelMessage:
##   {channel_id, group_id, sender_id, username, content,
##    create_time, message_id, persistent}
## Consumers filter by `channel_id` / `group_id` so they only react
## to their own channel.
signal received_channel_message(message: Dictionary)


## Nakama channel-type enum. Mirrors the upstream
## ChannelJoin.ChannelType: 1=Room, 2=DirectMessage, 3=Group.
const CHANNEL_TYPE_GROUP := 3


const _RECONNECT_INITIAL_DELAY_SEC := 1.0
const _RECONNECT_MAX_DELAY_SEC := 30.0
const _RECONNECT_BACKOFF_MULTIPLIER := 2.0


var _socket: NakamaSocket = null
var _wants_connection := false
var _is_connecting := false
var _reconnect_delay_sec := _RECONNECT_INITIAL_DELAY_SEC
var _reconnect_timer: Timer = null


func _ready() -> void:
	process_mode = Node.PROCESS_MODE_ALWAYS

	_reconnect_timer = Timer.new()
	_reconnect_timer.name = "ReconnectTimer"
	_reconnect_timer.one_shot = true
	_reconnect_timer.timeout.connect(_attempt_connect)
	add_child(_reconnect_timer)

	Platform.auth.auth_completed.connect(_on_auth_completed)


## Whether the socket is currently connected to Nakama.
func is_socket_connected() -> bool:
	return (
		_socket != null
		and _socket.is_connected_to_host()
	)


## Open the socket and keep it open with reconnect on drop. Safe to
## call multiple times; no-ops if already connecting/connected.
func start() -> void:
	_wants_connection = true
	if _is_connecting:
		return
	if is_socket_connected():
		return
	_reconnect_delay_sec = _RECONNECT_INITIAL_DELAY_SEC
	_attempt_connect()


## Close the socket and stop reconnecting. Called on logout.
func stop() -> void:
	_wants_connection = false
	_reconnect_timer.stop()
	_reconnect_delay_sec = _RECONNECT_INITIAL_DELAY_SEC
	if _socket != null:
		_socket.close()
		_socket = null


# --------------------------------------------------------------
# Internals
# --------------------------------------------------------------


func _attempt_connect() -> void:
	if not _wants_connection:
		return
	if _is_connecting:
		return
	if is_socket_connected():
		return
	# Don't attempt when there's no valid token; defer until
	# _on_auth_completed fires again.
	if not Platform.token_store.is_token_valid():
		return
	# Anonymous users don't have a Nakama JWT we can authenticate
	# the socket with. Skip silently; the socket re-evaluates on
	# the next auth_completed.
	if Platform.token_store.is_anonymous:
		return

	var session: NakamaSession = (
		Platform.build_session_from_store())
	if session == null:
		_schedule_reconnect()
		return

	_is_connecting = true
	_socket = Nakama.create_socket_from(
		Platform.get_nakama_client())
	_socket.received_notification.connect(
		_on_received_notification)
	_socket.received_channel_message.connect(
		_on_received_channel_message)
	_socket.closed.connect(_on_socket_closed)
	_socket.connection_error.connect(
		_on_socket_connection_error)

	var result: NakamaAsyncResult = (
		await _socket.connect_async(session))
	_is_connecting = false

	if result.is_exception():
		var ex: NakamaException = result.get_exception()
		push_warning(
			"[NotificationSocket] connect failed: %s"
			% ex.message)
		# Drop the failed socket so a future _attempt_connect
		# doesn't see is_connected_to_host()=false on a stale
		# handle and short-circuit.
		_socket = null
		_schedule_reconnect()
		return

	_reconnect_delay_sec = _RECONNECT_INITIAL_DELAY_SEC
	print("[NotificationSocket] connected")
	socket_connected.emit()


func _schedule_reconnect() -> void:
	if not _wants_connection:
		return
	_reconnect_timer.start(_reconnect_delay_sec)
	_reconnect_delay_sec = min(
		_reconnect_delay_sec * _RECONNECT_BACKOFF_MULTIPLIER,
		_RECONNECT_MAX_DELAY_SEC,
	)


## Join the Nakama group channel for the given party / group id.
## Returns the channel id on success, "" on failure. The socket
## must already be connected — call after `socket_connected` has
## fired. Persistent=true so the server retains message history;
## hidden=false so other members see the join presence event.
func join_chat_group(group_id: String) -> String:
	if group_id.is_empty():
		return ""
	if not is_socket_connected():
		return ""
	var result = await _socket.join_chat_async(
		group_id, CHANNEL_TYPE_GROUP, true, false)
	if result.is_exception():
		push_warning(
			(
				"[NotificationSocket] join chat failed:"
				+ " group=%s err=%s"
			) % [group_id, result.get_exception().message])
		return ""
	return result.id


## Leave the named chat channel. Best-effort; failures are
## logged but not bubbled because the caller no longer wants the
## subscription anyway.
func leave_chat(channel_id: String) -> void:
	if channel_id.is_empty():
		return
	if not is_socket_connected():
		return
	var result = await _socket.leave_chat_async(channel_id)
	if result.is_exception():
		push_warning(
			(
				"[NotificationSocket] leave chat failed:"
				+ " channel=%s err=%s"
			) % [channel_id, result.get_exception().message])


## Send a chat message dict on the given channel. Nakama serializes
## the dict to JSON on the wire; we wrap a `{text}` key so future
## additions (e.g. system messages, reactions) can ride alongside
## without breaking the contract.
func send_chat_message(
	channel_id: String,
	content: Dictionary,
) -> bool:
	if channel_id.is_empty():
		return false
	if not is_socket_connected():
		return false
	var result = await _socket.write_chat_message_async(
		channel_id, content)
	if result.is_exception():
		push_warning(
			(
				"[NotificationSocket] send chat failed:"
				+ " channel=%s err=%s"
			) % [channel_id, result.get_exception().message])
		return false
	return true


func _on_received_channel_message(p_message) -> void:
	# Flatten the Nakama ApiChannelMessage into a plain dict so
	# downstream consumers don't have to depend on the SDK type.
	# `content` is a JSON string on the wire; parse it if possible
	# and pass both the raw string and the parsed dict (consumers
	# typically care about the parsed shape).
	var raw_content: String = str(p_message.content)
	var parsed_content: Dictionary = {}
	if not raw_content.is_empty():
		var parsed: Variant = JSON.parse_string(raw_content)
		if parsed is Dictionary:
			parsed_content = parsed
	received_channel_message.emit({
		"channel_id": str(p_message.channel_id),
		"group_id": str(p_message.group_id),
		"sender_id": str(p_message.sender_id),
		"username": str(p_message.username),
		"content_raw": raw_content,
		"content": parsed_content,
		"create_time": str(p_message.create_time),
		"message_id": str(p_message.message_id),
		"persistent": bool(p_message.persistent),
	})


func _on_received_notification(p_notification) -> void:
	var subject: String = str(p_notification.subject)
	var raw_id: String = str(p_notification.id)
	var content_dict: Dictionary = {}
	# Nakama wraps the notification content as a JSON string. Empty
	# string is legal (notifications without bodies); parse_string
	# returns null on empty input.
	var raw_content: String = str(p_notification.content)
	if not raw_content.is_empty():
		var parsed: Variant = JSON.parse_string(raw_content)
		if parsed is Dictionary:
			content_dict = parsed
	notification_received.emit(
		subject, content_dict, raw_id)


func _on_socket_closed() -> void:
	print("[NotificationSocket] closed")
	socket_disconnected.emit()
	if _wants_connection:
		_schedule_reconnect()


func _on_socket_connection_error(error) -> void:
	# _on_socket_closed typically fires too; reconnect there.
	push_warning(
		"[NotificationSocket] connection error: %s" % error)


func _on_auth_completed(
	success: bool, _error: String,
) -> void:
	if not success:
		stop()
		return
	if not Platform.token_store.is_token_valid():
		stop()
		return
	if Platform.token_store.is_anonymous:
		# Anonymous users don't have a Nakama JWT; the platform-
		# level features that rely on this socket (party state,
		# friend notifications) all require a non-anonymous
		# account anyway.
		stop()
		return
	start()
