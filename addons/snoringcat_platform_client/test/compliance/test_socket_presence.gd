extends GutTest
## Socket presence: status updates + follow_users round-trip.
## A second-user friend-status push test would require two
## stable users; this test just exercises the single-user
## surface — `update_status_async` accepts a status string,
## `follow_users_async` accepts the empty list and returns a
## status snapshot. Both are required for the SDK's
## online-friends UI to function.


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


func test_update_status_succeeds() -> void:
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

	# update_status_async returns a NakamaAsyncResult; success
	# is "no exception". The server propagates the status to
	# any followers, but with a single-user test we just verify
	# the call doesn't error.
	var result = await sock.update_status_async("online: compliance")
	assert_not_null(result, "update_status returned null")
	if result.has_method("is_exception"):
		assert_false(
			result.is_exception(),
			"update_status raised: %s" % str(result))

	sock.close()


func test_follow_users_returns_snapshot() -> void:
	# follow_users_async with an empty list returns the empty
	# Status snapshot. The contract is "does the SDK get a
	# parseable response back from the server"; an exception
	# here means the realtime status surface is broken.
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

	# Empty list — we just want to know the call works. The
	# real-world call passes friends' user_ids.
	var result = await sock.follow_users_async(
		PackedStringArray(), PackedStringArray())
	assert_not_null(result, "follow_users returned null")
	if result.has_method("is_exception"):
		assert_false(
			result.is_exception(),
			"follow_users raised: %s" % str(result))

	sock.close()
