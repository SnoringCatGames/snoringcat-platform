extends GutTest
## Liveness + version contract: probe Nakama's /healthcheck and
## the snoringcat-platform runtime's `version_check` RPC. If
## either fails, no other compliance test result is meaningful.


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_healthcheck_returns_200() -> void:
	if not _helper.is_live_mode():
		pending(
			"mock mode not yet implemented;"
			+ " skipping live /healthcheck probe")
		return
	var result: Dictionary = await _helper.http_get("/healthcheck")
	assert_true(
		result.error.is_empty(),
		"transport error: %s" % result.error)
	assert_eq(
		result.status_code, 200,
		"non-200 status: %d body=%s"
			% [result.status_code, result.text])


func test_runtime_version_check_rpc_responds() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending(
			"NAKAMA_HTTP_KEY not set;"
			+ " can't probe version_check RPC")
		return
	# version_check is a server-to-server RPC that echoes the
	# runtime's view of the platform contract: the game_version
	# baked into runtime.env (NAKAMA_GAME_VERSION) and the
	# protocol_version. Empty body is the "tell me what you've
	# got" form.
	var result: Dictionary = await _helper.http_key_rpc("version_check")
	assert_true(
		result.error.is_empty(),
		"transport error: %s" % result.error)
	assert_eq(
		result.status_code, 200,
		"non-200: body=%s" % result.text)
	assert_not_null(
		result.inner,
		"version_check returned no inner payload: %s"
			% result.text)


func test_runtime_status_reports_edgegap_config() -> void:
	# This test catches a class of regressions where the runtime
	# loaded but the config.yml render lost a placeholder
	# substitution (a real bug we hit during the migration: the
	# matchmaker hook silently allocated against the literal
	# string "${EDGEGAP_APP_NAME}" instead of the actual app).
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending(
			"NAKAMA_HTTP_KEY not set;"
			+ " can't probe runtime_status RPC")
		return
	var result: Dictionary = await _helper.http_key_rpc("runtime_status")
	assert_eq(result.status_code, 200, "runtime_status RPC")
	assert_true(
		result.inner is Dictionary,
		"runtime_status payload not a dict: %s" % result.text)
	var info: Dictionary = result.inner

	# build_id should always be set (ldflags inject git SHA;
	# falls back to "dev" only on local builds without ldflags).
	assert_true(
		info.has("build_id"),
		"runtime_status missing build_id: %s" % str(info))
	assert_true(
		not str(info.get("build_id", "")).is_empty(),
		"build_id is empty (ldflags injection broken?)")

	# Edgegap config should be populated (no unsubstituted
	# placeholders).
	var app_name: String = str(info.get("edgegap_app_name", ""))
	var app_version: String = str(info.get("edgegap_app_version", ""))
	assert_true(
		not app_name.is_empty(),
		"edgegap_app_name empty: %s" % str(info))
	assert_false(
		app_name.contains("${"),
		"edgegap_app_name has unsubstituted placeholder: '%s'"
			% app_name)
	assert_true(
		not app_version.is_empty(),
		"edgegap_app_version empty: %s" % str(info))
	assert_false(
		app_version.contains("${"),
		"edgegap_app_version has unsubstituted placeholder: '%s'"
			% app_version)
	# The matchmaker hook is registered iff EDGEGAP_TOKEN is set.
	# A live deployment should always have it set; a missing hook
	# means matched players never get match_ready notifications.
	assert_true(
		info.get("edgegap_token_set", false),
		"matchmaker hook is not active (EDGEGAP_TOKEN unset)")
