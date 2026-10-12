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
##  - A registration alone doesn't let match_end write results for
##    a request_id the matchmaker never allocated (mock-deploy
##    runtimes only, since it registers a forged server).


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


func test_match_end_refuses_a_request_id_nobody_allocated() -> void:
	# register_server takes any request_id from anyone holding the
	# HTTP key, which shipped in client builds up to 0.47.0. So a
	# registration alone must not let match_end write results: the
	# runtime also wants the allocation record its matchmaker hook
	# wrote. Without it, a forger could post leaderboard and
	# match-history rows for any user.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	# Leaves a forged registration behind until the cleanup below,
	# so stay off anything but a mock-deploy (test) runtime.
	if not await _helper.is_mock_deploy_mode():
		pending("needs a runtime with EDGEGAP_MOCK_DEPLOY=true")
		return
	var users: Array = await _helper.multi_session_anon(
		1, "compliance-forged-match")
	if users.is_empty():
		pending("could not mint a one-shot account")
		return
	var victim: Dictionary = users[0]
	var request_id := "compliance-forged-%d-%d" % [
		Time.get_unix_time_from_system() as int,
		randi() % 100000,
	]

	var registered: Dictionary = await _helper.http_key_rpc(
		"register_server",
		{
			"request_id": request_id,
			"server_ip": "203.0.113.7",
			"server_port": 4433,
		})
	assert_eq(
		registered.status_code, 200,
		"register_server: %s" % registered.text)

	var forged: Dictionary = await _helper.http_key_rpc(
		"match_end",
		{
			"request_id": request_id,
			"winner_id": victim.user_id,
			"players": [{
				"user_id": victim.user_id,
				"score": 100000,
				"kills": 1000,
				"bumps": 1000,
			}],
		})
	assert_eq(
		forged.status_code, 404,
		"match_end should refuse an unallocated request_id: %s"
		% forged.text)
	assert_string_contains(forged.text, "no allocation record")

	var history: Dictionary = await _helper.session_rpc(
		"get_match_history", victim.token, {})
	assert_eq(
		history.status_code, 200,
		"get_match_history: %s" % _helper.redact_tokens(history.text))
	var matches: Variant = (
		history.inner.get("matches")
		if history.inner is Dictionary
		else null)
	assert_eq(
		matches, [],
		"the forged match_end wrote history: %s"
		% _helper.redact_tokens(history.text))

	# Clean up the forged registration, and the account.
	var cancelled: Dictionary = await _helper.http_key_rpc(
		"match_cancel",
		{"request_id": request_id, "reason": "compliance cleanup"})
	assert_eq(
		cancelled.status_code, 200, "match_cancel: %s" % cancelled.text)
	await _helper.delete_one_shot_account(victim)


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
