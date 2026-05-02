extends SceneTree
## End-to-end smoke test against the live snoringcat-platform.
##
## Loads the addon's api_client at runtime (avoids the Godot 4.6
## parser-cache bug with addon-internal preload) and calls
## /v1/version.
##
## Run from repo root:
##   powershell -Command "godot --headless --path . -s scripts/platform_smoke_test.gd"

const _API_URL := (
	"https://r20b7wqop6.execute-api.us-west-2.amazonaws.com/prod"
)
const _API_CLIENT_PATH := (
	"res://addons/snoringcat_platform_client/core/api_client.gd"
)


func _init() -> void:
	# Defer the actual work to the first physics frame so the
	# SceneTree is fully initialized (HTTPRequest needs the main
	# loop running to dispatch).
	process_frame.connect(_on_first_frame, CONNECT_ONE_SHOT)


func _on_first_frame() -> void:
	print("[smoke] Loading api_client.gd via runtime load...")
	var ApiClientScript: GDScript = load(_API_CLIENT_PATH)
	if ApiClientScript == null:
		print("[smoke] FAIL: load returned null")
		quit(3)
		return

	var api: Node = ApiClientScript.new()
	api.name = "PlatformApiClient"
	api.base_url = _API_URL
	root.add_child(api)

	api.response_received.connect(_on_response)
	api.request_failed.connect(_on_failed)
	print("[smoke] GET %s/v1/version" % _API_URL)
	api.do_get("/v1/version")


func _on_response(
	ok: bool,
	status_code: int,
	body: Dictionary,
	path: String,
) -> void:
	print(
		"[smoke] response: ok=%s status=%d path=%s body=%s"
		% [str(ok), status_code, path, str(body)])
	if ok:
		print("[smoke] PASS")
		quit(0)
	else:
		print("[smoke] FAIL: non-2xx")
		quit(1)


func _on_failed(error: String, path: String) -> void:
	print("[smoke] FAIL: transport %s on %s"
			% [error, path])
	quit(2)
