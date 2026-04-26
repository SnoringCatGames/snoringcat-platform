class_name PlatformApiClient
extends Node
## Thin generic HTTP client for the snoringcat-platform backend.
##
## Provides `get(path)` and `post(path, body)` helpers that
## auto-prefix the configured base URL and attach the current
## JWT (when one exists in the AuthTokenStore). One in-flight
## request at a time per instance; overlapping requests should
## use multiple instances or queue on the response signal.
##
## This is the minimum needed for the first end-to-end smoke
## test (Platform.api.get("/v1/version")). Higher-level
## per-endpoint helpers (auth flows, leaderboard, friends, etc.)
## are added incrementally as those subsystems migrate over
## from hopnbop_private/src/core/backend_api_client.gd.


## Emitted on every request completion. `ok` is true when the
## HTTP status was 2xx; `body` is the parsed JSON dict (or
## empty dict for non-JSON / error responses); `status_code` is
## the HTTP status code. `path` is the request path so callers
## can disambiguate when one client services multiple requests.
signal response_received(
	ok: bool,
	status_code: int,
	body: Dictionary,
	path: String,
)

## Emitted on transport-level failure (DNS, TCP, TLS, etc.).
## A non-2xx HTTP response is NOT a failure — that fires
## response_received(ok=false, ...).
signal request_failed(
	error: String,
	path: String,
)


# Configured by Platform.initialize() at app startup.
var base_url: String = ""
## Optional reference to an AuthTokenStore. When set, the
## client adds `Authorization: Bearer <jwt>` headers
## automatically when a JWT exists.
var token_store: PlatformAuthTokenStore

# In-flight request bookkeeping.
var _http: HTTPRequest
var _current_path := ""


func _ready() -> void:
	_http = HTTPRequest.new()
	add_child(_http)
	_http.request_completed.connect(_on_request_completed)


## Issue a GET. Result arrives via response_received signal.
func get(path: String) -> void:
	_send(path, HTTPClient.METHOD_GET, "")


## Issue a POST with a JSON body. Result arrives via
## response_received signal.
func post(path: String, body: Dictionary) -> void:
	_send(path, HTTPClient.METHOD_POST, JSON.stringify(body))


## Issue a PUT with a JSON body.
func put(path: String, body: Dictionary) -> void:
	_send(path, HTTPClient.METHOD_PUT, JSON.stringify(body))


func _send(
	path: String,
	method: HTTPClient.Method,
	body: String,
) -> void:
	if base_url.is_empty():
		push_error(
			"PlatformApiClient.base_url not configured")
		request_failed.emit("not_configured", path)
		return
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
