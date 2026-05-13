extends Node
## Shared helpers for compliance tests.
##
## NO class_name — the addon's api_client.gd documents a Godot 4.6
## parser-cache bug that bites class_name'd scripts inside an
## addon. Compliance tests preload this file directly.
##
## Usage from a GutTest:
##
##   const _Helper = preload(
##       "res://addons/snoringcat_platform_client/test/"
##       + "compliance/compliance_helper.gd")
##   var _helper
##
##   func before_each():
##       _helper = _Helper.new()
##       add_child_autofree(_helper)
##
##   func test_health():
##       var result = await _helper.http_get("/healthcheck")
##       assert_eq(result.status_code, 200)


## Live Nakama base URL. Override via PLATFORM_API_URL env var when
## targeting a different stack (e.g. a future staging instance).
const _DEFAULT_BASE_URL := "https://nakama.snoringcat.games"
const _DEFAULT_TIMEOUT_SEC := 10.0


## Returns the live backend URL, overridable via env var so the
## hourly synthetic monitor and game CIs can target a non-prod URL
## without code changes.
func get_base_url() -> String:
	var override := OS.get_environment("PLATFORM_API_URL")
	if not override.is_empty():
		return override
	return _DEFAULT_BASE_URL


## True when tests should hit the live backend. Default true; flip
## off when mock-mode HTTP interception is added in a later phase.
func is_live_mode() -> bool:
	var mode := OS.get_environment("PLATFORM_COMPLIANCE_MODE")
	if mode.is_empty():
		return true
	return mode == "live"


## Server-key Basic auth header used by /v2/account/authenticate/*.
## Reads NAKAMA_SERVER_KEY from env. Returns empty string when
## unset so the test can pending() with a clear message instead
## of producing a malformed request.
func server_key_basic_header() -> String:
	var key := OS.get_environment("NAKAMA_SERVER_KEY")
	if key.is_empty():
		return ""
	return ("Authorization: Basic "
		+ Marshalls.utf8_to_base64(key + ":"))


## HTTP key for server-to-server runtime RPCs. Required for
## runtime_status, register_server, match_end, etc. — endpoints
## the snoringcat-platform runtime gates with requireServerToServer.
## Empty when unset.
func http_key() -> String:
	return OS.get_environment("NAKAMA_HTTP_KEY")


## Generic HTTP request against the configured base URL. Handles
## auth via the `auth` argument:
##
##   "" or null         — no Authorization header.
##   "basic_server_key" — Basic <base64(server_key:)>.
##   "bearer:<token>"   — Bearer <token>.
##
## For HTTP-key RPCs, append "?http_key=..." to the path (Nakama
## takes the key as a query parameter, not an Authorization
## header).
##
## Returns Dictionary with keys:
##   status_code (int), body (Variant: Dictionary/Array/null),
##   text (String), error (String — empty if no transport error).
func http_request(
	method: int,
	path: String,
	body: Variant = null,
	auth: String = "",
) -> Dictionary:
	var http := HTTPRequest.new()
	# use_threads is REQUIRED in --headless. Without it, the
	# polling-based completion path never fires.
	http.use_threads = true
	add_child(http)

	var url := get_base_url().rstrip("/") + path
	var headers: PackedStringArray = [
		"Content-Type: application/json",
		"Accept: application/json",
	]
	if auth == "basic_server_key":
		var h := server_key_basic_header()
		if not h.is_empty():
			headers.append(h)
	elif auth.begins_with("bearer:"):
		headers.append("Authorization: Bearer " + auth.substr(7))

	var body_str := ""
	if body != null:
		# Nakama's REST RPC endpoint accepts an empty body OR a
		# JSON-quoted string. A bare object {} returns 400. Pass
		# strings through verbatim so callers can do that.
		if body is Dictionary or body is Array:
			body_str = JSON.stringify(body)
		else:
			body_str = str(body)

	var err := http.request(url, headers, method, body_str)
	var result: Dictionary = {
		"status_code": 0,
		"body": null,
		"text": "",
		"error": "",
	}
	if err != OK:
		result.error = "http_request_error_%d" % err
		http.queue_free()
		return result

	var completed: Array = await http.request_completed
	http.queue_free()
	# request_completed signature: (result, response_code,
	# headers, body).
	var rc: int = completed[0]
	var status: int = completed[1]
	var resp_bytes: PackedByteArray = completed[3]

	if rc != HTTPRequest.RESULT_SUCCESS:
		result.error = "transport_error_%d" % rc
		return result
	result.status_code = status
	result.text = resp_bytes.get_string_from_utf8()
	if not result.text.is_empty():
		var json := JSON.new()
		if json.parse(result.text) == OK:
			result.body = json.data
	return result


## Convenience: GET against the base URL.
func http_get(path: String, auth: String = "") -> Dictionary:
	return await http_request(HTTPClient.METHOD_GET, path, null, auth)


## Convenience: POST against the base URL.
func http_post(
	path: String,
	body: Variant = null,
	auth: String = "",
) -> Dictionary:
	return await http_request(
		HTTPClient.METHOD_POST, path, body, auth)


## Convenience: PUT against the base URL.
func http_put(
	path: String,
	body: Variant = null,
	auth: String = "",
) -> Dictionary:
	return await http_request(
		HTTPClient.METHOD_PUT, path, body, auth)


## Convenience: DELETE against the base URL.
func http_delete(path: String, auth: String = "") -> Dictionary:
	return await http_request(
		HTTPClient.METHOD_DELETE, path, null, auth)


## End-to-end: anon-authenticate against Nakama, return the
## session token (or empty string on failure). The device_id is
## stable across runs so the same compliance account is reused
## (avoids polluting the users table).
##
## When the consuming game has initialized the `Platform`
## autoload with a `game_id`, that value is included in the
## auth request's `vars` map so the runtime's
## BeforeAuthenticateDevice hook accepts the call. Falls back
## to the `PLATFORM_GAME_ID` env var when Platform is
## unavailable (compliance suite running standalone).
func nakama_anon_session(device_id: String) -> String:
	var result: Dictionary = await http_post(
		"/v2/account/authenticate/device?create=true",
		device_auth_body(device_id),
		"basic_server_key")
	if result.status_code != 200:
		return ""
	if result.body == null or not (result.body is Dictionary):
		return ""
	return str(result.body.get("token", ""))


## Mint `count` independently-authenticated anonymous sessions
## in one call. Each user gets a one-shot device_id (run
## timestamp + random + index) so concurrent CI runs don't
## collide and a single run doesn't bind any persistent state.
##
## Returns Array[Dictionary] of
##   {token, refresh_token, user_id, username, device_id}
## on success. Returns [] on the first auth failure so callers
## can pending() with a clean state. The accounts linger after
## the test for ops-side sweep (prefix "compliance-multi-");
## tests that want strict cleanup can call
## delete_one_shot_account(user) per user in after_each.
func multi_session_anon(
	count: int,
	prefix: String = "compliance-multi",
) -> Array:
	var users: Array = []
	var run_id := (
		"%d-%d"
		% [
			Time.get_unix_time_from_system() as int,
			randi() % 100000,
		])
	for i in range(count):
		var device_id := "%s-%s-%d" % [prefix, run_id, i]
		var result: Dictionary = await http_post(
			"/v2/account/authenticate/device?create=true",
			device_auth_body(device_id),
			"basic_server_key")
		if result.status_code != 200:
			return []
		if result.body == null or not (result.body is Dictionary):
			return []
		var token := str(result.body.get("token", ""))
		if token.is_empty():
			return []
		var refresh := str(result.body.get("refresh_token", ""))
		var claims := decode_jwt_claims(token)
		var user_id := str(claims.get("uid", ""))
		var username := str(claims.get("usn", ""))
		users.append({
			"token": token,
			"refresh_token": refresh,
			"user_id": user_id,
			"username": username,
			"device_id": device_id,
		})
	return users


## Hard-delete a one-shot account created by multi_session_anon
## via Nakama's built-in `DELETE /v2/account` (bypasses the
## platform soft-delete RPC's 30-day grace, which would leave
## the account intact for the cron). Best-effort: a non-2xx
## status returns false; nothing else fails. Useful in
## after_each() for tests that want to keep the users table
## clean during local iteration.
func delete_one_shot_account(user: Dictionary) -> bool:
	var token := str(user.get("token", ""))
	if token.is_empty():
		return false
	var result: Dictionary = await http_delete(
		"/v2/account", "bearer:" + token)
	return (
		result.status_code >= 200 and result.status_code < 300)


## Returns a `/v2/account/authenticate/device` POST body with
## `game_id` injected into `vars` so the runtime's
## BeforeAuthenticateDevice hook accepts the call. Use this in
## place of `{"id": device_id}` for any compliance test that
## hits the authenticate endpoint directly.
func device_auth_body(device_id: String) -> Dictionary:
	var body: Dictionary = {"id": device_id}
	var game_id := _resolve_game_id()
	if not game_id.is_empty():
		body["vars"] = {"game_id": game_id}
	return body


## Returns a `/v2/account/session/refresh` POST body with
## `game_id` injected into `vars`. Mirrors device_auth_body
## for the refresh flow.
func session_refresh_body(refresh_token: String) -> Dictionary:
	var body: Dictionary = {"token": refresh_token}
	var game_id := _resolve_game_id()
	if not game_id.is_empty():
		body["vars"] = {"game_id": game_id}
	return body


## Resolves the `game_id` to attach to authenticate vars.
## Prefers a live `Platform.game_id` (when the addon's autoload
## has been initialized by the consuming game) over the
## `PLATFORM_GAME_ID` env var. Returns "" when neither is
## available; callers default to legacy no-vars behavior.
func _resolve_game_id() -> String:
	if Engine.has_singleton("Platform"):
		var platform: Object = Engine.get_singleton("Platform")
		if (
			platform != null
			and platform.get("is_initialized") == true
		):
			return str(platform.get("game_id"))
	# The autoload is registered as a script (not a singleton),
	# so look it up through the SceneTree's root.
	var tree := Engine.get_main_loop()
	if tree is SceneTree:
		var root: Node = (tree as SceneTree).root
		var node: Node = root.get_node_or_null("Platform")
		if (
			node != null
			and node.get("is_initialized") == true
		):
			return str(node.get("game_id"))
	return OS.get_environment("PLATFORM_GAME_ID")


## Convenience: call a server-to-server RPC by name. The RPC's
## payload is sent as the request body. We add `unwrap=true`
## to the URL so Nakama treats the body as the raw payload
## (no JSON-string double-encoding required) and returns the
## runtime's raw response JSON. The parsed response is exposed
## as `inner`.
##
## Returns Dictionary with same shape as http_request, plus:
##   inner (Variant) — parsed response JSON, null on failure
func http_key_rpc(
	rpc_name: String,
	body: Variant = null,
) -> Dictionary:
	var key := http_key()
	if key.is_empty():
		return {
			"status_code": 0, "body": null, "text": "",
			"error": "NAKAMA_HTTP_KEY env var not set",
			"inner": null,
		}
	var path := (
		"/v2/rpc/%s?http_key=%s&unwrap=true"
		% [rpc_name, key])
	var result: Dictionary = await http_post(path, body)
	result.inner = result.body
	return result


## Convenience: call a session-authenticated RPC by name.
## See http_key_rpc for the unwrap semantics.
func session_rpc(
	rpc_name: String,
	session_token: String,
	body: Variant = null,
) -> Dictionary:
	var path := "/v2/rpc/%s?unwrap=true" % rpc_name
	var auth := "bearer:" + session_token
	var result: Dictionary = await http_post(path, body, auth)
	result.inner = result.body
	return result


## Decode the claims segment of a JWT (base64url, no padding).
## Returns the parsed claims Dictionary, or {} on parse failure.
func decode_jwt_claims(token: String) -> Dictionary:
	var parts := token.split(".")
	if parts.size() != 3:
		return {}
	var b64: String = parts[1]
	# base64url -> base64 (translate + pad).
	b64 = b64.replace("-", "+").replace("_", "/")
	while b64.length() % 4 != 0:
		b64 += "="
	var bytes := Marshalls.base64_to_raw(b64)
	var text := bytes.get_string_from_utf8()
	var json := JSON.new()
	if json.parse(text) != OK:
		return {}
	if not (json.data is Dictionary):
		return {}
	return json.data
