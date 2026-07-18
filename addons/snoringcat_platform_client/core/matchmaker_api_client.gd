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

## Poll granularity while the leader waits for party members to
## show up in the realtime party. Fine enough that a fully-present
## party starts near-instantly, coarse enough not to spin.
const _PARTY_WAIT_POLL_SEC := 0.25


var _socket: NakamaSocket = null
var _ticket: String = ""
var _is_searching := false
var _elapsed_timer: Timer = null
var _elapsed_sec := 0.0

## Nakama *realtime* party id, when this client is matchmaking as
## part of a group. Empty for solo matchmaking.
##
## Not to be confused with the persistent Nakama group that backs
## the social party (PlatformPartyApiClient). The realtime party is
## created fresh for each matchmaking attempt, lives on this
## socket, and exists for exactly one reason: PartyMatchmakerAdd
## submits ONE ticket for its whole membership, which is the only
## construct Nakama offers that guarantees a group is matched into
## the same match.
var _rt_party_id := ""

## True when this client created the realtime party and is
## therefore the one that submits the ticket. Followers join and
## wait; only the leader ticketing is what keeps the party on a
## single ticket.
var _is_rt_party_leader := false

## user_ids of OTHER members currently present in the realtime
## party (self excluded). Fed by received_party_presence; read by
## wait_for_rt_party_members.
var _rt_party_member_ids: Dictionary = {}

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

	if not await ensure_socket():
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
##
## Two server-side cancels fire in sequence:
##   1. `remove_matchmaker_async` removes the ticket from the
##      Nakama matchmaker pool. This is the only one that matters
##      while the user is still queued (phase=queued/searching) —
##      it prevents the ticket from being matched. No-op once the
##      ticket has been consumed by OnMatchmakerMatched.
##   2. `cancel_matchmaking_allocation` RPC (Stage 7.2) signals
##      the runtime to abort an in-flight Edgegap allocation if
##      the user has already been matched and the runtime is
##      currently polling Edgegap. The server fans out a
##      `match_failed` reason=cancelled notification to every
##      matched player; the canceller's local `_is_searching`
##      is already false here so the late-arriving notification
##      no-ops on this client (see `_handle_match_failed`).
##
## Both calls are fire-and-forget. The new RPC silently no-ops
## server-side when no in-flight allocation exists, so calling it
## during the searching phase (when the cancel happened before
## OnMatchmakerMatched fired) is cheap and safe.
func cancel_matchmaking() -> void:
	if not _is_searching:
		return
	_is_searching = false
	_elapsed_timer.stop()
	if _socket != null and _socket.is_connected_to_host():
		if not _ticket.is_empty():
			# A party ticket has to be withdrawn through the party
			# API; remove_matchmaker_async doesn't know about it and
			# would leave the whole party queued.
			if not _rt_party_id.is_empty() and _is_rt_party_leader:
				_socket.remove_matchmaker_party_async(
					_rt_party_id, _ticket)
			else:
				_socket.remove_matchmaker_async(_ticket)
		_socket.rpc_async(
			"cancel_matchmaking_allocation", "{}")
	_ticket = ""


## Cancel any active search and close the socket. Use on
## logout / app exit.
func cleanup() -> void:
	cancel_matchmaking()
	# Drop realtime-party state without a server round trip: the
	# socket close below evicts us anyway, and awaiting a leave on
	# a socket we're about to destroy just risks hanging shutdown.
	_rt_party_id = ""
	_rt_party_member_ids.clear()
	_is_rt_party_leader = false
	if _socket != null:
		_socket.close()
		_socket = null


## Open the matchmaker socket if it isn't already up. Returns true
## when a live socket is available.
##
## Split out of start_matchmaking so the party flow can get a
## socket without submitting a ticket: the leader needs one to
## create the realtime party, and followers need one to join it
## while never ticketing at all.
##
## On failure `matchmaking_failed` has already been emitted, so
## callers just bail.
##
## `preview_device_id` / `local_player_count` should be set before
## calling — the session resolution reads the former.
func ensure_socket() -> bool:
	if _socket != null and _socket.is_connected_to_host():
		return true

	# Resolve the Nakama session for the socket. Either the
	# per-instance preview device session or the standard
	# token-store-derived session (with anonymous-JWT refresh on
	# demand). On failure, `_resolve_socket_session` already
	# emitted `matchmaking_failed` so callers don't need to.
	var session: NakamaSession = await _resolve_socket_session()
	if session == null:
		return false

	_socket = Nakama.create_socket_from(
		Platform.get_nakama_client())
	_socket.received_matchmaker_matched.connect(
		_on_matchmaker_matched)
	_socket.received_notification.connect(_on_notification)
	_socket.received_party_presence.connect(
		_on_received_party_presence)
	_socket.received_party_close.connect(
		_on_received_party_close)
	_socket.closed.connect(_on_socket_closed)
	_socket.connection_error.connect(
		_on_socket_connection_error)
	var connect_result: NakamaAsyncResult = (
		await _socket.connect_async(session))
	if connect_result.is_exception():
		var connect_ex: NakamaException = (
			connect_result.get_exception())
		# Drop the failed handle so a later ensure_socket doesn't
		# see is_connected_to_host()=false on a stale socket and
		# skip re-creating it.
		_socket = null
		matchmaking_failed.emit(
			"Nakama socket connect failed: %s"
			% connect_ex.message)
		return false
	return true


# --------------------------------------------------------------
# Realtime party (group matchmaking)
# --------------------------------------------------------------


## The realtime party this client is currently in, or "" .
func get_rt_party_id() -> String:
	return _rt_party_id


## user_ids of other members present in the realtime party.
func get_rt_party_member_ids() -> Array:
	return _rt_party_member_ids.keys()


## Create a realtime party and become its leader. Returns the new
## party id, or "" on failure (matchmaking_failed already emitted).
##
## The party is created `open` so members admit themselves without
## a leader-approval round trip. That's safe because the id is a
## server-minted UUID delivered only to the social party's members
## via a Nakama notification — holding it already implies
## membership, and the approval dance would just add a failure mode
## to a window we're trying to keep short.
##
## `max_size`: the SDK documents this as excluding the leader,
## which we can't verify from here. Passing the social party's cap
## is safe under either reading — it's either exact or one seat
## generous, and both beat under-provisioning (a member who can't
## get in aborts the whole attempt).
## Takes no preview_device_id, unlike start_matchmaking: the
## editor's multi-instance preview authenticates each slot as an
## anonymous device account, and every party path is gated on
## non-anonymous, so a preview slot can never reach this. The
## socket resolves the normal token-store session.
func create_rt_party(max_size: int) -> String:
	if not await ensure_socket():
		return ""
	var result = await _socket.create_party_async(true, max_size)
	if result.is_exception():
		var ex: NakamaException = result.get_exception()
		matchmaking_failed.emit(
			"Party create failed: %s" % ex.message)
		return ""
	_rt_party_id = str(result.party_id)
	_is_rt_party_leader = true
	_rt_party_member_ids.clear()
	print("[PlatformMatchmaking] rt party created: %s"
		% _rt_party_id)
	return _rt_party_id


## Join the leader's realtime party. Follower path. Returns true on
## success.
func join_rt_party(rt_party_id: String) -> bool:
	if rt_party_id.is_empty():
		return false
	if not await ensure_socket():
		return false
	var result = await _socket.join_party_async(rt_party_id)
	if result.is_exception():
		var ex: NakamaException = result.get_exception()
		matchmaking_failed.emit(
			"Party join failed: %s" % ex.message)
		return false
	_rt_party_id = rt_party_id
	_is_rt_party_leader = false
	_rt_party_member_ids.clear()
	print("[PlatformMatchmaking] joined rt party: %s"
		% rt_party_id)
	return true


## Block until `expected_others` other members are present in the
## realtime party, or the timeout expires. Returns true only when
## the full roster showed up.
##
## The leader uses this to decide whether to submit the ticket at
## all. A false return means at least one member never got their
## socket into the party, and the caller is expected to abort
## rather than start a match the party didn't agree to.
func wait_for_rt_party_members(
	expected_others: int,
	timeout_sec: float,
) -> bool:
	if expected_others <= 0:
		return true
	var deadline := (
		Time.get_ticks_msec() + int(timeout_sec * 1000.0))
	while Time.get_ticks_msec() < deadline:
		if _rt_party_member_ids.size() >= expected_others:
			return true
		# The socket dropping mid-wait can never be satisfied by
		# waiting longer.
		if _socket == null or not _socket.is_connected_to_host():
			return false
		await get_tree().create_timer(
			_PARTY_WAIT_POLL_SEC).timeout
	return _rt_party_member_ids.size() >= expected_others


## Submit ONE matchmaker ticket covering the whole realtime party.
## Leader only.
##
## min/max counts are TOTAL match sizes (as with a solo ticket);
## Nakama accounts for the party's own size when filling.
func start_rt_party_matchmaking(
	query: String,
	min_count: int,
	max_count: int,
	string_props: Dictionary,
	numeric_props: Dictionary,
	local_player_count: int = 1,
) -> void:
	if _is_searching:
		push_warning("[PlatformMatchmaking] Already searching")
		return
	if _rt_party_id.is_empty():
		matchmaking_failed.emit(
			"No realtime party to matchmake for")
		return
	if not _is_rt_party_leader:
		matchmaking_failed.emit(
			"Only the party leader submits the ticket")
		return
	_local_player_count = local_player_count
	if _socket == null or not _socket.is_connected_to_host():
		matchmaking_failed.emit("Matchmaker socket is down")
		return

	await _record_client_ip()

	print((
		"[PlatformMatchmaking] Joining matchmaker as party"
		+ " rt_party=%s query=%s min=%d max=%d"
	) % [_rt_party_id, query, min_count, max_count])

	var ticket_result = await _socket.add_matchmaker_party_async(
		_rt_party_id,
		query,
		min_count,
		max_count,
		string_props,
		numeric_props,
	)
	if ticket_result.is_exception():
		var ex: NakamaException = ticket_result.get_exception()
		matchmaking_failed.emit(
			"Party matchmaker add failed: %s" % ex.message)
		return

	_ticket = str(ticket_result.ticket)
	_is_searching = true
	_elapsed_sec = 0.0
	_elapsed_timer.start()
	print("[PlatformMatchmaking] Party ticket: %s" % _ticket)
	progress_updated.emit("queued", 0.0, -1.0)


## Follower path: we hold no ticket (the leader's party ticket
## covers us), but we still need to sit in the searching state so
## the incoming match_ready is accepted and so the same timeout
## applies if the leader goes silent.
##
## Records our client IP first: the allocator feeds every matched
## player's IP to Edgegap for region selection, so skipping it for
## followers would bias placement toward the leader.
func begin_rt_party_wait(
	local_player_count: int = 1,
) -> void:
	if _is_searching:
		return
	if _rt_party_id.is_empty():
		matchmaking_failed.emit("Not in a realtime party")
		return
	_local_player_count = local_player_count
	await _record_client_ip()
	_ticket = ""
	_is_searching = true
	_elapsed_sec = 0.0
	_elapsed_timer.start()
	progress_updated.emit("queued", 0.0, -1.0)


## Tear down realtime-party state and leave the party server-side.
## Safe to call in any state.
##
## The leader closes the party (which evicts everyone, so followers
## don't linger in a party whose leader has moved on); followers
## just leave.
func leave_rt_party() -> void:
	var party_id := _rt_party_id
	_rt_party_id = ""
	_rt_party_member_ids.clear()
	var was_leader := _is_rt_party_leader
	_is_rt_party_leader = false
	if party_id.is_empty():
		return
	if _socket == null or not _socket.is_connected_to_host():
		return
	if was_leader:
		await _socket.close_party_async(party_id)
	else:
		await _socket.leave_party_async(party_id)


# --------------------------------------------------------------
# Internals
# --------------------------------------------------------------


## Track who is actually in the realtime party. The leader gates
## ticket submission on this, so it has to exclude self: Nakama
## reports the creator's own presence, which would otherwise let a
## solo leader satisfy a 1-other-member wait immediately.
func _on_received_party_presence(p_event) -> void:
	if _rt_party_id.is_empty():
		return
	if str(p_event.party_id) != _rt_party_id:
		return
	var self_id := _socket_user_id()
	for presence in p_event.joins:
		var uid := str(presence.user_id)
		if uid.is_empty() or uid == self_id:
			continue
		_rt_party_member_ids[uid] = true
	for presence in p_event.leaves:
		_rt_party_member_ids.erase(str(presence.user_id))


## The leader closed the party (or Nakama tore it down). For a
## follower still waiting this is terminal — without the party
## there's no ticket covering us and no match coming.
##
## Ignored once `_is_searching` is false: a close we triggered
## ourselves during our own teardown isn't a failure.
func _on_received_party_close(p_event) -> void:
	if _rt_party_id.is_empty():
		return
	if str(p_event.party_id) != _rt_party_id:
		return
	_rt_party_id = ""
	_rt_party_member_ids.clear()
	if not _is_searching:
		return
	_is_searching = false
	_elapsed_timer.stop()
	matchmaking_failed.emit("The party stopped matchmaking")


## The user_id this socket is authenticated as. Preview instances
## authenticate a per-slot device account, so they must not read
## the shared token store.
func _socket_user_id() -> String:
	if not _preview_user_id.is_empty():
		return _preview_user_id
	return Platform.token_store.player_id


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
	# Stage 3 game_id scoping requires every authenticate call to
	# carry `game_id` in session vars so the runtime's
	# BeforeAuthenticate* hook accepts it and downstream stateful
	# RPCs can read it back via RUNTIME_CTX_VARS. The mainline
	# auth path threads this through `auth_api_client._build_
	# session_vars()`; the preview path mints its own per-instance
	# device session outside that flow, so we inline the vars dict.
	var session: NakamaSession = (
		await client.authenticate_device_async(
			_preview_device_id,
			null,
			true,
			{"game_id": Platform.game_id}))
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
