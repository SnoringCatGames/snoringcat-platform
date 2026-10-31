extends GutTest
## The runtime's public leaderboard route, GET /snoringcat/leaderboard
## (runtime/leaderboard_public.go). It needs no credential, returns
## only rank, display name, and score per player, allows any origin,
## and lists only players who may be listed.
##
## The data test records scores the way production does. Five
## one-shot players matchmake together on the mock-deploy runtime,
## then the test plays the game server's part: register_server and
## match_end with the HTTP key. match_end can write to the board only
## because register_game created it from game.yaml (the stack boots
## with no games registered), so that is covered too. It then checks
## that get_player_stats gives each player their own best score and
## the route's rank, and that get_match_history totals the match.
##
## The route caches each board for 60 seconds, but match_end
## invalidates that cache, so the data test sees its own scores even
## when an earlier test (or get_player_stats) has read the board. The
## other tests send only requests the route refuses before reading
## anything, and preflights, which Nakama's CORS middleware answers
## itself.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)
const _SocketHelper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_socket_helper.gd"
)
## The route: the runtime's publicLeaderboardHTTPPath. Spelled out
## so the contract is pinned independently of the runtime.
const _PATH := "/snoringcat/leaderboard"
## The board game.yaml declares.
const _BOARD := "ffa"
## The origin a browser on the game website sends.
const _ORIGIN := "https://hopnbop.net"
## Display name delete_account anonymizes to.
const _ANONYMIZED_DISPLAY_NAME := "[deleted]"
## Every field the response may have, sorted.
const _RESPONSE_KEYS := [
	"entries", "game_id", "generated_at", "leaderboard",
	"next_reset_at", "operator", "sort_order", "version",
]
## Every field an entry may have, sorted.
const _ENTRY_KEYS := ["display_name", "rank", "score"]
## The response schema version.
const _VERSION := 1
## Players in the match: listed Alpha and Bravo, plus one each of
## the deleted, nameless, and probe accounts the route must hide.
const _PLAYER_COUNT := 5
## Scores sit at a random base, so a rerun against a kept stack can't
## mistake an earlier run's records for this run's. Every base keeps
## the scores under match_end's clamp (maxScore, 100000).
const _SCORE_BASE := 20000
const _SCORE_BASE_STEPS := 700
const _SCORE_BASE_STEP := 100
## Offsets from the base. Every hidden player outscores Alpha, so a
## leak would show up at the top of the board.
const _SCORE_DELETED := 9
const _SCORE_PROBE := 7
const _SCORE_NAMELESS := 5
const _SCORE_ALPHA := 3
const _SCORE_BRAVO := 1
## Matchmaker pool tick plus the runtime hook's fan-out, as in
## test_matchmaking.gd.
const _MATCH_TIMEOUT_SEC := 8.0
const _NOTIFY_TIMEOUT_SEC := 5.0
## What register_server reports. A documentation-range address: the
## mock deploy has no real server.
const _SERVER_IP := "203.0.113.7"
const _SERVER_PORT := 4433
## The most entries the route returns.
const _MAX_LIMIT := 100

var _helper
var _sock_helper
## One-shot users to hard-delete in after_each. Deleting an account
## also deletes its leaderboard records.
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


func test_lists_ranked_scores_without_credentials() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.server_key_basic_header().is_empty():
		pending("NAKAMA_SERVER_KEY env var not set")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY env var not set")
		return
	# Matchmaking allocates a game server, so stay on a mock-deploy
	# runtime.
	if not await _helper.is_mock_deploy_mode():
		pending("needs a runtime with EDGEGAP_MOCK_DEPLOY=true")
		return

	var run_id := "%d-%d" % [
		Time.get_unix_time_from_system() as int,
		randi() % 100000,
	]
	var base := _SCORE_BASE + (randi() % _SCORE_BASE_STEPS) * _SCORE_BASE_STEP
	var minted: Array = await _helper.multi_session_anon(
		_PLAYER_COUNT - 1, "compliance-leaderboard")
	_users.append_array(minted)
	if minted.size() != _PLAYER_COUNT - 1:
		pending("multi_session_anon did not return the users")
		return
	var probe: Dictionary = await _mint_probe_user(run_id)
	if probe.is_empty():
		pending("could not mint the probe-named account")
		return
	_users.append(probe)
	var alpha: Dictionary = minted[0]
	var bravo: Dictionary = minted[1]
	var deleted: Dictionary = minted[2]
	var nameless: Dictionary = minted[3]

	var alpha_name := "LB Alpha %s" % run_id
	var bravo_name := "LB Bravo %s" % run_id
	var deleted_name := "LB Delta %s" % run_id
	var probe_name := "LB Papa %s" % run_id
	for named in [
		[alpha, alpha_name], [bravo, bravo_name],
		[deleted, deleted_name], [probe, probe_name],
	]:
		if not await _set_display_name(named[0], named[1]):
			return
	# Fresh accounts get a generated name at creation (SP106);
	# clear the nameless player's through the console so the
	# route's hide rule for pre-H59 accounts still applies.
	if not _helper.get_console_refusal().is_empty():
		pending(_helper.get_console_refusal())
		return
	assert_true(
		await _helper.console_update_account(
			str(nameless.user_id), {"display_name": ""}),
		"console could not clear the display name")
	# The deleted player asks for deletion first and keeps playing
	# through the grace window, so their record is written after the
	# cascade cleared the board, and only the route's own rules can
	# hide it.
	var deletion: Dictionary = await _helper.session_rpc(
		"delete_account", str(deleted.token), null)
	assert_eq(
		deletion.status_code, 200,
		"delete_account: %s" % _helper.redact_tokens(deletion.text))
	# delete_account ended that session; the player signs
	# back in to keep playing through the grace window, so
	# the steps below use the fresh token.
	var renewed: String = await _helper.nakama_anon_session(
		str(deleted.device_id))
	assert_false(
		renewed.is_empty(), "deleted player re-sign-in failed")
	if renewed.is_empty():
		return
	deleted.token = renewed

	var request_id: String = await _matchmake(_users, run_id)
	if request_id.is_empty():
		return

	var registered: Dictionary = await _helper.http_key_rpc(
		"register_server",
		{
			"request_id": request_id,
			"server_ip": _SERVER_IP,
			"server_port": _SERVER_PORT,
		})
	assert_eq(
		registered.status_code, 200,
		"register_server: %s" % registered.text)
	var scores := {
		str(alpha.user_id): base + _SCORE_ALPHA,
		str(bravo.user_id): base + _SCORE_BRAVO,
		str(deleted.user_id): base + _SCORE_DELETED,
		str(nameless.user_id): base + _SCORE_NAMELESS,
		str(probe.user_id): base + _SCORE_PROBE,
	}
	var players: Array = []
	for user_id: String in scores:
		players.append({
			"user_id": user_id,
			"score": scores[user_id],
			"kills": 1,
			"bumps": 1,
		})
	var ended: Dictionary = await _helper.http_key_rpc(
		"match_end",
		{
			"request_id": request_id,
			"winner_id": str(deleted.user_id),
			"players": players,
		})
	assert_eq(ended.status_code, 200, "match_end: %s" % ended.text)
	if ended.status_code != 200:
		return

	var result: Dictionary = await _get_board(
		"?game_id=%s&leaderboard=%s&limit=%d"
		% [_game_id().uri_encode(), _BOARD, _MAX_LIMIT])
	assert_eq(
		result.status_code, 200,
		"GET %s: %d %s" % [_PATH, result.status_code, result.text])
	_assert_public_headers(result)
	assert_eq(
		_helper.header_value(result, "Cache-Control"),
		"public, max-age=60",
		"a leaderboard response is cacheable for its 60 seconds")
	if not (result.body is Dictionary):
		fail_test("response is not a JSON object: %s" % result.text)
		return
	var body: Dictionary = result.body

	# The field allowlist, at both levels.
	var keys: Array = body.keys()
	keys.sort()
	assert_eq(keys, _RESPONSE_KEYS, "response fields")
	assert_eq(int(body.get("version", 0)), _VERSION, "version")
	assert_eq(str(body.get("game_id", "")), _game_id(), "game_id")
	assert_eq(str(body.get("leaderboard", "")), _BOARD, "leaderboard")
	assert_eq(str(body.get("sort_order", "")), "desc", "sort_order")
	assert_eq(str(body.get("operator", "")), "best", "operator")
	assert_null(
		body.get("next_reset_at", 0),
		"game.yaml's ffa board never resets")
	var entries: Variant = body.get("entries")
	if not (entries is Array):
		fail_test("entries is not a list: %s" % result.text)
		return
	for entry: Variant in entries:
		if not (entry is Dictionary):
			fail_test("an entry is not an object: %s" % str(entry))
			return
		var entry_keys: Array = (entry as Dictionary).keys()
		entry_keys.sort()
		assert_eq(entry_keys, _ENTRY_KEYS, "entry fields: %s" % str(entry))
		assert_false(
			str(entry.get("display_name", "")).strip_edges().is_empty(),
			"an entry has no display name: %s" % str(entry))

	_assert_ranked(entries)

	# Alpha outranks Bravo, both at their own scores.
	var alpha_at := _index_of_name(entries, alpha_name)
	var bravo_at := _index_of_name(entries, bravo_name)
	assert_true(alpha_at >= 0, "Alpha is not listed: %s" % result.text)
	assert_true(bravo_at >= 0, "Bravo is not listed: %s" % result.text)
	if alpha_at >= 0 and bravo_at >= 0:
		assert_lt(alpha_at, bravo_at, "Alpha should be above Bravo")
		assert_eq(
			int(entries[alpha_at].score), base + _SCORE_ALPHA,
			"Alpha's score")
		assert_eq(
			int(entries[bravo_at].score), base + _SCORE_BRAVO,
			"Bravo's score")
		assert_lt(
			int(entries[alpha_at].rank), int(entries[bravo_at].rank),
			"Alpha's rank should be better than Bravo's")

	# The hidden players are nowhere, under any name.
	for name in [_ANONYMIZED_DISPLAY_NAME, deleted_name, probe_name]:
		assert_eq(
			_index_of_name(entries, name), -1,
			"%s must not be listed: %s" % [name, result.text])
	for hidden_score in [
		base + _SCORE_DELETED, base + _SCORE_PROBE, base + _SCORE_NAMELESS,
	]:
		for entry: Variant in entries:
			assert_ne(
				int(entry.score), hidden_score,
				"a hidden player's score is listed: %s" % str(entry))

	# No identifier of any player appears anywhere in the response.
	for user: Dictionary in _users:
		for field in ["user_id", "username", "device_id"]:
			var value := str(user.get(field, ""))
			if value.is_empty():
				continue
			assert_false(
				result.text.contains(value),
				"the response reveals a %s" % field)

	# get_player_stats agrees with the route: a player's own best
	# score and their public rank. The deleted player holds the
	# board's top record, which the runtime used to hand everyone.
	if alpha_at >= 0:
		await _assert_player_stats(
			alpha, base + _SCORE_ALPHA, int(entries[alpha_at].rank))
	await _assert_player_stats(deleted, base + _SCORE_DELETED, 0)
	await _assert_player_stats(nameless, base + _SCORE_NAMELESS, 0)

	# get_match_history totals the match for each player.
	var history: Dictionary = await _helper.session_rpc(
		"get_match_history", str(alpha.token), {})
	assert_eq(history.status_code, 200, "get_match_history: %s"
		% _helper.redact_tokens(history.text))
	var totals: Variant = (
		history.inner.get("totals") if history.inner is Dictionary else null)
	assert_eq(
		totals,
		{"matches": 1.0, "wins": 0.0, "kills": 1.0, "bumps": 1.0},
		"Alpha's totals: %s" % _helper.redact_tokens(history.text))


func test_refuses_bad_requests_without_credentials() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	var game_id := _game_id().uri_encode()
	var requests := [
		["", 400, "missing_game_id"],
		["?game_id=%s" % game_id, 400, "missing_leaderboard"],
		[
			"?game_id=compliance-no-such-game&leaderboard=%s" % _BOARD,
			404, "unknown_game",
		],
		[
			"?game_id=%s&leaderboard=compliance_no_such_board" % game_id,
			404, "unknown_leaderboard",
		],
		[
			"?game_id=%s&leaderboard=%s&limit=0" % [game_id, _BOARD],
			400, "invalid_limit",
		],
		[
			"?game_id=%s&leaderboard=%s&limit=%d"
			% [game_id, _BOARD, _MAX_LIMIT + 1],
			400, "invalid_limit",
		],
	]
	for request: Array in requests:
		var result: Dictionary = await _get_board(request[0])
		assert_eq(
			result.status_code, request[1],
			"GET %s%s: %s" % [_PATH, request[0], result.text])
		_assert_error_body(result, request[2])

	var posted: Dictionary = await _helper.http_request(
		HTTPClient.METHOD_POST,
		"%s?game_id=%s&leaderboard=%s" % [_PATH, game_id, _BOARD],
		{},
		"",
		PackedStringArray(["Origin: " + _ORIGIN]))
	assert_eq(posted.status_code, 405, "POST: %s" % posted.text)
	_assert_error_body(posted, "method_not_allowed")


func test_cors_preflight_is_allowed() -> void:
	# A plain GET needs no preflight, but a page that adds a header
	# would send one. Nakama's CORS middleware answers it.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	var result: Dictionary = await _helper.http_request(
		HTTPClient.METHOD_OPTIONS,
		"%s?game_id=%s&leaderboard=%s"
		% [_PATH, _game_id().uri_encode(), _BOARD],
		null,
		"",
		PackedStringArray([
			"Origin: " + _ORIGIN,
			"Access-Control-Request-Method: GET",
		]))
	assert_true(
		result.status_code >= 200 and result.status_code < 300,
		"preflight: %d %s" % [result.status_code, result.text])
	assert_eq(
		_helper.header_value(result, "Access-Control-Allow-Origin"), "*",
		"preflight Access-Control-Allow-Origin")


## Sends an uncredentialed GET of the route, as a browser on the game
## website would.
func _get_board(query: String) -> Dictionary:
	return await _helper.http_request(
		HTTPClient.METHOD_GET,
		_PATH + query,
		null,
		"",
		PackedStringArray(["Origin: " + _ORIGIN]))


## Headers every response of the route carries.
func _assert_public_headers(result: Dictionary) -> void:
	assert_eq(
		_helper.header_value(result, "Access-Control-Allow-Origin"), "*",
		"Access-Control-Allow-Origin")
	assert_eq(
		_helper.header_value(result, "Cross-Origin-Resource-Policy"),
		"cross-origin",
		"Cross-Origin-Resource-Policy")
	assert_eq(
		_helper.header_value(result, "Content-Type"), "application/json",
		"Content-Type")


## An error response: the public headers, never cached, and a body
## naming the error.
func _assert_error_body(result: Dictionary, code: String) -> void:
	_assert_public_headers(result)
	assert_eq(
		_helper.header_value(result, "Cache-Control"), "no-store",
		"an error is never cached")
	var body: Variant = result.body
	assert_true(
		body is Dictionary
		and int(body.get("version", 0)) == _VERSION
		and str(body.get("error", "")) == code
		and not str(body.get("message", "")).is_empty(),
		"want error %s, got %s" % [code, result.text])


## Entries come best first, and equal scores share a rank (1, 2, 2,
## 4).
func _assert_ranked(entries: Array) -> void:
	for i in entries.size():
		var rank := int(entries[i].rank)
		var score := int(entries[i].score)
		if i == 0:
			assert_eq(rank, 1, "the first entry should rank 1")
			continue
		var previous_score := int(entries[i - 1].score)
		assert_true(
			score <= previous_score,
			"entries out of order at %d: %s" % [i, str(entries)])
		var want := i + 1
		if score == previous_score:
			want = int(entries[i - 1].rank)
		assert_eq(
			rank, want, "entry %d rank (%s)" % [i, str(entries[i])])


## Checks get_player_stats for `user`: their own best score, and
## their public rank (0 for a player the route doesn't list, which
## the RPC reports as null).
func _assert_player_stats(
	user: Dictionary,
	best_score: int,
	rank: int,
) -> void:
	var result: Dictionary = await _helper.session_rpc(
		"get_player_stats", str(user.token), {})
	assert_eq(result.status_code, 200, "get_player_stats: %s"
		% _helper.redact_tokens(result.text))
	if not (result.inner is Dictionary):
		fail_test("get_player_stats: %s" % _helper.redact_tokens(result.text))
		return
	var stats: Dictionary = result.inner
	assert_eq(
		int(stats.get("best_score", -1)), best_score,
		"best_score should be the player's own: %s" % str(stats))
	if rank > 0:
		assert_eq(
			int(stats.get("rank", 0)), rank,
			"rank should match the public route: %s" % str(stats))
	else:
		assert_null(
			stats.get("rank", 0),
			"an unlisted player has no public rank: %s" % str(stats))


## Returns the index of the entry listed under `display_name`, or -1.
func _index_of_name(entries: Array, display_name: String) -> int:
	for i in entries.size():
		if str(entries[i].get("display_name", "")) == display_name:
			return i
	return -1


## Mints a one-shot account whose username marks it as a
## synthetic-match-probe identity (the probe signs in as
## `probe-<host>-<slot>`). Returns {} on failure.
func _mint_probe_user(run_id: String) -> Dictionary:
	var device_id := "compliance-leaderboard-probe-%s" % run_id
	var username := "probe-compliance-%s" % run_id
	var result: Dictionary = await _helper.http_post(
		"/v2/account/authenticate/device?create=true&username=%s"
		% username.uri_encode(),
		_helper.device_auth_body(device_id),
		"basic_server_key")
	if result.status_code != 200 or not (result.body is Dictionary):
		return {}
	var token := str(result.body.get("token", ""))
	if token.is_empty():
		return {}
	var claims: Dictionary = _helper.decode_jwt_claims(token)
	return {
		"token": token,
		"refresh_token": str(result.body.get("refresh_token", "")),
		"user_id": str(claims.get("uid", "")),
		"username": str(claims.get("usn", "")),
		"device_id": device_id,
	}


## Sets a user's display name. Fails the test and returns false when
## Nakama refuses.
func _set_display_name(user: Dictionary, display_name: String) -> bool:
	var update: Dictionary = await _helper.http_put(
		"/v2/account", {"display_name": display_name},
		"bearer:" + str(user.token))
	var ok: bool = update.status_code >= 200 and update.status_code < 300
	assert_true(
		ok,
		"display_name update: %d %s"
		% [update.status_code, _helper.redact_tokens(update.text)])
	return ok


## Puts every user in one match on the mock-deploy runtime and returns
## its request_id, or "" after failing the test or pending.
func _matchmake(users: Array, run_id: String) -> String:
	var connection: Dictionary = await _sock_helper.connect_users(users)
	if not connection.ok:
		pending(str(connection.reason))
		return ""
	var sockets: Array = connection.sockets
	var captures: Array = []
	for sock in sockets:
		var capture := _SocketHelper.NotificationCapture.new()
		capture.attach(sock)
		captures.append(capture)
	# Ticket properties as the game sends them (see
	# test_matchmaking.gd); compliance_run pairs this run's tickets
	# with each other only.
	var props: Dictionary = {
		"platform": "native",
		"player_count": "1",
		"game_id": _game_id(),
		"client_protocol_version": _protocol_version_for_ticket(),
		"game_mode": _BOARD,
		"compliance_run": run_id,
	}
	var count := users.size()
	for i in sockets.size():
		var ticket: Variant = await sockets[i].add_matchmaker_async(
			"+properties.compliance_run:" + run_id, count, count, props, {})
		if ticket == null or ticket.is_exception():
			fail_test("user %d add_matchmaker failed: %s" % [i, str(ticket)])
			_close_all(captures, sockets)
			return ""
	var is_settled := func() -> bool:
		for capture in captures:
			if not capture.match_failed.is_empty():
				return true
			if capture.match_ready.is_empty():
				return false
		return true
	await _sock_helper.wait_until(
		is_settled, _MATCH_TIMEOUT_SEC + _NOTIFY_TIMEOUT_SEC)
	_close_all(captures, sockets)
	var request_id := ""
	for i in captures.size():
		var capture: Variant = captures[i]
		if not capture.match_failed.is_empty():
			fail_test("user %d got match_failed: %s"
				% [i, str(capture.match_failed)])
			return ""
		if capture.match_ready.is_empty():
			fail_test("user %d got no match_ready" % i)
			return ""
		var id := str(capture.match_ready.get("request_id", ""))
		if request_id.is_empty():
			request_id = id
		assert_eq(id, request_id, "every player should get one deploy")
	return request_id


## Detaches every capture, then closes every socket.
func _close_all(captures: Array, sockets: Array) -> void:
	for capture in captures:
		capture.detach()
	for sock in sockets:
		sock.close()


## The game_id the suite signs in with, defaulting to hopnbop like
## test_matchmaking.gd's ticket resolver.
func _game_id() -> String:
	var resolved: String = _helper.game_id()
	if resolved.is_empty():
		return "hopnbop"
	return resolved


## The client's protocol_version as a ticket property string, from
## ProjectSettings, as NakamaMatchmakerClient sends it.
func _protocol_version_for_ticket() -> String:
	if not ProjectSettings.has_setting(
			"application/config/protocol_version"):
		return "0"
	return str(int(ProjectSettings.get_setting(
		"application/config/protocol_version")))
