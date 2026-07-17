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


## Visible player IDs (caller's game by default). "Online" here
## means "visible" — it includes friends in a match, not just
## friends idling in menus. See STATUS_* below.
signal presence_received(online_ids: Array[String])
## Visible friends with their full presence record per id. Record
## shape: {game_id, rich_presence, status, updated_at}.
signal presence_received_rich(online_friends: Dictionary)
signal request_failed(error: String)


## Presence status values. Mirrors the closed set in
## third_party/snoringcat-platform/runtime/presence.go. Callers
## should use these constants rather than bare strings so the two
## sides can't drift.
##
## The runtime hides OFFLINE rows and rows whose last heartbeat is
## older than its staleness window; ONLINE and IN_MATCH both stay
## visible, distinguished by the record's `status` field.
const STATUS_OFFLINE := "offline"
const STATUS_ONLINE := "online"
const STATUS_IN_MATCH := "in_match"


var cached_online_ids: Array[String] = []
var cached_online_friends: Dictionary = {}

var _is_presence_busy := false


## Publish this player's (rich_presence, status) and read every
## visible friend's presence row back. Emits presence_received +
## presence_received_rich on success. On RPC failure (e.g. the
## runtime predates the RPC) emits empty results so consumers'
## UI stays in a defined state.
##
## `include_other_games` defaults to true so a friends list can
## show "in another game" for friends playing a different title on
## the shared platform. Pass false to restrict the response to the
## caller's own game.
func fetch_presence(
	rich_presence: String = "",
	status: String = STATUS_ONLINE,
	include_other_games: bool = true,
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
			"include_other_games": include_other_games,
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


## Announce that this player is leaving (log-out, app quit). Writes
## a single OFFLINE row so friends' lists drop them immediately
## instead of waiting out the runtime's staleness window.
##
## Best-effort and deliberately unguarded by `_is_presence_busy`: a
## heartbeat racing a quit must not swallow the goodbye. Callers
## should await it where they can, but the runtime's staleness TTL
## is the real backstop — a crash, a killed process, or a closed
## browser tab never gets to run this, and that's expected.
##
## Must be called before the auth token is cleared; without a valid
## session there's nobody to write the row as.
func announce_offline() -> void:
	var session := _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.nakama_client.rpc_async(
		session, "update_and_get_presence",
		JSON.stringify({
			"rich_presence": "",
			"status": STATUS_OFFLINE,
			"include_other_games": false,
		}))
	if rpc_result.is_exception():
		# Nothing to recover: we're on our way out, and the TTL
		# covers us. Don't emit request_failed — a toast on the way
		# out the door is worse than silence.
		return


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
