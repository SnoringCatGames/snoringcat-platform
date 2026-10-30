package main

import (
	"context"
	"encoding/json"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// SP58 (RT1): hourly sweep of per-match rows that never resolved.
// match_end and match_cancel delete the registration, metadata and
// synthetic rows when a match ends, but a game server that dies
// mid-match leaves all three behind forever. The account cron runs
// this pass: metadata allocated more than staleMatchAfter ago with
// no resolution counts as never ended; its rows are deleted and the
// count is logged so a stuck allocator shows up in the Nakama log.

const staleMatchAfter = 24 * time.Hour

const staleMatchBatch = 100

const staleMatchMaxPages = 10

type staleMatchMetadata struct {
	AllocatedAt int64 `json:"allocated_at"`
}

func sweepStaleMatchRows(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
) {
	now := time.Now().Unix()
	cutoff := now - int64(staleMatchAfter/time.Second)
	cursor := ""
	neverEnded := 0
	scanned := 0
	for page := 0; page < staleMatchMaxPages; page++ {
		objects, next, err := nk.StorageList(
			ctx, "", "", matchMetadataCollection, staleMatchBatch,
			cursor)
		if err != nil {
			logger.Warn("match_sweep list: %v", err)
			return
		}
		var deletes []*runtime.StorageDelete
		for _, obj := range objects {
			scanned++
			var meta staleMatchMetadata
			if err := json.Unmarshal(
				[]byte(obj.Value), &meta); err != nil {
				// Malformed or legacy rows without a timestamp
				// are an operator's call, not the sweep's.
				continue
			}
			if meta.AllocatedAt == 0 || meta.AllocatedAt > cutoff {
				continue
			}
			neverEnded++
			deletes = append(deletes,
				&runtime.StorageDelete{
					Collection: matchMetadataCollection,
					Key:        obj.Key,
				},
				&runtime.StorageDelete{
					Collection: "server_registrations",
					Key:        obj.Key,
				},
				&runtime.StorageDelete{
					Collection: syntheticMatchCollection,
					Key:        obj.Key,
				})
		}
		if len(deletes) > 0 {
			if err := nk.StorageDelete(ctx, deletes); err != nil {
				logger.Warn("match_sweep delete: %v", err)
				return
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if neverEnded > 0 {
		logger.Warn(
			"match_sweep: %d match(es) never ended (scanned %d); rows swept",
			neverEnded, scanned)
	}
}
