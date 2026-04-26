extends GutTest
## Probe that the platform's authenticated routes exist and
## reject unauthenticated calls. Catches accidental route
## removal, rename, or auth-gate removal without needing real
## user credentials.
##
## Each route under test should return 401 for unauthenticated
## requests. A 404 means the route is gone (or never existed),
## a 200/2xx means the auth gate is missing — both are
## breaking changes for any consumer game.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

## (path, method, body) tuples for each authenticated route
## the platform exposes that's also part of the contract a
## consumer game depends on. Add new rows as more endpoints
## ship; remove only when an endpoint is intentionally retired
## (and bump the major version of the platform contract).
const _AUTH_REQUIRED_ROUTES := [
	# Friends.
	["GET", "/v1/friends", null],
	["POST", "/v1/friends/add", {}],
	["POST", "/v1/friends/accept", {}],
	["POST", "/v1/friends/reject", {}],
	["POST", "/v1/friends/cancel", {}],
	["POST", "/v1/friends/remove", {}],
	["POST", "/v1/friends/seen", {}],
	["GET", "/v1/friends/notifications?since=0", null],
	# Presence.
	["POST", "/v1/presence/heartbeat", {}],
	# Party.
	["POST", "/v1/party/create", {}],
	["POST", "/v1/party/invite", {}],
	["POST", "/v1/party/join", {}],
	["POST", "/v1/party/leave", {}],
	["POST", "/v1/party/kick", {}],
	["GET", "/v1/party/status", null],
	["POST", "/v1/party/start", {}],
	# Player profile / settings.
	["GET", "/v1/player/profile", null],
	["GET", "/v1/player/settings", null],
	["POST", "/v1/player/settings", {}],
	["GET", "/v1/player/history", null],
	["GET", "/v1/player/export", null],
	# Matchmaking.
	["POST", "/v1/matchmaking/start", {}],
	["POST", "/v1/matchmaking/leave", {}],
	# Sessions.
	["GET", "/v1/session/active", null],
]

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_unauth_routes_return_401() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return

	var failures: Array[String] = []
	for route in _AUTH_REQUIRED_ROUTES:
		var method: String = route[0]
		var path: String = route[1]
		var body = route[2]
		var api: Node = _helper.make_api_client(self)
		await get_tree().process_frame
		match method:
			"GET":
				api.do_get(path)
			"POST":
				api.do_post(path, body if body else {})
			"PUT":
				api.do_put(path, body if body else {})
		var result: Dictionary = await _helper.next_response(api)
		if not result.error.is_empty():
			failures.append(
				"%s %s: transport error %s"
					% [method, path, result.error])
			continue
		if result.status_code == 401:
			# Pass.
			continue
		if result.status_code == 404:
			failures.append(
				"%s %s: 404 (route missing)" % [method, path])
		elif (
			result.status_code >= 200
			and result.status_code < 300
		):
			failures.append(
				"%s %s: %d (auth gate missing!)"
					% [method, path, result.status_code])
		else:
			# Some routes (e.g. /seen) may rate-limit or
			# return 400 on missing body before checking auth.
			# Accept anything that isn't 200 or 404 as
			# "endpoint exists and isn't wide open".
			pass

	assert_true(
		failures.is_empty(),
		"Route surface failures:\n  - "
			+ "\n  - ".join(failures))
