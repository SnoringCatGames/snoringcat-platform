extends GutTest
## Layer 1 regression test for the WebRTC cross-play
## transport-selection rule. Probes the runtime's
## `transport_select` RPC (a pure function over a list of
## platform strings) so a refactor that changes the rule
## fails CI immediately, without needing a real match
## allocation.
##
## The rule (from runtime/transport_select.go):
##   any "web" → "webrtc"
##   otherwise → "enet"
## Empty / unknown platforms count as native (not web).


const _Helper = preload(
	"res://addons/snoringcat_platform_client/test/"
	+ "compliance/compliance_helper.gd"
)

var _helper


func before_each() -> void:
	_helper = _Helper.new()
	add_child_autofree(_helper)


func _select(platforms: Array) -> String:
	# Helper: call the RPC and return the transport_type
	# string, or an error string starting with "ERR" if the
	# call failed.
	var result: Dictionary = await _helper.http_key_rpc(
		"transport_select", {"platforms": platforms})
	if result.status_code != 200:
		return "ERR status=%d body=%s" % [
			result.status_code, result.text]
	if not (result.inner is Dictionary):
		return "ERR inner is not a dict: %s" % result.text
	return str(result.inner.get("transport_type", ""))


func test_native_only_match_uses_enet() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var got: String = await _select(["native", "native", "native"])
	assert_eq(got, "enet",
		"3 native players should pick ENet")


func test_any_web_player_triggers_webrtc() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	# 1 web + 2 native: cross-play, must be WebRTC.
	var got: String = await _select(["native", "web", "native"])
	assert_eq(got, "webrtc",
		"any web player should force WebRTC")


func test_all_web_match_uses_webrtc() -> void:
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var got: String = await _select(["web", "web"])
	assert_eq(got, "webrtc")


func test_empty_platform_strings_treated_as_native() -> void:
	# Older clients (or buggy ones) might send empty platform
	# strings. The runtime should treat them as native (not
	# trigger WebRTC for unknown values) — defaulting to ENet
	# is the safer choice since native+ENet is the most-tested
	# path.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var got: String = await _select(["", ""])
	assert_eq(got, "enet",
		"empty platform strings should not force WebRTC")


func test_empty_platform_list_defaults_to_enet() -> void:
	# Edge case: zero entries. Shouldn't 5xx; should fall
	# through to the default.
	if not _helper.is_live_mode():
		pending("mock mode not yet implemented")
		return
	if _helper.http_key().is_empty():
		pending("NAKAMA_HTTP_KEY not set")
		return
	var got: String = await _select([])
	assert_eq(got, "enet",
		"empty platforms list should default to ENet")
