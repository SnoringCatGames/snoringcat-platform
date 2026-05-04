extends GutTest
## Socket matchmaker surface: add a ticket, get a ticket id
## back, remove it. We deliberately use min_count=1 so the
## ticket fires `matchmaker_matched` immediately, exercising
## the runtime hook (which would catch transport_type and
## allocation regressions if we asserted on the resulting
## payload — that's deferred to a separate test once §3 of the
## test architecture plan ships).


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

	# min_count=1 / max_count=1 so the matchmaker fires
	# immediately. Use min/max=1 deliberately even though the
	# game's real flow is 2-4: the test goal is "is the ticket
	# accepted and does the runtime hook fire", not "match real
	# players". The matchmaker_matched event might cause an
	# Edgegap allocation — but with min=max=1 the allocator is
	# expected to no-op or fail-fast since 1 player isn't a
	# real match. We tolerate either; the assertion is on
	# ticket creation, not on what happens after.
	var ticket = await sock.add_matchmaker_async(
		"*", 1, 1,
		{"platform": "native", "player_count": "1"},
		{},
		0)
	assert_not_null(ticket, "add_matchmaker_async returned null")
	if ticket != null:
		# NakamaRTAPI.MatchmakerTicket has a `ticket` string
		# field. (Field name differs across SDK versions; use
		# get() to be defensive.)
		var ticket_str: Variant = ticket.get("ticket")
		if ticket_str == null:
			ticket_str = ticket.get("ticket_id")
		assert_true(
			ticket_str is String and not (ticket_str as String).is_empty(),
			"ticket id missing on response: %s" % str(ticket))

		# Cancel so we don't leave a dangling ticket. Best-effort
		# — the socket close on its own would also drop it.
		if ticket_str is String:
			await sock.remove_matchmaker_async(ticket_str)

	sock.close()
