package main

import (
	"context"
	"database/sql"
	"fmt"
)

// SP40: nothing ever removed notification rows, and Nakama's
// list returns the oldest first, so a player's HTTP poll froze
// on their oldest 50 rows (hopnbop H54 is the client half).
// The account cron deletes rows past the retention age. The
// friend requests themselves live in the friends table, not
// in these rows, so an old request still badges from the
// friends list after its notification row is gone.

const (
	// notificationRetentionSec bounds the toast history a
	// poll can replay. 30 days is far past any toast's
	// useful life and keeps the table (and the per-match
	// rows SP14 adds) from growing without bound.
	notificationRetentionSec = 30 * 24 * 3600
)

// pruneOldNotifications deletes notification rows older than
// notificationRetentionSec. Run from the account cron.
// Reports how many it deleted.
func pruneOldNotifications(
	ctx context.Context,
	db *sql.DB,
) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("no database handle")
	}
	result, err := db.ExecContext(
		ctx,
		`DELETE FROM notification
		 WHERE create_time < now() - $1::integer * interval '1 second'`,
		notificationRetentionSec,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
