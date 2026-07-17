extends GutTest
## Presence status contract: offline / online / in_match.
##
## Guards the agreement between what the client SENDS as `status`
## and what the runtime treats as VISIBLE. That agreement is not
## checkable by either side's unit tests — presence.go's
## TestIsPresenceVisible pins the predicate, but only a live
## round trip catches the client and runtime disagreeing about
## the vocabulary. They did, for a long time: the client sent
## status="in_match" during a match while the runtime dropped
## every record whose status wasn't literally "online", so
## playing the game made you vanish from your friends' lists and
## the PRESENCE.IN_MATCH rich-presence string never reached
## another human being.
##
## What this guards:
##   - in_match stays VISIBLE, with status/rich_presence/game_id
##     surviving the round trip. A friends UI needs all three to
##     tell "in the lobby" from "in a match" from "in another
##     game".
##   - explicit offline HIDES. The other half of the
##     disambiguation; without it "offline" is unrepresentable.
##
## Not covered here: the staleness TTL that infers offline from a
## missed heartbeat. Asserting it would mean sleeping out the
## runtime's window (90 s) inside a test. presence.go's
## TestIsPresenceVisible covers that predicate directly instead.
##
## Live-only. Runs against whatever NAKAMA_SERVER_KEY points at.


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


## Make A and B mutual friends. Returns [a, b] or [] on failure.
func _mutual_pair() -> Array:
	var users: Array = await _helper.multi_session_anon(2)
	if users.size() != 2:
		return []
	var a: Dictionary = users[0]
	var b: Dictionary = users[1]
	var add_b: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(b.user_id),
		null,
		"bearer:" + str(a.token))
	if add_b.status_code < 200 or add_b.status_code >= 300:
		return []
	var accept: Dictionary = await _helper.http_post(
		"/v2/friend?ids=" + str(a.user_id),
		null,
		"bearer:" + str(b.token))
	if accept.status_code < 200 or accept.status_code >= 300:
		return []
	return [a, b]


func test_in_match_friend_stays_visible_with_status() -> void:
	if not _helper.is_live_mode():
		pending("live mode only")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var pair: Array = await _mutual_pair()
	if pair.is_empty():
		pending("could not establish a mutual friendship")
		return
	_users = pair
	var a: Dictionary = pair[0]
	var b: Dictionary = pair[1]

	# B is playing. This is the state that used to make B disappear.
	var b_write: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		str(b.token),
		{"rich_presence": "In match", "status": "in_match"})
	assert_eq(
		b_write.status_code, 200,
		"B in_match write: %s" % b_write.text)

	var a_read: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		str(a.token),
		{"rich_presence": "In lobby", "status": "online"})
	assert_eq(
		a_read.status_code, 200,
		"A read: %s" % a_read.text)

	var inner: Variant = a_read.inner
	assert_true(inner is Dictionary, "shape: %s" % a_read.text)
	var online_ids: Variant = inner.get("online_ids", [])
	var online_friends: Variant = inner.get("online_friends", {})

	assert_true(
		online_ids.has(str(b.user_id)),
		(
			"REGRESSION: a friend in a match must stay visible;"
			+ " online_ids=%s" % str(online_ids)
		))

	var rec: Variant = online_friends.get(str(b.user_id))
	assert_true(
		rec is Dictionary,
		"B record missing: %s" % str(online_friends))
	assert_eq(
		str(rec.get("status", "")), "in_match",
		"B's status must survive the round trip so the UI can"
		+ " render 'in a match' distinctly")
	assert_eq(
		str(rec.get("rich_presence", "")), "In match",
		"B's rich_presence must reach A (it never did before:"
		+ " the record was filtered out before delivery)")
	assert_false(
		str(rec.get("game_id", "")).is_empty(),
		"B's record must carry game_id so the UI can tell"
		+ " same-game from another-game: %s" % str(rec))


func test_explicit_offline_hides_friend() -> void:
	if not _helper.is_live_mode():
		pending("live mode only")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var pair: Array = await _mutual_pair()
	if pair.is_empty():
		pending("could not establish a mutual friendship")
		return
	_users = pair
	var a: Dictionary = pair[0]
	var b: Dictionary = pair[1]

	# B announces a clean exit.
	var b_write: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		str(b.token),
		{"rich_presence": "", "status": "offline"})
	assert_eq(
		b_write.status_code, 200,
		"B offline write: %s" % b_write.text)

	var a_read: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		str(a.token),
		{"rich_presence": "In lobby", "status": "online"})
	assert_eq(a_read.status_code, 200, "A read: %s" % a_read.text)

	var inner: Variant = a_read.inner
	var online_ids: Variant = inner.get("online_ids", [])
	assert_false(
		online_ids.has(str(b.user_id)),
		(
			"a friend who announced offline must be hidden;"
			+ " online_ids=%s" % str(online_ids)
		))
