package main

import (
	"context"
	"database/sql"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Client storage-write allowlist (SP59). Before this hook any
// signed-in player could create any collection and write
// unbounded values through Nakama's Storage API.
//
// Inventory behind the list (checked against 0.56.0 and the
// dedicated server before denying anything):
//   - The client writes exactly one collection directly:
//     "settings" (settings_api_client.gd, keys "global" and
//     "game/{id}"). The game writes no storage directly.
//   - "recent_players" rows are written by the runtime, but
//     the compliance suite seeds a player's own row the same
//     way; a forged own-recents row harms only the forger's
//     own social list, so it stays allowed.
//   - Everything else (presence, friend codes, party rows,
//     match history, crash reports, the deletion queue, client
//     IPs, server registrations) is written by the runtime
//     through nk, which this hook does not see.
//
// Runtime nk writes bypass Before hooks, so only client API
// writes are gated here.

const (
	// storageWriteMaxBytes caps one object's value. Settings
	// payloads and recent-player lists are a few hundred
	// bytes; this leaves generous headroom.
	storageWriteMaxBytes = 32 * 1024
)

// clientStorageCollections is the allowlist.
var clientStorageCollections = map[string]bool{
	"settings":       true,
	"recent_players": true,
}

// registerStorageAllowlistHook installs the
// BeforeWriteStorageObjects hook.
func registerStorageAllowlistHook(
	initializer runtime.Initializer,
) error {
	return initializer.RegisterBeforeWriteStorageObjects(
		func(
			_ context.Context,
			_ runtime.Logger,
			_ *sql.DB,
			_ runtime.NakamaModule,
			in *api.WriteStorageObjectsRequest,
		) (*api.WriteStorageObjectsRequest, error) {
			for _, obj := range in.GetObjects() {
				if err := validateClientStorageWrite(
					obj.GetCollection(), obj.GetValue(),
				); err != nil {
					return nil, err
				}
			}
			return in, nil
		})
}

// validateClientStorageWrite checks one client storage write.
func validateClientStorageWrite(
	collection, value string,
) error {
	if !clientStorageCollections[collection] {
		return runtime.NewError(
			"storage collection is not client-writable", 7)
	}
	if len(value) > storageWriteMaxBytes {
		return runtime.NewError("storage value is too large", 3)
	}
	return nil
}
