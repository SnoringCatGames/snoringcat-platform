extends GutTest
## update_and_get_presence RPC: writes the caller's presence row
## and returns every online friend's presence in one round trip.
## Powers the "online friends" indicator. The contract:
##   request:  {rich_presence: string, status: string}
##              status defaults to "online" when absent
##   response: {online_ids: [string], online_friends: {<id>: rec}}
## Auth: Bearer session token (requireClientSession in runtime).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_presence_write_returns_well_formed_response() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty(), "anon auth failed")

	# Empty payload exercises the default-status path.
	var result: Dictionary = await _helper.session_rpc(
		"update_and_get_presence", token,
		{"rich_presence": "compliance test", "status": "online"})
	assert_eq(
		result.status_code, 200,
		"update_and_get_presence: %s" % result.text)
	assert_true(
		result.inner is Dictionary,
		"inner not a dict: %s" % result.text)

	# Both fields are required by the SDK. online_ids is the
	# fast lookup; online_friends is the rich payload.
	var inner: Dictionary = result.inner
	assert_true(
		inner.has("online_ids"),
		"missing online_ids: %s" % str(inner))
	assert_true(
		inner.has("online_friends"),
		"missing online_friends: %s" % str(inner))
	assert_true(
		inner.online_ids is Array,
		"online_ids not an array")
	assert_true(
		inner.online_friends is Dictionary,
		"online_friends not a dict")


func test_presence_rejects_unauthenticated_call() -> void:
	# server_to_server callers (http_key) must NOT be able to
	# write someone's presence. The runtime's requireClientSession
	# enforces this; verify the gate is wired.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return

	var result: Dictionary = await _helper.http_key_rpc(
		"update_and_get_presence",
		{"rich_presence": "", "status": "online"})
	# Nakama maps the runtime's UNAUTHENTICATED (16) error to
	# HTTP 401 in the REST gateway. A 200 here means the gate is
	# missing — anyone with NAKAMA_HTTP_KEY could forge presence.
	assert_ne(
		result.status_code, 200,
		"presence accepted http_key call (auth gate broken!)"
			+ " body=%s" % result.text)
