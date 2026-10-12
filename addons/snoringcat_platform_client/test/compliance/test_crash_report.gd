extends GutTest
## crash_report RPC (runtime/crash_report.go). Game clients report
## through their session with role "client"; dedicated servers
## report through the HTTP key with role "server". A report is
## stored under the system user in the crash_reports collection,
## server-only, with nothing that names the caller.
##
## No client can read that collection, so the read-back goes
## through the Nakama console API. local-smoke-test.ps1 passes the
## dev stack's console as NAKAMA_CONSOLE_URL,
## NAKAMA_CONSOLE_USERNAME, and NAKAMA_CONSOLE_PASSWORD. Without
## them the read-back test pends. The console URL follows the same
## loopback rule as PLATFORM_API_URL.
##
## Contract:
##   request:  {game_id, game_version, protocol_version, platform,
##              os, role, message, signature, log_excerpt?}
##   response: {accepted: true, id: <storage key>}
##   errors:   400 (code 3) for any invalid report, 429 (code 8)
##             past a rate limit (5 per hour per session user).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _RPC := "crash_report"
const _COLLECTION := "crash_reports"
const _SYSTEM_USER_ID := "00000000-0000-0000-0000-000000000000"
const _USER_PREFIX := "compliance-crash"
const _SIGNATURE := "c0ffee00c0ffee00"
const _UNKNOWN_GAME_ID := "compliance-unknown-game"
const _HTTP_OK := 200
const _HTTP_BAD_REQUEST := 400
const _HTTP_UNAUTHORIZED := 401
const _HTTP_TOO_MANY_REQUESTS := 429
const _GRPC_INVALID_ARGUMENT := 3
const _GRPC_RESOURCE_EXHAUSTED := 8
const _USER_REPORT_LIMIT := 5
const _MAX_PAYLOAD_BYTES := 16384

var _helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_crash_report_rpc_is_registered() -> void:
	if not _can_run_http_key_tests():
		return
	var result: Dictionary = await _helper.http_key_rpc("runtime_status")
	assert_eq(result.status_code, _HTTP_OK, "runtime_status")
	assert_true(result.inner is Dictionary, "runtime_status payload")
	if result.inner is Dictionary:
		assert_has(result.inner.get("registered_rpcs", []), _RPC)


func test_session_report_is_stored_server_only_without_identity() -> void:
	if not _can_run_session_tests():
		return
	if _console_url().is_empty():
		pending(
			"NAKAMA_CONSOLE_URL not set; can't read crash_reports"
			+ " back (local-smoke-test.ps1 sets it)")
		return
	var user := await _new_user()
	if user.is_empty():
		return
	var jwt := _fake_jwt()
	var report := _report("client")
	report["log_excerpt"] = (
		"Authorization: Bearer %s\nuser %s left" % [jwt, user.user_id])
	# Unknown fields are ignored, never stored.
	report["player_id"] = user.user_id

	var result: Dictionary = await _helper.session_rpc(
		_RPC, str(user.token), report)
	assert_eq(
		result.status_code, _HTTP_OK,
		"crash_report: %s" % _helper.redact_tokens(result.text))
	if not result.inner is Dictionary:
		fail_test("crash_report returned no payload")
		return
	assert_true(bool(result.inner.get("accepted", false)), "accepted")
	var key := str(result.inner.get("id", ""))
	assert_false(key.is_empty(), "storage key returned")

	var stored := await _console_read_report(key)
	assert_eq(str(stored.get("collection", "")), _COLLECTION)
	assert_eq(
		str(stored.get("user_id", "")), _SYSTEM_USER_ID,
		"owned by the system user")
	assert_eq(int(stored.get("permission_read", -1)), 0, "no client read")
	assert_eq(
		int(stored.get("permission_write", -1)), 0, "no client write")
	var raw_value := str(stored.get("value", ""))
	var value: Variant = JSON.parse_string(raw_value)
	if not value is Dictionary:
		fail_test("stored value is not a JSON object")
		return
	assert_eq(str(value.get("role", "")), "client")
	assert_eq(str(value.get("signature", "")), _SIGNATURE)
	assert_eq(str(value.get("message", "")), str(report.message))
	assert_eq(int(value.get("protocol_version", -1)), 2)
	assert_false(
		raw_value.contains(str(user.user_id)),
		"the stored report names the caller")
	assert_false(raw_value.contains("player_id"), "player_id stored")
	assert_false(raw_value.contains(jwt), "token stored unredacted")


func test_client_cannot_list_crash_reports() -> void:
	if not _can_run_session_tests():
		return
	var user := await _new_user()
	if user.is_empty():
		return
	var report_result: Dictionary = await _helper.session_rpc(
		_RPC, str(user.token), _report("client"))
	assert_eq(report_result.status_code, _HTTP_OK, "report stored")

	var listing: Dictionary = await _helper.http_get(
		"/v2/storage/%s?user_id=%s&limit=100"
			% [_COLLECTION, _SYSTEM_USER_ID],
		"bearer:" + str(user.token))
	assert_eq(listing.status_code, _HTTP_OK, "listing call")
	var objects: Variant = null
	if listing.body is Dictionary:
		objects = listing.body.get("objects", [])
	assert_true(
		objects == null or (objects is Array and objects.is_empty()),
		"a client listed crash reports: %s" % listing.text)


func test_server_report_with_http_key_is_accepted() -> void:
	if not _can_run_http_key_tests():
		return
	var result: Dictionary = await _helper.http_key_rpc(
		_RPC, _report("server"))
	assert_eq(result.status_code, _HTTP_OK, "server report: %s" % result.text)
	assert_true(
		result.inner is Dictionary
		and bool(result.inner.get("accepted", false)),
		"accepted")


func test_invalid_reports_are_rejected() -> void:
	if not _can_run_session_tests() or not _can_run_http_key_tests():
		return
	var user := await _new_user()
	if user.is_empty():
		return
	var token := str(user.token)

	var session_as_server: Dictionary = await _helper.session_rpc(
		_RPC, token, _report("server"))
	_assert_invalid(session_as_server, "session caller claiming server")

	var key_as_client: Dictionary = await _helper.http_key_rpc(
		_RPC, _report("client"))
	_assert_invalid(key_as_client, "HTTP-key caller claiming client")

	var unknown_game := _report("server")
	unknown_game["game_id"] = _UNKNOWN_GAME_ID
	_assert_invalid(
		await _helper.http_key_rpc(_RPC, unknown_game), "unknown game_id")

	var oversized := _report("client")
	oversized["log_excerpt"] = "x".repeat(_MAX_PAYLOAD_BYTES)
	_assert_invalid(
		await _helper.session_rpc(_RPC, token, oversized),
		"payload over 16 KiB")

	var bad_signature := _report("client")
	bad_signature["signature"] = "not-a-hash"
	_assert_invalid(
		await _helper.session_rpc(_RPC, token, bad_signature),
		"malformed signature")

	_assert_invalid(
		await _helper.session_rpc(_RPC, token, "{not json"),
		"payload that is not JSON")

	var anonymous: Dictionary = await _helper.http_post(
		"/v2/rpc/%s?unwrap=true" % _RPC, _report("client"))
	assert_eq(
		anonymous.status_code, _HTTP_UNAUTHORIZED,
		"no session and no HTTP key: %s" % anonymous.text)


func test_session_user_is_rate_limited() -> void:
	if not _can_run_session_tests():
		return
	var user := await _new_user()
	if user.is_empty():
		return
	for i in _USER_REPORT_LIMIT:
		var accepted: Dictionary = await _helper.session_rpc(
			_RPC, str(user.token), _report("client"))
		assert_eq(
			accepted.status_code, _HTTP_OK,
			"report %d: %s" % [i + 1, accepted.text])
	var limited: Dictionary = await _helper.session_rpc(
		_RPC, str(user.token), _report("client"))
	assert_eq(limited.status_code, _HTTP_TOO_MANY_REQUESTS, limited.text)
	assert_eq(_body_code(limited), _GRPC_RESOURCE_EXHAUSTED)


# ------------------------------------------------------------------
# Helpers
# ------------------------------------------------------------------


func _can_run_session_tests() -> bool:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return false
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return false
	return true


func _can_run_http_key_tests() -> bool:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return false
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return false
	return true


## Mints a one-shot anonymous account, deleted in after_each.
## Returns {} (and pends) when sign-in fails.
func _new_user() -> Dictionary:
	var users: Array = await _helper.multi_session_anon(1, _USER_PREFIX)
	if users.size() != 1:
		pending("multi_session_anon did not return a user")
		return {}
	_users.append_array(users)
	return users[0]


func _report(role: String) -> Dictionary:
	return {
		"game_id": _helper._resolve_game_id(),
		"game_version": "0.0.1",
		"protocol_version": 2,
		"platform": "linux",
		"os": "Compliance Suite",
		"role": role,
		"message": "ERROR  : compliance crash report",
		"signature": _SIGNATURE,
		"log_excerpt": "compliance line one\ncompliance line two",
	}


func _assert_invalid(result: Dictionary, what: String) -> void:
	assert_eq(
		result.status_code, _HTTP_BAD_REQUEST,
		"%s: %s" % [what, _helper.redact_tokens(result.text)])
	assert_eq(_body_code(result), _GRPC_INVALID_ARGUMENT, what)


func _body_code(result: Dictionary) -> int:
	if result.body is Dictionary:
		return int(result.body.get("code", -1))
	return -1


func _console_url() -> String:
	return OS.get_environment("NAKAMA_CONSOLE_URL").strip_edges().rstrip("/")


## Reads one crash report through the console API. Returns {} on
## any failure, after failing the test with the reason.
func _console_read_report(key: String) -> Dictionary:
	var base := _console_url()
	var host: String = _Helper.parse_host(base)
	if (
		not host in ["127.0.0.1", "localhost", "::1"]
		and OS.get_environment("ALLOW_PROD_COMPLIANCE") != "1"
	):
		fail_test((
			"NAKAMA_CONSOLE_URL=%s is not loopback; set"
			+ " ALLOW_PROD_COMPLIANCE=1 to read it"
		) % base)
		return {}
	var login := await _console_request(
		HTTPClient.METHOD_POST,
		base + "/v2/console/authenticate",
		JSON.stringify({
			"username": OS.get_environment("NAKAMA_CONSOLE_USERNAME"),
			"password": OS.get_environment("NAKAMA_CONSOLE_PASSWORD"),
		}),
		"",
	)
	var token := ""
	if login.body is Dictionary:
		token = str(login.body.get("token", ""))
	if token.is_empty():
		fail_test("console sign-in failed: HTTP %d" % login.status_code)
		return {}
	# Keys hold only [0-9a-fTZ:.-], all legal in a URL path.
	var read := await _console_request(
		HTTPClient.METHOD_GET,
		"%s/v2/console/storage/%s/%s/%s" % [
			base, _COLLECTION, key, _SYSTEM_USER_ID,
		],
		"",
		token,
	)
	if read.status_code != _HTTP_OK or not read.body is Dictionary:
		fail_test("console read failed: HTTP %d %s" % [
			read.status_code, read.text,
		])
		return {}
	return read.body


func _console_request(
	method: int,
	url: String,
	body: String,
	bearer_token: String,
) -> Dictionary:
	var http := HTTPRequest.new()
	# use_threads is required in --headless (see compliance_helper).
	http.use_threads = true
	add_child_autofree(http)
	var headers := PackedStringArray([
		"Content-Type: application/json",
		"Accept: application/json",
	])
	if not bearer_token.is_empty():
		headers.append("Authorization: Bearer " + bearer_token)
	var result := {"status_code": 0, "body": null, "text": ""}
	if http.request(url, headers, method, body) != OK:
		return result
	var completed: Array = await http.request_completed
	# Resume after the signal finishes emitting, as the helper does.
	await get_tree().process_frame
	result.status_code = int(completed[1])
	result.text = (completed[3] as PackedByteArray).get_string_from_utf8()
	result.body = JSON.parse_string(result.text)
	return result


## A JWT-shaped string built at run time, so no token literal sits
## in the source.
func _fake_jwt() -> String:
	return "%s.%s.ZmFrZS1zaWduYXR1cmU" % [
		_base64url("{\"alg\":\"none\"}"),
		_base64url("{\"uid\":\"compliance\"}"),
	]


func _base64url(text: String) -> String:
	return (
		Marshalls.utf8_to_base64(text)
		.replace("+", "-")
		.replace("/", "_")
		.replace("=", "")
	)
