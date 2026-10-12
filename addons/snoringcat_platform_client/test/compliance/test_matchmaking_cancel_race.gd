extends GutTest
## Matchmaker cancel paths over the Nakama realtime socket.
## Stage 8.20.
##
## Two scenarios, each with two one-shot users whose tickets are
## scoped to the test run, so they can only pair with each other.
## Nakama 3.25 rejects tickets with a minimum count below 2, so
## every ticket here is min=max=2.
##   1. test_cancel_before_match_prevents_match_ready:
##      user A adds a ticket and removes it, then user B adds a
##      compatible one. If the cancel had left A's ticket in the
##      pool, the two would pair. The test asserts that neither
##      side sees received_matchmaker_matched or match_ready in
##      the window, then runs a positive control: A queues again
##      and the pair must match. Without the control, a window
##      too short for any match would pass the first half
##      vacuously.
##   2. test_cancel_after_match_is_safe: A and B queue and
##      match, the runtime hook fans out match_ready, and THEN A
##      calls remove_matchmaker_async on its now-consumed ticket.
##      Confirms the call is safe (no crash, socket usable for a
##      follow-on add/remove pair afterward). Documents the
##      current Stage 7.2 limitation: post-match cancel is best-
##      effort, Edgegap deploy stays alive until the game
##      server's own grace timer fires.
##
## Gated on EDGEGAP_MOCK_DEPLOY=true (see compliance_helper's
## is_mock_deploy_mode). Both scenarios allocate; we never want
## CI to burn a real Edgegap container just to test the cancel
## codepath.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
## How long we wait after B queues before declaring "A's
## cancelled ticket did not pair with B". The dev and prod
## matchmakers tick every second, so 3 s spans several ticks.
## The positive control proves a real pairing fits in this
## window.
const _NO_MATCH_WAIT_SEC := 3.0
## How long we wait for match_ready in the post-match cancel
## scenario, and for the positive control's pairing. Same shape
## as test_matchmaking.gd's combined deadline.
const _MATCH_TIMEOUT_SEC := 8.0
const _NOTIFY_TIMEOUT_SEC := 5.0

var _helper
var _sock_helper
## One-shot users from multi_session_anon. Captured at the
## script scope so after_each can hard-delete regardless of
## which test ran.
var _users: Array = []
## Property value that scopes this test's tickets to each other.
var _run_id := ""


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)
	_run_id = "run%dx%d" % [
		Time.get_unix_time_from_system() as int,
		randi() % 100000,
	]


func after_each() -> void:
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_cancel_before_match_prevents_match_ready() -> void:
	var sockets: Array = await _setup_pair()
	if sockets.is_empty():
		return
	var a_sock: Variant = sockets[0]
	var b_sock: Variant = sockets[1]

	# Attach BEFORE adding tickets so a fast matchmaker fire
	# isn't lost to a connect-order race.
	var a_capture := _SocketHelper.NotificationCapture.new()
	var b_capture := _SocketHelper.NotificationCapture.new()
	a_capture.attach(a_sock)
	b_capture.attach(b_sock)
	var captures: Array = [a_capture, b_capture]

	# A queues, then cancels. Nakama resolves the remove with a
	# plain ack; an exception means the cancel itself failed.
	var a_ticket_id: String = await _add_ticket(a_sock, "A")
	if a_ticket_id.is_empty():
		_close_all(captures, sockets)
		return
	var removed: Variant = await a_sock.remove_matchmaker_async(
		a_ticket_id)
	assert_false(
		removed.is_exception(),
		"remove_matchmaker_async after add raised: %s" % str(removed))

	# B queues a ticket A's would have paired with.
	var b_ticket_id: String = await _add_ticket(b_sock, "B")
	if b_ticket_id.is_empty():
		_close_all(captures, sockets)
		return
	var any_matched := func() -> bool:
		return a_capture.matched_count > 0 or b_capture.matched_count > 0
	await _sock_helper.wait_until(any_matched, _NO_MATCH_WAIT_SEC)

	assert_eq(
		a_capture.matched_count, 0,
		"A was matched after cancelling its ticket; the cancel"
		+ " left the entry in the pool.")
	assert_eq(
		b_capture.matched_count, 0,
		"B was matched against A's cancelled ticket; the cancel"
		+ " left the entry in the pool.")
	assert_true(
		a_capture.match_ready.is_empty()
			and b_capture.match_ready.is_empty(),
		"match_ready arrived after a cancelled ticket; the runtime"
		+ " hook should not have allocated.")

	# Positive control: with B still queued, A queues again. The
	# pair must match inside the matchmaker's normal latency, or
	# the empty window above proved nothing.
	var a_requeue_id: String = await _add_ticket(a_sock, "A (requeue)")
	if not a_requeue_id.is_empty():
		var both_matched := func() -> bool:
			return (
				a_capture.matched_count > 0
				and b_capture.matched_count > 0
			)
		await _sock_helper.wait_until(both_matched, _MATCH_TIMEOUT_SEC)
		assert_gt(
			a_capture.matched_count, 0,
			"positive control: A was not matched with B within %.1fs"
				% _MATCH_TIMEOUT_SEC)
		assert_gt(
			b_capture.matched_count, 0,
			"positive control: B was not matched with A within %.1fs"
				% _MATCH_TIMEOUT_SEC)

	_close_all(captures, sockets)


func test_cancel_after_match_is_safe() -> void:
	var sockets: Array = await _setup_pair()
	if sockets.is_empty():
		return
	var a_sock: Variant = sockets[0]
	var b_sock: Variant = sockets[1]

	var a_capture := _SocketHelper.NotificationCapture.new()
	var b_capture := _SocketHelper.NotificationCapture.new()
	a_capture.attach(a_sock)
	b_capture.attach(b_sock)
	var captures: Array = [a_capture, b_capture]

	# The pair matches and the runtime allocates (mock mode).
	var a_ticket_id: String = await _add_ticket(a_sock, "A")
	var b_ticket_id: String = await _add_ticket(b_sock, "B")
	if a_ticket_id.is_empty() or b_ticket_id.is_empty():
		_close_all(captures, sockets)
		return

	# Wait for match_ready to arrive — the ticket is consumed
	# by the time it does.
	var deadline := _MATCH_TIMEOUT_SEC + _NOTIFY_TIMEOUT_SEC
	await _sock_helper.wait_until(
		func() -> bool: return not a_capture.match_ready.is_empty(),
		deadline)
	assert_false(
		a_capture.match_ready.is_empty(),
		"match_ready did not arrive within %.1fs;" % deadline
		+ " can't exercise post-match cancel without it.")

	# Now cancel the (already consumed) ticket. Nakama either
	# returns success (idempotent) or returns an error; in
	# both cases the socket must stay usable. The production
	# matchmaker client's cleanup() / cancel_matchmaking()
	# both call remove_matchmaker_async unconditionally
	# after match_ready (see matchmaker_api_client.gd:204-211)
	# — this test guards that path.
	var remove_result: Variant = await a_sock.remove_matchmaker_async(
		a_ticket_id)
	# Don't assert success: a server-side "unknown ticket"
	# response is acceptable. The socket must stay open, and a
	# fresh add/remove pair must still work on it. B's ticket was
	# consumed by the same match, so the follow-up has nothing to
	# pair with.
	assert_not_null(
		remove_result,
		"remove_matchmaker_async on consumed ticket returned null")
	assert_true(
		a_sock.is_connected_to_host(),
		"socket closed after cancelling a consumed ticket")

	var followup_id: String = await _add_ticket(a_sock, "follow-up")
	if not followup_id.is_empty():
		var followup_removed: Variant = await (
			a_sock.remove_matchmaker_async(followup_id))
		assert_false(
			followup_removed.is_exception(),
			"follow-up remove_matchmaker_async raised: %s"
				% str(followup_removed))

	_close_all(captures, sockets)


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


## Applies the shared skip gates, then mints two one-shot users
## and connects a socket for each. Returns the two sockets, or []
## after calling pending().
func _setup_pair() -> Array:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return []
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return []
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return []
	var mock_on: bool = await _helper.is_mock_deploy_mode()
	if not mock_on:
		pending(
			"EDGEGAP_MOCK_DEPLOY=true required on the runtime;"
			+ " this test would otherwise allocate a real"
			+ " Edgegap container.")
		return []
	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return []
	var connection: Dictionary = await _sock_helper.connect_users(_users)
	if not connection.ok:
		pending(str(connection.reason))
		return []
	return connection.sockets


## Adds a min=max=2 ticket scoped to this test run. Returns the
## ticket id, or "" after failing the test.
func _add_ticket(sock: Variant, label: String) -> String:
	var ticket: Variant = await sock.add_matchmaker_async(
		"+properties.compliance_run:" + _run_id, 2, 2, _make_props(), {})
	if ticket == null or ticket.is_exception():
		fail_test(
			"%s add_matchmaker_async failed: %s" % [label, str(ticket)])
		return ""
	var ticket_id: String = ticket.ticket
	if ticket_id.is_empty():
		fail_test(
			"%s ticket id missing on response: %s"
				% [label, str(ticket)])
	return ticket_id


## Detaches every capture, then closes every socket.
func _close_all(captures: Array, sockets: Array) -> void:
	for capture in captures:
		capture.detach()
	for sock in sockets:
		sock.close()


## Build the standard ticket-properties dict the runtime
## hook expects. player_count drives `EXPECTED_PLAYER_COUNT`
## on the Edgegap env; mock mode doesn't care about its
## value but the runtime still reads it. game_id and
## protocol_version are Stage 3.6/3.9 invariants.
## compliance_run scopes the pairing to this test.
func _make_props() -> Dictionary:
	return {
		"platform": "native",
		"player_count": "1",
		"game_id": _resolve_game_id_for_ticket(),
		"client_protocol_version": (
			_resolve_protocol_version_for_ticket()),
		"game_mode": "ffa",
		"compliance_run": _run_id,
	}


## Mirror NakamaMatchmakerClient's game_id resolver so the
## ticket the test sends matches what production would send.
func _resolve_game_id_for_ticket() -> String:
	var resolved: String = str(_helper._resolve_game_id())
	if resolved.is_empty():
		return "hopnbop"
	return resolved


## Mirror NakamaMatchmakerClient's protocol_version resolver.
func _resolve_protocol_version_for_ticket() -> String:
	if not ProjectSettings.has_setting(
		"application/config/protocol_version"
	):
		return "0"
	var raw: Variant = ProjectSettings.get_setting(
		"application/config/protocol_version")
	return str(int(raw))
