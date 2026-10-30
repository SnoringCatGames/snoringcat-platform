package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Hourly account cron. Each tick it walks account_deletion_queue
// across all users and:
//
//   - hard-deletes every account whose scheduled_for has passed,
//     with nk.AccountDeleteId (recorded=true, so the user_id is
//     tombstoned and can't be reused), after purging what names the
//     player outside their own rows (privacy.go), then deletes the
//     queue row so the same user isn't processed twice;
//   - keeps every still-pending account's profile clear: it re-
//     applies the deletion-request scrub and strips the profile copy
//     from queue rows written before 2026-10-11 (privacy.go). Rows
//     queued before that date get their profile cleared on the first
//     tick after the deploy.
//
// It then deletes stored client IPs too old to use (client_ip.go).
//
// Caveats / behaviors:
//   - The scan tolerates malformed rows. A row whose JSON we
//     can't parse logs a warning and gets skipped (NOT deleted —
//     a future operator can inspect / fix it manually).
//   - The scan tolerates "user already gone" (e.g., a manual
//     `psql DELETE FROM users` between rows): AccountDeleteId
//     errors are logged but the queue row is still cleared, so
//     the cron doesn't get stuck retrying.
//   - The cron uses context.Background(), not the InitModule
//     context. The InitModule context is the boot context and
//     gets cancelled the moment InitModule returns; using it
//     here would cause the very first tick to fire against a
//     cancelled context. Plugin reload tears down the goroutine
//     by recreating the plugin, so no shutdown plumbing needed.

const (
	// accountCronInterval — how often the cron wakes up to scan
	// account_deletion_queue. Hourly is fine: the grace period
	// is 30 days, so up to ~1h of latency on the hard-delete is
	// not user-visible. The first scan runs immediately on boot
	// so a host that was down past a scheduled_for boundary
	// doesn't wait a full interval to catch up.
	accountCronInterval = 1 * time.Hour

	// accountCronBatch — page size for the StorageList scan.
	// Each page is filtered in-memory; sized to keep per-tick
	// memory bounded while still draining a busy queue in a
	// handful of pages.
	accountCronBatch = 100

	// accountCronMaxPages — safety cap on a single tick's scan.
	// Worst-case we look at 10k rows per tick; anything beyond
	// that gets picked up next tick. Prevents a runaway scan
	// from blocking the goroutine forever.
	accountCronMaxPages = 100
)

// startAccountCron launches a background goroutine. The
// goroutine outlives InitModule because plugin reload is the
// only stop signal (Nakama recreates the plugin on reload).
func startAccountCron(
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
) {
	go runAccountCron(context.Background(), logger, db, nk)
}

// runAccountCron is the goroutine entry. It ticks on
// accountCronInterval and runs one pass per tick. The first pass
// runs immediately so a host that was down past one or more
// scheduled_for boundaries doesn't wait a full interval to catch
// up.
func runAccountCron(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
) {
	logger.Info(
		"account_cron started (interval=%s)", accountCronInterval)
	runAccountCronPass(ctx, logger, db, nk)
	ticker := time.NewTicker(accountCronInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("account_cron stopped: %v", ctx.Err())
			return
		case <-ticker.C:
			runAccountCronPass(ctx, logger, db, nk)
		}
	}
}

func runAccountCronPass(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
) {
	processAccountDeletionQueue(ctx, logger, db, nk)
	sweepStaleMatchRows(ctx, logger, nk)
	if pruned, err := pruneOldNotifications(ctx, db); err != nil {
		logger.Warn("account_cron prune notifications: %v", err)
	} else if pruned > 0 {
		logger.Info("account_cron pruned %d old notifications", pruned)
	}
	if pruned, err := pruneStaleClientIPs(ctx, db); err != nil {
		logger.Warn("account_cron prune client IPs: %v", err)
	} else if pruned > 0 {
		logger.Info("account_cron pruned %d stale client IPs", pruned)
	}
}

// processAccountDeletionQueue paginates through the
// account_deletion_queue collection across all users, hard-
// deletes any row whose scheduled_for has elapsed, and keeps the
// rest of the pending accounts' profiles clear.
func processAccountDeletionQueue(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
) {
	now := time.Now().Unix()
	cursor := ""
	deleted := 0
	scanned := 0
	rescrubbed := 0
	for page := 0; page < accountCronMaxPages; page++ {
		// nk.StorageList(ctx, callerID, userID, collection,
		// limit, cursor). callerID="" + userID="" walks every
		// user's rows in the given collection — exactly what
		// the cron needs.
		objects, next, err := nk.StorageList(
			ctx, "", "",
			accountDeletionCollection,
			accountCronBatch,
			cursor)
		if err != nil {
			logger.Warn("account_cron list: %v", err)
			return
		}
		for _, obj := range objects {
			scanned++
			var record deletionQueueRecord
			if err := json.Unmarshal(
				[]byte(obj.Value), &record); err != nil {
				logger.Warn(
					"account_cron skip malformed row user=%s key=%s: %v",
					obj.UserId, obj.Key, err)
				continue
			}
			if record.ScheduledFor > now {
				if keepPendingProfileClear(
					ctx, logger, db, obj.UserId) {
					rescrubbed++
				}
				continue
			}
			hardDeleteAccount(
				ctx, logger, db, nk, obj.UserId, obj.Key, record)
			deleted++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if deleted > 0 || scanned > 0 {
		logger.Info(
			"account_cron tick: scanned=%d hard_deleted=%d"+
				" profiles_recleared=%d",
			scanned, deleted, rescrubbed)
	}
}

// keepPendingProfileClear re-applies the deletion-request profile
// scrub to a pending account, strips any profile copy from its queue
// row, and deletes rows naming it in other players' recent-players
// lists (in case the request's own delete failed). Reports whether
// the profile needed clearing. Best-effort; failures log and the
// next tick retries.
func keepPendingProfileClear(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	userID string,
) bool {
	changed, err := enforcePendingDeletionProfile(ctx, db, userID)
	if err != nil {
		logger.Warn(
			"account_cron re-clear profile user=%s: %v", userID, err)
	}
	if err := minimizeDeletionQueueRow(ctx, db, userID); err != nil {
		logger.Warn(
			"account_cron minimize queue row user=%s: %v",
			userID, err)
	}
	if err := deleteRecentPlayerReferences(ctx, db, userID); err != nil {
		logger.Warn(
			"account_cron delete recent-player rows user=%s: %v",
			userID, err)
	}
	return changed
}

// hardDeleteAccount calls nk.AccountDeleteId(recorded=true) so
// Nakama tombstones the user_id and refuses to reuse it. Rows
// that name the player outside their own (friend code, other
// players' recent-players lists) go first, since Nakama's delete
// doesn't reach them. The queue row is then removed so the cron
// doesn't retry. Each step is best-effort: a Nakama-side delete
// failure (e.g., the user was manually removed between the
// cascade and the cron) logs a warning but still drops the queue
// row so the cron doesn't get stuck. A queue-row delete failure
// logs and returns — the next tick will retry.
func hardDeleteAccount(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	userID, key string,
	record deletionQueueRecord,
) {
	purgeUserReferences(ctx, logger, db, nk, userID)
	// recorded=true creates an entry in system_user_tombstones
	// so the same user_id can't be re-issued. The tombstone
	// stores no PII — just the id and a delete-time stamp —
	// which is appropriate for the "this account is gone but
	// we promise we won't recycle the row" contract the soft-
	// delete promise establishes.
	if err := nk.AccountDeleteId(ctx, userID, true); err != nil {
		logger.Warn(
			"account_cron hard-delete user=%s: %v;"+
				" clearing queue row anyway",
			userID, err)
	} else {
		logger.Info(
			"account_cron hard-deleted user=%s scheduled_for=%s"+
				" (queued %s ago)",
			userID,
			time.Unix(record.ScheduledFor, 0).Format(time.RFC3339),
			time.Since(time.Unix(record.RequestedAt, 0)).Round(
				time.Minute))
	}
	if err := nk.StorageDelete(ctx, []*runtime.StorageDelete{{
		Collection: accountDeletionCollection,
		Key:        key,
		UserID:     userID,
	}}); err != nil {
		logger.Warn(
			"account_cron clear queue row user=%s key=%s: %v",
			userID, key, err)
	}
}
