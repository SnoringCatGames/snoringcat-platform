package main

import (
	"context"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Server-side ready gate for party_start_matchmaking (SP33).
// Any active member may start, but only when every active
// member is ready; the ready check used to live only in the
// lobby UI, so any member (or a raw API caller) could pull
// the whole party into a match load unready.

// allMembersReady reports whether every listed member has a
// ready row. Unready is a deleted row (party_set_ready with
// ready=false), so a missing row means not ready.
func allMembersReady(
	ctx context.Context,
	nk runtime.NakamaModule,
	partyID string,
	memberIDs []string,
) (bool, error) {
	if len(memberIDs) == 0 {
		return false, nil
	}
	reads := make([]*runtime.StorageRead, 0, len(memberIDs))
	for _, uid := range memberIDs {
		reads = append(reads, &runtime.StorageRead{
			Collection: partyReadyCollection,
			Key:        partyID,
			UserID:     uid,
		})
	}
	objects, err := nk.StorageRead(ctx, reads)
	if err != nil {
		return false, err
	}
	readyByUser := map[string]bool{}
	for _, obj := range objects {
		var row struct {
			Ready bool `json:"ready"`
		}
		if err := json.Unmarshal(
			[]byte(obj.GetValue()), &row); err != nil {
			continue
		}
		readyByUser[obj.GetUserId()] = row.Ready
	}
	for _, uid := range memberIDs {
		if !readyByUser[uid] {
			return false, nil
		}
	}
	return true, nil
}
