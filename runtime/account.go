package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Account self-deletion (GDPR / CCPA / app-store TOS). The flow is a
// soft delete with a grace period, per PLATFORM_ARCHITECTURE.md
// "Account deletion":
//
//  1. Queue a record in `account_deletion_queue` with
//     `scheduled_for = now + 30d`. The hourly account_cron hard-
//     deletes the account once that passes (account_cron.go).
//  2. Clear everything identifying that a cancellation doesn't
//     need, at once: the display name becomes "[deleted]"; the
//     avatar URL, any email, language, location, time zone, and
//     metadata go (privacy.go); and the cascade below deletes
//     friends, group memberships, presence, leaderboard records,
//     user-owned storage (match history, settings, recent players,
//     the stored IP, ...), the friend code, notifications (received
//     and sent), sent chat messages, wallet history, and the rows
//     naming the player in other players' recent-players lists.
//
// What stays for the grace window is what a cancellation needs: the
// account row with its sign-in ids (provider ids, device ids, custom
// id), so the player can sign back in, and the queue record, which
// holds only the request and deletion times. While it is pending the
// account cron and the sign-in hooks keep that profile clear, since
// a provider sign-in refills an empty avatar (privacy.go).
//
// Sign-in stays available during the grace window so the player can
// cancel via `cancel_account_deletion` (Stage 1.5), which can't put
// back what was cleared. `get_account_deletion_status` lets the
// client detect the queued state and prompt at auth time. The pre-
// 1.5 design also banned the user via UsersBanId; that was dropped so
// the cancellation surface is reachable.

const (
	// accountDeletionCollection holds grace-period soft-delete
	// records keyed by user_id. Owner+read+write perms are 0/0
	// so the user's own client can't enumerate or delete the
	// audit trail.
	accountDeletionCollection = "account_deletion_queue"
	accountDeletionKey        = "current"

	// accountDeletionGracePeriod is the soft-delete window. The
	// player can sign back in within this window and cancel the
	// deletion.
	accountDeletionGracePeriod = 30 * 24 * time.Hour

	// anonymizedDisplayName replaces the user's display name on
	// soft-delete. Per PLATFORM_ARCHITECTURE.md §"Account
	// deletion".
	anonymizedDisplayName = "[deleted]"
)

// leaderboardIDsToScrub returns every leaderboard ID the
// cascade should clear records from. Stage 3.6 sources this from
// each registered game's `game.yaml::leaderboards[]` (prefixed
// with `{game_id}_` to match match_lifecycle's writes) instead
// of a hardcoded list. The legacy bare "ffa" board is always
// appended so the cascade also scrubs records from before
// Stage 3.6 prefixing landed. The cascade adds any other board
// the player has a record on (leaderboardIDsForOwner).
func leaderboardIDsToScrub(games *perGameConfig) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, gc := range games.List() {
		for _, id := range parseLeaderboardIDs(gc) {
			add(gc.GameID + "_" + id)
		}
	}
	// Pre-Stage-3.6 records lived on a bare leaderboard ID with
	// no game_id prefix. Always include "ffa" so historical
	// hopnbop records get scrubbed too.
	add("ffa")
	return out
}

// parseLeaderboardIDs pulls the `leaderboards: [{id, ...}]`
// list out of a game's raw config. Tolerant of missing /
// malformed entries — anything unparseable is skipped silently
// (the cascade is best-effort, not a critical-path read).
func parseLeaderboardIDs(gc *GameConfig) []string {
	if gc == nil || len(gc.Raw) == 0 {
		return nil
	}
	var blob struct {
		Leaderboards []struct {
			ID string `json:"id"`
		} `json:"leaderboards"`
	}
	if err := json.Unmarshal(gc.Raw, &blob); err != nil {
		return nil
	}
	out := make([]string, 0, len(blob.Leaderboards))
	for _, lb := range blob.Leaderboards {
		if lb.ID != "" {
			out = append(out, lb.ID)
		}
	}
	return out
}

// mergeLeaderboardIDs appends the ids in extra that base lacks,
// keeping base's order.
func mergeLeaderboardIDs(base, extra []string) []string {
	seen := make(map[string]bool, len(base))
	out := make([]string, 0, len(base)+len(extra))
	for _, id := range append(append([]string{}, base...), extra...) {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

type deleteAccountResp struct {
	OK           bool   `json:"ok"`
	UserID       string `json:"user_id"`
	ScheduledFor int64  `json:"scheduled_for"`
	GraceDays    int    `json:"grace_days"`
}

// accountDeletionStatusResp is the public-facing view of the
// caller's account_deletion_queue row, surfaced via the
// get_account_deletion_status RPC (Stage 1.5). `pending=false`
// means the caller has no queued row; the other fields are then
// undefined.
type accountDeletionStatusResp struct {
	Pending      bool   `json:"pending"`
	UserID       string `json:"user_id,omitempty"`
	RequestedAt  int64  `json:"requested_at,omitempty"`
	ScheduledFor int64  `json:"scheduled_for,omitempty"`
}

// cancelAccountDeletionResp confirms a cancellation. Nothing is
// restored: the profile was cleared when the deletion was requested,
// and the queue record never kept a copy.
type cancelAccountDeletionResp struct {
	OK     bool   `json:"ok"`
	UserID string `json:"user_id"`
}

// deletionQueueRecord is the queue row: who and when, nothing else.
// Rows written before 2026-10-11 also carried original_username and
// original_display_name; JSON decoding ignores them, and the account
// cron strips them from stored rows (minimizeDeletionQueueRow).
type deletionQueueRecord struct {
	UserID       string `json:"user_id"`
	RequestedAt  int64  `json:"requested_at"`
	ScheduledFor int64  `json:"scheduled_for"`
}

// deleteAccountRpcFactory threads the per-game config store
// through so the handler can validate the caller's session
// game_id. Account deletion is intentionally cross-game (the
// audit trail covers the whole identity, not just the calling
// game's slice) — the game_id check is a session-identity
// assertion only.
func deleteAccountRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		db *sql.DB,
		nk runtime.NakamaModule,
		payload string,
	) (string, error) {
		return deleteAccountRpc(ctx, logger, db, nk, games, payload)
	}
}

// deleteAccountRpc handles a player's request to soft-delete
// their own account. See the file-level comment for the full
// flow. Returns UNAUTHENTICATED (16) when called outside a
// client session.
func deleteAccountRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	games *perGameConfig,
	_ string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	scheduledFor := now.Add(accountDeletionGracePeriod)

	// The friend and group calls below take the username as an
	// argument. It is only read here, never copied anywhere.
	username := ""
	if acc, accErr := nk.AccountGetId(ctx, userID); accErr == nil {
		username = acc.GetUser().GetUsername()
	}

	// 1. Queue the soft-delete record (server-only read/write). It
	// holds only what the cron and a cancellation need.
	queueBytes, _ := json.Marshal(deletionQueueRecord{
		UserID:       userID,
		RequestedAt:  now.Unix(),
		ScheduledFor: scheduledFor.Unix(),
	})
	if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
		Collection:      accountDeletionCollection,
		Key:             accountDeletionKey,
		UserID:          userID,
		Value:           string(queueBytes),
		PermissionRead:  0,
		PermissionWrite: 0,
	}}); err != nil {
		logger.Error(
			"queue account deletion for %s: %v", userID, err)
		return "", runtime.NewError(
			"failed to queue account deletion", 13)
	}

	// 2. Clear the profile. Best-effort like the rest of the
	// cascade: the queue record is durable, and the account cron
	// re-applies this every hour while the deletion is pending.
	if err := scrubDeletedAccountProfile(ctx, db, userID); err != nil {
		logger.Error(
			"clear profile for deleted account %s: %v", userID, err)
	}

	// 3a. Friends: pull every entry and delete the relationship
	// bidirectionally. FriendsDelete handles pending/incoming/
	// outgoing/mutual/blocked states.
	friendIDs := []string{}
	cursor := ""
	for i := 0; i < 10; i++ {
		friends, next, fErr := nk.FriendsList(
			ctx, userID, 100, nil, cursor)
		if fErr != nil {
			logger.Warn(
				"list friends for delete %s: %v", userID, fErr)
			break
		}
		for _, f := range friends {
			if f.User != nil && f.User.Id != "" {
				friendIDs = append(friendIDs, f.User.Id)
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(friendIDs) > 0 {
		if err := nk.FriendsDelete(
			ctx, userID, username, friendIDs, nil); err != nil {
			logger.Warn(
				"delete friends for %s: %v", userID, err)
		}
	}

	// 3b. Group memberships, listed in full before leaving any so
	// the pagination cursor can't skip rows.
	leaveAllGroups(ctx, logger, nk, userID, username)

	// 3c. Presence records — one per game the user has touched,
	// plus the pre-Stage-3 legacy key for users whose last
	// presence ping predates the migration.
	presenceDeletes := []*runtime.StorageDelete{}
	for _, gid := range games.GameIDs() {
		presenceDeletes = append(presenceDeletes,
			&runtime.StorageDelete{
				Collection: presenceCollection,
				Key:        presenceKey(gid),
				UserID:     userID,
			})
	}
	presenceDeletes = append(presenceDeletes,
		&runtime.StorageDelete{
			Collection: presenceCollection,
			Key:        presenceKeyLegacy,
			UserID:     userID,
		})
	if err := nk.StorageDelete(
		ctx, presenceDeletes); err != nil {
		logger.Warn(
			"delete presence for %s: %v", userID, err)
	}

	// 3d. Leaderboard records: every registered game's boards, the
	// legacy bare board, and any other board the player has a
	// record on. Through nk rather than SQL so Nakama's rank cache
	// forgets the player too.
	boards := leaderboardIDsToScrub(games)
	if owned, err := leaderboardIDsForOwner(ctx, db, userID); err != nil {
		logger.Warn(
			"list leaderboards for delete %s: %v", userID, err)
	} else {
		boards = mergeLeaderboardIDs(boards, owned)
	}
	for _, lb := range boards {
		if err := nk.LeaderboardRecordDelete(
			ctx, lb, userID); err != nil {
			logger.Warn(
				"delete leaderboard %s for %s: %v",
				lb, userID, err)
		}
	}

	// 3e. User-owned storage records across all collections.
	// Skip the deletion-queue record so the soft-delete audit
	// trail survives until the hard-delete cron consumes it.
	//
	// We use a direct SQL DELETE rather than the obvious
	// `nk.StorageList → nk.StorageDelete` pair because the
	// underlying SQL in nk.StorageList filters with
	// `WHERE collection = $1`, so passing collection="" matches
	// zero rows (no record has an empty collection). The
	// pre-Stage-7.7 loop here silently no-op'd for this reason —
	// every user-owned game-side storage row survived the
	// "cascade." Direct SQL also lets us scrub arbitrary game-
	// specific collections without having to enumerate them
	// first, which matters for compliance: a game that introduces
	// a new collection shouldn't quietly leak data through GDPR
	// deletes until someone remembers to update a registry. This
	// is also what deletes the stored IP (client_ip).
	if db != nil {
		// `user_id` is a UUID column; an explicit ::uuid cast keeps
		// the WHERE clause type-safe across PG / pgx-driver versions
		// rather than relying on implicit text→uuid coercion.
		if _, err := db.ExecContext(
			ctx,
			`DELETE FROM storage
			 WHERE user_id = $1::uuid AND collection != $2`,
			userID,
			accountDeletionCollection,
		); err != nil {
			logger.Warn(
				"scrub user storage for %s: %v", userID, err)
		}
	}

	// 3f. The friend code, the rows naming the player in other
	// players' recent-players lists, and the notifications they
	// sent. None is user-owned, so the scrub above doesn't reach
	// them.
	purgeUserReferences(ctx, logger, db, nk, userID)

	// 3g. Notifications, sent chat messages (including the group
	// join and leave events Nakama records), and wallet history.
	// These would otherwise wait for the hard delete's cascade.
	if err := deleteUserActivityRows(ctx, db, userID); err != nil {
		logger.Warn(
			"delete notifications and messages for %s: %v",
			userID, err)
	}

	// Stage 1.5: no more UsersBanId. The user remains able to
	// authenticate during the 30-day grace window so the
	// cancellation flow can prompt them on next sign-in via
	// get_account_deletion_status + cancel_account_deletion.

	logger.Info(
		"account_deletion soft-deleted user=%s"+
			" scheduled_for=%s friends=%d",
		userID, scheduledFor.Format(time.RFC3339),
		len(friendIDs))

	resp := deleteAccountResp{
		OK:           true,
		UserID:       userID,
		ScheduledFor: scheduledFor.Unix(),
		GraceDays: int(
			accountDeletionGracePeriod / (24 * time.Hour)),
	}
	out, _ := json.Marshal(resp)
	return string(out), nil
}

// leaveAllGroups takes the player out of every group. A party the
// player created is deleted with its settings rows, so it can't
// linger as a ghost the rest of the party can't escape. In a party
// they only joined, the remaining members get a leader if the player
// led it, fresh ready flags, and a party_state_changed notification:
// what afterLeaveGroupHook does for a player who leaves through the
// client, which a runtime GroupUserLeave doesn't trigger.
func leaveAllGroups(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	userID, username string,
) {
	memberships := []*api.UserGroupList_UserGroup{}
	cursor := ""
	for i := 0; i < 10; i++ {
		page, next, err := nk.UserGroupsList(
			ctx, userID, 100, nil, cursor)
		if err != nil {
			logger.Warn(
				"list groups for delete %s: %v", userID, err)
			break
		}
		memberships = append(memberships, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	for _, membership := range memberships {
		group := membership.GetGroup()
		groupID := group.GetId()
		if groupID == "" {
			continue
		}
		isParty := strings.HasPrefix(group.GetName(), partyGroupPrefix)
		if err := nk.GroupUserLeave(
			ctx, groupID, userID, username); err != nil {
			logger.Warn(
				"leave group %s for %s: %v", groupID, userID, err)
		}
		if !isParty {
			continue
		}
		if group.GetCreatorId() == userID {
			if err := nk.GroupDelete(ctx, groupID); err != nil {
				logger.Warn(
					"delete party group %s: %v", groupID, err)
				continue
			}
			deletePartySettingsRows(ctx, logger, nk, groupID)
			continue
		}
		clearPartyReadyRows(ctx, logger, nk, groupID, []string{userID})
		autoTransferIfLeaderDeparted(ctx, logger, nk, groupID, userID)
		notifyPartyMembers(
			ctx, logger, nk, groupID, partyEventLeft, nil)
	}
}

// deletePartySettingsRows removes a deleted party's system-owned
// rows: its leader override, game mode, level and cheat preferences
// (which record who set them), and its invite code.
func deletePartySettingsRows(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	partyID string,
) {
	deletes := []*runtime.StorageDelete{}
	for _, collection := range []string{
		partyLeaderCollection,
		partyModeCollection,
		partyLevelPrefsCollection,
		partyCheatPrefsCollection,
	} {
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: collection,
			Key:        partyID,
			UserID:     "",
		})
	}
	if err := nk.StorageDelete(ctx, deletes); err != nil {
		logger.Warn(
			"delete settings of party %s: %v", partyID, err)
	}
	reverse, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: partyInviteCodeCollection,
		Key:        partyInviteCodeReverseKey(partyID),
		UserID:     "",
	}})
	if err != nil {
		logger.Warn(
			"read invite code of party %s: %v", partyID, err)
		return
	}
	for _, obj := range reverse {
		var parsed struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal([]byte(obj.GetValue()), &parsed)
		deleteStalePartyInviteCode(
			ctx, logger, nk, parsed.Code, partyID)
	}
}

// getAccountDeletionStatusRpcFactory threads the per-game config
// store for the standard requireGameID check. Stage 1.5.
func getAccountDeletionStatusRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		_ *sql.DB,
		nk runtime.NakamaModule,
		_ string,
	) (string, error) {
		return getAccountDeletionStatusRpc(
			ctx, logger, nk, games)
	}
}

// getAccountDeletionStatusRpc returns whether the caller's
// account has an active soft-deletion queue row. Clients hit
// this on auth_completed and, if pending=true, prompt the user
// hasPendingDeletion reports whether the user has a soft-delete
// queued. Session revocation on delete (SP47) reads it from the
// refresh hook: a queued account cannot refresh, so a second
// device's session ends when its access token expires. A fresh
// authenticate still works, which is how the player reaches
// the cancel prompt.
func hasPendingDeletion(
	ctx context.Context,
	nk runtime.NakamaModule,
	userID string,
) (bool, error) {
	reads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: accountDeletionCollection,
			Key:        accountDeletionKey,
			UserID:     userID,
		}},
	)
	if err != nil {
		return false, err
	}
	return len(reads) > 0, nil
}

// to either confirm or cancel via cancel_account_deletion.
// Stage 1.5.
func getAccountDeletionStatusRpc(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	games *perGameConfig,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	reads, err := nk.StorageRead(
		ctx,
		[]*runtime.StorageRead{{
			Collection: accountDeletionCollection,
			Key:        accountDeletionKey,
			UserID:     userID,
		}},
	)
	if err != nil {
		logger.Error(
			"account_deletion_status read user=%s: %v",
			userID, err)
		return "", err
	}
	if len(reads) == 0 {
		out, _ := json.Marshal(
			accountDeletionStatusResp{Pending: false})
		return string(out), nil
	}
	record := deletionQueueRecord{}
	if err := json.Unmarshal(
		[]byte(reads[0].Value), &record); err != nil {
		// Malformed row — surface as "no pending deletion"
		// rather than blocking the user. The cron will
		// eventually skip-and-warn the same row.
		logger.Warn(
			"account_deletion_status: malformed row user=%s: %v",
			userID, err)
		out, _ := json.Marshal(
			accountDeletionStatusResp{Pending: false})
		return string(out), nil
	}
	out, _ := json.Marshal(accountDeletionStatusResp{
		Pending:      true,
		UserID:       userID,
		RequestedAt:  record.RequestedAt,
		ScheduledFor: record.ScheduledFor,
	})
	return string(out), nil
}

func cancelAccountDeletionRpcFactory(
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		db *sql.DB,
		nk runtime.NakamaModule,
		_ string,
	) (string, error) {
		return cancelAccountDeletionRpc(
			ctx, logger, db, nk, games)
	}
}

// cancelAccountDeletionRpc keeps the caller's account during the
// grace window. In one transaction (cancelPendingDeletion) it
// deletes the queue row, so the cron stops counting down, and clears
// the "[deleted]" display name marker, so the player doesn't look
// deleted to everyone. The rest of the profile was cleared at the
// request and the queue record kept no copy, so nothing else comes
// back; the SDK refills the name and picture from the provider the
// player just signed in with.
//
// Returns FAILED_PRECONDITION (9) when the caller has no pending
// deletion (caller asked to cancel nothing) so the client UI can
// show "your account isn't scheduled for deletion" cleanly.
//
// Stage 1.5.
func cancelAccountDeletionRpc(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	_ runtime.NakamaModule,
	games *perGameConfig,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
		return "", err
	}
	if _, err := requireGameID(ctx, games); err != nil {
		return "", err
	}
	if db == nil {
		return "", runtime.NewError(
			"failed to cancel account deletion", 13)
	}
	scheduledFor, cancelled, err := cancelPendingDeletion(
		ctx, db, userID)
	if err != nil {
		logger.Error(
			"cancel_account_deletion user=%s: %v", userID, err)
		return "", runtime.NewError(
			"failed to cancel account deletion", 13)
	}
	if !cancelled {
		return "", runtime.NewError(
			"no pending account deletion to cancel", 9)
	}

	logger.Info(
		"account_deletion cancelled user=%s"+
			" (was scheduled_for=%s)",
		userID,
		time.Unix(scheduledFor, 0).UTC().Format(time.RFC3339))

	out, _ := json.Marshal(cancelAccountDeletionResp{
		OK:     true,
		UserID: userID,
	})
	return string(out), nil
}
