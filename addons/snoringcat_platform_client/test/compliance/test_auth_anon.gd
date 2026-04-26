extends GutTest
## Anonymous sign-in is the entry point for every player. Verifies
## the platform issues a JWT with the expected claims (game_id,
## player_id) and returns a usable account record.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
## Stable device_id so the test reuses the same account each
## run instead of bloating the accounts table. The "compliance-"
## prefix lets ops grep for and prune these later if needed.
const _COMPLIANCE_DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_anon_sign_in_returns_token_and_player_id() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	var api: Node = _helper.make_api_client(self)
	await get_tree().process_frame
	api.do_post("/v1/auth/anon", {
		"device_id": _COMPLIANCE_DEVICE_ID,
	})
	var result: Dictionary = await _helper.next_response(api)

	assert_true(
		result.error.is_empty(),
		"transport error: %s" % result.error)
	assert_true(
		result.ok,
		"non-2xx: %d body=%s"
			% [result.status_code, str(result.body)])

	# The platform contract: anon sign-in returns a JWT plus a
	# bare-minimum account snapshot. Future schema additions are
	# fine; missing required keys here would break every game.
	assert_true(
		result.body.has("jwt_token"),
		"missing jwt_token: %s" % str(result.body))
	assert_true(
		result.body.has("player_id"),
		"missing player_id: %s" % str(result.body))
	assert_true(
		result.body.has("refresh_token"),
		"missing refresh_token: %s" % str(result.body))

	var token: String = result.body.get("jwt_token", "")
	assert_true(
		token.length() > 50,
		"token suspiciously short: %d chars" % token.length())
	# JWT format: three dot-separated base64url segments.
	assert_eq(
		token.split(".").size(), 3,
		"token is not a 3-segment JWT")


func test_anon_token_contains_game_id_claim() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	var api: Node = _helper.make_api_client(self)
	await get_tree().process_frame
	api.do_post("/v1/auth/anon", {
		"device_id": _COMPLIANCE_DEVICE_ID,
	})
	var result: Dictionary = await _helper.next_response(api)

	assert_true(result.ok, "anon sign-in must succeed")
	var token: String = result.body.get("jwt_token", "")
	var parts := token.split(".")
	assert_eq(parts.size(), 3, "JWT format")

	# Decode the claims segment (base64url, no padding).
	var claims_b64: String = parts[1]
	# Translate base64url -> base64 and pad.
	claims_b64 = claims_b64.replace("-", "+").replace("_", "/")
	while claims_b64.length() % 4 != 0:
		claims_b64 += "="
	var claims_bytes := Marshalls.base64_to_raw(claims_b64)
	var claims_text := claims_bytes.get_string_from_utf8()
	var json := JSON.new()
	var err := json.parse(claims_text)
	assert_eq(err, OK, "claims parse: %s" % claims_text)
	var claims: Dictionary = json.data
	assert_true(
		claims.has("game_id"),
		"JWT missing game_id claim: %s" % str(claims))
	# The standard JWT subject claim "sub" carries the player_id;
	# the platform doesn't duplicate it under a "player_id" key.
	assert_true(
		claims.has("sub"),
		"JWT missing sub (player_id) claim: %s" % str(claims))
	assert_eq(
		claims.get("sub", ""), result.body.get("player_id", ""),
		"sub claim should match response.player_id")
	assert_true(
		claims.has("exp"),
		"JWT missing exp claim: %s" % str(claims))
