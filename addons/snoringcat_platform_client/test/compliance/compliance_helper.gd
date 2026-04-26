extends Node
## Shared helpers for compliance tests.
##
## NO class_name — the addon's api_client.gd documents a Godot 4.6
## parser-cache bug that bites class_name'd scripts inside an
## addon. Compliance tests preload this file directly.
##
## Usage from a GutTest:
##
##   var Helper = preload(
##       "res://addons/snoringcat_platform_client/test/"
##       + "compliance/compliance_helper.gd")
##   var helper
##
##   func before_each():
##       helper = Helper.new()
##       add_child_autofree(helper)
##
##   func test_version():
##       var api = helper.make_api_client(self)
##       api.do_get("/v1/version")
##       var result = await helper.next_response(api)
##       assert_true(result.ok)
##       assert_eq(result.status_code, 200)


const _DEFAULT_BASE_URL := (
	"https://r20b7wqop6.execute-api.us-west-2.amazonaws.com/prod"
)
const _API_CLIENT_PATH := (
	"res://addons/snoringcat_platform_client/core/api_client.gd"
)
const _DEFAULT_TIMEOUT_SEC := 10.0


## Returns the live backend URL, overridable via env var so the
## hourly synthetic monitor and game CIs can target a staging URL
## without code changes.
func get_base_url() -> String:
	var override := OS.get_environment("PLATFORM_API_URL")
	if not override.is_empty():
		return override
	return _DEFAULT_BASE_URL


## True when tests should hit the live backend. Default true; flip
## off when mock-mode HTTP interception is added in a later phase.
func is_live_mode() -> bool:
	var mode := OS.get_environment(
		"PLATFORM_COMPLIANCE_MODE")
	if mode.is_empty():
		return true
	return mode == "live"


## Constructs an api_client and parents it under the given test so
## GUT autofrees it after the test. Caller still has to await one
## process_frame before firing requests so the HTTPRequest enters
## the tree.
func make_api_client(parent: Node) -> Node:
	var ApiClientScript: GDScript = load(_API_CLIENT_PATH)
	var api: Node = ApiClientScript.new()
	api.name = "ComplianceApiClient"
	api.base_url = get_base_url()
	parent.add_child(api)
	return api


## Awaits the next response_received or request_failed signal on
## the api client. Returns a Dictionary with keys:
##   ok (bool), status_code (int), body (Dictionary),
##   path (String), error (String — empty if no transport error)
## Times out after _DEFAULT_TIMEOUT_SEC and returns a synthetic
## error result so tests fail with a useful message instead of
## hanging.
func next_response(api: Node) -> Dictionary:
	var tree := api.get_tree()
	var result: Dictionary = {
		"ok": false,
		"status_code": 0,
		"body": {},
		"path": "",
		"error": "",
	}
	var done := [false]

	var on_response := func(
		ok: bool,
		status_code: int,
		body: Dictionary,
		path: String,
	) -> void:
		if done[0]:
			return
		done[0] = true
		result.ok = ok
		result.status_code = status_code
		result.body = body
		result.path = path

	var on_failed := func(error: String, path: String) -> void:
		if done[0]:
			return
		done[0] = true
		result.error = error
		result.path = path

	api.response_received.connect(on_response)
	api.request_failed.connect(on_failed)

	var elapsed := 0.0
	while not done[0] and elapsed < _DEFAULT_TIMEOUT_SEC:
		await tree.process_frame
		elapsed += tree.root.get_process_delta_time()

	api.response_received.disconnect(on_response)
	api.request_failed.disconnect(on_failed)

	if not done[0]:
		result.error = "compliance_helper_timeout_%ds" % (
			int(_DEFAULT_TIMEOUT_SEC))

	return result
