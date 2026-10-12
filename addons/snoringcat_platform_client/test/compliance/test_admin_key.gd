extends GutTest
## register_game needs GAME_ADMIN_KEY on top of the runtime HTTP key.
##
## The HTTP key shipped in client builds up to 0.47.0, so it can't be
## what stands between the internet and a game's live config:
## register_game replaces the whole thing (matchmaker modes,
## allocator mode, Edgegap app version, legal paths). The runtime
## also refuses every call while GAME_ADMIN_KEY is unset.
##
## Every request here sends a config that fails validation (no
## display_name). A refused call never gets that far, and an
## accepted one stops there, so no test writes anything, wherever the
## suite points.

const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

## Parses, but fails validation on the missing display_name.
const _INVALID_CONFIG := {
	"schema_version": 1,
	"game_id": "compliance-admin-key-probe",
}
## What the runtime says when validation runs, i.e. when the gate let
## the call through.
const _VALIDATION_ERROR := "display_name required"
const _WRONG_ADMIN_KEY := "compliance-wrong-admin-key"
const _HTTP_BAD_REQUEST := 400
const _HTTP_UNAUTHORIZED := 401

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func test_register_game_refuses_the_http_key_alone() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var result: Dictionary = await _helper.http_key_rpc(
		"register_game", _INVALID_CONFIG)
	_assert_refused_by_admin_gate(result)


func test_register_game_refuses_a_wrong_admin_key() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var result: Dictionary = await _helper.admin_key_rpc(
		"register_game", _WRONG_ADMIN_KEY, _INVALID_CONFIG)
	_assert_refused_by_admin_gate(result)


func test_register_game_accepts_the_admin_key() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty() or _helper.admin_key().is_empty():
		pending("NAKAMA_HTTP_KEY or GAME_ADMIN_KEY not set")
		return
	var result: Dictionary = await _helper.admin_key_rpc(
		"register_game", _helper.admin_key(), _INVALID_CONFIG)
	assert_eq(
		result.status_code,
		_HTTP_BAD_REQUEST,
		"the right key should reach validation: %s" % result.text,
	)
	assert_string_contains(result.text, _VALIDATION_ERROR)


func test_runtime_status_reports_admin_key_without_the_key() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var result: Dictionary = await _helper.http_key_rpc("runtime_status")
	assert_eq(result.status_code, 200, "runtime_status: %s" % result.text)
	var info: Dictionary = (
		result.inner if result.inner is Dictionary else {})
	assert_true(
		info.get("admin_key_configured") is bool,
		"runtime_status has no admin_key_configured boolean: %s"
		% result.text,
	)
	var key: String = _helper.admin_key()
	if key.is_empty():
		return
	assert_true(
		info.get("admin_key_configured", false),
		"GAME_ADMIN_KEY is set here, so the runtime should have it",
	)
	assert_false(
		result.text.contains(key), "runtime_status echoed the admin key")


## A refused call is 401 (key missing or wrong) or 400 when the
## runtime has no GAME_ADMIN_KEY at all. Either way the error names
## the admin key, and validation never ran.
func _assert_refused_by_admin_gate(result: Dictionary) -> void:
	assert_true(
		result.error.is_empty(), "transport error: %s" % result.error)
	assert_true(
		result.status_code in [_HTTP_UNAUTHORIZED, _HTTP_BAD_REQUEST],
		"register_game was not refused: %d %s"
		% [result.status_code, result.text],
	)
	assert_true(
		result.text.contains("admin_key")
		or result.text.contains("GAME_ADMIN_KEY"),
		"refusal should come from the admin-key gate: %s" % result.text,
	)
	assert_false(
		result.text.contains(_VALIDATION_ERROR),
		"the call got past the admin-key gate: %s" % result.text,
	)
