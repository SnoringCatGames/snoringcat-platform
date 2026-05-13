extends GutTest
## Presence RPC visibility filter. Stage 8.22.
##
## The single-user test_presence.gd only confirms that the
## RPC returns a well-formed response for the caller's own
## row. The multi-user shape exercises the bits Stage 3.3
## introduced: only `MUTUAL` friends surface in the
## online_friends map, the caller's game_id is recorded on
## the row, and a pending-friend-request relationship does
## NOT leak presence to either side.
##
## What this test guards:
##   - Stage 3.2: returned presence record carries
##     `game_id` matching the caller's session game_id.
##   - Stage 3.3: only state=0 (MUTUAL) friends contribute
##     to online_friends. Pending invites
##     (state=1/INVITE_SENT, state=2/INVITE_RECEIVED) must
##     not surface.
##   - Stage 2.5/2.6: the RPC requires game_id in session
##     vars; multi_session_anon's auth flow already injects
##     it via device_auth_body so the test exercises the
##     production code path.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper
var _users: Array = []


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	for user in _users:
		await _helper.delete_one_shot_account(user)
	_users = []


func test_only_mutual_friends_appear_in_presence() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	# Three users: A (the caller doing the presence read),
	# B (mutual friend, should appear), C (pending request
	# only — must NOT appear).
	_users = await _helper.multi_session_anon(3)
	if _users.size() != 3:
		pending("multi_session_anon did not return three users")
		return
	var a: Dictionary = _users[0]
	var b: Dictionary = _users[1]
	var c: Dictionary = _users[2]

	# Step 1: A↔B mutual friendship.
	var add_b: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		add_b.status_code >= 200 and add_b.status_code < 300,
		"A→B request: status=%d" % add_b.status_code)
	var accept_b: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(a.user_id),
		null,
		"bearer:" + str(b.token))
	assert_true(
		accept_b.status_code >= 200 and accept_b.status_code < 300,
		"B→A accept: status=%d" % accept_b.status_code)

	# Step 2: A→C pending only (no accept). Should NOT
	# surface in presence.
	var add_c: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(c.user_id),
		null,
		"bearer:" + str(a.token))
	assert_true(
		add_c.status_code >= 200 and add_c.status_code < 300,
		"A→C request: status=%d" % add_c.status_code)

	# Step 3: B publishes "online" presence so there's
	# something for A to read back.
	var b_presence: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		str(b.token),
		{"rich_presence": "In Lobby", "status": "online"})
	assert_eq(
		b_presence.status_code, 200,
		"B presence write: status=%d body=%s"
			% [b_presence.status_code, b_presence.text])

	# Step 4: C also publishes online. C is a pending
	# invite, NOT a mutual friend, so even though C has a
	# row, A should not see it in their response.
	var c_presence: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		str(c.token),
		{"rich_presence": "Lurking", "status": "online"})
	assert_eq(
		c_presence.status_code, 200,
		"C presence write: status=%d body=%s"
			% [c_presence.status_code, c_presence.text])

	# Step 5: A calls update_and_get_presence. Should see B
	# (mutual) but not C (pending request).
	var a_presence: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		str(a.token),
		{"rich_presence": "In Menu", "status": "online"})
	assert_eq(
		a_presence.status_code, 200,
		"A presence read: status=%d body=%s"
			% [a_presence.status_code, a_presence.text])

	var inner: Variant = a_presence.inner
	assert_true(
		inner is Dictionary,
		"A presence response shape: %s" % a_presence.text)
	var online_ids: Variant = inner.get("online_ids", [])
	var online_friends: Variant = inner.get("online_friends", {})
	assert_true(
		online_ids is Array,
		"online_ids not an array: %s" % a_presence.text)
	assert_true(
		online_friends is Dictionary,
		"online_friends not a dict: %s" % a_presence.text)

	# Step 6: B is present, C is absent.
	assert_true(
		online_ids.has(str(b.user_id)),
		"B should appear in online_ids; got %s" % online_ids)
	assert_false(
		online_ids.has(str(c.user_id)),
		(
			"C is pending-invite, must not surface in"
			+ " online_ids; got %s" % online_ids
		))
	assert_true(
		online_friends.has(str(b.user_id)),
		(
			"B should appear in online_friends keys;"
			+ " got %s" % online_friends.keys()
		))
	assert_false(
		online_friends.has(str(c.user_id)),
		(
			"C is pending-invite, must not surface in"
			+ " online_friends; got %s" % online_friends.keys()
		))

	# Step 7: B's record carries the correct game_id (the
	# field A's session was authenticated with). Stage 3.2
	# wrote the field; Stage 3.3 reads it back.
	var b_record: Variant = online_friends.get(str(b.user_id))
	assert_true(
		b_record is Dictionary,
		"B record shape: %s" % b_record)
	var b_game_id := str(b_record.get("game_id", ""))
	assert_false(
		b_game_id.is_empty(),
		"B record missing game_id field; got %s" % b_record)
	# Compare against the caller's session game_id. Pulled
	# via the same resolver compliance_helper uses to
	# inject vars on auth, so a test running with a
	# non-default game_id stays correct.
	var expected_game := _resolve_caller_game_id()
	if not expected_game.is_empty():
		assert_eq(
			b_game_id, expected_game,
			"B record game_id mismatch: got %s want %s"
				% [b_game_id, expected_game])

	# Step 8: rich_presence round-trips.
	var b_rich := str(b_record.get("rich_presence", ""))
	assert_eq(
		b_rich, "In Lobby",
		"B rich_presence didn't round-trip: got %s" % b_rich)


## Mirrors compliance_helper._resolve_game_id. Returns the
## caller's effective game_id ("" when neither Platform nor
## env var is set; presence record game_id assertion is
## skipped in that case rather than locked to a hardcoded
## value).
func _resolve_caller_game_id() -> String:
	var tree := Engine.get_main_loop()
	if tree is SceneTree:
		var root: Node = (tree as SceneTree).root
		var node: Node = root.get_node_or_null("Platform")
		if node != null and node.get("is_initialized") == true:
			return str(node.get("game_id"))
	return OS.get_environment("PLATFORM_GAME_ID")
