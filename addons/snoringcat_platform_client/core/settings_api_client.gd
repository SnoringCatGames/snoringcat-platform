class_name PlatformSettingsApiClient
extends Node
## Cloud-backed settings storage. Splits settings into per-scope
## rows so one Nakama account can host multiple games' settings
## without one game overwriting the other.
##
## Storage layout:
##   collection="settings", key="global"       → cross-game prefs
##   collection="settings", key="game/{id}"    → per-game prefs
##
## Game code provides the serialize / apply mapping; the addon
## owns the round trip. Returns Nakama's storage-row update_time
## as the cloud sync-at timestamp (parsed RFC3339 → unix seconds)
## so consumers can implement cloud-wins-by-timestamp merge logic.
##
## Reads `Platform.token_store` and `Platform.get_nakama_client()`.


## Emitted on successful fetch. `payload` is empty when the row
## doesn't exist; `updated_at` is 0 in that case.
signal settings_received(
	scope: String, payload: Dictionary, updated_at: int)
## Emitted on successful save.
signal settings_saved(scope: String)
## Emitted on transport / auth failure.
signal request_failed(scope: String, error: String)


## Read a settings row by scope.
## Common scopes: "global", "game/{game_id}". Legacy callers pass
## "user" to read the pre-split single-blob row.
func fetch(scope: String) -> void:
	var session := _ensure_session(scope)
	if session == null:
		return
	var ids := [NakamaStorageObjectId.new(
		"settings", scope, session.user_id)]
	var result = await Platform.get_nakama_client().read_storage_objects_async(
		session, ids)
	if result.is_exception():
		request_failed.emit(
			scope, _describe(result.get_exception()))
		return
	if result.objects.size() == 0:
		settings_received.emit(scope, {}, 0)
		return
	var raw: String = result.objects[0].value
	var updated_at := _parse_iso8601_to_seconds(
		result.objects[0].update_time)
	var data: Variant = JSON.parse_string(raw)
	var payload: Dictionary = data if data is Dictionary else {}
	settings_received.emit(scope, payload, updated_at)


## Write a settings dict to the given scope. PermissionRead=1
## (owner-only), PermissionWrite=1 (owner-write); same access
## shape as the pre-split row.
func save(scope: String, payload: Dictionary) -> void:
	var session := _ensure_session(scope)
	if session == null:
		return
	var obj := NakamaWriteStorageObject.new(
		"settings", scope, 1, 1,
		JSON.stringify(payload), "")
	var result = await Platform.get_nakama_client().write_storage_objects_async(
		session, [obj])
	if result.is_exception():
		request_failed.emit(
			scope, _describe(result.get_exception()))
		return
	settings_saved.emit(scope)


func _ensure_session(scope: String) -> NakamaSession:
	var s := Platform.build_session_from_store()
	if s == null:
		request_failed.emit(scope, "Not authenticated")
		return null
	return s


func _describe(ex: NakamaException) -> String:
	if ex == null:
		return "Unknown Nakama error"
	return "%s (status=%d)" % [ex.message, ex.status_code]


## Parse Nakama's RFC3339 update_time string into a unix-seconds
## int. Returns 0 on parse failure or empty input.
func _parse_iso8601_to_seconds(s: String) -> int:
	if s.is_empty():
		return 0
	# Nakama emits strings like "2026-05-12T18:23:45.123Z". Strip
	# any fractional seconds + trailing "Z" before parsing — Godot's
	# Time.get_unix_time_from_datetime_string accepts only the
	# date+time portion.
	var trimmed := s
	var dot := trimmed.find(".")
	if dot >= 0:
		trimmed = trimmed.substr(0, dot)
	if trimmed.ends_with("Z"):
		trimmed = trimmed.substr(0, trimmed.length() - 1)
	var t := Time.get_unix_time_from_datetime_string(trimmed)
	return int(t)
