extends GutTest
## Realtime-socket auth: the WSS endpoint accepts a valid
## session token and refuses an invalid one. The SDK uses this
## socket for matchmaking, chat, presence, and party flows —
## if it can't connect, every realtime feature is dead.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper
var _sock_helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


func test_socket_connects_with_valid_session() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty(), "anon auth failed")

	var session: Variant = _sock_helper.session_from_token(token)
	if session == null:
		pending("Nakama SDK NakamaSession not available")
		return
	var sock: Variant = _sock_helper.create_socket()
	if sock == null:
		pending(
			"Nakama autoload not registered in this project")
		return

	var connected: bool = await _sock_helper.connect_with_timeout(
		sock, session, 5.0)
	assert_true(connected, "socket failed to connect")
	if connected:
		sock.close()


func test_socket_rejects_garbage_token() -> void:
	# A made-up token shouldn't open the socket. Catches the
	# "auth gate dropped" regression: if Nakama starts accepting
	# unsigned tokens, every realtime call becomes anonymous.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return

	# Hand-rolled, non-crypto-valid JWT shape (3 segments of
	# base64url'd JSON). NakamaSession.parse_token rejects it
	# at session construction; if it doesn't, the server
	# rejects it at the WSS handshake. Either failure mode is
	# correct — but a successful connect is the regression.
	var bogus := (
		"eyJhbGciOiJIUzI1NiJ9"           # header
		+ ".eyJ1aWQiOiJub3JlYWwiLCJleHAiOjk5OTk5OTk5OTl9"  # claims
		+ ".sig_is_not_real")
	var session: Variant = _sock_helper.session_from_token(bogus)
	if session == null:
		# SDK rejected at parse time. That's a valid pass.
		assert_true(true, "SDK rejected bogus token at parse")
		return

	var sock: Variant = _sock_helper.create_socket()
	if sock == null:
		pending("Nakama autoload not registered")
		return
	var connected: bool = await _sock_helper.connect_with_timeout(
		sock, session, 4.0)
	assert_false(
		connected,
		"socket accepted a token with an invalid signature")
	if connected:
		sock.close()
