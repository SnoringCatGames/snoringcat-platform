package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Hard-delete cron for account_deletion_queue. Stage 1.4 shipped
// the soft-delete + cascade flow and the durable audit-trail row;
// this file consumes those rows once the per-row scheduled_for
// timestamp has elapsed and the grace period is over.
//
// The cron is a goroutine started from InitModule. It scans
// account_deletion_queue across all users on a fixed interval,
// calls nk.AccountDeleteId for each elapsed row (with recorded=
// true so the user_id is tombstoned and can't be reused), and
// then deletes the queue row so the same user isn't processed
// twice.
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
	nk runtime.NakamaModule,
) {
	go runAccountCron(context.Background(), logger, nk)
}

// runAccountCron is the goroutine entry. It ticks on
// accountCronInterval and calls processAccountDeletionQueue on
// each tick. The first tick fires immediately so a host that
// was down past one or more scheduled_for boundaries doesn't
// wait a full interval to catch up.
func runAccountCron(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
) {
	logger.Info(
		"account_cron started (interval=%s)", accountCronInterval)
	processAccountDeletionQueue(ctx, logger, nk)
	ticker := time.NewTicker(accountCronInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("account_cron stopped: %v", ctx.Err())
			return
		case <-ticker.C:
			processAccountDeletionQueue(ctx, logger, nk)
		}
	}
}

// processAccountDeletionQueue paginates through the
// account_deletion_queue collection across all users, hard-
// deletes any row whose scheduled_for has elapsed, and clears
// the queue row.
//
// Exported for unit tests; the goroutine entry is the only
// production caller.
func processAccountDeletionQueue(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
) {
	now := time.Now().Unix()
	cursor := ""
	deleted := 0
	scanned := 0
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
				continue
			}
			hardDeleteAccount(
				ctx, logger, nk, obj.UserId, obj.Key, record)
			deleted++
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if deleted > 0 || scanned > 0 {
		logger.Info(
			"account_cron tick: scanned=%d hard_deleted=%d",
			scanned, deleted)
	}
}

// hardDeleteAccount calls nk.AccountDeleteId(recorded=true) so
// Nakama tombstones the user_id and refuses to reuse it. The
// queue row is then removed so the cron doesn't retry. Both
// steps are best-effort: a Nakama-side delete failure (e.g.,
// the user was manually removed between the cascade and the
// cron) logs a warning but still drops the queue row so the
// cron doesn't get stuck. A queue-row delete failure logs and
// returns — the next tick will retry.
func hardDeleteAccount(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	userID, key string,
	record deletionQueueRecord,
) {
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
