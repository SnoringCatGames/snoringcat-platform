class_name PlatformPartyApiClient
extends Node
## Nakama-backed party client. Parties are mapped onto Nakama
## groups (closed, max-size 4 by default) plus a custom RPC for
## starting matchmaking on behalf of the whole party.
##
## Reads `Platform.nakama_client`, `Platform.token_store`, and
## `Platform.build_session_from_store()`. All must be populated
## before any RPC method is called.


signal party_created(data: Dictionary)
signal party_invited(data: Dictionary)
signal party_joined(data: Dictionary)
signal party_left(data: Dictionary)
signal party_kicked(data: Dictionary)
signal party_status_received(data: Dictionary)
signal party_matchmaking_started(data: Dictionary)
signal party_ready_updated(data: Dictionary)
signal party_invite_code_received(data: Dictionary)
signal party_invite_code_redeemed(data: Dictionary)
signal party_leader_transferred(data: Dictionary)
signal request_failed(error: String)


const _PARTY_GROUP_PREFIX := "party-"

## Storage collection name for per-member ready rows. Mirrors
## third_party/snoringcat-platform/runtime/party.go
## :partyReadyCollection.
const _PARTY_READY_COLLECTION := "party_ready"

## Storage collection name for the party-leader override. Each
## party can have one row at (collection, party_id, "") owned by
## the server; the runtime writes it via
## `party_transfer_leadership` and reads it via
## `resolvePartyLeader`. Read permission is server-only so the row
## is opaque to clients, but `fetch_party_status` resolves the
## current leader through the runtime's existing party_status
## payload — clients never read this row directly.
const _PARTY_LEADER_COLLECTION := "party_leader"

## Storage collection name for the party's matchmaker game-mode
## override (Stage 5.7). Each party can have one row at
## (collection, party_id, "") owned by the server; the runtime
## writes it via `party_set_mode` (leader-only) and reads it in
## `party_start_matchmaking` to default the game_mode when the
## caller doesn't override.
## See: third_party/snoringcat-platform/runtime/party.go
## :partyModeCollection.
const _PARTY_MODE_COLLECTION := "party_mode"


var _is_busy := false


func is_busy() -> bool:
	return _is_busy


func create_party() -> void:
	if _is_busy:
		return
	_is_busy = true
	var session := await _ensure_session()
	if session == null:
		_is_busy = false
		return
	var name := _PARTY_GROUP_PREFIX + _short_id(session.user_id)
	var result = await Platform.get_nakama_client().create_group_async(
		session, name, "", "", "en", false, 4)
	_is_busy = false
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	party_created.emit({
		"party_id": result.id,
		"name": result.name,
	})


func invite_to_party(
	party_id: String,
	player_id: String,
) -> void:
	# Routed through the party_invite runtime RPC, not Nakama's client
	# add_group_users (which requires the caller to be a group admin,
	# i.e. the leader). The RPC checks active membership and performs
	# the add as the leader server-side, so ANY member can invite.
	var session := await _ensure_session()
	if session == null:
		return
	var result = await Platform.get_nakama_client().rpc_async(
		session, "party_invite",
		JSON.stringify({
			"party_id": party_id,
			"target_id": player_id,
		}))
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	party_invited.emit({
		"party_id": party_id,
		"player_id": player_id,
	})


func join_party(party_id: String) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var result = await Platform.get_nakama_client().join_group_async(
		session, party_id)
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	party_joined.emit({"party_id": party_id})


func leave_party(party_id: String) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var result = await Platform.get_nakama_client().leave_group_async(
		session, party_id)
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	party_left.emit({"party_id": party_id})


func fetch_party_status() -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var result = await Platform.get_nakama_client().list_user_groups_async(
		session, session.user_id, null, null, null)
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	# Nakama parties are closed groups. The viewer can be either an
	# active member (state 0/1/2) or a pending invitee (state 3) for
	# the same group shape. Split the two so the UI can render an
	# accept/decline path for invites instead of falsely showing the
	# user as "in a party" before they've accepted.
	var party: Dictionary = {}
	var pending_invites: Array[Dictionary] = []
	for ug in result.user_groups:
		var g = ug.group
		if not g.name.begins_with(_PARTY_GROUP_PREFIX):
			continue
		var state := int(ug.state)
		if state == 3:
			pending_invites.append({
				"party_id": g.id,
				"party_name": g.name,
				"leader_id": g.creator_id,
				# leader_display_name resolved by the
				# UI from cached friends/members; the
				# list_user_groups response doesn't
				# carry it.
			})
			continue
		if party.is_empty():
			party = {
				"party_id": g.id,
				"name": g.name,
				"leader_id": g.creator_id,
				"member_count": g.edge_count,
				"members": [],
				"viewer_role": _group_state_to_role(state),
			}
	if not party.is_empty():
		var members_result = (
			await Platform.get_nakama_client()
				.list_group_users_async(
					session,
					party["party_id"],
					null,
					null,
					null,
				)
		)
		if members_result.is_exception():
			request_failed.emit(
				_describe(members_result.get_exception()))
			return
		var members: Array[Dictionary] = []
		for gu in members_result.group_users:
			var u = gu.user
			if u == null:
				continue
			members.append({
				"user_id": u.id,
				"username": u.username,
				"display_name": u.display_name,
				"role": _group_state_to_role(gu.state),
				"ready": false,
			})
		# Batch-read every active member's ready row + the
		# (optional) party-leader override row in a single
		# read_storage_objects_async call. Each ready row is
		# owned by the corresponding member; pending invitees
		# (state=3) are skipped because they can't ready up until
		# they accept the invite. The leader-override row is
		# server-owned (user_id=""); a present row carries
		# `{user_id: <new_leader>}` and supersedes the group's
		# immutable creator_id; an absent row leaves
		# `party.leader_id` set to creator_id (the default).
		var storage_ids: Array[NakamaStorageObjectId] = []
		var ready_user_ids: Dictionary = {}
		for m in members:
			if m.get("role", "") == "invited":
				continue
			var ready_id := NakamaStorageObjectId.new(
				_PARTY_READY_COLLECTION,
				party["party_id"],
				m["user_id"],
			)
			storage_ids.append(ready_id)
			ready_user_ids[m["user_id"]] = true
		# Leader override read. The empty user_id ("" string)
		# targets the server-owned row. PermissionRead=2 on the
		# row makes this readable by any session.
		var leader_override_id := NakamaStorageObjectId.new(
			_PARTY_LEADER_COLLECTION,
			party["party_id"],
			"",
		)
		storage_ids.append(leader_override_id)
		# Stage 5.7: party game-mode override (server-owned;
		# leader-only writes via party_set_mode). Folded into the
		# emitted party dict as `game_mode` so the lobby panel can
		# render the current mode and so PartyManager can pass it
		# through the matchmaker on start.
		var mode_override_id := NakamaStorageObjectId.new(
			_PARTY_MODE_COLLECTION,
			party["party_id"],
			"",
		)
		storage_ids.append(mode_override_id)
		if not storage_ids.is_empty():
			var storage_result = (
				await Platform.get_nakama_client()
					.read_storage_objects_async(
						session, storage_ids)
			)
			if storage_result.is_exception():
				# Ready / leader-override state is non-critical
				# — log via the error signal but still surface
				# the party so the panel can render with
				# defaults. Most likely cause is a transient
				# network blip; the next poll picks them up.
				request_failed.emit(
					_describe(storage_result.get_exception()))
			else:
				var ready_by_user: Dictionary = {}
				for obj in storage_result.objects:
					if obj.collection == _PARTY_LEADER_COLLECTION:
						var leader_parsed: Variant = (
							JSON.parse_string(obj.value))
						if leader_parsed is Dictionary:
							var override_id: String = (
								leader_parsed.get(
									"user_id", ""))
							if not override_id.is_empty():
								party["leader_id"] = (
									override_id)
						continue
					if obj.collection == _PARTY_MODE_COLLECTION:
						var mode_parsed: Variant = (
							JSON.parse_string(obj.value))
						if mode_parsed is Dictionary:
							var mode_value: String = (
								mode_parsed.get("mode_id", ""))
							if not mode_value.is_empty():
								party["game_mode"] = mode_value
						continue
					if ready_user_ids.has(obj.user_id):
						var parsed: Variant = JSON.parse_string(
							obj.value)
						if parsed is Dictionary:
							ready_by_user[obj.user_id] = (
								bool(parsed.get(
									"ready", false)))
				for m in members:
					m["ready"] = bool(
						ready_by_user.get(
							m["user_id"], false))
		# Recompute viewer_role from the (possibly overridden)
		# leader_id so the panel surfaces leader affordances for
		# the post-transfer leader. The original
		# `_group_state_to_role(state)` reflected Nakama's group-
		# user state at fetch time, which doesn't change on
		# leader transfer (the override is app-level, not
		# Nakama-level group state).
		var resolved_leader: String = party.get(
			"leader_id", "")
		var viewer_id: String = Platform.token_store.player_id
		if not resolved_leader.is_empty():
			if resolved_leader == viewer_id:
				party["viewer_role"] = "leader"
			elif party.get("viewer_role", "") == "leader":
				# Viewer was the original creator (state=0) but
				# is no longer the resolved leader; demote the
				# in-memory role to "member" so the panel hides
				# leader-only rows.
				party["viewer_role"] = "member"
		# Tag the role on each member dict too so the
		# panel's per-member crown / kick affordances
		# match the resolved leader, not Nakama state.
		for m in members:
			if m["user_id"] == resolved_leader:
				m["role"] = "leader"
			elif m.get("role", "") == "leader":
				# Original Nakama creator who's no longer the
				# resolved leader. Demote the displayed role
				# to "member" so the crown shows next to the
				# right person.
				m["role"] = "member"
		party["members"] = members
	# Wrapped emit so PartyManager._on_party_status_received can read
	# both surfaces. The pre-wrap shape (bare party Dict) silently
	# never matched the receiver's `data.get("party")` lookup, which
	# is why polling-cycle updates appeared to do nothing in
	# production.
	party_status_received.emit({
		"party": party,
		"pending_invites": pending_invites,
	})


func kick_from_party(
	party_id: String,
	player_id: String,
) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var result = await Platform.get_nakama_client().kick_group_users_async(
		session, party_id, [player_id])
	if result.is_exception():
		request_failed.emit(_describe(result.get_exception()))
		return
	party_kicked.emit({
		"party_id": party_id,
		"player_id": player_id,
	})


## Toggle the caller's per-party ready flag. Calls the
## party_set_ready runtime RPC, which validates membership,
## writes the storage row server-side, and fans out a
## party_state_changed notification so every member's UI
## refreshes without waiting for the catch-up poll.
func set_ready(party_id: String, ready: bool) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_set_ready",
		JSON.stringify({
			"party_id": party_id,
			"ready": ready,
		}))
	if rpc_result.is_exception():
		request_failed.emit(_describe(rpc_result.get_exception()))
		return
	var data: Variant = JSON.parse_string(rpc_result.payload)
	party_ready_updated.emit(
		data if data is Dictionary else
		{"party_id": party_id, "ready": ready})


## Set the party's matchmaker game mode (leader-only). The
## server persists the choice on a server-owned storage row and
## fans out a party_state_changed notification with
## event=mode_changed so other members refresh and see the
## change. Stage 5.7.
##
## Emits `party_mode_set({party_id, mode_id})` on success and
## `request_failed(error)` on failure (e.g. caller is not the
## leader).
signal party_mode_set(data: Dictionary)


func set_mode(party_id: String, mode_id: String) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_set_mode",
		JSON.stringify({
			"party_id": party_id,
			"mode_id": mode_id,
		}))
	if rpc_result.is_exception():
		request_failed.emit(_describe(rpc_result.get_exception()))
		return
	var data: Variant = JSON.parse_string(rpc_result.payload)
	party_mode_set.emit(
		data if data is Dictionary else
		{"party_id": party_id, "mode_id": mode_id})


## Store the leader's level preferences for the party (leader-only;
## the runtime enforces). `prefs` is the LevelPreferences.to_dict
## shape ({inclusion, exclusion, preferred}). Fire-and-forget: this
## keeps the party's stored prefs in sync with the leader's so any
## member's matchmaking start reflects the leader's choices. The
## runtime forwards it to the game server at allocation.
func set_level_prefs(party_id: String, prefs: Dictionary) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_set_level_prefs",
		JSON.stringify({
			"party_id": party_id,
			"prefs": prefs,
		}))
	if rpc_result.is_exception():
		push_warning(
			"[Party] set_level_prefs failed: %s"
			% _describe(rpc_result.get_exception()))


## Store the leader's gameplay-cheat prefs for the party (leader-
## only). `networked_cheats` lists the enabled gameplay cheat names;
## aesthetic cheats are never party-scoped. Fire-and-forget.
func set_cheat_prefs(
	party_id: String,
	are_cheats_enabled: bool,
	networked_cheats: Array,
) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_set_cheat_prefs",
		JSON.stringify({
			"party_id": party_id,
			"are_cheats_enabled": are_cheats_enabled,
			"networked_cheats": networked_cheats,
		}))
	if rpc_result.is_exception():
		push_warning(
			"[Party] set_cheat_prefs failed: %s"
			% _describe(rpc_result.get_exception()))


## Fetch (or generate) the shareable 6-char invite code for the
## given party. Any active member can call; pending invitees are
## rejected by the runtime. Emits `party_invite_code_received`
## with `{party_id, code}` on success.
func get_invite_code(party_id: String) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_get_invite_code",
		JSON.stringify({
			"party_id": party_id,
		}))
	if rpc_result.is_exception():
		request_failed.emit(_describe(rpc_result.get_exception()))
		return
	var data: Variant = JSON.parse_string(rpc_result.payload)
	if data is Dictionary:
		party_invite_code_received.emit(data)
	else:
		party_invite_code_received.emit({
			"party_id": party_id, "code": ""})


## Join a party by previously-issued invite code. Server validates
## the code, confirms the party still exists and has room, then
## adds the caller as an active member (state=2). Emits both
## `party_invite_code_redeemed` (so UI can show success copy) and
## `party_joined` (so PartyManager runs its standard join-state
## machinery).
func join_by_code(code: String) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var normalized := code.strip_edges().to_upper()
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_join_by_code",
		JSON.stringify({
			"code": normalized,
		}))
	if rpc_result.is_exception():
		request_failed.emit(_describe(rpc_result.get_exception()))
		return
	var data: Variant = JSON.parse_string(rpc_result.payload)
	if not (data is Dictionary):
		data = {"party_id": "", "code": normalized}
	party_invite_code_redeemed.emit(data)
	# Routing the success through party_joined keeps PartyManager's
	# state machine simple: the join-by-code path is just another
	# way of becoming a party member, so the same post-join
	# bookkeeping (seed current_party, immediate refetch, kick off
	# polling) applies.
	party_joined.emit({"party_id": data.get("party_id", "")})


## Hand off party leadership to another active member. Calls the
## party_transfer_leadership runtime RPC, which validates the
## caller is the current leader, writes an override storage row
## that subsequent fetch_party_status calls fold into `leader_id`,
## and fans out a party_state_changed notification so every
## member's UI refreshes.
##
## On success emits both `party_leader_transferred` (for callers
## that want to react specifically to a leader change, e.g. a
## toast) and `party_status_received` indirectly via the
## notification → refetch path.
func transfer_leadership(
	party_id: String,
	target_user_id: String,
) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_transfer_leadership",
		JSON.stringify({
			"party_id": party_id,
			"target_user_id": target_user_id,
		}))
	if rpc_result.is_exception():
		request_failed.emit(_describe(rpc_result.get_exception()))
		return
	var data: Variant = JSON.parse_string(rpc_result.payload)
	party_leader_transferred.emit(
		data if data is Dictionary else
		{"party_id": party_id, "leader_id": target_user_id})


## Signals every member to join the leader's realtime party, so the
## leader can submit one matchmaker ticket covering the group.
##
## `rt_party_id` is the realtime party the caller must have already
## created on its matchmaker socket (see
## PlatformMatchmakingClient.create_rt_party). The runtime rejects
## the call without one — there is no longer a path where members
## enqueue independently, because that path could not keep a party
## together.
func start_matchmaking(
	party_id: String,
	rt_party_id: String,
	game_mode: String = "ffa",
) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_start_matchmaking",
		JSON.stringify({
			"party_id": party_id,
			"rt_party_id": rt_party_id,
			"game_mode": game_mode,
		}))
	if rpc_result.is_exception():
		request_failed.emit(_describe(rpc_result.get_exception()))
		return
	var data: Variant = JSON.parse_string(rpc_result.payload)
	party_matchmaking_started.emit(
		data if data is Dictionary else
		{"party_id": party_id, "ticket_id": ""})


## Tell every member to stop waiting. Leader-only; the runtime
## enforces it.
##
## Fire-and-forget by design: the caller is already on an abort
## path, and surfacing a second failure ("the abort failed") would
## only add noise. Members who miss the notification fall back to
## their own matchmaking timeout.
func abort_matchmaking(
	party_id: String,
	reason: String = "aborted",
) -> void:
	var session := await _ensure_session()
	if session == null:
		return
	var rpc_result = await Platform.get_nakama_client().rpc_async(
		session, "party_abort_matchmaking",
		JSON.stringify({
			"party_id": party_id,
			"reason": reason,
		}))
	if rpc_result.is_exception():
		push_warning(
			"[PlatformParty] abort_matchmaking failed: %s"
			% _describe(rpc_result.get_exception()))


# --------------------------------------------------------------
# Internals
# --------------------------------------------------------------

func _ensure_session() -> NakamaSession:
	var s := Platform.build_session_from_store()
	if s == null:
		request_failed.emit("Not authenticated")
		return null
	return s


func _describe(ex: NakamaException) -> String:
	if ex == null:
		return "Unknown Nakama error"
	return "%s (status=%d)" % [ex.message, ex.status_code]


func _short_id(uuid: String) -> String:
	return uuid.replace("-", "").substr(0, 8)


# Nakama group_user state enum:
#   0 = Superadmin (creator/owner)
#   1 = Admin
#   2 = Member
#   3 = JoinRequest (pending invite the user has not yet accepted).
func _group_state_to_role(state: int) -> String:
	match state:
		0:
			return "leader"
		1:
			return "admin"
		2:
			return "member"
		3:
			return "invited"
		_:
			return "unknown"
