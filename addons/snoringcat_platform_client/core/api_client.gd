extends Node
## Generic HTTP client for snoringcat-platform.
##
## NO `class_name` on this script — Godot 4.6 has a parser-cache
## bug where preloading a class_name'd script that lives next to
## other class_name'd scripts in the same addon yields stale
## content from a previous parse. Until that's resolved upstream,
## consumers reach this class via runtime `load()` (not preload).
##
## Usage:
##   var Script = load("res://addons/.../api_client.gd")
##   var api = Script.new()
##   api.base_url = "https://..."
##   root.add_child(api)
##   api.response_received.connect(_on_response)
##   await get_tree().process_frame  # let HTTPRequest enter tree
##   api.do_get("/v1/version")

signal response_received(
	ok: bool,
	status_code: int,
	body: Dictionary,
	path: String,
)
signal request_failed(error: String, path: String)

var base_url: String = ""
## Optional reference to a token store with a `jwt_token`
## property (e.g. PlatformAuthTokenStore). When set, requests
## include `Authorization: Bearer <jwt>`. Untyped to dodge the
## class_name parser-cache bug.
var token_store

var _http: HTTPRequest
var _current_path := ""


func _ready() -> void:
	_ensure_http_request()


func _ensure_http_request() -> void:
	# Lazy init so callers that fire a request immediately after
	# add_child (before _ready runs) still work.
	if is_instance_valid(_http):
		return
	_http = HTTPRequest.new()
	# use_threads is REQUIRED in --headless. Without it, the
	# polling-based completion path never fires (HTTPRequest's
	# internal state never advances past CONNECTING). Threaded
	# mode dispatches via a worker so the main loop's frame rate
	# doesn't matter.
	_http.use_threads = true
	add_child(_http)
	_http.request_completed.connect(_on_request_completed)


func do_get(path: String) -> void:
	_send(path, HTTPClient.METHOD_GET, "")


func do_post(path: String, body: Dictionary) -> void:
	_send(path, HTTPClient.METHOD_POST, JSON.stringify(body))


func do_put(path: String, body: Dictionary) -> void:
	_send(path, HTTPClient.METHOD_PUT, JSON.stringify(body))


func _send(
	path: String,
	method: HTTPClient.Method,
	body: String,
) -> void:
	if base_url.is_empty():
		push_error("api_client.base_url not configured")
		request_failed.emit("not_configured", path)
		return
	_ensure_http_request()
	_current_path = path
	var url := base_url.rstrip("/") + path
	var err := _http.request(
		url, _build_headers(), method, body)
	if err != OK:
		request_failed.emit(
			"http_request_error_%d" % err, path)
		_current_path = ""


func _build_headers() -> PackedStringArray:
	var headers: PackedStringArray = [
		"Content-Type: application/json",
		"Accept: application/json",
	]
	if (token_store != null
			and not token_store.jwt_token.is_empty()):
		headers.append(
			"Authorization: Bearer "
			+ token_store.jwt_token)
	return headers


func _on_request_completed(
	result: int,
	response_code: int,
	_headers: PackedStringArray,
	body: PackedByteArray,
) -> void:
	var path := _current_path
	_current_path = ""

	if result != HTTPRequest.RESULT_SUCCESS:
		request_failed.emit(
			"transport_error_%d" % result, path)
		return

	var parsed := {}
	if body.size() > 0:
		var json := JSON.new()
		var parse_err := json.parse(
			body.get_string_from_utf8())
		if parse_err == OK and json.data is Dictionary:
			parsed = json.data
	var ok := response_code >= 200 and response_code < 300
	response_received.emit(
		ok, response_code, parsed, path)
