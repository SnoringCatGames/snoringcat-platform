package main

import (
	"context"
	"testing"
)

// SP40: the prune keeps a useful toast history without
// letting the table grow forever, and it refuses to run
// without a database (the cron logs that, it must not crash).

func TestNotificationRetentionBoundsHistory(t *testing.T) {
	if notificationRetentionSec < 24*3600 {
		t.Errorf(
			"retention %ds is under a day of toast history",
			notificationRetentionSec)
	}
	if notificationRetentionSec > 90*24*3600 {
		t.Errorf(
			"retention %ds lets notification rows pile up again",
			notificationRetentionSec)
	}
}

func TestPruneOldNotificationsNeedsDatabase(t *testing.T) {
	if _, err := pruneOldNotifications(
		context.Background(), nil,
	); err == nil {
		t.Fatal("expected an error without a database handle")
	}
}
