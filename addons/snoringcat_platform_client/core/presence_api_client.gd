class_name PlatformPresenceApiClient
extends Node
## Rich-presence write + online-friends read.
##
## Nakama doesn't have a built-in rich-presence wire, so we route
## through a custom runtime RPC `update_and_get_presence` in
## snoringcat-platform's runtime/. The RPC stores the caller's
## presence in Storage(scope=presence/{game_id}) and returns the
## union of all friends' presence rows in one round trip.
##
## Reads `Platform.nakama_client` and `Platform.token_store`. The
## consuming game's bootstrap is responsible for ensuring both are
## populated before any RPC method here is called.


## Online player IDs (caller's game by default).
signal presence_received(online_ids: Array[String])
## Online friends with current rich-presence string per id.
signal presence_received_rich(online_friends: Dictionary)
signal request_failed(error: String)


var cached_online_ids: Array[String] = []
var cached_online_friends: Dictionary = {}

var _is_presence_busy := false


## Publish this player's (rich_presence, status) and read every
## online friend's presence row back. Emits presence_received +
## presence_received_rich on success. On RPC failure (e.g. the
## runtime predates the RPC) emits empty results so consumers'
## UI stays in a defined state.
func fetch_presence(
	rich_presence: String = "",
	status: String = "online",
) -> void:
	if _is_presence_busy:
		return
	_is_presence_busy = true
	var session := _ensure_session()
	if session == null:
		_is_presence_busy = false
		return
	var rpc_result = await Platform.nakama_client.rpc_async(
		session, "update_and_get_presence",
		JSON.stringify({
			"rich_presence": rich_presence,
			"status": status,
		}))
	_is_presence_busy = false
	if rpc_result.is_exception():
		# Pre-RPC deploys: assume nobody online.
		cached_online_ids = []
		cached_online_friends = {}
		presence_received.emit(cached_online_ids)
		presence_received_rich.emit(cached_online_friends)
		return
	var data: Variant = JSON.parse_string(rpc_result.payload)
	if not (data is Dictionary):
		presence_received.emit([])
		presence_received_rich.emit({})
		return
	cached_online_ids.clear()
	for v in data.get("online_ids", []):
		cached_online_ids.append(str(v))
	cached_online_friends = data.get("online_friends", {})
	presence_received.emit(cached_online_ids)
	presence_received_rich.emit(cached_online_friends)


func is_presence_busy() -> bool: return _is_presence_busy


## Clear cached online state. Called on log-out / account switch.
func clear_cache() -> void:
	cached_online_ids.clear()
	cached_online_friends.clear()


func _ensure_session() -> NakamaSession:
	var s := Platform.build_session_from_store()
	if s == null:
		request_failed.emit("Not authenticated")
		return null
	return s
