extends GutTest
## Stage 8.15: friend block-list lifecycle + matchmaker abort.
##
## Exercises Stage 7.4 end-to-end. Two tests:
##
##   1. `test_block_list_lifecycle_bidirectional_add_rejection`:
##      single-user contract for the three RPCs. Walks A blocks B
##      → list_blocked has B → self-block fails → A→B add_friend
##      rejected → B→A add_friend also rejected (Nakama's
##      bidirectional FriendsAdd semantics over state=3 BANNED) →
##      unblock_user → list_blocked empty → A→B add_friend
##      succeeds. Uses plain HTTP, no socket, no mock-mode gate.
##
##   2. `test_blocked_pair_aborts_matchmaker_fanout`: matchmaker
##      hook's blocked-pair check aborts the match before Edgegap
##      allocation. A blocks B, both add tickets with min=max=2 so
##      the pool only fires when both are in. Asserts BOTH users
##      receive `match_failed reason=blocked_pair` and NEITHER
##      receives `match_ready`. Mock-mode gated like every other
##      Stage 8 matchmaker test.
##
## What this test guards:
##   - block_user RPC writes a state=3 BANNED row + returns
##     {ok, user_id, ...}
##   - list_blocked_users RPC returns the caller's BANNED entries
##   - unblock_user RPC removes the state=3 row idempotently
##   - self-block rejected with INVALID_ARGUMENT
##   - Nakama's native bidirectional block-add rejection — neither
##     side can send a friend request when one has blocked the
##     other (no separate hook required). Nakama's FriendsAdd is
##     lenient at the HTTP layer — it returns 200 even when every
##     target is silently skipped — so the assertion verifies the
##     friend-row state, not the HTTP status.
##   - fleet_allocator.go::OnMatchmakerMatched blocked-pair branch
##     fires `abortBlockedPair`, emits `match_failed reason=blocked_pair`,
##     and returns "" so no Edgegap allocation happens


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
## Nakama friend-state enum (mirrors the SDK constants without
## importing them — keeps the test independent of SDK pinning).
##   FRIEND=0, INVITE_SENT=1, INVITE_RECEIVED=2, BANNED=3.
const _STATE_FRIEND := 0
const _STATE_INVITE_SENT := 1
const _STATE_INVITE_RECEIVED := 2
const _STATE_BANNED := 3
## How long to wait for the runtime's match_failed notification.
## abortBlockedPair fires synchronously inside OnMatchmakerMatched
## right after the FriendsList round trips for each matched user,
## so this is sub-second in practice. 8 s bound for CI load.
const _ABORT_TIMEOUT_SEC := 8.0
## Belt-and-suspenders wait to confirm match_ready did NOT arrive.
## The blocked-pair branch returns "" before allocation, so no
## fan-out should happen.
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
	# Hard-delete every one-shot account so the per-run state
	# (BANNED rows, matchmaker tickets, match_metadata rows) is
	# cleaned up. The platform's soft-delete path leaves the
	# block row in place during the 30-day grace; /v2/account
	# (Nakama's built-in) cascades it.
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


## Test 1: single-user RPC contract for block_user / unblock_user /
## list_blocked_users plus the bidirectional FriendsAdd rejection
## the BANNED state buys us for free.
func test_block_list_lifecycle_bidirectional_add_rejection() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	assert_false(str(a.user_id).is_empty(), "user A missing user_id")
	assert_false(str(b.user_id).is_empty(), "user B missing user_id")

	# Step 1: A blocks B.
	var block_resp: Dictionary = await _helper.session_rpc(
		"block_user",
		str(a.token),
		{"user_id": str(b.user_id)})
	assert_eq(
		block_resp.status_code, 200,
		"block_user: status=%d body=%s"
			% [block_resp.status_code, block_resp.text])
	assert_true(
		block_resp.inner is Dictionary,
		"block_user response not a dict: %s" % block_resp.text)
	if not (block_resp.inner is Dictionary):
		return
	assert_true(
		bool(block_resp.inner.get("ok", false)),
		"block_user returned ok=false: %s" % block_resp.text)
	assert_eq(
		str(block_resp.inner.get("user_id", "")), str(b.user_id),
		"block_user response should echo B's user_id")

	# Step 2: list_blocked_users returns B.
	var list_resp: Dictionary = await _helper.session_rpc(
		"list_blocked_users", str(a.token), {})
	assert_eq(
		list_resp.status_code, 200,
		"list_blocked_users: status=%d body=%s"
			% [list_resp.status_code, list_resp.text])
	assert_true(
		_blocked_list_contains(list_resp.inner, str(b.user_id)),
		"list_blocked_users should include B; got %s"
			% list_resp.text)

	# Step 3: self-block is rejected. The RPC layer returns
	# INVALID_ARGUMENT(3) which Nakama maps to HTTP 400.
	var self_block: Dictionary = await _helper.session_rpc(
		"block_user",
		str(a.token),
		{"user_id": str(a.user_id)})
	assert_ne(
		self_block.status_code, 200,
		"self-block should fail; got status=%d body=%s"
			% [self_block.status_code, self_block.text])

	# Step 4: A → B add_friend silently no-ops. Nakama's
	# FriendsAdd returns 200 even when every target is skipped,
	# so the contract assertion is "A's view of B is unchanged"
	# rather than "the HTTP call errored". Specifically: A still
	# sees B as state=3 (BANNED), NOT state=1 (INVITE_SENT).
	var ab_add: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		ab_add.status_code >= 200 and ab_add.status_code < 300,
		"A→B add_friend should return 2xx (Nakama is lenient);"
			+ " got status=%d body=%s"
				% [ab_add.status_code, ab_add.text])
	var a_state_after_ab: int = await _fetch_friend_state(
		str(a.token), str(b.user_id))
	assert_eq(
		a_state_after_ab, _STATE_BANNED,
		"A→B add_friend should NOT promote B out of BANNED; A's"
			+ " view of B should still be state=3 (BANNED), got"
			+ " state=%d" % a_state_after_ab)

	# Step 5: B → A add_friend ALSO silently no-ops. The block
	# row is bidirectional — Nakama reads A's BANNED row when
	# deciding whether to write a fresh state=1 row from B's
	# side. After the call, B's view of A should NOT be state=1
	# (INVITE_SENT). It may be absent (no row at all) or state=3
	# (some Nakama builds write a mirror BANNED row on B's side
	# the moment A's block is established).
	var ba_add: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(a.user_id),
		null,
		"bearer:" + str(b.token))
	assert_true(
		ba_add.status_code >= 200 and ba_add.status_code < 300,
		"B→A add_friend should return 2xx (Nakama is lenient);"
			+ " got status=%d body=%s"
				% [ba_add.status_code, ba_add.text])
	var b_state_after_ba: int = await _fetch_friend_state(
		str(b.token), str(a.user_id))
	assert_ne(
		b_state_after_ba, _STATE_INVITE_SENT,
		"B→A add_friend should NOT create an INVITE_SENT row"
			+ " while A has B in BANNED; got state=%d"
				% b_state_after_ba)
	# Also assert from A's side that no INVITE_RECEIVED row
	# materialized — defense in depth against a future Nakama
	# version that lets the row through with one side's view
	# only.
	var a_state_after_ba: int = await _fetch_friend_state(
		str(a.token), str(b.user_id))
	assert_eq(
		a_state_after_ba, _STATE_BANNED,
		"A's view of B should remain state=3 (BANNED) after B's"
			+ " add attempt; got state=%d" % a_state_after_ba)

	# Step 6: A unblocks B.
	var unblock: Dictionary = await _helper.session_rpc(
		"unblock_user",
		str(a.token),
		{"user_id": str(b.user_id)})
	assert_eq(
		unblock.status_code, 200,
		"unblock_user: status=%d body=%s"
			% [unblock.status_code, unblock.text])
	assert_true(
		unblock.inner is Dictionary
			and bool((unblock.inner as Dictionary).get("ok", false)),
		"unblock_user returned ok=false: %s" % unblock.text)

	# Step 7: list_blocked_users no longer contains B.
	var list_after: Dictionary = await _helper.session_rpc(
		"list_blocked_users", str(a.token), {})
	assert_eq(
		list_after.status_code, 200,
		"list_blocked_users (after unblock): status=%d body=%s"
			% [list_after.status_code, list_after.text])
	assert_false(
		_blocked_list_contains(list_after.inner, str(b.user_id)),
		"list_blocked_users should NOT include B after unblock;"
			+ " got %s" % list_after.text)

	# Step 8: A → B add_friend now succeeds and creates the
	# state=1 (INVITE_SENT) row. Verifies both the HTTP shape
	# AND the row state, mirroring Step 4's "verify state, not
	# just response" pattern.
	var ab_add2: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		ab_add2.status_code >= 200 and ab_add2.status_code < 300,
		"A→B add_friend should return 2xx after unblock; got"
			+ " status=%d body=%s"
				% [ab_add2.status_code, ab_add2.text])
	var a_state_final: int = await _fetch_friend_state(
		str(a.token), str(b.user_id))
	assert_eq(
		a_state_final, _STATE_INVITE_SENT,
		"A→B add_friend after unblock should create state=1"
			+ " (INVITE_SENT) on A's side; got state=%d"
				% a_state_final)


## Test 2: matchmaker hook aborts the match when a blocked-pair is
## present in the matched-users set. Mock-mode gated.
func test_blocked_pair_aborts_matchmaker_fanout() -> void:
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

	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]

	# Step 1: A blocks B before either user joins the matchmaker.
	# The runtime hook reads the BANNED state on entry, not at
	# ticket-add time, so the block must be in place before the
	# pool fires.
	var block_resp: Dictionary = await _helper.session_rpc(
		"block_user",
		str(a.token),
		{"user_id": str(b.user_id)})
	assert_eq(
		block_resp.status_code, 200,
		"block_user (setup): status=%d body=%s"
			% [block_resp.status_code, block_resp.text])

	# Step 2: open sockets for both users. The runtime fans the
	# match_failed notification through persistent notifications,
	# which the SDK delivers as `received_notification`.
	var a_session: Variant = _sock_helper.session_from_token(
		str(a.token))
	var b_session: Variant = _sock_helper.session_from_token(
		str(b.token))
	if a_session == null or b_session == null:
		pending("Nakama SDK NakamaSession not available")
		return
	var a_sock: Variant = _sock_helper.create_socket()
	var b_sock: Variant = _sock_helper.create_socket()
	if a_sock == null or b_sock == null:
		pending("Nakama autoload not registered")
		return
	var a_connected: bool = await _sock_helper.connect_with_timeout(
		a_sock, a_session, 5.0)
	var b_connected: bool = await _sock_helper.connect_with_timeout(
		b_sock, b_session, 5.0)
	if not a_connected or not b_connected:
		pending("one of the sockets would not connect")
		return

	# Step 3: wire signal capture BEFORE adding the tickets so a
	# fast abort doesn't race the connect.
	var a_capture := _Capture.new()
	var b_capture := _Capture.new()
	a_sock.received_matchmaker_matched.connect(a_capture._on_matched)
	a_sock.received_notification.connect(a_capture._on_notification)
	b_sock.received_matchmaker_matched.connect(b_capture._on_matched)
	b_sock.received_notification.connect(b_capture._on_notification)

	# Step 4: both users add matchmaker tickets. min=max=2 so the
	# pool only fires when both are in.
	var game_id := _resolve_game_id_for_ticket()
	var protocol_version := _resolve_protocol_version_for_ticket()
	var props: Dictionary = {
		"platform": "native",
		"player_count": "1",
		"game_id": game_id,
		"client_protocol_version": protocol_version,
		"game_mode": "ffa",
	}
	var a_ticket = await a_sock.add_matchmaker_async(
		"*", 2, 2, props, {})
	var b_ticket = await b_sock.add_matchmaker_async(
		"*", 2, 2, props, {})
	assert_not_null(a_ticket, "A add_matchmaker returned null")
	assert_not_null(b_ticket, "B add_matchmaker returned null")

	# Step 5: wait for both sockets to receive match_failed. The
	# matchmaker_matched signal still fires (the runtime hook
	# runs after pairing); the abort happens inside the hook.
	var elapsed := 0.0
	var step := 0.05
	while (
		(a_capture.failed_payload.is_empty()
			or b_capture.failed_payload.is_empty())
		and a_capture.match_ready_payload.is_empty()
		and b_capture.match_ready_payload.is_empty()
		and elapsed < _ABORT_TIMEOUT_SEC
	):
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	# Step 6: belt-and-suspenders. Give the runtime a couple more
	# seconds to incorrectly emit match_ready, so we can assert
	# its absence with high confidence rather than a tight race.
	elapsed = 0.0
	while elapsed < _NO_MATCH_READY_WAIT_SEC:
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	_cleanup_sockets(a_sock, b_sock, a_capture, b_capture)

	# Assert both users received match_failed with the right
	# reason. The runtime's abortBlockedPair sends a flat-JSON
	# notification with reason="blocked_pair" + message.
	assert_false(
		a_capture.failed_payload.is_empty(),
		"A did not receive match_failed within %.1fs"
			% _ABORT_TIMEOUT_SEC)
	assert_false(
		b_capture.failed_payload.is_empty(),
		"B did not receive match_failed within %.1fs"
			% _ABORT_TIMEOUT_SEC)
	if (
		a_capture.failed_payload.is_empty()
		or b_capture.failed_payload.is_empty()
	):
		return
	assert_eq(
		str(a_capture.failed_payload.get("reason", "")),
		"blocked_pair",
		"A's match_failed.reason should be 'blocked_pair'; got"
			+ " %s" % str(a_capture.failed_payload))
	assert_eq(
		str(b_capture.failed_payload.get("reason", "")),
		"blocked_pair",
		"B's match_failed.reason should be 'blocked_pair'; got"
			+ " %s" % str(b_capture.failed_payload))
	assert_false(
		str(a_capture.failed_payload.get("message", "")).is_empty(),
		"A's match_failed.message should be non-empty")
	assert_false(
		str(b_capture.failed_payload.get("message", "")).is_empty(),
		"B's match_failed.message should be non-empty")

	# Assert no match_ready arrived for either user — the
	# matchmaker hook returned "" from OnMatchmakerMatched before
	# allocating an Edgegap deploy.
	assert_true(
		a_capture.match_ready_payload.is_empty(),
		"A should NOT receive match_ready after a blocked-pair"
			+ " abort; got %s" % str(a_capture.match_ready_payload))
	assert_true(
		b_capture.match_ready_payload.is_empty(),
		"B should NOT receive match_ready after a blocked-pair"
			+ " abort; got %s" % str(b_capture.match_ready_payload))


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


## Returns the Nakama friend-state int for `target_id` from the
## perspective of the session that owns `token`. Returns -1 when
## the target isn't in the friend list at all. /v2/friend returns
## every state (FRIEND / INVITE_SENT / INVITE_RECEIVED / BANNED)
## without a filter, so this works for asserting both the BANNED
## state after a block AND the INVITE_SENT state after an unblock-
## and-add.
func _fetch_friend_state(token: String, target_id: String) -> int:
	var result: Dictionary = await _helper.http_get(
		"/v2/friend", "bearer:" + token)
	if result.status_code != 200:
		return -1
	if not (result.body is Dictionary):
		return -1
	var friends: Variant = result.body.get("friends", [])
	if not (friends is Array):
		return -1
	for entry in friends:
		if not (entry is Dictionary):
			continue
		var user: Variant = entry.get("user")
		if not (user is Dictionary):
			continue
		if str(user.get("id", "")) == target_id:
			return int(entry.get("state", -1))
	return -1


## Returns true when the list_blocked_users response payload
## contains an entry with the given user_id. Tolerates non-dict
## bodies (returns false) so the assertion can run regardless of
## the runtime returning a normal dict, an error, or a truncated
## envelope.
func _blocked_list_contains(
	inner: Variant, target_user_id: String,
) -> bool:
	if not (inner is Dictionary):
		return false
	var entries: Variant = (inner as Dictionary).get(
		"blocked_users", [])
	if not (entries is Array):
		return false
	for entry in entries:
		if not (entry is Dictionary):
			continue
		if str(entry.get("user_id", "")) == target_user_id:
			return true
	return false


func _resolve_game_id_for_ticket() -> String:
	var resolved: String = str(_helper._resolve_game_id())
	if resolved.is_empty():
		return "hopnbop"
	return resolved


func _resolve_protocol_version_for_ticket() -> String:
	if not ProjectSettings.has_setting(
		"application/config/protocol_version"
	):
		return "0"
	var raw: Variant = ProjectSettings.get_setting(
		"application/config/protocol_version")
	return str(int(raw))


func _cleanup_sockets(
	a_sock: Variant,
	b_sock: Variant,
	a_capture,
	b_capture,
) -> void:
	if a_sock != null:
		if a_sock.received_matchmaker_matched.is_connected(
			a_capture._on_matched
		):
			a_sock.received_matchmaker_matched.disconnect(
				a_capture._on_matched)
		if a_sock.received_notification.is_connected(
			a_capture._on_notification
		):
			a_sock.received_notification.disconnect(
				a_capture._on_notification)
		a_sock.close()
	if b_sock != null:
		if b_sock.received_matchmaker_matched.is_connected(
			b_capture._on_matched
		):
			b_sock.received_matchmaker_matched.disconnect(
				b_capture._on_matched)
		if b_sock.received_notification.is_connected(
			b_capture._on_notification
		):
			b_sock.received_notification.disconnect(
				b_capture._on_notification)
		b_sock.close()


# --------------------------------------------------------------
# Inner: per-user signal capture
# --------------------------------------------------------------


## Captures match_failed and match_ready notifications from one
## user's socket. Inner class so two concurrent receivers can
## carry independent state without leaking it into the test body.
class _Capture:
	var matched_seen := false
	## match_failed content (flat JSON: {reason, message, ...}).
	var failed_payload: Dictionary = {}
	## match_ready content (inner connection JSON, parsed). Should
	## stay empty for the blocked-pair test.
	var match_ready_payload: Dictionary = {}

	func _on_matched(_matched) -> void:
		matched_seen = true

	func _on_notification(n) -> void:
		var subj: String = str(n.subject)
		if subj == "match_failed":
			var parsed: Variant = JSON.parse_string(str(n.content))
			if parsed is Dictionary:
				failed_payload = parsed
		elif subj == "match_ready":
			var outer: Variant = JSON.parse_string(str(n.content))
			if outer is Dictionary:
				var conn_raw: String = str(
					outer.get("connection", ""))
				var conn: Variant = JSON.parse_string(conn_raw)
				if conn is Dictionary:
					match_ready_payload = conn
