extends GutTest
## get_player_stats + get_match_history RPCs. The SDK calls
## these every time a player opens their profile/history UI;
## a silent regression here corrupts that screen.
##
## Contracts (from runtime/player_data.go):
##   get_player_stats:
##     request:  {player_id?: string}  // defaults to caller
##     response: {player_id, best_score: int|null, rank: int|null,
##                rating: int, matches: int}
##     best_score is the player's own best single-match score and
##     rank their place on the public leaderboard; both null before
##     the first recorded match. rating (best_score, or 1500) and
##     matches (Nakama's num_score) are deprecated. Unranked is NOT
##     a 500 error.
##   get_match_history:
##     request:  empty
##     response: {matches: [{match_id, ended_at, is_winner,
##                           score, kills, bumps, ...}],
##                totals: {matches, wins, kills, bumps}}
##     Empty list and zero totals when no history. Newest first,
##     capped 50; totals cover every match.
## test_public_leaderboard.gd checks real values after a match.
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
	for field in ["player_id", "best_score", "rank", "rating", "matches"]:
		assert_true(
			stats.has(field),
			"missing %s: %s" % [field, str(stats)])
	# The shared compliance account never plays a match, so it is
	# unranked: no best score and no rank, null rather than zero.
	assert_null(stats.get("best_score", 0), "best_score: %s" % str(stats))
	assert_null(stats.get("rank", 0), "rank: %s" % str(stats))


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
	var totals: Variant = result.inner.get("totals")
	assert_true(
		totals is Dictionary and totals.has("matches"),
		"totals missing: %s" % str(result.inner))
