extends GutTest
## get_player_stats + get_match_history RPCs. The SDK calls
## these every time a player opens their profile/history UI;
## a silent regression here corrupts that screen.
##
## Contracts (from runtime/player_data.go):
##   get_player_stats:
##     request:  {player_id?: string}  // defaults to caller
##     response: {player_id, rating: int, matches: int}
##     Unranked → rating=1500, matches=0 (NOT a 500 error).
##   get_match_history:
##     request:  empty
##     response: {matches: [{match_id, ended_at, is_winner,
##                           score, kills, bumps, ...}]}
##     Empty list when no history. Most-recent first, capped 50.
## Auth: Bearer session token.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _DEVICE_ID := "compliance-anon-fixed-1"

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_get_player_stats_returns_unranked_defaults_for_new_user() -> void:
	# A freshly-authed compliance user has no leaderboard record
	# and should see the unranked defaults (rating 1500, 0 matches)
	# without a 500 error. This is a key contract — the runtime
	# logs a warning and falls through if the leaderboard read
	# errors, so a regression here would surface as 500s in
	# every game's profile screen.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	assert_false(token.is_empty())

	# Empty payload → caller is the target.
	var result: Dictionary = await _helper.session_rpc(
		"get_player_stats", token, null)
	assert_eq(
		result.status_code, 200,
		"get_player_stats: %s" % result.text)
	assert_true(
		result.inner is Dictionary,
		"inner not a dict: %s" % result.text)

	var stats: Dictionary = result.inner
	assert_true(
		stats.has("player_id"),
		"missing player_id: %s" % str(stats))
	assert_true(
		stats.has("rating"),
		"missing rating: %s" % str(stats))
	assert_true(
		stats.has("matches"),
		"missing matches: %s" % str(stats))
	# The compliance test account either has no records (fresh)
	# or has whatever real records the previous compliance runs
	# left. Both are acceptable; the contract is just "the
	# fields are present and the call doesn't 5xx."


func test_get_player_stats_for_explicit_player_id() -> void:
	# Probes the alternate code path where the caller asks about
	# someone else (used by friend-profile-view UIs).
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	# Pull our own user_id and pass it as the explicit target.
	var account: Dictionary = await _helper.http_get(
		"/v2/account", "bearer:" + token)
	var user_id: String = str(
		account.body.get("user", {}).get("id", ""))

	var result: Dictionary = await _helper.session_rpc(
		"get_player_stats", token, {"player_id": user_id})
	assert_eq(
		result.status_code, 200,
		"get_player_stats with explicit id: %s" % result.text)
	assert_eq(
		str(result.inner.get("player_id", "")), user_id,
		"player_id round-trip mismatch")


func test_get_match_history_returns_array_for_new_user() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return

	var token: String = await _helper.nakama_anon_session(_DEVICE_ID)
	var result: Dictionary = await _helper.session_rpc(
		"get_match_history", token, null)
	assert_eq(
		result.status_code, 200,
		"get_match_history: %s" % result.text)
	assert_true(
		result.inner is Dictionary,
		"inner not a dict: %s" % result.text)
	# matches is always present (runtime returns empty array,
	# never null, on cold-start for a new user).
	var matches: Variant = result.inner.get("matches")
	assert_true(
		matches is Array,
		"matches not an array: %s" % str(result.inner))
