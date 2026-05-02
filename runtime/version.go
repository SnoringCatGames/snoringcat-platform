package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// versionConfig is the snapshot of game/protocol versions read
// from env vars at module init. The version_check RPC echoes
// these back so clients can compare.
type versionConfig struct {
	GameVersion     string
	ProtocolVersion int
}

type versionCheckArgs struct {
	ClientProtocolVersion int    `json:"client_protocol_version"`
	ClientGameVersion     string `json:"client_game_version"`
}

type versionCheckResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	GameVersion     string `json:"game_version"`
	IsCompatible    bool   `json:"is_compatible"`
}

// versionCheckRpcFactory returns the RPC handler bound to the
// configured server-side versions. The client decides
// compatibility using the returned values, but we also surface
// is_compatible so future logic (forced upgrades, soft deprecation
// windows) can move server-side without a client rebuild.
func versionCheckRpcFactory(cfg versionConfig) func(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	payload string,
) (string, error) {
	return func(
		_ context.Context,
		_ runtime.Logger,
		_ *sql.DB,
		_ runtime.NakamaModule,
		payload string,
	) (string, error) {
		args := versionCheckArgs{}
		if payload != "" {
			// Best-effort: clients without payloads still get a
			// sensible response.
			_ = json.Unmarshal([]byte(payload), &args)
		}
		compatible := args.ClientProtocolVersion == 0 ||
			args.ClientProtocolVersion == cfg.ProtocolVersion
		out, err := json.Marshal(versionCheckResponse{
			ProtocolVersion: cfg.ProtocolVersion,
			GameVersion:     cfg.GameVersion,
			IsCompatible:    compatible,
		})
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}
