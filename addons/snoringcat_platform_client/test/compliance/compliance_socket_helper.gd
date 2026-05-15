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
## Typical usage:
##
##   var session := await _socket_helper.session_from_token(token)
##   var sock := _socket_helper.create_socket()
##   var ok := await _socket_helper.connect_with_timeout(
##       sock, session, 5.0)
##   ...
##   sock.close()


const _DEFAULT_TIMEOUT_SEC := 5.0


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


## Construct a NakamaSocket pointed at the configured base URL.
## Returns null if the Nakama autoload isn't available in this
## project (uncommon — both hopnbop and the addon's parent
## project register it).
##
## When `host`/`port`/`scheme` are not passed, they are derived
## from `PLATFORM_API_URL`: scheme `http` -> socket `ws`, scheme
## `https` -> socket `wss`, default port 443. The dev stack
## (Stage 8.29) hits `http://localhost:7350` and so produces
## `ws://localhost:7350`.
func create_socket(
	host: String = "",
	port: int = -1,
	scheme: String = "",
) -> Variant:
	if not Engine.has_singleton("Nakama") and not _has_nakama_autoload():
		return null
	var derived: Dictionary = _derive_socket_target()
	if host.is_empty():
		host = derived.host
	if port < 0:
		port = derived.port
	if scheme.is_empty():
		scheme = derived.scheme
	# Nakama is the autoload (see project.godot). Cast through
	# Object.get to avoid a hard typed-property reference.
	var nakama_autoload: Variant = (
		Engine.get_main_loop()
			.root.get_node_or_null("/root/Nakama"))
	if nakama_autoload == null:
		return null
	return nakama_autoload.create_socket(host, port, scheme)


## Awaitable connect with a hard timeout. Returns true on
## success, false on transport error or timeout. The
## NakamaSocket's connect_async returns Variant on completion;
## we treat any non-error completion as success.
func connect_with_timeout(
	socket: Variant,
	session: Variant,
	timeout_sec: float = _DEFAULT_TIMEOUT_SEC,
) -> bool:
	if socket == null or session == null:
		return false
	# connect_async resolves with the result; the SDK fires
	# `connected` on success and `connection_error` on
	# failure.
	var connected := false
	var error_seen := false
	var on_connected := func(): connected = true
	var on_error := func(_e): error_seen = true
	socket.connected.connect(on_connected)
	socket.connection_error.connect(on_error)

	# Kick the connect; this returns when the SDK finishes its
	# handshake (or times out internally at p_connect_timeout
	# seconds).
	var _result: Variant = await socket.connect_async(
		session, false, int(timeout_sec))

	# Belt-and-suspenders: also race a timer in case the SDK's
	# completion signal didn't fire (defensive — the SDK should
	# always complete).
	var elapsed := 0.0
	var step := 0.05
	while not connected and not error_seen and elapsed < timeout_sec:
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	socket.connected.disconnect(on_connected)
	socket.connection_error.disconnect(on_error)
	return connected and not error_seen


## Wait up to `timeout_sec` for the named signal on `obj`.
## Returns the emitted args on success, null on timeout.
## Useful for asserting matchmaker/chat/presence pushes arrive.
func wait_for_signal_with_timeout(
	obj: Object,
	signal_name: String,
	timeout_sec: float = _DEFAULT_TIMEOUT_SEC,
) -> Variant:
	if obj == null or signal_name.is_empty():
		return null
	var got: Variant = null
	var fired := false
	var on_emit := func(arg: Variant = null):
		got = arg
		fired = true
	obj.connect(signal_name, on_emit)
	var elapsed := 0.0
	var step := 0.05
	while not fired and elapsed < timeout_sec:
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step
	obj.disconnect(signal_name, on_emit)
	return got if fired else null


func _default_host() -> String:
	return _derive_socket_target().host


## Parse `PLATFORM_API_URL` into a {host, port, scheme} target
## suitable for `NakamaClient.create_socket`. Falls back to the
## prod `wss://nakama.snoringcat.games:443` triple when the env
## var is unset.
##
## URL scheme maps to socket scheme: `http` -> `ws`, `https` ->
## `wss`. Explicit `ws`/`wss` are passed through. Missing port
## defaults to 443 for `wss` and 80 for `ws`.
func _derive_socket_target() -> Dictionary:
	var override := OS.get_environment("PLATFORM_API_URL")
	if override.is_empty():
		return {
			"host": "nakama.snoringcat.games",
			"port": 443,
			"scheme": "wss",
		}

	var scheme := "wss"
	var rest := override
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
