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
//      `scheduled_for = now + 30d`. A future cron job (TBD)
//      reads this and runs `nk.AccountDeleteId` once the grace
//      period elapses.
//   2. Anonymize the user's display name to "[deleted]".
//   3. Cascade-clear: friends, group memberships, presence,
//      leaderboards, and user-owned storage.
//   4. Ban the user so the existing JWT can no longer
//      authenticate and any retained identity link (Google /
//      device / etc.) can't be used to re-enter during the
//      grace period.
//
// The hard-delete cron itself is not yet implemented. For Stage
// 1 the audit trail (queue record) is the durable artifact; the
// game-visible state is scrubbed immediately and the user is
// banned, so the soft-delete is the user-facing fact.
//
// Cancellation-from-grace UI is also TBD. Once it lands, the
// flow will read `account_deletion_queue` on sign-in, prompt the
// user to confirm cancellation, and remove the queue record +
// unban the user.

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

// leaderboardsToScrub lists every leaderboard the platform
// writes player records to. The hopnbop runtime currently only
// uses "ffa"; when Stage 3.6 (per-game leaderboard scoping)
// lands, this list should be sourced from per_game_config rather
// than hardcoded.
var leaderboardsToScrub = []string{"ffa"}

type deleteAccountResp struct {
	OK           bool   `json:"ok"`
	UserID       string `json:"user_id"`
	ScheduledFor int64  `json:"scheduled_for"`
	GraceDays    int    `json:"grace_days"`
}

type deletionQueueRecord struct {
	UserID              string `json:"user_id"`
	RequestedAt         int64  `json:"requested_at"`
	ScheduledFor        int64  `json:"scheduled_for"`
	OriginalUsername    string `json:"original_username"`
	OriginalDisplayName string `json:"original_display_name"`
}

// deleteAccountRpc handles a player's request to soft-delete
// their own account. See the file-level comment for the full
// flow. Returns UNAUTHENTICATED (16) when called outside a
// client session.
func deleteAccountRpc(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	nk runtime.NakamaModule,
	_ string,
) (string, error) {
	userID, err := requireClientSession(ctx)
	if err != nil {
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

	// 3c. Presence record (collection="presence", key="current").
	if err := nk.StorageDelete(ctx, []*runtime.StorageDelete{{
		Collection: "presence",
		Key:        "current",
		UserID:     userID,
	}}); err != nil {
		logger.Warn(
			"delete presence for %s: %v", userID, err)
	}

	// 3d. Leaderboard records.
	for _, lb := range leaderboardsToScrub {
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
	storageCursor := ""
	storageDeletes := []*runtime.StorageDelete{}
	for i := 0; i < 10; i++ {
		objects, next, sErr := nk.StorageList(
			ctx, "", userID, "", 100, storageCursor)
		if sErr != nil {
			logger.Warn(
				"list storage for delete %s: %v", userID, sErr)
			break
		}
		for _, obj := range objects {
			if obj.Collection == accountDeletionCollection &&
				obj.Key == accountDeletionKey {
				continue
			}
			storageDeletes = append(storageDeletes,
				&runtime.StorageDelete{
					Collection: obj.Collection,
					Key:        obj.Key,
					UserID:     userID,
				})
		}
		if next == "" {
			break
		}
		storageCursor = next
	}
	if len(storageDeletes) > 0 {
		if err := nk.StorageDelete(
			ctx, storageDeletes); err != nil {
			logger.Warn(
				"delete storage for %s: %v", userID, err)
		}
	}

	// 4. Ban so the existing session token and any linked
	// identity provider can no longer authenticate.
	if err := nk.UsersBanId(
		ctx, []string{userID}); err != nil {
		logger.Warn(
			"ban account %s: %v", userID, err)
	}

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
