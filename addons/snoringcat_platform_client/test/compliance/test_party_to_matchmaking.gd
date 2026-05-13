extends GutTest
## Two-user party → matchmaking → match_ready flow. Stage 8.18.
##
## Exercises the Stage 1.1a/1.1b contract: a party leader can
## call `party_start_matchmaking`, the runtime fans out
## `party_matchmaking_start` notifications to followers, both
## members enqueue with the shared matchmaker_properties, and the
## matchmaker pairs them on `party_id` to land on the SAME
## Edgegap deploy (same `request_id` in both clients'
## `match_ready` payloads).
##
## Gated on EDGEGAP_MOCK_DEPLOY=true so the test doesn't burn
## paid container-hours when run against the live prod runtime.
## See compliance_helper.is_mock_deploy_mode.
##
## What this test guards:
##   - Stage 1.1a: party_start_matchmaking RPC validates leader,
##     returns matchmaker_properties carrying party_id, fans out
##     party_matchmaking_start to followers.
##   - Stage 1.1b: follower-side path treats the notification as
##     a "enqueue with these properties" trigger.
##   - Stage 1.2 / 1.3: party member list reachable + leader_id
##     resolved so the leader-only RPC gate accepts the leader's
##     session.
##   - Per-game matchmaker properties: each ticket carries
##     game_id + client_protocol_version + party_id; the runtime
##     hook's matchGameID / protocol-mismatch checks should pass
##     for paired clients.
##   - Mock-mode allocation: synthetic request_id matches across
##     both matched players' match_ready payloads.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
const _MOCK_REQUEST_ID_PREFIX := "mock-"
## Total per-user wait budget for matchmaker_matched + match_ready.
## The runtime's mock-mode allocation is fast; the bound is set
## by the matchmaker's internal tick (sub-second on a quiet pool).
const _MATCH_TIMEOUT_SEC := 12.0
const _NOTIFY_TIMEOUT_SEC := 6.0
## How long to wait for the follower's party_matchmaking_start
## notification after the leader's RPC. Persistent notifications
## are delivered immediately on the open socket.
const _START_NOTIFY_TIMEOUT_SEC := 5.0

var _helper
var _sock_helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)
	_sock_helper = _SocketHelper.new()
	add_child_autofree(_sock_helper)


func after_each() -> void:
	# Hard-delete both users via /v2/account so the per-run
	# state (party group, matchmaker tickets, match_metadata
	# rows, synthetic_matches rows) cascades cleanly.
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_party_pair_matched_into_same_mock_deploy() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return

	# Gate on EDGEGAP_MOCK_DEPLOY so this never runs against a
	# real prod runtime.
	var mock_on: bool = await _helper.is_mock_deploy_mode()
	if not mock_on:
		pending(
			"EDGEGAP_MOCK_DEPLOY=true required on the runtime."
			+ " This test would otherwise allocate real Edgegap"
			+ " containers for both matched players.")
		return

	_users = await _helper.multi_session_anon(2)
	if _users.size() != 2:
		pending("multi_session_anon did not return two users")
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]

	# Step 1: A creates a closed party group + invites B. Same
	# shape as test_party_invite_flow's setup, which proved B
	# lands as state=2 on Nakama 3.25.0 admin-add. The
	# party_start_matchmaking RPC skips state=3, so we need an
	# active membership.
	var group_name := (
		"party-mm-compliance-"
		+ str(Time.get_unix_time_from_system() as int)
		+ "-"
		+ str(randi() % 100000))
	var create: Dictionary = await _helper.http_post(
		"/v2/group",
		{"name": group_name, "open": false, "max_count": 4},
		"bearer:" + str(a.token))
	assert_true(
		create.status_code >= 200 and create.status_code < 300,
		"create-group: status=%d body=%s"
			% [create.status_code, create.text])
	if not (create.body is Dictionary):
		fail_test("create-group response missing body")
		return
	var party_id: String = str(create.body.get("id", ""))
	assert_false(
		party_id.is_empty(),
		"create-group missing id: %s" % create.text)

	var add: Dictionary = await _helper.http_post(
		"/v2/group/" + party_id + "/add?user_ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		add.status_code >= 200 and add.status_code < 300,
		"invite A→B: status=%d body=%s"
			% [add.status_code, add.text])

	# Step 2: if Nakama parked B at state=3, accept. We tolerate
	# either state=2 (admin-add direct) or state=3 (closed-group
	# invite) as the post-invite state. See
	# test_party_invite_flow.gd for the Nakama-version
	# behavior discussion.
	var b_state: int = await _fetch_user_group_state(
		str(b.token), str(b.user_id), party_id)
	if b_state == 3:
		var join: Dictionary = await _helper.http_post(
			"/v2/group/" + party_id + "/join",
			null,
			"bearer:" + str(b.token))
		assert_true(
			join.status_code >= 200 and join.status_code < 300,
			"B accept-via-join: status=%d body=%s"
				% [join.status_code, join.text])

	# Step 3: open sockets for both users. The runtime fans the
	# party_matchmaking_start + match_ready notifications via
	# persistent notifications, which the SDK delivers as
	# `received_notification` on the open socket.
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

	# Step 4: wire up signal capture on both sockets BEFORE the
	# RPC fires so an immediate notification doesn't race the
	# connect.
	var a_capture := _Capture.new()
	var b_capture := _Capture.new()
	a_sock.received_matchmaker_matched.connect(a_capture._on_matched)
	a_sock.received_notification.connect(a_capture._on_notification)
	b_sock.received_matchmaker_matched.connect(b_capture._on_matched)
	b_sock.received_notification.connect(b_capture._on_notification)

	# Step 5: leader (A) calls party_start_matchmaking. Response
	# carries matchmaker_properties + leader_id + member_ids.
	var start_resp: Dictionary = await _helper.session_rpc(
		"party_start_matchmaking",
		str(a.token),
		{"party_id": party_id})
	assert_eq(
		start_resp.status_code, 200,
		"party_start_matchmaking: status=%d body=%s"
			% [start_resp.status_code, start_resp.text])
	if not (start_resp.inner is Dictionary):
		fail_test(
			"party_start_matchmaking response not a dict: %s"
				% start_resp.text)
		_cleanup_sockets(a_sock, b_sock, a_capture, b_capture)
		return
	var resp_dict: Dictionary = start_resp.inner
	assert_true(
		bool(resp_dict.get("ok", false)),
		"party_start_matchmaking returned ok=false: %s"
			% start_resp.text)
	assert_eq(
		str(resp_dict.get("party_id", "")), party_id,
		"party_start_matchmaking returned wrong party_id")
	assert_eq(
		str(resp_dict.get("leader_id", "")), str(a.user_id),
		"party_start_matchmaking returned wrong leader_id")
	var member_ids: Variant = resp_dict.get("member_ids", [])
	assert_true(
		member_ids is Array and (member_ids as Array).size() == 2,
		"party_start_matchmaking member_ids should be 2; got %s"
			% str(member_ids))
	var leader_props: Variant = resp_dict.get(
		"matchmaker_properties", {})
	assert_true(
		leader_props is Dictionary
			and str((leader_props as Dictionary).get("party_id", ""))
				== party_id,
		"matchmaker_properties.party_id missing: %s"
			% str(leader_props))

	# Step 6: wait for B's party_matchmaking_start notification.
	var follower_props := await _wait_for_party_matchmaking_start(
		b_capture, _START_NOTIFY_TIMEOUT_SEC)
	assert_false(
		follower_props.is_empty(),
		"B did not receive party_matchmaking_start within %.1fs"
			% _START_NOTIFY_TIMEOUT_SEC)
	if follower_props.is_empty():
		_cleanup_sockets(a_sock, b_sock, a_capture, b_capture)
		return

	# Step 7: both users add matchmaker tickets. min=max=2 so the
	# pool only fires when both tickets are in. Each carries the
	# shared party_id property so the matchmaker can pair them
	# (the query stays `*` for now per Stage 1.1b's deferred
	# query-filter note; even so the small pool reliably pairs
	# party_id mates first in mock mode).
	var game_id := _resolve_game_id_for_ticket()
	var protocol_version := _resolve_protocol_version_for_ticket()
	var a_props := _build_ticket_props(
		game_id, protocol_version, leader_props as Dictionary)
	var b_props := _build_ticket_props(
		game_id, protocol_version, follower_props)

	var a_ticket = await a_sock.add_matchmaker_async(
		"*", 2, 2, a_props, {})
	var b_ticket = await b_sock.add_matchmaker_async(
		"*", 2, 2, b_props, {})
	assert_not_null(a_ticket, "A add_matchmaker returned null")
	assert_not_null(b_ticket, "B add_matchmaker returned null")

	# Step 8: both sockets should receive matchmaker_matched then
	# a match_ready notification with the SAME request_id (same
	# Edgegap deploy / mock synthesis).
	var deadline := _MATCH_TIMEOUT_SEC + _NOTIFY_TIMEOUT_SEC
	var elapsed := 0.0
	var step := 0.05
	while (
		(a_capture.match_ready_payload.is_empty()
			or b_capture.match_ready_payload.is_empty())
		and a_capture.failed_subject.is_empty()
		and b_capture.failed_subject.is_empty()
		and elapsed < deadline
	):
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step

	_cleanup_sockets(a_sock, b_sock, a_capture, b_capture)

	assert_eq(
		a_capture.failed_subject, "",
		"A received match_failed: %s" % a_capture.failed_subject)
	assert_eq(
		b_capture.failed_subject, "",
		"B received match_failed: %s" % b_capture.failed_subject)
	assert_true(
		a_capture.matched_seen,
		"A's received_matchmaker_matched did not fire within %.1fs"
			% _MATCH_TIMEOUT_SEC)
	assert_true(
		b_capture.matched_seen,
		"B's received_matchmaker_matched did not fire within %.1fs"
			% _MATCH_TIMEOUT_SEC)
	assert_false(
		a_capture.match_ready_payload.is_empty(),
		"A did not receive match_ready within %.1fs" % deadline)
	assert_false(
		b_capture.match_ready_payload.is_empty(),
		"B did not receive match_ready within %.1fs" % deadline)
	if (
		a_capture.match_ready_payload.is_empty()
		or b_capture.match_ready_payload.is_empty()
	):
		return

	# Step 9: assert the SAME request_id across both payloads —
	# this is the core party-block guarantee. fleet_allocator
	# only allocates one deploy per matchmaker fan-out, so any
	# inconsistency here means the matchmaker split the party.
	var a_request_id: String = str(
		a_capture.match_ready_payload.get("request_id", ""))
	var b_request_id: String = str(
		b_capture.match_ready_payload.get("request_id", ""))
	assert_eq(
		a_request_id, b_request_id,
		"A and B should land on the same deploy. A=%s B=%s"
			% [a_request_id, b_request_id])
	assert_true(
		a_request_id.begins_with(_MOCK_REQUEST_ID_PREFIX),
		"request_id should start with '%s'; got '%s'"
			% [_MOCK_REQUEST_ID_PREFIX, a_request_id])
	assert_true(
		bool(a_capture.match_ready_payload.get("mock", false)),
		"A's payload missing mock=true: %s"
			% str(a_capture.match_ready_payload))
	assert_true(
		bool(b_capture.match_ready_payload.get("mock", false)),
		"B's payload missing mock=true: %s"
			% str(b_capture.match_ready_payload))

	# Step 10: per-player session_ids are isolated (each user
	# only gets their own ids). The runtime's match_ready
	# notification is constructed per-user (see fleet_allocator's
	# loop), so A's session_ids and B's must differ.
	var a_sids: Variant = a_capture.match_ready_payload.get(
		"session_ids", [])
	var b_sids: Variant = b_capture.match_ready_payload.get(
		"session_ids", [])
	assert_true(
		a_sids is Array and (a_sids as Array).size() == 1,
		"A.session_ids should be a 1-element Array; got %s"
			% str(a_sids))
	assert_true(
		b_sids is Array and (b_sids as Array).size() == 1,
		"B.session_ids should be a 1-element Array; got %s"
			% str(b_sids))
	assert_ne(
		str((a_sids as Array)[0]),
		str((b_sids as Array)[0]),
		"A and B should have distinct session_ids")


# --------------------------------------------------------------
# Helpers
# --------------------------------------------------------------


## Awaits B's party_matchmaking_start notification on the given
## capture and returns the embedded matchmaker_properties dict.
## Returns {} on timeout.
func _wait_for_party_matchmaking_start(
	capture, timeout_sec: float,
) -> Dictionary:
	var elapsed := 0.0
	var step := 0.05
	while (
		capture.party_matchmaking_start_props.is_empty()
		and elapsed < timeout_sec
	):
		await Engine.get_main_loop().create_timer(step).timeout
		elapsed += step
	return capture.party_matchmaking_start_props


func _build_ticket_props(
	game_id: String,
	protocol_version: String,
	matchmaker_properties: Dictionary,
) -> Dictionary:
	var props: Dictionary = {
		"platform": "native",
		"player_count": "1",
		"game_id": game_id,
		"client_protocol_version": protocol_version,
	}
	# Merge the per-party properties on top. party_id and
	# game_mode are the standard pair the leader's RPC returns.
	for k in matchmaker_properties.keys():
		props[str(k)] = str(matchmaker_properties[k])
	return props


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


## Returns the Nakama group-user state int for `group_id` from
## the perspective of `user_id`. Returns -1 when the group isn't
## in the user's list at all.
func _fetch_user_group_state(
	token: String,
	user_id: String,
	group_id: String,
) -> int:
	var result: Dictionary = await _helper.http_get(
		"/v2/user/" + user_id + "/group",
		"bearer:" + token)
	if result.status_code != 200:
		return -1
	if not (result.body is Dictionary):
		return -1
	var groups: Variant = result.body.get("user_groups", [])
	if not (groups is Array):
		return -1
	for entry in groups:
		if not (entry is Dictionary):
			continue
		var group: Variant = entry.get("group")
		if not (group is Dictionary):
			continue
		if str(group.get("id", "")) == group_id:
			return int(entry.get("state", -1))
	return -1


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


## Captures signals from one user's socket. Inner class so we
## can keep a pair of independent receivers without leaking
## test-local state into the global namespace.
class _Capture:
	var matched_seen := false
	var match_ready_payload: Dictionary = {}
	var party_matchmaking_start_props: Dictionary = {}
	var failed_subject := ""

	func _on_matched(_matched) -> void:
		matched_seen = true

	func _on_notification(n) -> void:
		var subj: String = str(n.subject)
		if subj == "match_ready":
			var outer: Variant = JSON.parse_string(str(n.content))
			if outer is Dictionary:
				var conn_raw: String = str(
					outer.get("connection", ""))
				var conn: Variant = JSON.parse_string(conn_raw)
				if conn is Dictionary:
					match_ready_payload = conn
		elif subj == "match_failed":
			failed_subject = subj
		elif subj == "party_matchmaking_start":
			# Persistent notification's content is the JSON-
			# stringified `notification` dict from
			# partyStartMatchmakingRpc:
			#   {party_id, game_mode, leader_id,
			#    matchmaker_properties}.
			var parsed: Variant = JSON.parse_string(str(n.content))
			if parsed is Dictionary:
				var props: Variant = (parsed as Dictionary).get(
					"matchmaker_properties", {})
				if props is Dictionary:
					party_matchmaking_start_props = props
