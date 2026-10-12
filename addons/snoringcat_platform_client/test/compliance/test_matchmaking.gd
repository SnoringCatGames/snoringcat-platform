extends GutTest
## End-to-end two-player matchmaking flow over the Nakama realtime
## socket. Stage 8.19.
##
## Exercises:
##   1. Open a Nakama socket for each of two anonymous users.
##   2. Each adds a min=max=2 matchmaker ticket. The query
##      requires a property only this run's tickets carry, so
##      they pair with each other and with nothing else. Nakama
##      3.25 rejects tickets with a minimum count below 2, which
##      is why this is no longer a one-player flow.
##   3. Wait for `received_matchmaker_matched` (Nakama pool
##      match event) on both sockets.
##   4. Wait for `received_notification` with subject
##      `match_ready` (runtime hook fan-out) on both.
##   5. Assert the payload shape: server_ip, ports, request_id,
##      session_ids, transport_type, signaling_url, mock=true.
##   6. Assert both payloads name the same deploy, and the
##      request_id starts with the mock prefix so a
##      misconfigured prod (mock mode flipped off mid-test)
##      surfaces here loudly.
##
## Gated on EDGEGAP_MOCK_DEPLOY=true (see compliance_helper's
## is_mock_deploy_mode). When the runtime isn't in mock mode the
## test pending()s — we don't want CI runs against the live
## prod Nakama to burn paid Edgegap container-hours just to
## verify the matchmaker fires.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
## The runtime's synthesized request_id prefix in mock mode.
## Must match `mockEdgegapRequestIDPrefix` in fleet_allocator.go.
const _MOCK_REQUEST_ID_PREFIX := "mock-"
## How long we'll wait for the matchmaker pool + runtime hook to
## fire. The runtime's matchmaker tick is sub-second on a quiet
## pool; 8 s is a comfortable upper bound even under CI load.
const _MATCH_TIMEOUT_SEC := 8.0
## How long we'll wait for the match_ready notification after the
## matchmaker_matched event. The runtime allocation in mock mode
## is essentially synchronous (no Edgegap round-trip), so ~5 s
## is plenty.
const _NOTIFY_TIMEOUT_SEC := 5.0

var _helper
var _sock_helper
## One-shot users from multi_session_anon, so the test never
## bleeds state across runs. Captured at top level so after_each
## can hard-delete them.
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


func test_pair_matchmaker_flow_emits_match_ready() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return

	# Gate on the runtime's EDGEGAP_MOCK_DEPLOY flag so we never
	# burn paid allocations against the live prod runtime.
	var mock_on: bool = await _helper.is_mock_deploy_mode()
	if not mock_on:
		pending(
			"EDGEGAP_MOCK_DEPLOY=true required on the runtime;"
			+ " this test would otherwise allocate a real"
			+ " Edgegap container.")
		return

	# One-shot users so concurrent CI runs don't collide on a
	# shared device id (two clients on the same uid would fight
	# over one presence).
	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	var connection: Dictionary = await _sock_helper.connect_users(_users)
	if not connection.ok:
		pending(str(connection.reason))
		return
	var sockets: Array = connection.sockets

	# Watch for both events on both sockets. The Nakama SDK fires
	# `received_matchmaker_matched` when the pool pairs the
	# entries; `received_notification` carries the runtime hook's
	# match_ready fan-out. Attach before adding the tickets so a
	# fast-firing matchmaker doesn't race the connect.
	var captures: Array = []
	for sock in sockets:
		var capture := _SocketHelper.NotificationCapture.new()
		capture.attach(sock)
		captures.append(capture)

	# game_id + protocol_version are required ticket properties
	# (Stage 3.6 / 3.9). compliance_run scopes the pairing to this
	# run's two tickets.
	var run_id := "run%dx%d" % [
		Time.get_unix_time_from_system() as int,
		randi() % 100000,
	]
	var props: Dictionary = {
		"platform": "native",
		"player_count": "1",
		"game_id": _resolve_game_id_for_ticket(),
		"client_protocol_version": (
			_resolve_protocol_version_for_ticket()),
		"game_mode": "ffa",
		"compliance_run": run_id,
	}
	for i in sockets.size():
		var ticket: Variant = await sockets[i].add_matchmaker_async(
			"+properties.compliance_run:" + run_id, 2, 2, props, {})
		if ticket == null or ticket.is_exception():
			fail_test(
				"user %d add_matchmaker failed: %s" % [i, str(ticket)])
			_close_all(captures, sockets)
			return

	# Wait for the matchmaker pool to fire and the runtime to fan
	# out match_ready (or match_failed) to both users.
	var deadline := _MATCH_TIMEOUT_SEC + _NOTIFY_TIMEOUT_SEC
	var is_settled := func() -> bool:
		for capture in captures:
			if not capture.match_failed.is_empty():
				return true
			if capture.match_ready.is_empty():
				return false
		return true
	await _sock_helper.wait_until(is_settled, deadline)
	_close_all(captures, sockets)

	var payloads: Array[Dictionary] = []
	for i in captures.size():
		var capture: Variant = captures[i]
		assert_gt(
			capture.matched_count, 0,
			"user %d: received_matchmaker_matched did not fire within %.1fs"
				% [i, deadline])
		assert_true(
			capture.match_failed.is_empty(),
			"user %d: received match_failed instead of match_ready: %s"
				% [i, str(capture.match_failed)])
		assert_false(
			capture.match_ready.is_empty(),
			"user %d: match_ready not received within %.1fs"
				% [i, deadline])
		payloads.append(capture.match_ready)
	if payloads.any(func(p: Dictionary) -> bool: return p.is_empty()):
		return

	# Both players land on one deploy: one allocation per matchmaker
	# fan-out.
	var request_id: String = str(payloads[0].get("request_id", ""))
	assert_eq(
		str(payloads[1].get("request_id", "")), request_id,
		"both players should get the same deploy's request_id")
	assert_true(
		request_id.begins_with(_MOCK_REQUEST_ID_PREFIX),
		"request_id should start with '%s'; got '%s'"
			% [_MOCK_REQUEST_ID_PREFIX, request_id])

	var session_ids: Array = []
	for payload in payloads:
		_assert_payload_shape(payload)
		var ids: Variant = payload.get("session_ids", [])
		if ids is Array and not (ids as Array).is_empty():
			session_ids.append(str((ids as Array)[0]))
	assert_eq(
		session_ids.size(), 2,
		"each player should get a session id: %s" % str(payloads))
	if session_ids.size() == 2:
		assert_ne(
			session_ids[0], session_ids[1],
			"players should get distinct session ids")


## Payload contract for one player's match_ready: every required
## key present and shaped.
func _assert_payload_shape(payload: Dictionary) -> void:
	assert_true(
		bool(payload.get("mock", false)),
		(
			"match_ready missing mock=true flag; runtime is not"
			+ " in EDGEGAP_MOCK_DEPLOY mode despite runtime_status"
			+ " reporting it was. Payload: %s"
		) % str(payload))

	var server_ip: String = str(payload.get("server_ip", ""))
	assert_false(server_ip.is_empty(), "server_ip missing")

	var session_ids: Variant = payload.get("session_ids", [])
	assert_true(
		session_ids is Array and (session_ids as Array).size() == 1,
		"session_ids should be a 1-element Array; got %s"
			% str(session_ids))

	# Both players are native, so the transport rule picks ENet.
	assert_eq(
		str(payload.get("transport_type", "")), "enet",
		"a native-only match should use ENet: %s" % str(payload))

	var signaling_url: String = str(payload.get("signaling_url", ""))
	# Mock mode uses placeholder signaling defaults; the URL
	# format is "wss://<domain>/connect/<token>".
	assert_true(
		signaling_url.begins_with("wss://"),
		"signaling_url should start with wss://; got '%s'"
			% signaling_url)

	var ports: Variant = payload.get("ports", {})
	assert_true(
		ports is Dictionary and not (ports as Dictionary).is_empty(),
		"ports missing or empty: %s" % str(ports))


## Detaches every capture, then closes every socket.
func _close_all(captures: Array, sockets: Array) -> void:
	for capture in captures:
		capture.detach()
	for sock in sockets:
		sock.close()


## Returns the game_id the ticket should carry. Mirrors
## NakamaMatchmakerClient's resolver.
func _resolve_game_id_for_ticket() -> String:
	var resolved: String = str(_helper._resolve_game_id())
	if resolved.is_empty():
		return "hopnbop"
	return resolved


## Returns the client's compile-time protocol_version as a string,
## sourced from ProjectSettings (same source the production
## NakamaMatchmakerClient uses). Falls back to "0" so a project
## with the setting undeclared still produces a parseable value
## server-side (the runtime treats 0 as "no declared version" and
## skips the mismatch check gracefully — Stage 3.9).
func _resolve_protocol_version_for_ticket() -> String:
	if not ProjectSettings.has_setting("application/config/protocol_version"):
		return "0"
	var raw: Variant = ProjectSettings.get_setting(
		"application/config/protocol_version")
	return str(int(raw))
