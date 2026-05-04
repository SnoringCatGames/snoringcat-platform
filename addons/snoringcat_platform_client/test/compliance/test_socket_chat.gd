extends GutTest
## Socket chat: join a room channel, send a message, receive
## it back. Single-user echo since Nakama delivers your own
## messages back via the same `received_channel_message`
## signal that drives the SDK's chat UI.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"
## ChannelType.ROOM = 1 in NakamaSocket.ChannelType. Hard-coded
## here to avoid pulling the type through the addon-internal
## NakamaRTMessage class.
const _CHANNEL_TYPE_ROOM := 1

var _helper
var _sock_helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


func test_chat_send_then_receive_roundtrip() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	var session: Variant = _sock_helper.session_from_token(token)
	if session == null:
		pending("Nakama SDK not available")
		return
	var sock: Variant = _sock_helper.create_socket()
	if sock == null:
		pending("Nakama autoload not registered")
		return
	var connected: bool = await _sock_helper.connect_with_timeout(
		sock, session, 5.0)
	if not connected:
		pending("socket would not connect")
		return

	# Join an ephemeral room. Use a per-run name so messages
	# don't bleed across runs.
	var room_name := (
		"compliance-chat-"
		+ str(Time.get_unix_time_from_system() as int))
	var channel = await sock.join_chat_async(
		room_name, _CHANNEL_TYPE_ROOM, false, false)
	assert_not_null(channel, "join_chat returned null")
	if channel == null:
		sock.close()
		return
	if channel.has_method("is_exception"):
		assert_false(
			channel.is_exception(),
			"join_chat raised: %s" % str(channel))

	var channel_id: String = str(channel.get("id", ""))
	assert_false(
		channel_id.is_empty(), "no channel id from join")

	# Send a message + wait for the echo. A short timeout
	# (3s) is enough — Nakama echoes within a tick or two on
	# a healthy socket.
	var marker := "compliance-marker-%d" % (
		Time.get_unix_time_from_system() as int)
	var send_promise = sock.write_chat_message_async(
		channel_id, {"message": marker})

	var got: Variant = await _sock_helper.wait_for_signal_with_timeout(
		sock, "received_channel_message", 3.0)
	# Drain the send result so it doesn't dangle.
	await send_promise

	assert_not_null(
		got,
		"received_channel_message did not fire within 3s")
	if got != null:
		# `got` is a NakamaAPI.ApiChannelMessage.
		var content_str: String = str(got.get("content", ""))
		assert_true(
			content_str.contains(marker),
			"echoed message body did not contain marker"
				+ "; got=%s" % content_str)

	# Cleanup: leave the channel + close the socket.
	await sock.leave_chat_async(channel_id)
	sock.close()
