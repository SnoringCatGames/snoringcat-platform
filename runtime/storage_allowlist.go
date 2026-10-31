package main

import (
	"context"
	"database/sql"
	"strings"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// Client storage-write allowlist (SP59, key rules SP87).
// Before this hook any signed-in player could create any
// collection and write unbounded values through Nakama's
// Storage API.
//
// Inventory behind the list (checked against 0.56.0 and the
// dedicated server before denying anything):
//   - The client writes exactly one collection directly:
//     "settings" (settings_api_client.gd), under the keys
//     "global", "game/{registered game id}" and
//     "game/unknown" (the no-game-id fallback). The game
//     writes no storage directly.
//   - "recent_players" rows are server-only (match_end is
//     their writer, at permission 0/0), so they are not
//     client-writable here; the compliance suite seeds them
//     through the console instead.
//   - Everything else (presence, friend codes, party rows,
//     match history, crash reports, the deletion queue,
//     client IPs, server registrations) is written by the
//     runtime through nk, which this hook does not see.
//
// Runtime nk writes bypass Before hooks, so only client API
// writes are gated here.

const (
	// storageWriteMaxBytes caps one object's value. Settings
	// payloads are a few hundred bytes; this leaves generous
	// headroom.
	storageWriteMaxBytes = 32 * 1024
	// storageWriteMaxObjects caps one write request. The
	// client writes one object per call; the cap bounds how
	// much a single request can store across keys.
	storageWriteMaxObjects = 16
	// settingsKeyGlobal and settingsKeyUnknownGame are the
	// settings keys that are not scoped to a registered
	// game.
	settingsKeyGlobal      = "global"
	settingsKeyUnknownGame = "game/unknown"
	settingsKeyGamePrefix  = "game/"
)

// registerStorageAllowlistHook installs the
// BeforeWriteStorageObjects hook.
func registerStorageAllowlistHook(
	initializer runtime.Initializer,
	games *perGameConfig,
) error {
	return initializer.RegisterBeforeWriteStorageObjects(
		func(
			_ context.Context,
			_ runtime.Logger,
			_ *sql.DB,
			_ runtime.NakamaModule,
			in *api.WriteStorageObjectsRequest,
		) (*api.WriteStorageObjectsRequest, error) {
			if len(in.GetObjects()) > storageWriteMaxObjects {
				return nil, runtime.NewError(
					"too many objects in one storage write", 8)
			}
			for _, obj := range in.GetObjects() {
				if err := validateClientStorageWrite(
					games, obj.GetCollection(),
					obj.GetKey(), obj.GetValue(),
				); err != nil {
					return nil, err
				}
			}
			return in, nil
		})
}

// validateClientStorageWrite checks one client storage
// write: the settings collection only, under an allowed
// key, within the value cap.
func validateClientStorageWrite(
	games *perGameConfig,
	collection, key, value string,
) error {
	if collection != "settings" {
		return runtime.NewError(
			"storage collection is not client-writable", 7)
	}
	if !settingsKeyAllowed(games, key) {
		return runtime.NewError(
			"storage key is not client-writable", 7)
	}
	if len(value) > storageWriteMaxBytes {
		return runtime.NewError("storage value is too large", 3)
	}
	return nil
}

// settingsKeyAllowed reports whether a settings key is one
// the client may write: the global key, the unknown-game
// fallback, or a key scoped to a registered game.
func settingsKeyAllowed(
	games *perGameConfig, key string,
) bool {
	if key == settingsKeyGlobal || key == settingsKeyUnknownGame {
		return true
	}
	gameID, found := strings.CutPrefix(key, settingsKeyGamePrefix)
	if !found || gameID == "" || games == nil {
		return false
	}
	_, registered := games.Get(gameID)
	return registered
}
