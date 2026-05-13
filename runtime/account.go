package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Account self-deletion (GDPR / CCPA / app-store TOS). The flow
// is soft-delete-with-grace-period per
// PLATFORM_ARCHITECTURE.md §"Account deletion":
//
//   1. Queue a record in `account_deletion_queue` with
//      `scheduled_for = now + 30d`. The hourly account_cron
//      consumes elapsed rows by calling nk.AccountDeleteId
//      (Stage 1.4).
//   2. Anonymize the user's display name to "[deleted]".
//   3. Cascade-clear: friends, group memberships, presence,
//      leaderboards, and user-owned storage.
//
// Sign-in stays available during the grace window so the user
// can cancel via `cancel_account_deletion` RPC (Stage 1.5).
// `get_account_deletion_status` lets the client detect the
// queued state and prompt at auth time. The pre-1.5 design also
// banned the user via UsersBanId — that was dropped here so the
// cancellation surface is reachable; the boot-time
// get_account_deletion_status check is now the gate that
// surfaces the prompt instead.

const (
	// accountDeletionCollection holds grace-period soft-delete
	// records keyed by user_id. Owner+read+write perms are 0/0
	// so the user's own client can't enumerate or delete the
	// audit trail.
	accountDeletionCollection = "account_deletion_queue"
	accountDeletionKey        = "current"

	// accountDeletionGracePeriod is the soft-delete window. The
	// player can sign back in within this window and the
	// deletion will be cancelled (cancellation flow not yet
	// implemented).
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
// Stage 3.6 prefixing landed.
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
	Pending             bool   `json:"pending"`
	UserID              string `json:"user_id,omitempty"`
	RequestedAt         int64  `json:"requested_at,omitempty"`
	ScheduledFor        int64  `json:"scheduled_for,omitempty"`
	OriginalUsername    string `json:"original_username,omitempty"`
	OriginalDisplayName string `json:"original_display_name,omitempty"`
}

// cancelAccountDeletionResp echoes the restored identity so the
// client can patch its session-store view without round-tripping
// through AccountGetId. Stage 1.5.
type cancelAccountDeletionResp struct {
	OK          bool   `json:"ok"`
	UserID      string `json:"user_id"`
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type deletionQueueRecord struct {
	UserID              string `json:"user_id"`
	RequestedAt         int64  `json:"requested_at"`
	ScheduledFor        int64  `json:"scheduled_for"`
	OriginalUsername    string `json:"original_username"`
	OriginalDisplayName string `json:"original_display_name"`
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

	// Capture pre-anonymization identity for the queue record
	// so a future cancellation flow has the data needed to
	// restore the original profile.
	var origUsername, origDisplay string
	if acc, accErr := nk.AccountGetId(ctx, userID); accErr == nil && acc != nil {
		origUsername = acc.User.Username
		origDisplay = acc.User.DisplayName
	}

	// 1. Queue the soft-delete record (server-only read/write).
	queueValue := deletionQueueRecord{
		UserID:              userID,
		RequestedAt:         now.Unix(),
		ScheduledFor:        scheduledFor.Unix(),
		OriginalUsername:    origUsername,
		OriginalDisplayName: origDisplay,
	}
	queueBytes, _ := json.Marshal(queueValue)
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

	// 2. Anonymize display name. Username left intact so the
	// queue record's `original_username` still matches the
	// account row Nakama owns. AccountUpdateId treats empty
	// strings as "no change" for the rest of the fields.
	if err := nk.AccountUpdateId(
		ctx,
		userID,
		"",                    // username (no change)
		nil,                   // metadata
		anonymizedDisplayName, // displayName
		"",                    // timezone
		"",                    // location
		"",                    // langTag
		"",                    // avatarUrl
	); err != nil {
		logger.Warn(
			"anonymize display name for %s: %v", userID, err)
		// Continue: queue record is durable.
	}

	// 3a. Friends: pull every entry and delete the relationship
	// bidirectionally. FriendsDelete handles pending/incoming/
	// outgoing/mutual states.
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
			ctx, userID, origUsername, friendIDs, nil); err != nil {
			logger.Warn(
				"delete friends for %s: %v", userID, err)
		}
	}

	// 3b. Group memberships. For party-prefixed groups the user
	// created, also hard-delete the group so it doesn't linger
	// as a ghost the rest of the party can't escape.
	groupCursor := ""
	for i := 0; i < 10; i++ {
		userGroups, next, gErr := nk.UserGroupsList(
			ctx, userID, 100, nil, groupCursor)
		if gErr != nil {
			logger.Warn(
				"list groups for delete %s: %v", userID, gErr)
			break
		}
		for _, ug := range userGroups {
			if ug.Group == nil || ug.Group.Id == "" {
				continue
			}
			if err := nk.GroupUserLeave(
				ctx, ug.Group.Id, userID, origUsername); err != nil {
				logger.Warn(
					"leave group %s for %s: %v",
					ug.Group.Id, userID, err)
			}
			if strings.HasPrefix(
				ug.Group.Name, partyGroupPrefix) &&
				ug.Group.CreatorId == userID {
				if err := nk.GroupDelete(
					ctx, ug.Group.Id); err != nil {
					logger.Warn(
						"delete party group %s: %v",
						ug.Group.Id, err)
				}
			}
		}
		if next == "" {
			break
		}
		groupCursor = next
	}

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

	// 3d. Leaderboard records.
	for _, lb := range leaderboardIDsToScrub(games) {
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
	// deletes until someone remembers to update a registry.
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

	// Stage 1.5: no more UsersBanId. The user remains able to
	// authenticate during the 30-day grace window so the
	// cancellation flow can prompt them on next sign-in via
	// get_account_deletion_status + cancel_account_deletion.
	// The cascade above has already scrubbed the game-visible
	// state; the queue row + this remaining (anonymized) account
	// shell is what the cron consumes when the grace elapses.

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
		Pending:             true,
		UserID:              userID,
		RequestedAt:         record.RequestedAt,
		ScheduledFor:        record.ScheduledFor,
		OriginalUsername:    record.OriginalUsername,
		OriginalDisplayName: record.OriginalDisplayName,
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
		_ *sql.DB,
		nk runtime.NakamaModule,
		_ string,
	) (string, error) {
		return cancelAccountDeletionRpc(
			ctx, logger, nk, games)
	}
}

// cancelAccountDeletionRpc resurrects the caller's account
// during the grace window:
//   1. Reads the original username + display name from the
//      account_deletion_queue row.
//   2. Restores them via AccountUpdateId so the user-visible
//      profile is back to its pre-deletion shape.
//   3. Deletes the queue row so the cron stops counting down.
//
// Returns INVALID_ARGUMENT (9, FailedPrecondition) when the
// caller has no pending deletion (caller asked to cancel
// nothing) so the client UI can show "your account isn't
// scheduled for deletion" cleanly.
//
// Stage 1.5.
func cancelAccountDeletionRpc(
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
			"cancel_account_deletion read user=%s: %v",
			userID, err)
		return "", err
	}
	if len(reads) == 0 {
		return "", runtime.NewError(
			"no pending account deletion to cancel", 9)
	}
	record := deletionQueueRecord{}
	if err := json.Unmarshal(
		[]byte(reads[0].Value), &record); err != nil {
		// Treat as "no row" — clearer for the caller than
		// returning an internal error for a corrupt audit row
		// they didn't cause.
		logger.Warn(
			"cancel_account_deletion: malformed row user=%s: %v",
			userID, err)
		return "", runtime.NewError(
			"no pending account deletion to cancel", 9)
	}

	// Restore the pre-anonymization profile. AccountUpdateId
	// treats empty strings as "no change", so we only push the
	// fields we actually captured. If the queue row lacked an
	// original username (pre-Stage-1.4 audit shape), we leave
	// the field alone.
	if err := nk.AccountUpdateId(
		ctx,
		userID,
		record.OriginalUsername,
		nil,
		record.OriginalDisplayName,
		"",
		"",
		"",
		"",
	); err != nil {
		logger.Warn(
			"cancel_account_deletion restore user=%s: %v",
			userID, err)
		// Continue: queue-row delete below is the bit that
		// stops the cron. A failed restore leaves the user
		// looking like "[deleted]" but their account is alive.
	}

	if err := nk.StorageDelete(
		ctx,
		[]*runtime.StorageDelete{{
			Collection: accountDeletionCollection,
			Key:        accountDeletionKey,
			UserID:     userID,
		}},
	); err != nil {
		logger.Error(
			"cancel_account_deletion delete row user=%s: %v",
			userID, err)
		return "", err
	}

	logger.Info(
		"account_deletion cancelled user=%s"+
			" (was scheduled_for=%s)",
		userID,
		time.Unix(record.ScheduledFor, 0).Format(time.RFC3339))

	resp := cancelAccountDeletionResp{
		OK:          true,
		UserID:      userID,
		Username:    record.OriginalUsername,
		DisplayName: record.OriginalDisplayName,
	}
	out, _ := json.Marshal(resp)
	return string(out), nil
}
