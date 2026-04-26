extends GutTest
## /v1/version is the simplest health check on the platform.
## If this fails, nothing else in the suite is meaningful.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_version_returns_200() -> void:
	if not _helper.is_live_mode():
		pending(
			"mock mode not yet implemented;"
			+ " skipping live /v1/version probe")
		return
	var api: Node = _helper.make_api_client(self)
	await get_tree().process_frame
	api.do_get("/v1/version")
	var result: Dictionary = await _helper.next_response(api)

	assert_true(
		result.error.is_empty(),
		"transport error: %s" % result.error)
	assert_true(
		result.ok,
		"non-2xx status: %d body=%s"
			% [result.status_code, str(result.body)])
	assert_eq(result.status_code, 200)


func test_version_payload_shape() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	var api: Node = _helper.make_api_client(self)
	await get_tree().process_frame
	api.do_get("/v1/version")
	var result: Dictionary = await _helper.next_response(api)

	assert_true(result.ok, "request must succeed")
	assert_true(
		result.body.has("game_version"),
		"missing game_version in body: %s" % str(result.body))
	assert_true(
		result.body.has("protocol_version"),
		"missing protocol_version in body: %s"
			% str(result.body))
