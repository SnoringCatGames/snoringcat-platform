class_name PlatformMatchmakingClient
extends Node
## Nakama-backed matchmaker socket layer. Opens the realtime
## socket, calls add_matchmaker_async, listens for
## `match_ready` notifications from the runtime, and emits
## platform-agnostic signals so a game-side adapter can plug it
## into whatever session-provider abstraction the game uses.
##
## Reads `Platform.auth`, `Platform.token_store`,
## `Platform.build_session_from_store()`,
## `Platform.get_nakama_client()`, and `Platform.game_id`. All
## must be populated before `start_matchmaking` is called.
##
## Transport-type handling: the runtime's match_ready payload
## carries `transport_type` as one of "enet" / "webrtc" /
## "websocket". This class emits it back to the adapter as a
## string and resolves the port based on protocol (UDP for
## ENet, TCP for WebRTC/WebSocket). The adapter translates the
## string into the game's transport-type enum.


## Fires once a `match_ready` notification has been received and
## parsed. Payload keys:
##   server_ip: String       — public IP of the allocated server
##   server_port: int        — host-side port matching transport
##   transport_type: String  — "enet" / "webrtc" / "websocket"
##   signaling_url: String   — pre-built wss:// URL (empty for ENet)
##   session_ids: Array      — Array[String], one per local player
##   level_id: String        — empty until the runtime echoes it
signal match_ready_received(payload: Dictionary)

## Fires on any pre-match failure (auth, socket connect,
## matchmaker add, payload parse, timeout, socket drop). Adapter
## maps this onto the game's session-failure surface.
signal matchmaking_failed(error: String)

## Fires every `_PROGRESS_TICK_SEC` while a ticket is in the pool
## (phase="searching"), once on ticket creation (phase="queued"),
## and once on `received_matchmaker_matched` (phase="placing").
## `estimated_total_sec` is -1.0 until the runtime exposes pool
## depth.
signal progress_updated(
	phase: String,
	elapsed_sec: float,
	estimated_total_sec: float,
)


const _MATCH_TIMEOUT_SEC := 120.0
const _MATCH_READY_SUBJECT := "match_ready"
## Stage 3.9: pushed by fleet_allocator.go when it aborts a
## match before allocation (currently fired on a
## client_protocol_version mismatch). Payload shape (flat
## JSON, single-encoded):
##   reason: String   — short tag ("protocol_mismatch")
##   message: String  — human-readable description
##   expected: int    — game's registered protocol_version
##   got: int         — this client's declared version, or 0
##                      when the abort was triggered by a
##                      different matched player's mismatch
## Distinct from `match_ready`'s double-encoded shape because
## there's no nested `connection` payload to carry.
const _MATCH_FAILED_SUBJECT := "match_failed"
const _PROGRESS_TICK_SEC := 1.0


var _socket: NakamaSocket = null
var _ticket: String = ""
var _is_searching := false
var _elapsed_timer: Timer = null
var _elapsed_sec := 0.0

## When start_matchmaking is called with a non-empty
## preview_device_id, this class authenticates that device id as
## a separate Nakama account (instead of using
## Platform.token_store) and uses the resulting session for the
## socket. Used so each preview slot in the editor's "Customize
## Run Instances" can appear to the matchmaker pool as a
## distinct user; without this every preview slot would share
## the same player_id and the pool would treat them as one user
## holding multiple tickets.
var _preview_device_id := ""
var _preview_user_id := ""

## Local player count carried over from start_matchmaking; used
## for the session_ids fallback when match_ready arrives without
## the session_ids array (pre-runtime-upgrade clients). When the
## runtime ships session_ids, this is unused.
var _local_player_count := 1


func _ready() -> void:
	process_mode = Node.PROCESS_MODE_ALWAYS

	_elapsed_timer = Timer.new()
	_elapsed_timer.wait_time = _PROGRESS_TICK_SEC
	_elapsed_timer.one_shot = false
	_elapsed_timer.timeout.connect(_on_elapsed_tick)
	add_child(_elapsed_timer)


## Whether a ticket is currently in the matchmaker pool.
func is_searching() -> bool:
	return _is_searching


## Open the matchmaker socket (if not already open), submit a
## ticket with the given properties, and wait for
## `match_ready`. Re-entrant: no-ops if already searching.
##
## `preview_device_id` (optional): when non-empty, authenticate
## this device id as a separate Nakama account and use that
## session for the socket. Used by the editor's multi-instance
## preview to give each slot a distinct uid.
##
## `local_player_count` (optional, default 1): used for the
## session_ids fallback when match_ready arrives without
## session_ids. Once every Nakama runtime ships session_ids
## this argument is unused.
func start_matchmaking(
	query: String,
	min_count: int,
	max_count: int,
	string_props: Dictionary,
	numeric_props: Dictionary,
	preview_device_id: String = "",
	local_player_count: int = 1,
) -> void:
	if _is_searching:
		push_warning("[PlatformMatchmaking] Already searching")
		return

	_preview_device_id = preview_device_id
	_local_player_count = local_player_count

	# Resolve the Nakama session for the socket. Either the
	# per-instance preview device session or the standard
	# token-store-derived session (with anonymous-JWT refresh on
	# demand). On failure, `_resolve_socket_session` already
	# emitted `matchmaking_failed` so callers don't need to.
	var session: NakamaSession = await _resolve_socket_session()
	if session == null:
		return

	if _socket == null or not _socket.is_connected_to_host():
		_socket = Nakama.create_socket_from(
			Platform.get_nakama_client())
		_socket.received_matchmaker_matched.connect(
			_on_matchmaker_matched)
		_socket.received_notification.connect(_on_notification)
		_socket.closed.connect(_on_socket_closed)
		_socket.connection_error.connect(
			_on_socket_connection_error)
		var connect_result: NakamaAsyncResult = (
			await _socket.connect_async(session))
		if connect_result.is_exception():
			var connect_ex: NakamaException = (
				connect_result.get_exception())
			_socket = null
			matchmaking_failed.emit(
				"Nakama socket connect failed: %s"
				% connect_ex.message)
			return

	# Record this client's public IP server-side before joining
	# the pool. The runtime's MatchmakerMatched hook reads the
	# recorded IPs and feeds them to Edgegap as `ip_list` for
	# region selection. Best-effort: a failure here just makes
	# the runtime fall back to a fixed geography.
	await _record_client_ip()

	print((
		"[PlatformMatchmaking] Joining matchmaker"
		+ " query=%s min=%d max=%d"
	) % [query, min_count, max_count])

	var ticket_result: NakamaRTAPI.MatchmakerTicket = (
		await _socket.add_matchmaker_async(
			query,
			min_count,
			max_count,
			string_props,
			numeric_props,
		))
	if ticket_result.is_exception():
		var ticket_ex: NakamaException = (
			ticket_result.get_exception())
		matchmaking_failed.emit(
			"Matchmaker add failed: %s" % ticket_ex.message)
		return

	_ticket = ticket_result.ticket
	_is_searching = true
	_elapsed_sec = 0.0
	_elapsed_timer.start()

	print("[PlatformMatchmaking] Ticket: %s" % _ticket)
	progress_updated.emit("queued", 0.0, -1.0)


## Remove the active ticket and stop the elapsed timer. The
## socket stays open so a subsequent start_matchmaking can
## re-use it. Safe to call when not searching.
func cancel_matchmaking() -> void:
	if not _is_searching:
		return
	_is_searching = false
	_elapsed_timer.stop()
	if _socket != null and not _ticket.is_empty():
		_socket.remove_matchmaker_async(_ticket)
	_ticket = ""


## Cancel any active search and close the socket. Use on
## logout / app exit.
func cleanup() -> void:
	cancel_matchmaking()
	if _socket != null:
		_socket.close()
		_socket = null


# --------------------------------------------------------------
# Internals
# --------------------------------------------------------------


func _record_client_ip() -> void:
	if _socket == null or not _socket.is_connected_to_host():
		return
	var result: NakamaAPI.ApiRpc = (
		await _socket.rpc_async("record_client_ip", "{}"))
	if result.is_exception():
		var ex: NakamaException = result.get_exception()
		push_warning(
			"[PlatformMatchmaking] record_client_ip failed: %s"
			% ex.message)


func _resolve_socket_session() -> NakamaSession:
	if not _preview_device_id.is_empty():
		return await _authenticate_preview_instance()

	var store: PlatformAuthTokenStore = Platform.token_store
	if store.is_anonymous and not store.is_token_valid():
		Platform.auth.get_guest_jwt()
		var result: Array = (
			await Platform.auth.guest_jwt_obtained)
		var success: bool = result[0]
		var error: String = result[1]
		if not success:
			matchmaking_failed.emit(
				"Failed to get session token: " + error)
			return null

	var session: NakamaSession = (
		Platform.build_session_from_store())
	if session == null:
		matchmaking_failed.emit("Not authenticated")
	return session


func _authenticate_preview_instance() -> NakamaSession:
	var client := Platform.get_nakama_client()
	var session: NakamaSession = (
		await client.authenticate_device_async(
			_preview_device_id))
	if session.is_exception():
		var ex: NakamaException = session.get_exception()
		matchmaking_failed.emit(
			"Preview matchmaker auth failed: %s" % ex.message)
		return null

	_preview_user_id = session.user_id
	print((
		"[PlatformMatchmaking] Preview device_id=%s uid=%s"
	) % [_preview_device_id, _preview_user_id])
	return session


func _on_matchmaker_matched(matched) -> void:
	print((
		"[PlatformMatchmaking] Matched match_id=%s users=%d"
	) % [matched.match_id, matched.users.size()])
	progress_updated.emit("placing", _elapsed_sec, -1.0)


func _on_notification(p_notification) -> void:
	if p_notification.subject == _MATCH_FAILED_SUBJECT:
		_handle_match_failed(p_notification)
		return
	if p_notification.subject != _MATCH_READY_SUBJECT:
		return
	if not _is_searching:
		# Stale notification arriving after we already cancelled
		# or moved on.
		return
	_is_searching = false
	_elapsed_timer.stop()

	# p_notification.content is a JSON string the runtime built
	# from `map[string]any{"connection": <json>}`. The inner
	# `connection` value was JSON-stringified before wrapping,
	# so we have to parse twice.
	var outer: Variant = JSON.parse_string(
		p_notification.content)
	if not (outer is Dictionary):
		matchmaking_failed.emit(
			"Invalid match_ready payload (outer)")
		return
	var conn_raw: String = str(outer.get("connection", ""))
	var conn: Variant = JSON.parse_string(conn_raw)
	if not (conn is Dictionary):
		matchmaking_failed.emit(
			"Invalid match_ready payload (connection)")
		return

	var server_ip: String = str(conn.get("server_ip", ""))
	var ports_dict: Variant = conn.get("ports", {})
	if server_ip.is_empty():
		matchmaking_failed.emit(
			"match_ready missing server_ip")
		return

	# transport_type chosen by the runtime: "webrtc" when any
	# web player is in the lobby, "enet" otherwise.
	var transport_type_str: String = str(
		conn.get("transport_type", ""))

	# signaling_url is the pre-signed FQDN URL the runtime
	# computes (e.g. wss://signaling.<zone>/connect/<token>).
	# Used for WebRTC/WebSocket transports; ENet ignores it.
	var signaling_url: String = str(
		conn.get("signaling_url", ""))

	var server_port := _pick_port(
		ports_dict, transport_type_str)
	if server_port <= 0:
		matchmaking_failed.emit(
			"match_ready missing usable port")
		return

	# session_ids: the Nakama runtime issues one ID per local
	# player at allocation time. The server's
	# EdgegapServerProvider validates against the same list
	# (passed in via EXPECTED_SESSION_IDS env var). Older
	# runtimes that don't ship session_ids fall back to
	# locally-derived IDs so a deploy mid-rollout keeps working;
	# the server's PreviewSessionProvider stand-in auto-accepts
	# in that case. Once every Nakama host runs the new plugin,
	# the fallback path is dead code.
	var session_ids: Array = []
	var raw_session_ids: Variant = conn.get("session_ids", null)
	if raw_session_ids is Array and raw_session_ids.size() > 0:
		for sid in raw_session_ids:
			session_ids.append(str(sid))
	else:
		push_warning((
			"[PlatformMatchmaking] match_ready missing"
			+ " session_ids; using locally-derived fallback"
			+ " (runtime out of date)"
		))
		var base_id: String = (
			_preview_user_id
			if not _preview_user_id.is_empty()
			else Platform.token_store.player_id
		)
		var count := _local_player_count
		if count < 1:
			count = 1
		for i in count:
			session_ids.append("%s_%d" % [base_id, i])

	# Level was chosen client-side and stored on game-side
	# session state before matchmaking began. The runtime
	# doesn't echo it yet, so emit an empty string and let the
	# adapter's existing local copy stand.
	var level_id := ""

	print((
		"[PlatformMatchmaking] match_ready %s:%d (level=%s)"
	) % [server_ip, server_port, level_id])

	match_ready_received.emit({
		"server_ip": server_ip,
		"server_port": server_port,
		"transport_type": transport_type_str,
		"signaling_url": signaling_url,
		"session_ids": session_ids,
		"level_id": level_id,
	})


func _handle_match_failed(p_notification) -> void:
	if not _is_searching:
		return
	_is_searching = false
	_elapsed_timer.stop()

	var parsed: Variant = JSON.parse_string(
		p_notification.content)
	var message := ""
	if parsed is Dictionary:
		message = str(parsed.get("message", ""))
	if message.is_empty():
		# Defensive fallback: an unparseable payload still has
		# to clear the matchmaker state and surface *some*
		# error so the client doesn't sit on the 120s timeout.
		message = (
			"Matchmaking aborted by server"
			+ " (unrecognized match_failed payload)")
	push_warning(
		"[PlatformMatchmaking] match_failed: %s" % message)
	matchmaking_failed.emit(message)


func _pick_port(
	ports: Variant,
	transport_type_str: String = "enet",
) -> int:
	# Edgegap status response shape:
	#   {"<name>": {"external": int, "internal": int,
	#               "protocol": "UDP"|"TCP"}, ...}
	# Pick UDP for ENet (game traffic) and TCP for WebRTC or
	# WebSocket (signaling / TCP transport). The Edgegap app
	# declares both 4433/UDP and 4434/TCP and forwards them to
	# host ports; we return the host port matching the
	# transport's protocol.
	if not (ports is Dictionary):
		return 0
	var lower := transport_type_str.to_lower()
	var want_protocol := "UDP"
	if lower == "webrtc" or lower == "websocket":
		want_protocol = "TCP"
	var fallback := 0
	for key in ports.keys():
		var entry: Variant = ports[key]
		if not (entry is Dictionary):
			continue
		var ext: int = int(entry.get("external", 0))
		if ext <= 0:
			continue
		if fallback == 0:
			fallback = ext
		var protocol: String = str(
			entry.get("protocol", "")).to_upper()
		if protocol == want_protocol:
			return ext
	return fallback


func _on_elapsed_tick() -> void:
	_elapsed_sec += _PROGRESS_TICK_SEC
	if _elapsed_sec > _MATCH_TIMEOUT_SEC:
		cancel_matchmaking()
		matchmaking_failed.emit(
			"Matchmaking timed out after %.0f seconds"
			% _MATCH_TIMEOUT_SEC)
		return
	progress_updated.emit(
		"searching", _elapsed_sec, -1.0)


func _on_socket_closed() -> void:
	print("[PlatformMatchmaking] Socket closed")


func _on_socket_connection_error(error) -> void:
	push_warning(
		"[PlatformMatchmaking] Socket error: %s" % error)
	if _is_searching:
		_is_searching = false
		_elapsed_timer.stop()
		matchmaking_failed.emit(
			"Matchmaker socket disconnected")
