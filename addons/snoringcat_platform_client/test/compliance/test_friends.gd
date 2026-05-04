extends GutTest
## Friends list contract. Nakama exposes /v2/friend (singular,
## query-paramed) for add/remove/list. The SDK's friends UI
## reads this and renders it as a list with online status.
##
## We deliberately don't add a real friend (would require two
## stable users). Instead we exercise:
##  - GET /v2/friend with a valid session returns 200 + a
##    well-formed list (possibly empty).
##  - POST /v2/friend with a syntactically valid but non-existent
##    user id returns a known error (not a 5xx). Confirms the
##    add-friend pipe is wired even when no real target exists.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_friends_list_returns_well_formed() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty())

	var result: Dictionary = await _helper.http_get(
		"/v2/friend", "bearer:" + token)
	assert_eq(
		result.status_code, 200,
		"/v2/friend GET: %s" % result.text)
	# Nakama returns {friends: [...], cursor: ""}. friends may
	# be absent when the list is empty (Nakama sometimes omits
	# empty arrays). Accept either shape.
	assert_true(
		result.body is Dictionary,
		"body not a dict: %s" % result.text)
	if result.body.has("friends"):
		assert_true(
			result.body.friends is Array,
			"friends key not an array: %s" % result.text)


func test_add_nonexistent_friend_returns_known_error() -> void:
	# Sending a syntactically valid user id (UUID v4 zeroes)
	# that points to no user should return a structured error,
	# not a 500.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	var fake_id := "00000000-0000-0000-0000-000000000000"
	var result: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + fake_id, null, "bearer:" + token)
	# Nakama can return 400 / 404 / 200 with empty effect for
	# this depending on version. The fail mode we're catching
	# is a 5xx, which would mean the handler crashed.
	assert_lt(
		result.status_code, 500,
		"add-friend with bogus id 5xx'd"
			+ " (status=%d body=%s)"
			% [result.status_code, result.text])
	assert_gt(
		result.status_code, 0,
		"transport failure: %s" % result.error)
