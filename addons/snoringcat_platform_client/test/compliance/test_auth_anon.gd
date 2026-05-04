extends GutTest
## Anonymous sign-in is the entry point for every player.
## Verifies Nakama issues a JWT session token via
## /v2/account/authenticate/device and the JWT shape is what
## consuming games expect (3-segment, parseable claims, includes
## `sub` = user_id).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
## Stable device_id so the test reuses the same account each
## run instead of bloating the users table. The "compliance-"
## prefix lets ops grep + prune these later if needed.
const _COMPLIANCE_DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_anon_sign_in_returns_token() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var result: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=true",
		{"id": _COMPLIANCE_DEVICE_ID},
		"basic_server_key")

	assert_true(
		result.error.is_empty(),
		"transport error: %s" % result.error)
	assert_eq(
		result.status_code, 200,
		"non-200: body=%s" % result.text)

	# Nakama's auth response shape: {created, token,
	# refresh_token}. token + refresh_token are JWTs.
	assert_true(
		result.body is Dictionary,
		"body not a dict: %s" % result.text)
	var b: Dictionary = result.body
	assert_true(
		b.has("token"),
		"missing token: %s" % str(b))
	assert_true(
		b.has("refresh_token"),
		"missing refresh_token: %s" % str(b))

	var token: String = str(b.get("token", ""))
	# Sanity check the token is JWT-shaped.
	assert_true(
		token.length() > 50,
		"token suspiciously short: %d chars" % token.length())
	assert_eq(
		token.split(".").size(), 3,
		"token is not a 3-segment JWT")


func test_anon_token_carries_user_id_claim() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var result: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=true",
		{"id": _COMPLIANCE_DEVICE_ID},
		"basic_server_key")
	assert_eq(
		result.status_code, 200,
		"anon sign-in must succeed; body=%s" % result.text)
	var token: String = str(result.body.get("token", ""))

	var claims: Dictionary = _helper.decode_jwt_claims(token)
	assert_false(
		claims.is_empty(),
		"failed to decode JWT claims")
	# Nakama's JWT claims include `uid` (user id), `usn`
	# (username), `tid` (token id), `exp`, `iat`. The contract
	# we expose to consuming games is just "user id is on the
	# token" — be liberal about which claim carries it.
	var has_user_id := (
		claims.has("uid")
		or claims.has("user_id")
		or claims.has("sub"))
	assert_true(
		has_user_id,
		"JWT carries no user-id claim (expected uid/user_id/sub)"
			+ "; claims=%s" % str(claims))
	assert_true(
		claims.has("exp"),
		"JWT missing exp claim: %s" % str(claims))


func test_session_token_unlocks_account_endpoint() -> void:
	# Auth -> use the token to read /v2/account. Catches the
	# class of regressions where Nakama issues a JWT but the
	# Bearer-auth pipe is broken.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(
		_COMPLIANCE_DEVICE_ID)
	assert_false(token.is_empty(), "anon auth failed")

	var result: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	assert_eq(
		result.status_code, 200,
		"/v2/account returned %d: %s"
			% [result.status_code, result.text])
	assert_true(
		result.body is Dictionary,
		"account body not a dict: %s" % result.text)
	# Nakama's /v2/account returns nested {user: {...},
	# devices: [...], wallet: ...}. Confirm the user block has
	# an id (the canonical user_id consumers reference).
	var user: Variant = result.body.get("user")
	assert_true(
		user is Dictionary and (user as Dictionary).has("id"),
		"/v2/account missing user.id: %s" % result.text)
