package main

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The SQL itself runs against Postgres in the compliance suite
// (test_account_delete.gd, test_data_export.gd). These tests lock
// the intent of the statements so an edit that drops a column, or
// starts clearing a sign-in id, fails here first.

func TestProfileScrubClearsEveryProfileField(t *testing.T) {
	for _, assignment := range []string{
		"display_name = $2",
		"avatar_url = NULL",
		"email = CASE WHEN password IS NULL THEN NULL ELSE email END",
		"lang_tag = 'en'",
		"location = NULL",
		"timezone = NULL",
		"metadata = '{}'::jsonb",
	} {
		if !strings.Contains(profileScrubAssignments, assignment) {
			t.Errorf("profile scrub lacks %q", assignment)
		}
	}
}

func TestProfileScrubKeepsSignInIDs(t *testing.T) {
	// A cancellation is a sign-back-in, so every identifier the
	// player can sign in with must survive the request.
	for _, column := range []string{
		"google_id", "facebook_id", "facebook_instant_game_id",
		"apple_id", "steam_id", "gamecenter_id", "custom_id",
		"password", "username",
	} {
		if strings.Contains(profileScrubAssignments, column+" =") {
			t.Errorf("profile scrub must not clear %s", column)
		}
	}
}

func TestProfileNeedsScrubChecksEveryClearedColumn(t *testing.T) {
	// The re-scrub skips rows that are already clean. A column the
	// scrub clears but this check ignores would never be re-cleared.
	for _, column := range []string{
		"display_name", "avatar_url", "email", "lang_tag",
		"location", "timezone", "metadata",
	} {
		if !strings.Contains(profileNeedsScrub, column) {
			t.Errorf("profileNeedsScrub ignores %s", column)
		}
	}
}

func TestEmailSignInIsRefusedAsPermissionDenied(t *testing.T) {
	if got := errorCode(errEmailSignInUnavailable); got != 7 {
		t.Errorf("code = %d, want 7 (PERMISSION_DENIED)", got)
	}
}

func TestClientIPRetentionKeepsUsableRecords(t *testing.T) {
	if clientIPRetentionSec < clientIPMaxAgeSec {
		t.Errorf(
			"retention %ds would delete records readClientIP still"+
				" uses (max age %ds)",
			clientIPRetentionSec, clientIPMaxAgeSec)
	}
}

func TestDeletionQueueRecordKeepsOnlyTimes(t *testing.T) {
	encoded, err := json.Marshal(deletionQueueRecord{
		UserID: "u", RequestedAt: 1, ScheduledFor: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertKeys(t, encoded, "requested_at", "scheduled_for", "user_id")
}

func TestDeletionQueueRecordReadsLegacyRows(t *testing.T) {
	legacy := `{"user_id":"u","requested_at":1,"scheduled_for":2,` +
		`"original_username":"x","original_display_name":"Ada"}`
	var record deletionQueueRecord
	if err := json.Unmarshal([]byte(legacy), &record); err != nil {
		t.Fatalf("legacy row: %v", err)
	}
	if record.RequestedAt != 1 || record.ScheduledFor != 2 {
		t.Errorf("legacy row = %+v", record)
	}
}

func TestDeletionStatusCarriesNoProfileCopy(t *testing.T) {
	encoded, err := json.Marshal(accountDeletionStatusResp{
		Pending: true, UserID: "u", RequestedAt: 1, ScheduledFor: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertKeys(
		t, encoded, "pending", "requested_at", "scheduled_for", "user_id")
}

func TestCancelResponseRestoresNothing(t *testing.T) {
	encoded, err := json.Marshal(cancelAccountDeletionResp{
		OK: true, UserID: "u",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertKeys(t, encoded, "ok", "user_id")
}

func TestMergeLeaderboardIDs(t *testing.T) {
	got := mergeLeaderboardIDs(
		[]string{"hopnbop_ffa", "ffa"},
		[]string{"ffa", "old_board", "", "old_board"},
	)
	want := []string{"hopnbop_ffa", "ffa", "old_board"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

// assertKeys checks a JSON object's top-level keys.
func assertKeys(t *testing.T, encoded []byte, want ...string) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
	got := make([]string, 0, len(fields))
	for key := range fields {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}
