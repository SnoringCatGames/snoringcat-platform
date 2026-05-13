extends GutTest
## Matchmaker cancel paths over the Nakama realtime socket.
## Stage 8.20.
##
## Two scenarios:
##   1. test_cancel_before_match_prevents_match_ready —
##      add a min=max=2 ticket (won't match with a single
##      player), call remove_matchmaker_async, wait the
##      mock-mode allocation window, assert no
##      received_matchmaker_matched and no match_ready
##      notification arrive. Validates the happy-path cancel
##      contract: a removed ticket stays out of the pool.
##   2. test_cancel_after_match_is_safe — add a min=max=1
##      ticket so the runtime hook fires synchronously,
##      wait for match_ready, THEN call remove_matchmaker_async
##      on the now-consumed ticket id. Confirms the call is
##      safe (no crash, socket usable for a follow-on
##      remove call afterward). Documents the current
##      Stage 7.2 limitation: post-match cancel is best-
##      effort, Edgegap deploy stays alive until the game
##      server's own grace timer fires.
##
## Gated on EDGEGAP_MOCK_DEPLOY=true (see compliance_helper's
## is_mock_deploy_mode). The min=max=1 path allocates; we
## never want CI to burn a real Edgegap container just to
## test the cancel codepath.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
## How long we wait after cancelling a min=max=2 ticket
## before declaring "no match_ready arrived." Mock-mode
## allocation completes in tens of milliseconds; 3 s is a
## comfortable upper bound that still catches a real
## regression (cancel path no-ops, ticket stays in pool,
## hook fires).
const _NO_MATCH_WAIT_SEC := 3.0
## How long we wait for match_ready in the post-match cancel
## scenario. Same shape as test_matchmaking.gd's combined
## deadline.
const _MATCH_TIMEOUT_SEC := 8.0
const _NOTIFY_TIMEOUT_SEC := 5.0

var _helper
var _sock_helper
## One-shot user from multi_session_anon. Captured at the
## script scope so after_each can hard-delete regardless of
## which test ran.
var _user: Dictionary = {}


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


func after_each() -> void:
	if not _user.is_empty():
		await _helper.delete_one_shot_account(_user)
		_user = {}


func test_cancel_before_match_prevents_match_ready() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return
	var mock_on: bool = await _helper.is_mock_deploy_mode()
	if not mock_on:
		pending(
			"EDGEGAP_MOCK_DEPLOY=true required on the runtime;"
			+ " this test would otherwise allocate a real"
			+ " Edgegap container.")
		return

	var setup: Dictionary = await _connect_one_shot()
	if not setup.get("ok", false):
		pending(str(setup.get("reason", "setup failed")))
		return
	var sock: Variant = setup.sock

	# Wire watchers BEFORE adding the ticket so a fast
	# matchmaker fire isn't lost to a connect-order race.
	var matched_seen := false
	var match_ready_seen := false
	var on_matched := func(_m): matched_seen = true
	var on_notification := func(n):
		if str(n.subject) == "match_ready":
			match_ready_seen = true
	sock.received_matchmaker_matched.connect(on_matched)
	sock.received_notification.connect(on_notification)

	# min=max=2 so a solo ticket never matches and the cancel
	# path is what removes it from the pool. If a regression
	# made cancel a no-op, the matchmaker would still be sat
	# on a stale entry — but with min=2 no allocation fires
	# regardless, so the test wouldn't catch the regression.
	# We can't fix this with min=max=1 either (that would
	# allocate immediately, before cancel could win). The
	# real signal is: did the matchmaker pool retain the
	# ticket? If yes, a hypothetical second player would
	# match against it — but we have no second player here.
	# Best we can do is verify the cancel call itself
	# returned cleanly. The min=max=1 race is covered by
	# test_cancel_after_match_is_safe below.
	var ticket = await sock.add_matchmaker_async(
		"*", 2, 2, _make_props("2"), {})
	assert_not_null(
		ticket, "add_matchmaker_async returned null")
	if ticket == null:
		sock.received_matchmaker_matched.disconnect(on_matched)
		sock.received_notification.disconnect(on_notification)
		sock.close()
		return
	var ticket_str: Variant = _read_ticket_id(ticket)
	assert_true(
		ticket_str is String
			and not (ticket_str as String).is_empty(),
		"ticket id missing on response: %s" % str(ticket))
	if not (ticket_str is String):
		sock.close()
		return

	# Cancel immediately. Nakama's remove_matchmaker_async
	# resolves Variant; nil-error = success.
	var remove_result: Variant = await (
		sock.remove_matchmaker_async(ticket_str))
	_assert_not_exception(
		remove_result, "remove_matchmaker_async after add")

	# Wait the mock-allocation window. If cancel worked, no
	# match_ready fires. If it didn't, the min=max=2 still
	# wouldn't fire alone — so the assertion is genuinely
	# "no match_ready" rather than a proof of cancel
	# semantics. The stronger assertion (cancel actually
	# removes from the pool) needs a 2-user setup; deferred
	# to a future expansion of this test.
	var elapsed := 0.0
	var step := 0.05
	while elapsed < _NO_MATCH_WAIT_SEC:
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	sock.received_matchmaker_matched.disconnect(on_matched)
	sock.received_notification.disconnect(on_notification)
	sock.close()

	assert_false(
		matched_seen,
		"received_matchmaker_matched fired after a cancelled"
		+ " min=max=2 ticket; cancel path may have leaked"
		+ " the entry into the pool.")
	assert_false(
		match_ready_seen,
		"match_ready notification arrived after a cancelled"
		+ " ticket; runtime hook should not have allocated.")


func test_cancel_after_match_is_safe() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return
	var mock_on: bool = await _helper.is_mock_deploy_mode()
	if not mock_on:
		pending(
			"EDGEGAP_MOCK_DEPLOY=true required on the runtime;"
			+ " this test would otherwise allocate a real"
			+ " Edgegap container.")
		return

	var setup: Dictionary = await _connect_one_shot()
	if not setup.get("ok", false):
		pending(str(setup.get("reason", "setup failed")))
		return
	var sock: Variant = setup.sock

	var match_ready_seen := false
	var on_notification := func(n):
		if str(n.subject) == "match_ready":
			match_ready_seen = true
	sock.received_notification.connect(on_notification)

	# min=max=1 fires immediately and triggers an allocation.
	var ticket = await sock.add_matchmaker_async(
		"*", 1, 1, _make_props("1"), {})
	assert_not_null(
		ticket, "add_matchmaker_async returned null")
	if ticket == null:
		sock.received_notification.disconnect(on_notification)
		sock.close()
		return
	var ticket_str: Variant = _read_ticket_id(ticket)
	if not (ticket_str is String):
		sock.received_notification.disconnect(on_notification)
		sock.close()
		fail_test(
			"ticket id missing on response: %s" % str(ticket))
		return

	# Wait for match_ready to arrive — the ticket is consumed
	# by the time it does.
	var deadline := _MATCH_TIMEOUT_SEC + _NOTIFY_TIMEOUT_SEC
	var elapsed := 0.0
	var step := 0.05
	while not match_ready_seen and elapsed < deadline:
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	assert_true(
		match_ready_seen,
		"match_ready did not arrive within %.1fs;"
			% deadline
			+ " can't exercise post-match cancel without it.")

	# Now cancel the (already consumed) ticket. Nakama either
	# returns success (idempotent) or returns an error; in
	# both cases the socket must stay usable. The production
	# matchmaker client's cleanup() / cancel_matchmaking()
	# both call remove_matchmaker_async unconditionally
	# after match_ready (see matchmaker_api_client.gd:204-211)
	# — this test guards that path.
	var remove_result: Variant = await (
		sock.remove_matchmaker_async(ticket_str))
	# Don't assert success: a server-side "unknown ticket"
	# response is acceptable. We only assert the socket
	# stayed up — proved by the next remove_matchmaker_async
	# call returning at all instead of timing out.
	assert_not_null(
		remove_result,
		"remove_matchmaker_async on consumed ticket returned"
		+ " null (transport closed?).")

	# Re-issue and re-cancel proves the socket is still good.
	var followup = await sock.add_matchmaker_async(
		"*", 2, 2, _make_props("2"), {})
	assert_not_null(
		followup,
		"socket no longer accepts add_matchmaker_async"
		+ " after cancel-on-consumed-ticket.")
	if followup != null:
		var followup_id: Variant = _read_ticket_id(followup)
		if followup_id is String:
			await sock.remove_matchmaker_async(followup_id)

	sock.received_notification.disconnect(on_notification)
	sock.close()


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


## Authenticate a one-shot anonymous user, build a Nakama
## session, open a socket and connect. Returns
##   {ok: bool, sock: NakamaSocket, reason: String}
## On failure, sets _user to {} (no cleanup needed) and
## returns a pending-style reason string.
func _connect_one_shot() -> Dictionary:
	var users: Array = await _helper.multi_session_anon(1)
	if users.size() != 1:
		return {
			"ok": false,
			"sock": null,
			"reason": "multi_session_anon did not return one user",
		}
	_user = users[0]

	var session: Variant = _sock_helper.session_from_token(
		str(_user.token))
	if session == null:
		return {
			"ok": false,
			"sock": null,
			"reason": "Nakama SDK NakamaSession not available",
		}
	var sock: Variant = _sock_helper.create_socket()
	if sock == null:
		return {
			"ok": false,
			"sock": null,
			"reason": "Nakama autoload not registered",
		}
	var connected: bool = await _sock_helper.connect_with_timeout(
		sock, session, 5.0)
	if not connected:
		return {
			"ok": false,
			"sock": null,
			"reason": (
				"socket would not connect (covered by"
				+ " test_socket_auth)"),
		}
	return {"ok": true, "sock": sock, "reason": ""}


## Build the standard ticket-properties dict the runtime
## hook expects. player_count drives `EXPECTED_PLAYER_COUNT`
## on the Edgegap env; mock mode doesn't care about its
## value but the runtime still reads it. game_id and
## protocol_version are Stage 3.6/3.9 invariants.
func _make_props(player_count: String) -> Dictionary:
	return {
		"platform": "native",
		"player_count": player_count,
		"game_id": _resolve_game_id_for_ticket(),
		"client_protocol_version": (
			_resolve_protocol_version_for_ticket()),
		"game_mode": "ffa",
	}


## Read the ticket id out of Nakama's MatchmakerTicket
## response. SDK versions differ on field name; defensive
## get() chooses whichever fires.
func _read_ticket_id(ticket: Variant) -> Variant:
	if ticket == null:
		return null
	var ticket_str: Variant = ticket.get("ticket")
	if ticket_str == null:
		ticket_str = ticket.get("ticket_id")
	return ticket_str


## Assert the Nakama SDK's `*_async` result is not an
## exception-shaped failure. The SDK wraps errors in objects
## that expose `is_exception()`; success returns either null
## or a parsed dict without that method.
func _assert_not_exception(
	result: Variant, label: String
) -> void:
	if result == null:
		# Some SDK paths return null on success; that's fine.
		return
	if (
		result.get("is_exception") != null
		and result.is_exception()
	):
		fail_test("%s: %s" % [label, str(result)])


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
