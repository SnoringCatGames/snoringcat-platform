extends Node
## Shared helpers for compliance tests.
##
## NO class_name — a Godot 4.6 parser-cache bug bites
## class_name'd scripts inside an addon (the SDK's script
## headers document it). Compliance tests preload this file
## directly.
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
##
## Target guard: the suite writes accounts, friendships, groups, and
## storage rows wherever it points, so it only talks to a loopback
## stack unless told otherwise. PLATFORM_API_URL must be set, and a
## non-loopback URL also needs ALLOW_PROD_COMPLIANCE=1. A refused
## request is never sent: it push_error()s, which GUT counts as a
## test failure, and returns an error result. See
## get_target_refusal().


## Hosts the suite may target without ALLOW_PROD_COMPLIANCE=1.
const _LOOPBACK_HOSTS := ["127.0.0.1", "localhost", "::1"]
## Nakama's session and refresh tokens are JWTs, whose base64url
## header always starts with "eyJ".
const _JWT_PATTERN := "eyJ[A-Za-z0-9_-]*\\.[A-Za-z0-9_-]*\\.[A-Za-z0-9_-]*"
const _REDACTED_JWT := "<redacted-jwt>"

# The console session token, signed in on first use.
var _console_token := ""


## Returns why the suite must not send requests to the configured
## target, or "" when it may. Static so compliance_socket_helper.gd
## applies the same rule to the realtime socket.
static func get_target_refusal() -> String:
	var url := OS.get_environment("PLATFORM_API_URL").strip_edges()
	if url.is_empty():
		return (
			"PLATFORM_API_URL is not set. The compliance suite no"
			+ " longer defaults to production Nakama. Run it against"
			+ " a local stack with scripts/local-smoke-test.ps1 in"
			+ " the consuming game."
		)
	if _LOOPBACK_HOSTS.has(parse_host(url)):
		return ""
	if OS.get_environment("ALLOW_PROD_COMPLIANCE") == "1":
		return ""
	var message := (
		"PLATFORM_API_URL=%s is not a loopback stack. The suite"
		+ " creates accounts, friendships, groups, and storage rows"
		+ " on its target. Set ALLOW_PROD_COMPLIANCE=1 to run it"
		+ " there on purpose."
	)
	return message % url


## Returns the lowercased host of a URL such as
## `http://127.0.0.1:7350/path` or `ws://[::1]:7350`, without the
## scheme, port, or path.
static func parse_host(url: String) -> String:
	var rest := url.strip_edges()
	var scheme_end := rest.find("://")
	if scheme_end >= 0:
		rest = rest.substr(scheme_end + "://".length())
	var slash := rest.find("/")
	if slash >= 0:
		rest = rest.substr(0, slash)
	if rest.begins_with("["):
		var close := rest.find("]")
		if close > 0:
			return rest.substr(1, close - 1).to_lower()
	var colon := rest.rfind(":")
	if colon >= 0:
		rest = rest.substr(0, colon)
	return rest.to_lower()


## Returns the configured backend URL (PLATFORM_API_URL). Callers
## must check get_target_refusal() before sending anything to it;
## http_request() does.
func get_base_url() -> String:
	return OS.get_environment("PLATFORM_API_URL").strip_edges()


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


## The runtime's GAME_ADMIN_KEY, which register_game and bulk_import
## need on top of the HTTP key. Read from the GAME_ADMIN_KEY env var
## (scripts/local-smoke-test.ps1 sets the dev stack's value). Empty
## when unset.
func admin_key() -> String:
	return OS.get_environment("GAME_ADMIN_KEY")


## The game_id the suite authenticates with (see _resolve_game_id).
func game_id() -> String:
	return _resolve_game_id()


## Generic HTTP request against the configured base URL. Handles
## auth via the `auth` argument:
##
##   "" or null         — no Authorization header.
##   "basic_server_key" — Basic <base64(server_key:)>.
##   "bearer:<token>"   — Bearer <token>.
##
## For HTTP-key RPCs, append "?http_key=..." to the path (Nakama
## takes the key as a query parameter, not an Authorization
## header). `extra_headers` are sent as given ("Name: value").
##
## Returns Dictionary with keys:
##   status_code (int), body (Variant: Dictionary/Array/null),
##   text (String), error (String, empty if no transport error),
##   headers (PackedStringArray of "Name: value"; read one with
##   header_value()).
func http_request(
	method: int,
	path: String,
	body: Variant = null,
	auth: String = "",
	extra_headers: PackedStringArray = PackedStringArray(),
) -> Dictionary:
	var result: Dictionary = {
		"status_code": 0,
		"body": null,
		"text": "",
		"error": "",
		"headers": PackedStringArray(),
	}
	var refusal := get_target_refusal()
	if not refusal.is_empty():
		push_error("compliance suite refused to send: " + refusal)
		result.error = "refused: " + refusal
		return result

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
	headers.append_array(extra_headers)

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
	if err != OK:
		result.error = "http_request_error_%d" % err
		http.queue_free()
		return result

	var completed: Array = await http.request_completed
	http.queue_free()
	# Without this, the caller resumes inside the signal emission
	# above. When the caller is a test that ends there, GUT frees this
	# helper and its children at once, freeing the HTTPRequest while it
	# is still emitting. Resume on the next frame, after queue_free()
	# has deleted it.
	await get_tree().process_frame
	# request_completed signature: (result, response_code,
	# headers, body).
	var rc: int = completed[0]
	var status: int = completed[1]
	var resp_bytes: PackedByteArray = completed[3]

	if rc != HTTPRequest.RESULT_SUCCESS:
		result.error = "transport_error_%d" % rc
		return result
	result.status_code = status
	result.headers = completed[2]
	result.text = resp_bytes.get_string_from_utf8()
	if not result.text.is_empty():
		var json := JSON.new()
		if json.parse(result.text) == OK:
			result.body = json.data
	return result


## Returns the value of the response header `name` (matched
## case-insensitively) from an http_request() result, or "" when
## the response has none.
func header_value(result: Dictionary, name: String) -> String:
	var prefix := name.to_lower() + ":"
	for line: String in result.get("headers", PackedStringArray()):
		if line.to_lower().begins_with(prefix):
			return line.substr(prefix.length()).strip_edges()
	return ""


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


## True when the runtime has EDGEGAP_MOCK_DEPLOY=true set. Tests
## that exercise the matchmaker-fires-allocation path must check
## this and pending() when false — otherwise a CI run against
## prod would burn real Edgegap allocations.
##
## Reads runtime_status (HTTP-key gated) and returns false on
## missing key / RPC failure / missing field, so callers can
## treat the result as "is it safe to exercise the matchmaker
## end-to-end."
func is_mock_deploy_mode() -> bool:
	if http_key().is_empty():
		return false
	var result: Dictionary = await http_key_rpc("runtime_status")
	if result.status_code != 200:
		return false
	if not (result.inner is Dictionary):
		return false
	return bool(result.inner.get("edgegap_mock_deploy", false))


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


## Like http_key_rpc, plus an `admin_key` query parameter, for the
## operator-only RPCs (register_game, bulk_import). Pass the key to
## send; tests probing the refusal paths pass wrong ones on purpose.
func admin_key_rpc(
	rpc_name: String,
	key: String,
	body: Variant = null,
) -> Dictionary:
	var runtime_http_key := http_key()
	if runtime_http_key.is_empty():
		return {
			"status_code": 0, "body": null, "text": "",
			"error": "NAKAMA_HTTP_KEY env var not set",
			"inner": null,
		}
	var path := (
		"/v2/rpc/%s?http_key=%s&admin_key=%s&unwrap=true"
		% [rpc_name, runtime_http_key.uri_encode(), key.uri_encode()])
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


## Returns why tests must not use the Nakama console, or "" when
## they may. The console reads and writes data no client can reach
## (any account's email, for one), so it needs NAKAMA_CONSOLE_URL,
## NAKAMA_CONSOLE_USERNAME, and NAKAMA_CONSOLE_PASSWORD, and follows
## the same loopback rule as PLATFORM_API_URL. local-smoke-test.ps1
## sets all three for the dev stack.
func get_console_refusal() -> String:
	var url := console_url()
	if url.is_empty():
		return (
			"NAKAMA_CONSOLE_URL not set (local-smoke-test.ps1 sets it)")
	if (
		not _LOOPBACK_HOSTS.has(parse_host(url))
		and OS.get_environment("ALLOW_PROD_COMPLIANCE") != "1"
	):
		return (
			"NAKAMA_CONSOLE_URL=%s is not loopback; set"
			+ " ALLOW_PROD_COMPLIANCE=1 to use it"
		) % url
	return ""


## The Nakama console's base URL (NAKAMA_CONSOLE_URL), or "".
func console_url() -> String:
	return (
		OS.get_environment("NAKAMA_CONSOLE_URL").strip_edges().rstrip("/"))


## Reads an account through the console: the full api.Account,
## including the email and device ids a client's /v2/account would
## also show, for any user. Returns {} on failure.
func console_get_account(user_id: String) -> Dictionary:
	var result := await console_request(
		HTTPClient.METHOD_GET, "/v2/console/account/" + user_id, "")
	if result.status_code != 200 or not (result.body is Dictionary):
		return {}
	var account: Variant = result.body.get("account", {})
	return account if account is Dictionary else {}


## Updates an account through the console, which can set fields no
## client may (email, custom_id) and bypasses the runtime's hooks.
## `fields` uses console.UpdateAccountRequest's names. Returns
## whether the console accepted it.
func console_update_account(user_id: String, fields: Dictionary) -> bool:
	var result := await console_request(
		HTTPClient.METHOD_POST,
		"/v2/console/account/" + user_id,
		JSON.stringify(fields),
	)
	return result.status_code >= 200 and result.status_code < 300


## Sends one console API request, signing in first. Refuses (with
## push_error, a test failure) per get_console_refusal(). Returns
## {status_code, body, text}.
func console_request(method: int, path: String, body: String) -> Dictionary:
	var result := {"status_code": 0, "body": null, "text": ""}
	var refusal := get_console_refusal()
	if not refusal.is_empty():
		push_error("compliance suite refused the console: " + refusal)
		return result
	if _console_token.is_empty():
		var login := await _console_http(
			HTTPClient.METHOD_POST,
			"/v2/console/authenticate",
			JSON.stringify({
				"username": OS.get_environment("NAKAMA_CONSOLE_USERNAME"),
				"password": OS.get_environment("NAKAMA_CONSOLE_PASSWORD"),
			}),
			"",
		)
		if login.body is Dictionary:
			_console_token = str(login.body.get("token", ""))
		if _console_token.is_empty():
			return login
	return await _console_http(method, path, body, _console_token)


## Returns `text` with every JWT replaced by a placeholder. Use it
## on any response body an assertion message embeds: GUT prints the
## message on failure, and at -glog=2 and above on success too, and
## auth responses carry live session and refresh tokens.
func redact_tokens(text: String) -> String:
	var jwt := RegEx.create_from_string(_JWT_PATTERN)
	return jwt.sub(text, _REDACTED_JWT, true)


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


func _console_http(
	method: int,
	path: String,
	body: String,
	bearer_token: String,
) -> Dictionary:
	var result := {"status_code": 0, "body": null, "text": ""}
	var http := HTTPRequest.new()
	# use_threads is required in --headless (see http_request).
	http.use_threads = true
	add_child(http)
	var headers := PackedStringArray([
		"Content-Type: application/json",
		"Accept: application/json",
	])
	if not bearer_token.is_empty():
		headers.append("Authorization: Bearer " + bearer_token)
	if http.request(console_url() + path, headers, method, body) != OK:
		http.queue_free()
		return result
	var completed: Array = await http.request_completed
	http.queue_free()
	# Resume after the signal finishes emitting (see http_request).
	await get_tree().process_frame
	result.status_code = int(completed[1])
	result.text = (completed[3] as PackedByteArray).get_string_from_utf8()
	if not result.text.is_empty():
		var json := JSON.new()
		if json.parse(result.text) == OK:
			result.body = json.data
	return result
