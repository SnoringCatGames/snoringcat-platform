extends GutTest
## list_recent_players RPC contract. Stage 7.6.
##
## Two tests:
##   1. Fresh-user empty-list: the RPC returns a well-formed
##      `{recent_players: []}` response for a user who has never
##      finished a match.
##   2. Seeded-rows order/cap: writes a handful of recent_players
##      storage rows directly as the test user (with
##      permission_write=1 — see note in `_seed_row`) then asserts
##      list_recent_players returns them sorted by matched_at
##      descending and capped at the runtime's recentPlayersCap.
##
## The match_end → writeRecentPlayersForMatch path is covered by
## Go unit tests (recent_players_test.go) that lock the pair-
## count, dedup, [deleted]-filter, and value-shape contracts
## without needing a Nakama DB. End-to-end through a real match
## is exercised manually post-deploy and via the player-facing UI.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
## Runtime-side cap from recent_players.go. If the runtime bumps
## this, the test must update too (and the
## TestRecentPlayersCapConstantStable Go test will fail to remind
## us).
const _SERVER_CAP := 50

var _helper
var _user: Dictionary = {}


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func after_each() -> void:
	if not _user.is_empty():
		await _helper.delete_one_shot_account(_user)
	_user = {}


func test_list_recent_players_returns_empty_for_fresh_user() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var users: Array = await _helper.multi_session_anon(
		1, "compliance-recent-fresh")
	if users.size() != 1:
		pending("multi_session_anon did not return a user")
		return
	_user = users[0]
	var token: String = str(_user.token)

	var result: Dictionary = await _helper.session_rpc(
		"list_recent_players", token, null)
	assert_eq(
		result.status_code, 200,
		"list_recent_players: status=%d body=%s"
			% [result.status_code, result.text])
	assert_true(
		result.inner is Dictionary,
		"response not a dict: %s" % result.text)
	var entries: Variant = result.inner.get("recent_players")
	assert_true(
		entries is Array,
		"missing/wrong-typed recent_players field: %s"
			% result.text)
	assert_eq(
		(entries as Array).size(), 0,
		"fresh user should have empty list, got %d entries"
			% (entries as Array).size())


func test_list_recent_players_sorts_by_matched_at_desc_and_caps() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var users: Array = await _helper.multi_session_anon(
		1, "compliance-recent-seeded")
	if users.size() != 1:
		pending("multi_session_anon did not return a user")
		return
	_user = users[0]
	var token: String = str(_user.token)

	# Seed _SERVER_CAP + 3 rows so we can verify both the
	# descending-by-matched_at sort AND the cap truncation in one
	# request. matched_at values are spread far enough apart that
	# the descending sort is unambiguous. The test's row keys are
	# distinguishable fake UUIDs so the row shape (key = other
	# user_id) matches what writeRecentPlayersForMatch would have
	# produced.
	var base_time: int = Time.get_unix_time_from_system() as int
	var total: int = _SERVER_CAP + 3
	for i in total:
		var fake_id := _fake_user_id(i)
		var matched_at := base_time + i  # newest = highest index
		var ok: bool = await _seed_row(
			token, fake_id, matched_at)
		assert_true(
			ok, "seed row %d failed for fake_id=%s" % [i, fake_id])

	var result: Dictionary = await _helper.session_rpc(
		"list_recent_players", token, null)
	assert_eq(
		result.status_code, 200,
		"list_recent_players: status=%d body=%s"
			% [result.status_code, result.text])
	var entries: Variant = result.inner.get("recent_players")
	assert_true(
		entries is Array,
		"recent_players field missing/wrong-typed: %s"
			% result.text)
	var arr: Array = entries
	assert_eq(
		arr.size(), _SERVER_CAP,
		"response should cap at %d entries, got %d"
			% [_SERVER_CAP, arr.size()])

	# Descending sort: the highest-index fake_id should be first.
	# The cap drops the LOWEST-index rows, so we expect indices
	# [total-1, total-2, ..., total-_SERVER_CAP].
	var first_entry: Dictionary = arr[0]
	assert_eq(
		str(first_entry.get("user_id", "")),
		_fake_user_id(total - 1),
		"first entry should be newest (idx %d); got %s"
			% [total - 1, str(first_entry.get("user_id", ""))])
	var last_entry: Dictionary = arr[arr.size() - 1]
	assert_eq(
		str(last_entry.get("user_id", "")),
		_fake_user_id(total - _SERVER_CAP),
		"last entry should be (total-cap)=%d; got %s"
			% [
				total - _SERVER_CAP,
				str(last_entry.get("user_id", "")),
			])

	# Sanity-check the value shape on a representative row. Drift
	# here would mean the runtime started returning a different
	# JSON shape, which would silently break the cached_recent_
	# players client cache.
	for required in ["user_id", "username", "display_name",
			"matched_at"]:
		assert_true(
			first_entry.has(required),
			"entry missing required field %s: %s"
				% [required, str(first_entry)])


## Writes a recent_players storage row owned by the calling user
## via Nakama's standard /v2/storage write endpoint. NOTE: the
## permission_write here is `1` (owner-writable) which diverges
## from the production runtime's `0` (server-only). The test
## isn't trying to mimic the write-permission surface — it just
## needs rows in place so the LIST RPC has something to return.
## The list response shape is the production-locked contract; the
## permission value is internal and isn't exposed by
## list_recent_players.
func _seed_row(
	token: String,
	fake_other_id: String,
	matched_at: int,
) -> bool:
	var value := JSON.stringify({
		"user_id": fake_other_id,
		"username": "seeded-%s" % fake_other_id,
		"display_name": "Seeded %s" % fake_other_id,
		"matched_at": matched_at,
	})
	var body := {
		"objects": [{
			"collection": "recent_players",
			"key": fake_other_id,
			"value": value,
			"permission_read": 1,
			"permission_write": 1,
		}],
	}
	var result: Dictionary = await _helper.http_put(
		"/v2/storage", body, "bearer:" + token)
	return (
		result.status_code >= 200 and result.status_code < 300)


## Builds a Nakama-shaped UUID string seeded by `i` so each
## seeded row has a distinct, recognizable user_id for assertion
## comparisons. Not a real UUID — the runtime doesn't validate
## the `other` user_id format on the read path.
func _fake_user_id(i: int) -> String:
	return "00000000-0000-0000-0000-%012d" % i
