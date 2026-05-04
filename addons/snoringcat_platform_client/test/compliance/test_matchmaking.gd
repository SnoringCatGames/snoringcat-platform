extends GutTest
## Matchmaker readiness. The full matchmaking flow uses Nakama's
## realtime socket (rt-api), which Godot's HTTPRequest can't
## drive — that's a follow-up phase that needs a real socket
## test rig.
##
## What we CAN verify over HTTP:
##   1. The matchmaker hook is registered (EDGEGAP_TOKEN set
##      causes the runtime to register matchmaker_matched, which
##      surfaces in runtime_status).
##   2. The Edgegap config (app name, version) the hook uses for
##      allocations is populated and not still a placeholder.
##
## Both already covered by test_version.gd's
## test_runtime_status_reports_edgegap_config — this test is a
## placeholder for the deeper socket flow once a rig exists,
## documenting what's missing.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_matchmaker_hook_registered_via_runtime_status() -> void:
	# Belt-and-suspenders cross-check: the hook is registered
	# iff edgegap_token_set is true AND edgegap_app_name doesn't
	# contain "${" (placeholder). test_version.gd asserts both
	# directly; this test asserts they imply hook registration
	# without the test file ordering becoming load-bearing.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return

	var result: Dictionary = await _helper.http_key_rpc("runtime_status")
	assert_eq(result.status_code, 200)
	var info: Dictionary = result.inner
	assert_true(info is Dictionary)

	var token_set: bool = info.get("edgegap_token_set", false)
	var app_name: String = str(info.get("edgegap_app_name", ""))
	var hook_active := (
		token_set
		and not app_name.is_empty()
		and not app_name.contains("${"))
	assert_true(
		hook_active,
		"matchmaker hook not in a healthy state:"
			+ " token_set=%s app_name='%s'"
			% [str(token_set), app_name])


func test_matchmaker_socket_flow_pending() -> void:
	# Documents the gap. When a socket test rig exists, this
	# becomes a real test: open a realtime socket, call
	# matchmaker_add, expect a matchmaker_matched envelope back
	# with a server allocation.
	pending("realtime-socket test rig not implemented yet")
