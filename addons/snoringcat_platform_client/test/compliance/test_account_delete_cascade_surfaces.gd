extends GutTest
## Full GDPR cascade verification for `delete_account`.
## Stage 7.7.
##
## Stage 1.4's `delete_account` cascade clears multiple state
## surfaces beyond friends (which 8.16 covers). This test seeds
## a fresh user's presence row, a party-prefixed group they
## created, and arbitrary user-owned storage rows. After calling
## delete_account it verifies each surface clears:
##
##   1. Presence storage (collection=presence,
##      key={game_id}/current): row removed.
##   2. Party-prefixed group with deleter as creator: hard
##      deleted (HTTP fetch returns 404).
##   3. User-owned storage objects across collections: removed.
##   4. account_deletion_queue audit row: preserved (pending=true
##      via get_account_deletion_status; the row is what the
##      hard-delete cron consumes once grace elapses).
##
## What this test guards (beyond 8.16):
##   - Presence cascade (account.go:316-335). A regression that
##     drops the presence scrub leaves stale "online" rows for a
##     deleted user, which would surface to their friends as
##     ghost-online.
##   - Party-group hard-delete cascade (account.go:286-310). A
##     regression that stops hard-deleting creator-owned party
##     groups leaves a zombie group the rest of the party can't
##     escape (no leader to invite-out or hand-off from).
##   - User-owned storage scrub (account.go:347-383). A
##     regression here leaves arbitrary game-side rows behind
##     after a GDPR delete, which is the kind of compliance gap
##     that app-store privacy audits hit.
##   - Audit-row preservation (account.go:361-364 exception).
##     A regression that nukes the queue row alongside the
##     scrub orphans the soft-delete: the cron has nothing to
##     consume, and `cancel_account_deletion` finds no row to
##     restore from, breaking 1.5 entirely.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper
var _user: Dictionary = {}


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	# Hard-delete the one-shot account so the audit-queue row +
	# user shell don't linger. Post-1.5 the deleter retains
	# sign-in capability, so the original token still authorizes
	# /v2/account DELETE.
	if not _user.is_empty():
		await _helper.delete_one_shot_account(_user)
	_user = {}


func test_delete_account_clears_all_state_surfaces() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	# Step 0: mint one fresh user A. multi_session_anon's prefix
	# keeps the device_id distinct from concurrent runs.
	var users: Array = await _helper.multi_session_anon(
		1, "compliance-gdpr-cascade")
	if users.size() != 1:
		pending("multi_session_anon did not return a user")
		return
	_user = users[0]
	var a: Dictionary = _user
	var token: String = str(a.token)
	var user_id: String = str(a.user_id)
	assert_false(user_id.is_empty(), "user A missing user_id")

	# Step 1: seed a presence row via the platform RPC. This is
	# the same code path the live client hits on lobby load.
	var presence_rich := "compliance-gdpr-cascade-rich"
	var presence_result: Dictionary = await _helper.session_rpc(
		"update_and_get_presence",
		token,
		{"rich_presence": presence_rich, "status": "online"})
	assert_eq(
		presence_result.status_code, 200,
		"update_and_get_presence: status=%d body=%s"
			% [presence_result.status_code, presence_result.text])

	# Step 2: create a party-prefixed group owned by A. The
	# cascade hard-deletes this kind of group; non-party groups
	# would just lose A as a member. We focus on the party path
	# since it's the one the platform RPC explicitly handles.
	var ts: int = Time.get_unix_time_from_system() as int
	var group_name := (
		"party-gdpr-cascade-%d-%d" % [ts, randi() % 100000])
	var create: Dictionary = await _helper.http_post(
		"/v2/group",
		{"name": group_name, "open": true, "max_count": 4},
		"bearer:" + token)
	assert_true(
		create.status_code >= 200 and create.status_code < 300,
		"create-group: status=%d body=%s"
			% [create.status_code, create.text])
	assert_true(
		create.body is Dictionary,
		"create-group body not a dict: %s" % create.text)
	var group_id: String = str(create.body.get("id", ""))
	assert_false(
		group_id.is_empty(),
		"create-group missing id: %s" % create.text)

	# Step 3: write two user-owned storage rows in a custom
	# collection. permission_read=1 / permission_write=1 mirrors
	# the typical client-owned shape (owner-only). The cascade
	# walks every collection via nk.StorageList so it should
	# reach these regardless of collection name.
	var custom_collection := "compliance_gdpr_test"
	var write_body := {
		"objects": [
			{
				"collection": custom_collection,
				"key": "alpha",
				"value": "{\"seeded\": true}",
				"permission_read": 1,
				"permission_write": 1,
			},
			{
				"collection": custom_collection,
				"key": "beta",
				"value": "{\"seeded\": true}",
				"permission_read": 1,
				"permission_write": 1,
			},
		],
	}
	var write: Dictionary = await _helper.http_put(
		"/v2/storage", write_body, "bearer:" + token)
	assert_true(
		write.status_code >= 200 and write.status_code < 300,
		"storage write: status=%d body=%s"
			% [write.status_code, write.text])

	# Step 4: pre-cascade sanity. If these fail, the rest of the
	# test isn't actually exercising the cascade.
	var pre_presence: bool = await _read_presence_exists(
		token, user_id)
	assert_true(
		pre_presence,
		"pre-cascade: presence row missing — seeding failed")
	var pre_group: Dictionary = await _helper.http_get(
		"/v2/group/" + group_id, "bearer:" + token)
	# Nakama doesn't expose a single-group GET — list-membership
	# is the closest proxy. We use user-groups for A; the seeded
	# group should be in the list.
	var pre_member_state: int = await _fetch_user_group_state(
		token, user_id, group_id)
	assert_gt(
		pre_member_state, -1,
		"pre-cascade: A not in own group (state=%d)"
			% pre_member_state)
	var pre_storage: int = await _count_storage_in_collection(
		token, custom_collection, user_id)
	assert_eq(
		pre_storage, 2,
		"pre-cascade: expected 2 storage rows; got %d"
			% pre_storage)

	# Step 5: A invokes the platform delete_account RPC. The
	# response contract is locked by 8.16; we just sanity-check
	# OK and proceed to the cascade verification.
	var delete_result: Dictionary = await _helper.session_rpc(
		"delete_account", token, null)
	assert_eq(
		delete_result.status_code, 200,
		"delete_account: status=%d body=%s"
			% [delete_result.status_code, delete_result.text])
	var delete_inner: Variant = delete_result.inner
	assert_true(
		delete_inner is Dictionary
		and bool(delete_inner.get("ok", false)),
		"delete_account ok!=true: %s" % delete_result.text)

	# Step 6: presence row cleared. update_and_get_presence
	# writes to (collection=presence, key={game_id}/current OR
	# legacy "current"). The cascade scrubs both via per-game
	# enumeration; a regression would leave the seeded row.
	var post_presence: bool = await _read_presence_exists(
		token, user_id)
	assert_false(
		post_presence,
		"post-cascade: presence row still exists — scrub regressed")

	# Step 7: party-prefixed group hard-deleted. The cascade
	# calls GroupDelete after GroupUserLeave on creator-owned
	# party-* groups. A regression that drops GroupDelete (or
	# the partyGroupPrefix branch) would leave the group as a
	# zombie. Verify via user-groups list — A would have moved
	# off, and the group ID lookup either returns 404 or the
	# group's member count is zero.
	var post_member_state: int = await _fetch_user_group_state(
		token, user_id, group_id)
	assert_eq(
		post_member_state, -1,
		(
			"post-cascade: A still appears in group %s (state=%d);"
			+ " cascade GroupUserLeave didn't run."
		) % [group_id, post_member_state])
	# Verify group itself is gone. The list-groups endpoint
	# accepts a name filter and is the cleanest "does this exist"
	# probe (single-group GET doesn't exist in Nakama's REST).
	var groups_lookup: Dictionary = await _helper.http_get(
		"/v2/group?name=" + group_name + "&limit=2",
		"bearer:" + token)
	assert_eq(
		groups_lookup.status_code, 200,
		"groups lookup status: %d body=%s"
			% [groups_lookup.status_code, groups_lookup.text])
	var matching_count: int = _count_groups_by_id(
		groups_lookup, group_id)
	assert_eq(
		matching_count, 0,
		(
			"post-cascade: party-prefixed group %s still exists;"
			+ " GroupDelete branch regressed."
		) % group_id)

	# Step 8: user-owned storage scrub. All non-audit storage
	# rows for the user must be gone. The cascade's storage
	# scrub explicitly excludes the account_deletion_queue row,
	# so the count check has to scope to the seeded collection.
	var post_storage: int = await _count_storage_in_collection(
		token, custom_collection, user_id)
	assert_eq(
		post_storage, 0,
		(
			"post-cascade: %d storage rows survived in collection"
			+ " %s; user-storage scrub regressed."
		) % [post_storage, custom_collection])

	# Step 9: account_deletion_queue audit row preserved. This
	# is the cascade's deliberate exception (account.go:361-364)
	# — dropping it would orphan the soft-delete and break the
	# 1.5 cancellation surface. get_account_deletion_status is
	# the gate the client uses to detect the grace state.
	var status: Dictionary = await _helper.session_rpc(
		"get_account_deletion_status", token, null)
	assert_eq(
		status.status_code, 200,
		"get_account_deletion_status: status=%d body=%s"
			% [status.status_code, status.text])
	var status_inner: Variant = status.inner
	assert_true(
		status_inner is Dictionary
		and bool(status_inner.get("pending", false)),
		(
			"post-cascade: account_deletion_queue row missing —"
			+ " cascade scrubbed its own audit trail."
			+ " body=%s"
		) % status.text)
	assert_eq(
		str(status_inner.get("user_id", "")), user_id,
		"queue row's user_id mismatch: %s" % status.text)
	assert_gt(
		int(status_inner.get("scheduled_for", 0)), 0,
		"queue row missing scheduled_for: %s" % status.text)


## Returns true when the deleter's presence storage row is
## readable via the StorageObjects API. Tries both the game-
## scoped key ({game_id}/current) and the pre-Stage-3 legacy
## key ("current") so the test stays correct regardless of
## which key the runtime wrote to.
func _read_presence_exists(
	token: String,
	user_id: String,
) -> bool:
	var game_id: String = _helper._resolve_game_id()
	var keys_to_check: Array[String] = ["current"]
	if not game_id.is_empty():
		keys_to_check.append(game_id + "/current")
	for key in keys_to_check:
		var result: Dictionary = await _helper.http_post(
			"/v2/storage",
			{
				"object_ids": [{
					"collection": "presence",
					"key": key,
					"user_id": user_id,
				}],
			},
			"bearer:" + token)
		if result.status_code != 200:
			continue
		if not (result.body is Dictionary):
			continue
		var objects: Variant = result.body.get("objects", [])
		if objects is Array and (objects as Array).size() > 0:
			return true
	return false


## Returns Nakama's user-group state int for `group_id` from
## the perspective of `user_id`. State 0=SUPERADMIN, 1=ADMIN,
## 2=MEMBER, 3=INVITED. Returns -1 when the user has no
## membership row (the post-cascade expected state for the
## seeded group).
func _fetch_user_group_state(
	token: String,
	user_id: String,
	group_id: String,
) -> int:
	var result: Dictionary = await _helper.http_get(
		"/v2/user/" + user_id + "/group?limit=50",
		"bearer:" + token)
	if result.status_code != 200:
		return -1
	if not (result.body is Dictionary):
		return -1
	var entries: Variant = result.body.get("user_groups", [])
	if not (entries is Array):
		return -1
	for entry in entries:
		if not (entry is Dictionary):
			continue
		var group: Variant = entry.get("group")
		if not (group is Dictionary):
			continue
		if str(group.get("id", "")) == group_id:
			return int(entry.get("state", -1))
	return -1


## Counts the user's own rows in a given storage collection
## via /v2/storage/{collection}/{user_id}. Used both pre- and
## post-cascade to verify the user-storage scrub ran.
func _count_storage_in_collection(
	token: String,
	collection: String,
	user_id: String,
) -> int:
	var result: Dictionary = await _helper.http_get(
		"/v2/storage/" + collection + "/" + user_id + "?limit=100",
		"bearer:" + token)
	if result.status_code != 200:
		return -1
	if not (result.body is Dictionary):
		return -1
	var objects: Variant = result.body.get("objects", [])
	if not (objects is Array):
		return -1
	return (objects as Array).size()


## Returns the number of groups in a /v2/group list response
## whose `id` matches `group_id`. Used to verify a seeded
## group is (or is not) present in a name-filtered list.
func _count_groups_by_id(
	list_result: Dictionary,
	group_id: String,
) -> int:
	if not (list_result.body is Dictionary):
		return 0
	var groups: Variant = list_result.body.get("groups", [])
	if not (groups is Array):
		return 0
	var n := 0
	for g in groups:
		if g is Dictionary and str(g.get("id", "")) == group_id:
			n += 1
	return n
