extends Node
## Realtime-socket helpers for compliance tests. Wraps the
## Nakama SDK's `NakamaSocket` so individual tests don't have
## to reimplement session parsing, host derivation, or the
## connect/disconnect dance.
##
## NO class_name — same Godot 4.6 parser-cache rationale as
## compliance_helper.gd. Tests preload this directly.
##
## Requires the `Nakama` autoload (included in
## `addons/snoringcat_platform_client` consumers and in this
## addon's parent project). If it's missing, helpers return
## null and tests `pending()`.
##
## Signal callbacks must never assign to a captured local.
## GDScript lambdas capture locals by value, so with
## `var seen := false` and `func(): seen = true`, only the
## lambda's copy changes and the test never sees it. Record into
## a Dictionary, an Array, or an object's fields instead, as
## NotificationCapture and wait_for_signal_with_timeout() do.
## This helper used to get that wrong: connect_with_timeout()
## reported every connect as a failure, so every realtime test
## pended without testing anything.
##
## A coroutine called through a Variant without `await` is a
## runtime SCRIPT ERROR in Godot 4.7 that aborts the caller. Always
## await the SDK's `*_async` calls.
##
## Typical usage:
##
##   var session: Variant = _socket_helper.session_from_token(token)
##   var sock: Variant = _socket_helper.create_socket()
##   var ok: bool = await _socket_helper.connect_with_timeout(
##       sock, session, 5.0)
##   ...
##   sock.close()


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEFAULT_TIMEOUT_SEC := 5.0
## How often wait_until() re-checks its condition.
const _POLL_INTERVAL_SEC := 0.05


## Build a NakamaSession from a raw JWT access token. Returns
## null on parse failure. The session is the input
## connect_async expects; we don't have to re-authenticate.
func session_from_token(token: String) -> Variant:
	if token.is_empty():
		return null
	# NakamaSession's _init parses the JWT and populates
	# user_id/username/expire_time. Pass refresh_token=null
	# (we don't need it for socket auth).
	var session_class: GDScript = (
		load("res://addons/nakama/api/NakamaSession.gd"))
	if session_class == null:
		return null
	var session = session_class.new(token, false, null)
	if not session.is_valid():
		return null
	return session


## Construct a NakamaSocket pointed at PLATFORM_API_URL: scheme
## `http` -> socket `ws`, `https` -> `wss`, default port 80 or 443.
## The dev stack's `http://127.0.0.1:7350` produces
## `ws://127.0.0.1:7350`.
##
## Returns null if the Nakama autoload isn't available in this
## project (uncommon — both hopnbop and the addon's parent
## project register it), or if compliance_helper.gd's target
## guard refuses PLATFORM_API_URL. A refusal also push_error()s,
## which GUT counts as a test failure.
func create_socket() -> Variant:
	var refusal: String = _Helper.get_target_refusal()
	if not refusal.is_empty():
		push_error("compliance suite refused to open a socket: " + refusal)
		return null
	if not Engine.has_singleton("Nakama") and not _has_nakama_autoload():
		return null
	var target: Dictionary = _derive_socket_target()
	# Nakama is the autoload (see project.godot). Cast through
	# Object.get to avoid a hard typed-property reference.
	var nakama_autoload: Variant = (
		Engine.get_main_loop()
			.root.get_node_or_null("/root/Nakama"))
	if nakama_autoload == null:
		return null
	return nakama_autoload.create_socket(
		target.host, target.port, target.scheme)


## Opens the socket and returns whether it ended up connected.
## connect_async resolves on success, on a refused or failed
## handshake, and when the SDK's own connect timeout (timeout_sec,
## rounded up) expires, so awaiting it is bounded. The socket's
## live state is the answer, not anything a callback recorded.
## `appear_online` makes the user visible to status followers.
func connect_with_timeout(
	socket: Variant,
	session: Variant,
	timeout_sec: float = _DEFAULT_TIMEOUT_SEC,
	appear_online: bool = false,
) -> bool:
	if socket == null or session == null:
		return false
	await socket.connect_async(
		session, appear_online, maxi(1, ceili(timeout_sec)))
	return bool(socket.is_connected_to_host())


## Opens one connected socket per user dict from
## compliance_helper.multi_session_anon(). Returns
##   {ok: bool, sockets: Array, reason: String}
## where `reason` is a pending()-style message on failure. Closes
## any sockets it opened when one of them fails.
func connect_users(
	users: Array,
	appear_online: bool = false,
) -> Dictionary:
	var sockets: Array = []
	var reason := ""
	for user in users:
		var session: Variant = session_from_token(str(user.token))
		if session == null:
			reason = "Nakama SDK NakamaSession not available"
			break
		var sock: Variant = create_socket()
		if sock == null:
			reason = "Nakama autoload not registered"
			break
		var connected: bool = await connect_with_timeout(
			sock, session, _DEFAULT_TIMEOUT_SEC, appear_online)
		if not connected:
			reason = "socket would not connect (covered by test_socket_auth)"
			break
		sockets.append(sock)
	if not reason.is_empty():
		for sock in sockets:
			sock.close()
		return {"ok": false, "sockets": [], "reason": reason}
	return {"ok": true, "sockets": sockets, "reason": ""}


## Waits until `condition` returns true or `timeout_sec` elapses.
## Returns the condition's final value. Use it with state the
## condition can actually observe: a Dictionary, an Array, or an
## object's fields that signal callbacks write to.
func wait_until(condition: Callable, timeout_sec: float) -> bool:
	var deadline_msec := (
		Time.get_ticks_msec() + int(timeout_sec * 1000.0))
	while not condition.call():
		if Time.get_ticks_msec() >= deadline_msec:
			return bool(condition.call())
		await get_tree().create_timer(_POLL_INTERVAL_SEC).timeout
	return true


## Wait up to `timeout_sec` for the named signal on `obj`.
## Returns the first emitted argument (signals with zero or one
## argument), or null on timeout. When `matcher` is valid, only
## an emission whose argument makes it return true counts.
##
## It only sees emissions after the call, so it can't watch for a
## reply to a request the caller has to await first. Record those
## from a callback connected before the request instead (see
## test_socket_chat.gd).
func wait_for_signal_with_timeout(
	obj: Object,
	signal_name: String,
	timeout_sec: float = _DEFAULT_TIMEOUT_SEC,
	matcher: Callable = Callable(),
) -> Variant:
	if obj == null or signal_name.is_empty():
		return null
	var state := {"fired": false, "arg": null}
	var on_emit := func(arg: Variant = null) -> void:
		if state.fired:
			return
		if matcher.is_valid() and not matcher.call(arg):
			return
		state.fired = true
		state.arg = arg
	obj.connect(signal_name, on_emit)
	await wait_until(func() -> bool: return state.fired, timeout_sec)
	obj.disconnect(signal_name, on_emit)
	return state.arg


## Parse `PLATFORM_API_URL` into a {host, port, scheme} target
## suitable for `NakamaClient.create_socket`. create_socket()
## checks the target guard before calling this.
##
## URL scheme maps to socket scheme: `http` -> `ws`, `https` ->
## `wss`. Explicit `ws`/`wss` are passed through. Missing port
## defaults to 443 for `wss` and 80 for `ws`.
func _derive_socket_target() -> Dictionary:
	var url := OS.get_environment("PLATFORM_API_URL").strip_edges()
	var scheme := "wss"
	var rest := url
	if rest.begins_with("https://"):
		scheme = "wss"
		rest = rest.substr("https://".length())
	elif rest.begins_with("http://"):
		scheme = "ws"
		rest = rest.substr("http://".length())
	elif rest.begins_with("wss://"):
		scheme = "wss"
		rest = rest.substr("wss://".length())
	elif rest.begins_with("ws://"):
		scheme = "ws"
		rest = rest.substr("ws://".length())

	# Trim trailing path. `host:port/path` -> `host:port`.
	var slash := rest.find("/")
	if slash >= 0:
		rest = rest.substr(0, slash)

	var host := rest
	var port := 443 if scheme == "wss" else 80
	var colon := rest.find(":")
	if colon >= 0:
		host = rest.substr(0, colon)
		var port_str := rest.substr(colon + 1)
		var parsed := int(port_str)
		if parsed > 0:
			port = parsed
	return {"host": host, "port": port, "scheme": scheme}


func _has_nakama_autoload() -> bool:
	return (
		Engine.get_main_loop()
			.root.has_node("/root/Nakama"))


## Records one socket's matchmaker events and runtime
## notifications. The state lives in this object's fields, which
## the signal callbacks and the test both see. Attach before the
## action that triggers the traffic, and detach before closing the
## socket.
class NotificationCapture:
	extends RefCounted

	## received_matchmaker_matched emissions so far.
	var matched_count := 0
	## Parsed `connection` payload of the first match_ready
	## notification. Its content is double-encoded:
	## `{"connection": "<inner json>"}`.
	var match_ready: Dictionary = {}
	## Content of the first match_failed notification (flat JSON:
	## reason, message, and reason-specific fields). Content that
	## won't parse lands under `unparsed`, so a waiting test still
	## stops waiting and its assertion shows the raw text.
	var match_failed: Dictionary = {}
	## matchmaker_properties of the first party_matchmaking_start
	## notification.
	var party_matchmaking_start: Dictionary = {}
	## Subject of every notification received, in order.
	var subjects: Array[String] = []

	var _socket: Variant = null


	func attach(socket: Variant) -> void:
		_socket = socket
		socket.received_matchmaker_matched.connect(_on_matched)
		socket.received_notification.connect(_on_notification)


	func detach() -> void:
		if _socket == null:
			return
		if _socket.received_matchmaker_matched.is_connected(_on_matched):
			_socket.received_matchmaker_matched.disconnect(_on_matched)
		if _socket.received_notification.is_connected(_on_notification):
			_socket.received_notification.disconnect(_on_notification)
		_socket = null


	func _on_matched(_matched: Variant) -> void:
		matched_count += 1


	func _on_notification(notification: Variant) -> void:
		var subject := str(notification.subject)
		subjects.append(subject)
		var raw := str(notification.content)
		var content: Variant = _parse_json(raw)
		match subject:
			"match_ready":
				if match_ready.is_empty() and content is Dictionary:
					var connection: Variant = _parse_json(
						str(content.get("connection", "")))
					if connection is Dictionary:
						match_ready = connection
			"match_failed":
				if match_failed.is_empty():
					if content is Dictionary:
						match_failed = content
					else:
						match_failed = {"unparsed": raw}
			"party_matchmaking_start":
				if (
					party_matchmaking_start.is_empty()
					and content is Dictionary
				):
					var properties: Variant = content.get(
						"matchmaker_properties", {})
					if properties is Dictionary:
						party_matchmaking_start = properties


	## JSON.parse_string() logs an engine error on bad input, which
	## GUT would count as a test failure. Parse quietly instead.
	static func _parse_json(text: String) -> Variant:
		var json := JSON.new()
		if json.parse(text) != OK:
			return null
		return json.data
