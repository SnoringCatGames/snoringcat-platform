extends GutTest
## Matchmaker failure-path notifications over the Nakama
## realtime socket. Stage 8.21.
##
## Today the runtime emits one synchronous, deterministic
## failure subject — `match_failed` with reason
## `protocol_mismatch` (Stage 3.9). This test exercises that
## path:
##   1. Authenticate two anonymous users: A runs an out-of-date
##      client, B a current one.
##   2. Each adds a min=max=2 matchmaker ticket scoped to this
##      run, so they pair with each other only. A's
##      `client_protocol_version` deliberately mismatches the
##      registered game's protocol_version. (Nakama 3.25 rejects
##      tickets with a minimum count below 2, so this can't be a
##      one-player match.)
##   3. Assert both receive `match_failed` with reason
##      `protocol_mismatch` and the registered `expected`. Only
##      A's payload carries `got`; each gets a message for its
##      own side.
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
## How long we wait for the match_failed notifications. The
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
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


func after_each() -> void:
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


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

	# Authenticate + connect both users.
	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	var connection: Dictionary = await _sock_helper.connect_users(_users)
	if not connection.ok:
		pending(str(connection.reason))
		return
	var stale_sock: Variant = connection.sockets[0]
	var current_sock: Variant = connection.sockets[1]

	# Attach BEFORE adding the tickets so the abort notifications
	# aren't lost to a connect-order race. match_failed is flat
	# JSON, a Stage 3.9 decision: a different shape from
	# match_ready's double-encoded `{connection: "<inner-json>"}`.
	var stale := _SocketHelper.NotificationCapture.new()
	var current := _SocketHelper.NotificationCapture.new()
	stale.attach(stale_sock)
	current.attach(current_sock)
	var captures: Array = [stale, current]
	var sockets: Array = [stale_sock, current_sock]

	var run_id := "run%dx%d" % [
		Time.get_unix_time_from_system() as int,
		randi() % 100000,
	]
	var query := "+properties.compliance_run:" + run_id
	var protocols: Array[String] = [
		_BOGUS_PROTOCOL_VERSION, str(expected_protocol),
	]
	for i in sockets.size():
		var props := _make_props(game_id, protocols[i], run_id)
		var ticket: Variant = await sockets[i].add_matchmaker_async(
			query, 2, 2, props, {})
		if ticket == null or ticket.is_exception():
			fail_test(
				"user %d add_matchmaker failed: %s" % [i, str(ticket)])
			_close_all(captures, sockets)
			return

	var both_failed := func() -> bool:
		return (
			not stale.match_failed.is_empty()
			and not current.match_failed.is_empty()
		)
	await _sock_helper.wait_until(both_failed, _ABORT_TIMEOUT_SEC)
	# Belt-and-suspenders: give the runtime time to wrongly fan
	# out match_ready, so its absence below means something.
	var any_ready := func() -> bool:
		return (
			not stale.match_ready.is_empty()
			or not current.match_ready.is_empty()
		)
	await _sock_helper.wait_until(any_ready, _NO_MATCH_READY_WAIT_SEC)
	_close_all(captures, sockets)

	# Contract assertions on the abort payloads. Mirror
	# fleet_allocator.go::abortProtocolMismatch.
	for side in [["out-of-date", stale], ["current", current]]:
		var label: String = side[0]
		var payload: Dictionary = side[1].match_failed
		assert_false(
			payload.is_empty(),
			(
				"%s client: match_failed did not arrive within"
				+ " %.1fs; runtime hook may not have aborted on"
				+ " protocol_version mismatch."
			) % [label, _ABORT_TIMEOUT_SEC])
		if payload.is_empty():
			continue
		assert_eq(
			str(payload.get("reason", "")), "protocol_mismatch",
			"%s client: match_failed.reason: %s"
				% [label, str(payload)])
		assert_eq(
			int(payload.get("expected", 0)), expected_protocol,
			"%s client: match_failed.expected should mirror the"
				% label
			+ " registered game's protocol_version.")
		assert_false(
			str(payload.get("message", "")).is_empty(),
			"%s client: match_failed.message empty; user-facing"
				% label
			+ " copy absent from the abort.")

	# Only the mismatched player hears what it sent. The
	# compatible peer's match was aborted on someone else's
	# account.
	if not stale.match_failed.is_empty():
		assert_eq(
			int(stale.match_failed.get("got", 0)),
			int(_BOGUS_PROTOCOL_VERSION),
			"out-of-date client: match_failed.got should echo the"
			+ " bogus protocol it sent.")
	if not current.match_failed.is_empty():
		assert_false(
			current.match_failed.has("got"),
			"current client: match_failed should not carry `got`:"
			+ " %s" % str(current.match_failed))

	assert_true(
		stale.match_ready.is_empty() and current.match_ready.is_empty(),
		"match_ready notification arrived after a Stage 3.9"
		+ " protocol_mismatch abort; runtime hook allocated"
		+ " an Edgegap deploy it shouldn't have.")


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


## Standard matchmaker ticket properties, scoped to this run so
## only this test's two tickets pair.
func _make_props(
	game_id: String,
	protocol_version: String,
	run_id: String,
) -> Dictionary:
	return {
		"platform": "native",
		"player_count": "1",
		"game_id": game_id,
		"client_protocol_version": protocol_version,
		"game_mode": "ffa",
		"compliance_run": run_id,
	}


## Detaches every capture, then closes every socket.
func _close_all(captures: Array, sockets: Array) -> void:
	for capture in captures:
		capture.detach()
	for sock in sockets:
		sock.close()


## Mirror NakamaMatchmakerClient's game_id resolver.
func _resolve_game_id_for_ticket() -> String:
	var resolved: String = str(_helper._resolve_game_id())
	if resolved.is_empty():
		return "hopnbop"
	return resolved
