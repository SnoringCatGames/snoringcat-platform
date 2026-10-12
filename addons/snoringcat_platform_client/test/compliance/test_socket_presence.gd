extends GutTest
## Socket presence: status updates + follow_users round-trip.
## `update_status_async` accepts a status string, and
## `follow_users_async` returns a snapshot that includes a
## followed user who is online.
##
## Neither the platform SDK nor hopnbop calls these today. The
## online-friends UI polls the `update_and_get_presence` RPC
## (test_presence*.gd). This file keeps the realtime status
## surface honest for whoever builds on it next.
##
## The follow target must be online. When none of the followed
## users are, Nakama omits the empty `presences` field, and the
## vendored Nakama Godot client's Status schema marks it
## required, so follow_users_async returns an exception instead
## of an empty snapshot.


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
## One-shot users to hard-delete in after_each.
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
	# A follows B while B's socket is online. The snapshot must
	# parse and list B's presence; an exception or a missing B
	# means the realtime status surface is broken.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	var follower: Dictionary = await _sock_helper.connect_users(
		[_users[0]])
	var target: Dictionary = await _sock_helper.connect_users(
		[_users[1]], true)
	if not follower.ok or not target.ok:
		for connection in [follower, target]:
			for sock in connection.sockets:
				sock.close()
		pending(str(follower.reason) + str(target.reason))
		return
	var sock: Variant = follower.sockets[0]

	var target_id := str(_users[1].user_id)
	var result = await sock.follow_users_async(
		PackedStringArray([target_id]), PackedStringArray())
	assert_not_null(result, "follow_users returned null")
	assert_false(
		result.is_exception(), "follow_users raised: %s" % str(result))
	if not result.is_exception():
		var followed_ids: Array[String] = []
		for presence in result.presences:
			followed_ids.append(str(presence.user_id))
		assert_has(
			followed_ids, target_id,
			"follow_users snapshot should list the online target")

	sock.close()
	target.sockets[0].close()
