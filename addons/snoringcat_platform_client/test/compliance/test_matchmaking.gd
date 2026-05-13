extends GutTest
## End-to-end solo matchmaking flow over the Nakama realtime
## socket. Stage 8.19.
##
## Exercises:
##   1. Open a Nakama socket as an anonymous user.
##   2. Add a matchmaker ticket with min/max=1 so the runtime
##      hook fires immediately.
##   3. Wait for `received_matchmaker_matched` (Nakama pool
##      match event).
##   4. Wait for `received_notification` with subject
##      `match_ready` (runtime hook fan-out).
##   5. Assert the payload shape: server_ip, ports, request_id,
##      session_ids, transport_type, signaling_url, mock=true.
##   6. Assert the request_id starts with the mock prefix so a
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
## User dict from multi_session_anon (one-shot account so the
## test never bleeds state across runs). Captured at top level so
## after_each can hard-delete it.
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


func test_solo_matchmaker_flow_emits_match_ready() -> void:
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

	# One-shot user so concurrent CI runs don't collide on a
	# shared device id (matchmaker properties' `player_count`
	# uses presence — two clients on the same uid would fight).
	var users: Array = await _helper.multi_session_anon(1)
	if users.size() != 1:
		pending("multi_session_anon did not return one user")
		return
	_user = users[0]

	var session: Variant = _sock_helper.session_from_token(
		str(_user.token))
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

	# Watch for both events. The Nakama SDK fires
	# `received_matchmaker_matched` when the pool pairs the
	# entry; `received_notification` carries the runtime hook's
	# match_ready fan-out. Wire callbacks before adding the
	# ticket so a fast-firing matchmaker doesn't race the connect.
	var matched_seen := false
	var match_ready_payload: Dictionary = {}
	var failed_subject := ""
	var on_matched := func(_matched): matched_seen = true
	var on_notification := func(n):
		var subj: String = str(n.subject)
		if subj == "match_ready":
			# Outer JSON wraps `{"connection": "<inner json>"}`.
			var outer: Variant = JSON.parse_string(str(n.content))
			if outer is Dictionary:
				var conn_raw: String = str(outer.get("connection", ""))
				var conn: Variant = JSON.parse_string(conn_raw)
				if conn is Dictionary:
					match_ready_payload = conn
		elif subj == "match_failed":
			failed_subject = subj

	sock.received_matchmaker_matched.connect(on_matched)
	sock.received_notification.connect(on_notification)

	# Add the ticket. min=max=1 so the pool matches our entry
	# without needing a second player. game_id + protocol_version
	# are required ticket properties (Stage 3.6 / 3.9).
	var game_id := _resolve_game_id_for_ticket()
	var protocol_version_str := _resolve_protocol_version_for_ticket()
	var props: Dictionary = {
		"platform": "native",
		"player_count": "1",
		"game_id": game_id,
		"client_protocol_version": protocol_version_str,
		"game_mode": "ffa",
	}
	var ticket = await sock.add_matchmaker_async(
		"*", 1, 1, props, {})
	assert_not_null(ticket, "add_matchmaker_async returned null")
	if ticket == null or (ticket.get("is_exception") != null and ticket.is_exception()):
		# Defensive: if the ticket add failed, surface it.
		fail_test("add_matchmaker failed: %s" % str(ticket))
		sock.close()
		return

	# Wait up to _MATCH_TIMEOUT_SEC for the matchmaker pool to
	# fire and the runtime to fan out match_ready.
	var deadline := _MATCH_TIMEOUT_SEC + _NOTIFY_TIMEOUT_SEC
	var elapsed := 0.0
	var step := 0.05
	while (
		match_ready_payload.is_empty()
		and failed_subject.is_empty()
		and elapsed < deadline
	):
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	sock.received_matchmaker_matched.disconnect(on_matched)
	sock.received_notification.disconnect(on_notification)
	sock.close()

	assert_true(
		matched_seen,
		"received_matchmaker_matched did not fire within %.1fs"
			% _MATCH_TIMEOUT_SEC)
	assert_eq(
		failed_subject, "",
		"received match_failed instead of match_ready: %s"
			% failed_subject)
	assert_false(
		match_ready_payload.is_empty(),
		"match_ready notification not received within %.1fs"
			% deadline)
	if match_ready_payload.is_empty():
		return

	# Payload contract: every required key present and shaped.
	assert_true(
		match_ready_payload.has("mock")
			and bool(match_ready_payload.get("mock", false)),
		(
			"match_ready missing mock=true flag; runtime is not"
			+ " in EDGEGAP_MOCK_DEPLOY mode despite runtime_status"
			+ " reporting it was. Payload: %s"
		) % str(match_ready_payload))

	var request_id: String = str(
		match_ready_payload.get("request_id", ""))
	assert_true(
		request_id.begins_with(_MOCK_REQUEST_ID_PREFIX),
		"request_id should start with '%s'; got '%s'"
			% [_MOCK_REQUEST_ID_PREFIX, request_id])

	var server_ip: String = str(
		match_ready_payload.get("server_ip", ""))
	assert_false(server_ip.is_empty(), "server_ip missing")

	var session_ids: Variant = match_ready_payload.get(
		"session_ids", [])
	assert_true(
		session_ids is Array and (session_ids as Array).size() == 1,
		"session_ids should be a 1-element Array; got %s"
			% str(session_ids))

	var transport_type: String = str(
		match_ready_payload.get("transport_type", ""))
	assert_true(
		transport_type == "enet"
			or transport_type == "webrtc"
			or transport_type == "websocket",
		"transport_type unexpected: '%s'" % transport_type)

	var signaling_url: String = str(
		match_ready_payload.get("signaling_url", ""))
	# Mock mode uses placeholder signaling defaults; the URL
	# format is "wss://<domain>/connect/<token>".
	assert_true(
		signaling_url.begins_with("wss://"),
		"signaling_url should start with wss://; got '%s'"
			% signaling_url)

	var ports: Variant = match_ready_payload.get("ports", {})
	assert_true(
		ports is Dictionary and not (ports as Dictionary).is_empty(),
		"ports missing or empty: %s" % str(ports))


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
