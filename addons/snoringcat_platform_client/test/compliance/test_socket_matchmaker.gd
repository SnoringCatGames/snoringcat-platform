extends GutTest
## Socket matchmaker surface: add a ticket, get a ticket id
## back, remove it.
##
## The ticket can never match. It needs two players and queries
## for a property only this run's ticket carries, so the
## matchmaker hook never fires and nothing is allocated. That
## keeps this test safe on any target. It used to queue a
## min=max=1 ticket on the assumption that the allocator no-ops
## for a single player. It doesn't: a solo match allocates a real
## deploy outside EDGEGAP_MOCK_DEPLOY. The hook's own paths are
## covered by the mock-gated test_matchmaking*.gd files.


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


func test_matchmaker_add_returns_ticket() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	var session: Variant = _sock_helper.session_from_token(token)
	if session == null:
		pending("Nakama SDK NakamaSession not available")
		return
	var sock: Variant = _sock_helper.create_socket()
	if sock == null:
		pending("Nakama autoload not registered")
		return
	var connected: bool = await _sock_helper.connect_with_timeout(
		sock, session, 5.0)
	if not connected:
		pending(
			"socket would not connect (covered by"
			+ " test_socket_auth)")
		return

	# min=max=2 plus a query on a property unique to this run: no
	# other ticket can satisfy it, so this one never matches. The
	# value stays alphanumeric so the query parser takes it as one
	# term.
	var probe_id := "probe%dx%d" % [
		Time.get_unix_time_from_system() as int,
		randi() % 100000,
	]
	var ticket = await sock.add_matchmaker_async(
		"+properties.compliance_probe:" + probe_id, 2, 2,
		{
			"platform": "native",
			"player_count": "1",
			"compliance_probe": probe_id,
		},
		{},
		0)
	assert_not_null(ticket, "add_matchmaker_async returned null")
	if ticket == null:
		sock.close()
		return
	assert_false(
		ticket.is_exception(),
		"add_matchmaker_async raised: %s" % str(ticket))
	# NakamaRTAPI.MatchmakerTicket carries the id in `ticket`.
	var ticket_id: String = ticket.ticket
	assert_false(
		ticket_id.is_empty(),
		"ticket id missing on response: %s" % str(ticket))

	if not ticket_id.is_empty():
		var removed: Variant = await sock.remove_matchmaker_async(
			ticket_id)
		assert_false(
			removed.is_exception(),
			"remove_matchmaker_async raised: %s" % str(removed))

	sock.close()
