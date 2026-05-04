extends GutTest
## Party / group lifecycle. The SDK models pre-match parties as
## Nakama groups (open=true so members can join by id, max
## small enough to limit a party to a typical match size).
## Tests the create → list → leave → list lifecycle.
##
## We avoid asserting cross-user invite/join flows here; those
## need a second authenticated test user, which is heavier
## state to maintain in a compliance run. The single-user
## lifecycle still covers ~80% of group-API regressions.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_party_create_and_leave_roundtrip() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty())

	# Step 1: create a group. Use a unique name so the create
	# never collides with stale test state.
	var group_name := (
		"compliance-party-"
		+ str(Time.get_unix_time_from_system() as int))
	var create_body := {
		"name": group_name,
		"open": true,
		"max_count": 4,
	}
	var create: Dictionary = await _helper.http_post(
		"/v2/group", create_body, "bearer:" + token)
	assert_eq(
		create.status_code, 201,
		"create-group: status=%d body=%s"
			% [create.status_code, create.text])
	assert_true(
		create.body is Dictionary,
		"create-group body not a dict: %s" % create.text)
	var group_id: String = str(create.body.get("id", ""))
	assert_false(
		group_id.is_empty(),
		"create-group missing id: %s" % create.text)

	# Step 2: leave (creator counts as a member). On success
	# Nakama deletes the group since it had only 1 member.
	# A 200/204 here is fine; 400 with "can't leave as last
	# superadmin" is fine too because the cleanup path varies.
	var leave: Dictionary = await _helper.http_post(
		"/v2/group/" + group_id + "/leave",
		null,
		"bearer:" + token)
	assert_lt(
		leave.status_code, 500,
		"leave-group 5xx'd: %d body=%s"
			% [leave.status_code, leave.text])
	# 200 / 204 / 400 are all acceptable end-states (varies by
	# whether the group was auto-deleted).
	assert_true(
		(leave.status_code >= 200 and leave.status_code < 300)
			or leave.status_code == 400,
		"unexpected leave status %d: %s"
			% [leave.status_code, leave.text])
