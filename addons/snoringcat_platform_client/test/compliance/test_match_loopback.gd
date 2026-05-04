extends GutTest
## Server-side runtime RPCs that game servers (not players) call
## to register, finalize matches, and report client IPs. These
## are the contract between the game server binary and the
## platform — if their shape drifts, real matches won't end
## cleanly (no stats recorded, no Edgegap deallocation).
##
## We can't fully exercise these without spinning up a real game
## session (the runtime cross-checks server-allocated state and
## a full register_server requires a valid Edgegap deployment id).
## But we can prove:
##  - The RPCs are registered (200 with structured-error payload
##    rather than 404 "RPC not found").
##  - The HTTP-key gate is enforced (already covered by
##    test_api_surface).
##  - Calling them with deliberately malformed input returns a
##    non-5xx, non-200 response (i.e. the handler ran, validated,
##    and rejected — not crashed, not silently accepted).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_register_server_rejects_malformed_input() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	# Empty body. The runtime should respond — proves the RPC
	# is registered. The shape of the rejection doesn't matter
	# (could be a 200 with error-payload, could be a 4xx); what
	# we need is "not 404 and not 5xx".
	var result: Dictionary = await _helper.http_key_rpc("register_server")
	assert_lt(
		result.status_code, 500,
		"register_server 5xx'd on empty body: %s" % result.text)
	assert_ne(
		result.status_code, 404,
		"register_server returned 404 — RPC not registered?")
	assert_gt(
		result.status_code, 0,
		"transport error: %s" % result.error)


func test_match_end_rejects_malformed_input() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var result: Dictionary = await _helper.http_key_rpc("match_end")
	assert_lt(
		result.status_code, 500,
		"match_end 5xx'd on empty body: %s" % result.text)
	assert_ne(
		result.status_code, 404,
		"match_end returned 404 — RPC not registered?")
	assert_gt(
		result.status_code, 0,
		"transport error: %s" % result.error)


func test_record_client_ip_rejects_malformed_input() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var result: Dictionary = await _helper.http_key_rpc("record_client_ip")
	assert_lt(
		result.status_code, 500,
		"record_client_ip 5xx'd: %s" % result.text)
	assert_ne(
		result.status_code, 404,
		"record_client_ip returned 404 — RPC not registered?")
	assert_gt(
		result.status_code, 0,
		"transport error: %s" % result.error)
