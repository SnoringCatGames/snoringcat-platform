extends GutTest
## Session token refresh. Nakama issues both an access token
## (token, ~2h) and a refresh token (~7 days) on every auth
## response. The SDK uses the refresh token to mint a new
## access token without prompting the user to sign in again.
##
## Endpoint: POST /v2/account/session/refresh
## Auth:     Basic server-key (refresh is a stateless server
##           call, not a session call).
## Body:     {token: "<refresh_token>", vars?: {...}}
## Response: {created, token, refresh_token} — same shape as
##           /v2/account/authenticate/device, with a fresh
##           access token + a rotated refresh token.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_refresh_returns_fresh_session_pair() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	# Step 1: anon-auth, capture refresh_token.
	var auth: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=true",
		_helper.device_auth_body(_DEVICE_ID),
		"basic_server_key")
	assert_eq(
		auth.status_code, 200,
		"initial auth: %s" % auth.text)
	var refresh_token: String = str(
		auth.body.get("refresh_token", ""))
	assert_false(
		refresh_token.is_empty(),
		"no refresh_token on initial auth")

	# Step 2: refresh.
	var refresh: Dictionary = await _helper.http_post(
		"/v2/account/session/refresh",
		_helper.session_refresh_body(refresh_token),
		"basic_server_key")
	assert_eq(
		refresh.status_code, 200,
		"session refresh: %s" % refresh.text)
	assert_true(
		refresh.body is Dictionary,
		"refresh body not a dict: %s" % refresh.text)

	var new_token: String = str(refresh.body.get("token", ""))
	var new_refresh: String = str(
		refresh.body.get("refresh_token", ""))
	assert_false(
		new_token.is_empty(),
		"refresh response missing token")
	assert_false(
		new_refresh.is_empty(),
		"refresh response missing refresh_token")
	# JWT shape sanity.
	assert_eq(
		new_token.split(".").size(), 3,
		"refresh access token isn't a 3-segment JWT")


func test_refresh_token_unlocks_account_endpoint() -> void:
	# End-to-end: refresh-derived access token must work for
	# Bearer auth. Catches regressions where Nakama issues a
	# token but the auth pipe drops it.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var auth: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=true",
		_helper.device_auth_body(_DEVICE_ID),
		"basic_server_key")
	var refresh: Dictionary = await _helper.http_post(
		"/v2/account/session/refresh",
		_helper.session_refresh_body(
			str(auth.body.get("refresh_token", ""))),
		"basic_server_key")
	assert_eq(refresh.status_code, 200)

	var new_token: String = str(refresh.body.get("token", ""))
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + new_token)
	assert_eq(
		account.status_code, 200,
		"/v2/account with refresh-derived token: %s"
			% account.text)


func test_refresh_rejects_garbage_token() -> void:
	# Negative path: a malformed/expired refresh token should
	# return 4xx, not 5xx. Catches regressions where the parser
	# crashes on unexpected input.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var result: Dictionary = await _helper.http_post(
		"/v2/account/session/refresh",
		{"token": "not-a-real-jwt"},
		"basic_server_key")
	assert_lt(
		result.status_code, 500,
		"refresh 5xx'd on garbage token: %s" % result.text)
	assert_gt(
		result.status_code, 0,
		"transport error: %s" % result.error)
	assert_true(
		result.status_code >= 400,
		"refresh accepted garbage token (status=%d)"
			% result.status_code)
