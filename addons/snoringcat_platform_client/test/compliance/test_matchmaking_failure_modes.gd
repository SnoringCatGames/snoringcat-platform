extends GutTest
## Matchmaker failure-path notifications over the Nakama
## realtime socket. Stage 8.21.
##
## Today the runtime emits one synchronous, deterministic
## failure subject — `match_failed` with reason
## `protocol_mismatch` (Stage 3.9). This test exercises that
## path:
##   1. Authenticate one anonymous user.
##   2. Add a min=max=1 matchmaker ticket whose
##      `client_protocol_version` deliberately mismatches
##      the registered game's protocol_version.
##   3. Assert `received_notification` arrives with subject
##      `match_failed` carrying the documented payload
##      (reason / expected / got / message).
##   4. Assert NO `match_ready` arrives — the runtime aborts
##      before allocating Edgegap, so no deploy is burned.
##
## Other failure modes the audit catalogs (Edgegap 503 mid-
## allocation, lost notification timeout, fleet allocator
## panics) require fault-injection hooks the runtime
## doesn't yet expose; revisit when Stage 7.1 introduces
## allocation retry + injectable failure surfaces.
##
## Gated on EDGEGAP_MOCK_DEPLOY=true so a misconfigured
## production never burns paid allocations from the
## protocol-mismatch happy path. Mock mode is also where the
## abort surface lives in the runtime — real mode would still
## hit it, but we use the mock flag as a "safe to exercise
## the matchmaker" gate consistently across Stage 8.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
## A deliberately-wrong protocol version. The registered
## value (set in game.yaml::protocol_version + verified by
## the game-config-parity CI guard against project.godot)
## is single-digit; 999 will never collide. If a future
## hopnbop release ever ships protocol_version=999 this
## sentinel needs to change.
const _BOGUS_PROTOCOL_VERSION := "999"
## How long we wait for the match_failed notification. The
## runtime aborts synchronously inside OnMatchmakerMatched,
## so this is sub-second in practice; 8 s upper bound for
## CI load.
const _ABORT_TIMEOUT_SEC := 8.0
## How long we then wait to confirm match_ready did NOT
## arrive. The match_failed path returns "" from
## OnMatchmakerMatched before any allocation; this is
## belt-and-suspenders.
const _NO_MATCH_READY_WAIT_SEC := 2.0

var _helper
var _sock_helper
var _user: Dictionary = {}


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


func after_each() -> void:
	if not _user.is_empty():
		await _helper.delete_one_shot_account(_user)
		_user = {}


func test_protocol_mismatch_emits_match_failed() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return
	var mock_on: bool = await _helper.is_mock_deploy_mode()
	if not mock_on:
		pending(
			"EDGEGAP_MOCK_DEPLOY=true required on the runtime;"
			+ " this test gates on the mock flag consistently"
			+ " with the rest of Stage 8 even though the abort"
			+ " path itself doesn't allocate.")
		return

	# Look up the registered protocol_version so we can
	# assert the abort's `expected` field. version_check is
	# HTTP-key gated; we pass game_id so the runtime returns
	# the per-game value.
	var game_id := _resolve_game_id_for_ticket()
	var vc: Dictionary = await _helper.http_key_rpc(
		"version_check",
		{"game_id": game_id, "client_version": "0.0.0"})
	if vc.status_code != 200:
		pending(
			"version_check RPC returned %d; cannot resolve"
				% vc.status_code
			+ " expected protocol_version for assertion.")
		return
	if not (vc.inner is Dictionary):
		pending(
			"version_check returned non-dict body: %s"
				% str(vc.inner))
		return
	# version_check returns the server's int protocol on a
	# `protocol_version` field; pinned by
	# snoringcat-platform/runtime/version.go's
	# versionCheckResponse.
	var expected_protocol: int = int(
		vc.inner.get("protocol_version", 0))
	if expected_protocol <= 0:
		pending(
			"version_check did not surface a positive"
			+ " protocol_version (got %d). Skipping —"
				% expected_protocol
			+ " runtime may predate Stage 3.9.")
		return
	# Sanity check our sentinel really is wrong. If a future
	# hopnbop release shipped protocol_version=999, this test
	# would falsely pass.
	assert_ne(
		expected_protocol, int(_BOGUS_PROTOCOL_VERSION),
		"_BOGUS_PROTOCOL_VERSION (%s) collides with the live"
			% _BOGUS_PROTOCOL_VERSION
			+ " registered protocol_version (%d); update the"
				% expected_protocol
			+ " sentinel.")

	# Authenticate + connect.
	var users: Array = await _helper.multi_session_anon(1)
	if users.size() != 1:
		pending("multi_session_anon did not return one user")
		return
	_user = users[0]

	var session: Variant = _sock_helper.session_from_token(
		str(_user.token))
	if session == null:
		pending("Nakama SDK NakamaSession not available")
		return
	var sock: Variant = _sock_helper.create_socket()
	if sock == null:
		pending("Nakama autoload not registered")
		return
	var connected: bool = await _sock_helper.connect_with_timeout(
		sock, session, 5.0)
	if not connected:
		pending(
			"socket would not connect (covered by"
			+ " test_socket_auth)")
		return

	# Wire handlers BEFORE adding the ticket so the abort
	# notification isn't lost to a connect-order race.
	var match_failed_payload: Dictionary = {}
	var match_ready_seen := false
	var on_notification := func(n):
		var subj: String = str(n.subject)
		if subj == "match_failed":
			# match_failed is flat JSON (Stage 3.9 decision —
			# different shape than match_ready's double-
			# encoded `{connection: "<inner-json>"}`).
			var parsed: Variant = JSON.parse_string(
				str(n.content))
			if parsed is Dictionary:
				match_failed_payload = parsed
		elif subj == "match_ready":
			match_ready_seen = true
	sock.received_notification.connect(on_notification)

	# Add the ticket. min=max=1 so the matchmaker pool fires
	# immediately. Bogus client_protocol_version forces the
	# Stage 3.9 abort branch.
	var props := _make_props_with_bad_protocol(game_id)
	var ticket = await sock.add_matchmaker_async(
		"*", 1, 1, props, {})
	assert_not_null(ticket, "add_matchmaker_async returned null")
	if ticket == null:
		sock.received_notification.disconnect(on_notification)
		sock.close()
		return

	# Wait for match_failed.
	var elapsed := 0.0
	var step := 0.05
	while match_failed_payload.is_empty() and elapsed < _ABORT_TIMEOUT_SEC:
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	assert_false(
		match_failed_payload.is_empty(),
		"match_failed notification did not arrive within"
		+ " %.1fs; runtime hook may not have aborted on"
			% _ABORT_TIMEOUT_SEC
		+ " protocol_version mismatch.")
	if match_failed_payload.is_empty():
		sock.received_notification.disconnect(on_notification)
		sock.close()
		return

	# Contract assertions on the abort payload. Mirror
	# fleet_allocator.go::abortProtocolMismatch.
	assert_eq(
		str(match_failed_payload.get("reason", "")),
		"protocol_mismatch",
		"match_failed.reason was not 'protocol_mismatch'.")
	assert_eq(
		int(match_failed_payload.get("expected", 0)),
		expected_protocol,
		"match_failed.expected should mirror the registered"
		+ " game's protocol_version.")
	# The bogus client is the mismatched player, so `got`
	# carries the value we sent. Compatible-but-aborted
	# matched peers (other entries in a multi-player match)
	# would NOT see `got`; this test only has one entry, so
	# the mismatched-side payload is what arrives.
	assert_eq(
		int(match_failed_payload.get("got", 0)),
		int(_BOGUS_PROTOCOL_VERSION),
		"match_failed.got should echo the bogus protocol we"
		+ " sent.")
	var message: String = str(
		match_failed_payload.get("message", ""))
	assert_false(
		message.is_empty(),
		"match_failed.message empty; user-facing copy"
		+ " absent from the abort.")

	# Confirm match_ready never arrives (defensive — the
	# runtime returned "" from OnMatchmakerMatched before
	# allocation, so nothing should fan out).
	elapsed = 0.0
	while not match_ready_seen and elapsed < _NO_MATCH_READY_WAIT_SEC:
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step
	assert_false(
		match_ready_seen,
		"match_ready notification arrived after a Stage 3.9"
		+ " protocol_mismatch abort; runtime hook allocated"
		+ " an Edgegap deploy it shouldn't have.")

	sock.received_notification.disconnect(on_notification)
	sock.close()


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


## Standard matchmaker ticket properties with the protocol
## version deliberately wrong so the runtime aborts.
func _make_props_with_bad_protocol(game_id: String) -> Dictionary:
	return {
		"platform": "native",
		"player_count": "1",
		"game_id": game_id,
		"client_protocol_version": _BOGUS_PROTOCOL_VERSION,
		"game_mode": "ffa",
	}


## Mirror NakamaMatchmakerClient's game_id resolver.
func _resolve_game_id_for_ticket() -> String:
	var resolved: String = str(_helper._resolve_game_id())
	if resolved.is_empty():
		return "hopnbop"
	return resolved
