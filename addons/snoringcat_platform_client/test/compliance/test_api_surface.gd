extends GutTest
## Probe that the platform's authenticated routes exist and
## reject unauthenticated calls. Catches accidental route
## removal, rename, or auth-gate removal without needing real
## user credentials.
##
## Two levels:
##   - Nakama HTTP routes the SDK depends on: each must return
##     401 when called without a Bearer session token. A 404
##     means the route was renamed or removed (breaking change
##     for consumers); a 2xx means the auth gate is missing.
##   - Server-to-server runtime RPCs gated by HTTP key: each
##     must return 401 (or whatever Nakama returns for missing
##     http_key) when called without one. A 200 means the
##     server-to-server gate is missing.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

## (path, method, body) tuples for each Nakama HTTP route the
## SDK depends on. Add new rows as more endpoints ship; remove
## only when an endpoint is intentionally retired (and bump the
## major version of the platform contract).
const _AUTH_REQUIRED_ROUTES := [
	# Account.
	["GET", "/v2/account", null],
	# Friends. Nakama uses /v2/friend (singular, query-paramed).
	["GET", "/v2/friend", null],
	["POST", "/v2/friend?ids=00000000-0000-0000-0000-000000000000", null],
	["DELETE", "/v2/friend?ids=00000000-0000-0000-0000-000000000000", null],
	# Groups (parties are Nakama groups under the hood).
	["GET", "/v2/group", null],
	# Storage (used for cloud-synced settings).
	["GET", "/v2/storage/settings", null],
	# Notifications inbox.
	["GET", "/v2/notification", null],
]

## Server-to-server runtime RPCs that must require the HTTP key.
## Bare GETs of /v2/rpc/<name> without ?http_key= should not
## execute the runtime function.
const _S2S_RUNTIME_RPCS := [
	"runtime_status",
	"register_server",
	"match_end",
	"record_client_ip",
	"version_check",
	"transport_select",
]

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_unauth_routes_reject() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return

	var failures: Array[String] = []
	for route in _AUTH_REQUIRED_ROUTES:
		var method: String = route[0]
		var path: String = route[1]
		var body: Variant = route[2]
		var result: Dictionary
		match method:
			"GET":
				result = await _helper.http_get(path)
			"POST":
				result = await _helper.http_post(path, body)
			"PUT":
				result = await _helper.http_put(path, body)
			"DELETE":
				result = await _helper.http_delete(path)
			_:
				failures.append("%s %s: unsupported method"
					% [method, path])
				continue

		if not result.error.is_empty():
			failures.append(
				"%s %s: transport error %s"
					% [method, path, result.error])
			continue
		if result.status_code == 401:
			continue  # Pass.
		if result.status_code == 404:
			failures.append(
				"%s %s: 404 (route missing or renamed)"
					% [method, path])
		elif (
			result.status_code >= 200
			and result.status_code < 300
		):
			failures.append(
				"%s %s: %d (auth gate missing!)"
					% [method, path, result.status_code])
		# Anything else (4xx with a different code, 5xx) is
		# acceptable for "endpoint exists and isn't wide open".

	assert_true(
		failures.is_empty(),
		"Route surface failures:\n  - "
			+ "\n  - ".join(failures))


func test_runtime_rpcs_reject_without_http_key() -> void:
	# Nakama's REST RPC endpoint requires either an Authorization
	# header (session token) or ?http_key=<key>. Without either,
	# the runtime function does not execute and the response is
	# 401 (or 400 for malformed). A 200 means the gate is broken.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return

	var failures: Array[String] = []
	for rpc_name in _S2S_RUNTIME_RPCS:
		var path: String = "/v2/rpc/" + str(rpc_name)
		var result: Dictionary = await _helper.http_post(path, null)
		if not result.error.is_empty():
			failures.append(
				"%s: transport error %s"
					% [rpc_name, result.error])
			continue
		if result.status_code >= 200 and result.status_code < 300:
			# A 2xx without auth is the regression we care about
			# — could mean the requireServerToServer gate was
			# removed in a refactor.
			failures.append(
				"%s: %d (server-to-server gate missing!)"
					% [rpc_name, result.status_code])

	assert_true(
		failures.is_empty(),
		"Runtime RPC gate failures:\n  - "
			+ "\n  - ".join(failures))
