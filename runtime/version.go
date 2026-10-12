package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/heroiclabs/nakama-common/runtime"
)

// versionConfig is the snapshot of game/protocol versions read
// from env vars at module init. The version_check RPC echoes
// these back when the caller doesn't provide a game_id. When a
// known game_id is in the payload, the per-game config from the
// `games` table wins.
type versionConfig struct {
	GameVersion     string
	ProtocolVersion int
}

type versionCheckArgs struct {
	ClientProtocolVersion int    `json:"client_protocol_version"`
	ClientGameVersion     string `json:"client_game_version"`
	// GameID, when set and matching a registered game, selects
	// per-game values for the response. Stage 3.10 wires the
	// client to pass this so legal_version comes from
	// game.yaml/games-table rather than a compile-time constant.
	GameID string `json:"game_id"`
}

type versionCheckResponse struct {
	ProtocolVersion int    `json:"protocol_version"`
	GameVersion     string `json:"game_version"`
	// LegalVersion is the game-scoped legal-consent version, read
	// from the per-game config's `legal.legal_version` field.
	// Empty when the caller didn't supply game_id or when the
	// requested game isn't registered. Clients with a cached
	// in-script constant treat empty as "no override; use the
	// compile-time fallback".
	LegalVersion string `json:"legal_version,omitempty"`
	// MatchmakerMinPlayers / MatchmakerMaxPlayers / MatchmakerQuery
	// surface game.yaml's matchmaker_rules block to the client.
	// Zero / empty values mean "no override; use the client's
	// compile-time fallback" — the same bootstrap-safe pattern as
	// LegalVersion. Stage 3.8.
	MatchmakerMinPlayers int    `json:"matchmaker_min_players,omitempty"`
	MatchmakerMaxPlayers int    `json:"matchmaker_max_players,omitempty"`
	MatchmakerQuery      string `json:"matchmaker_query,omitempty"`
	// MatchmakerModes is the game.yaml `matchmaker_rules.modes`
	// list, surfaced verbatim so the client can populate a
	// game-mode picker without authenticating first (clients read
	// it from the unauthenticated versionCheckHTTPPath route).
	// Empty slice when the game declares no modes — clients treat
	// that as "single-mode game; hide the picker". Stage 4.7 / 5.7.
	MatchmakerModes []gameMode `json:"matchmaker_modes,omitempty"`
	IsCompatible    bool       `json:"is_compatible"`
}

// gameMode is one entry in `matchmaker_rules.modes`. The client
// uses {id, display_name_key, description_key, is_default} for
// the picker UI; the runtime uses {min_players, max_players,
// query} to override the top-level matchmaker rules when the
// matched players' tickets agree on a `game_mode` property. JSON
// keys mirror the game.yaml field names so the per-game-config
// Raw blob round-trips through register_game without translation.
type gameMode struct {
	ID             string `json:"id"`
	DisplayNameKey string `json:"display_name_key,omitempty"`
	DescriptionKey string `json:"description_key,omitempty"`
	MinPlayers     int    `json:"min_players,omitempty"`
	MaxPlayers     int    `json:"max_players,omitempty"`
	Query          string `json:"query,omitempty"`
	IsDefault      bool   `json:"is_default,omitempty"`
}

// versionCheckRpcFactory returns the RPC handler bound to the
// configured server-side versions and the per-game config cache.
// The client decides compatibility using the returned values,
// but we also surface is_compatible so future logic (forced
// upgrades, soft deprecation windows) can move server-side
// without a client rebuild.
//
// Callable with the HTTP key or a session. Clients up to 0.47.0
// call it with the HTTP key before sign-in; newer clients use the
// unauthenticated versionCheckHTTPPath route below instead, and the
// synthetic-match probe calls it with its session.
func versionCheckRpcFactory(
	cfg versionConfig,
	games *perGameConfig,
) func(
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
		out, err := json.Marshal(
			buildVersionCheckResponse(cfg, games, args))
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
}

// buildVersionCheckResponse computes the version_check answer for
// one request. Shared by the RPC and the unauthenticated HTTP
// route, so the two can't drift apart.
func buildVersionCheckResponse(
	cfg versionConfig,
	games *perGameConfig,
	args versionCheckArgs,
) versionCheckResponse {
	protocolVersion := cfg.ProtocolVersion
	gameVersion := cfg.GameVersion
	legalVersion := ""
	mmMin := 0
	mmMax := 0
	mmQuery := ""
	var modes []gameMode
	if args.GameID != "" && games != nil {
		if gc, ok := games.Get(args.GameID); ok {
			protocolVersion = gc.ProtocolVersion
			gameVersion = gc.DisplayVersion
			legalVersion = parseLegalVersionFromConfig(gc)
			mmMin, mmMax, mmQuery =
				parseMatchmakerRulesFromConfig(gc)
			modes = parseModesFromConfig(gc)
		}
	}

	compatible := args.ClientProtocolVersion == 0 ||
		args.ClientProtocolVersion == protocolVersion
	return versionCheckResponse{
		ProtocolVersion:      protocolVersion,
		GameVersion:          gameVersion,
		LegalVersion:         legalVersion,
		MatchmakerMinPlayers: mmMin,
		MatchmakerMaxPlayers: mmMax,
		MatchmakerQuery:      mmQuery,
		MatchmakerModes:      modes,
		IsCompatible:         compatible,
	}
}

// versionCheckHTTPPath is an unauthenticated HTTP route on Nakama's
// client API port (behind Caddy at https://nakama.snoringcat.games)
// that answers exactly like the version_check RPC.
//
// It exists so clients can check compatibility before sign-in
// without a credential. Nakama only runs RPCs for a session or the
// runtime HTTP key, and the HTTP key is a server credential: clients
// up to 0.47.0 embedded it for this one call, which handed every
// player (and anyone reading the public repo) the ability to call
// any RPC as a trusted server. A route registered with RegisterHttp
// is not authenticated by Nakama at all, which is what we want for
// a response made of public data (versions, legal version,
// matchmaker modes). Both GET (query parameters) and POST (the RPC's
// JSON body) work. It sits outside /v2/ so it can never collide
// with Nakama's own routes.
const versionCheckHTTPPath = "/snoringcat/version_check"

// versionCheckMaxBodyBytes caps a POST body. A real request is under
// 200 bytes; anything near this is not a client.
const versionCheckMaxBodyBytes = 4096

// versionCheckHTTPHandler serves versionCheckHTTPPath.
func versionCheckHTTPHandler(
	cfg versionConfig,
	games *perGameConfig,
) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		args, status := parseVersionCheckHTTPArgs(r)
		if status != http.StatusOK {
			http.Error(w, http.StatusText(status), status)
			return
		}
		out, err := json.Marshal(
			buildVersionCheckResponse(cfg, games, args))
		if err != nil {
			http.Error(
				w, http.StatusText(http.StatusInternalServerError),
				http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Versions change on every deploy; never let a proxy or
		// browser serve a stale answer.
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(out)
	}
}

// parseVersionCheckHTTPArgs reads the request arguments from the
// query string (GET) or the JSON body (POST). Like the RPC, a body
// that doesn't parse still gets the default response. Returns an
// HTTP status other than 200 only for a request it refuses outright.
func parseVersionCheckHTTPArgs(
	r *http.Request,
) (versionCheckArgs, int) {
	args := versionCheckArgs{}
	switch r.Method {
	case http.MethodGet:
		query := r.URL.Query()
		args.GameID = query.Get("game_id")
		args.ClientGameVersion = query.Get("client_game_version")
		if raw := query.Get("client_protocol_version"); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				args.ClientProtocolVersion = v
			}
		}
	case http.MethodPost:
		body, err := io.ReadAll(
			io.LimitReader(r.Body, versionCheckMaxBodyBytes+1))
		if err != nil {
			return args, http.StatusBadRequest
		}
		if len(body) > versionCheckMaxBodyBytes {
			return args, http.StatusRequestEntityTooLarge
		}
		if len(bytes.TrimSpace(body)) > 0 {
			_ = json.Unmarshal(body, &args)
		}
	default:
		return args, http.StatusMethodNotAllowed
	}
	return args, http.StatusOK
}

// parseMatchmakerRulesFromConfig pulls min/max/query from
// `matchmaker_rules` in a game's raw config blob. Missing fields
// return zero values; the client treats those as "no override".
// `matchmaker_rules.query` is not in the schema today — the
// client's compile-time default (`*`) wins until a game opts in.
func parseMatchmakerRulesFromConfig(
	gc *GameConfig,
) (minPlayers, maxPlayers int, query string) {
	if gc == nil || len(gc.Raw) == 0 {
		return 0, 0, ""
	}
	var blob struct {
		MatchmakerRules struct {
			MinPlayers int    `json:"min_players"`
			MaxPlayers int    `json:"max_players"`
			Query      string `json:"query"`
		} `json:"matchmaker_rules"`
	}
	if err := json.Unmarshal(gc.Raw, &blob); err != nil {
		return 0, 0, ""
	}
	return blob.MatchmakerRules.MinPlayers,
		blob.MatchmakerRules.MaxPlayers,
		blob.MatchmakerRules.Query
}

// parseModesFromConfig pulls the `matchmaker_rules.modes` list
// out of a game's raw config blob. Returns nil when the game
// declares no modes (single-mode game; client hides picker) or
// when the blob doesn't parse. Entries with empty `id` are
// dropped — every other field is optional and the client falls
// back to compile-time defaults.
//
// Also used as the matchmaker hook's source of truth for per-mode
// min/max/query overrides (Stage 4.7 + 5.7); kept in version.go
// rather than per_game_config.go because the response shape is
// already shaped against this type via versionCheckResponse.
func parseModesFromConfig(gc *GameConfig) []gameMode {
	if gc == nil || len(gc.Raw) == 0 {
		return nil
	}
	var blob struct {
		MatchmakerRules struct {
			Modes []gameMode `json:"modes"`
		} `json:"matchmaker_rules"`
	}
	if err := json.Unmarshal(gc.Raw, &blob); err != nil {
		return nil
	}
	if len(blob.MatchmakerRules.Modes) == 0 {
		return nil
	}
	out := make([]gameMode, 0, len(blob.MatchmakerRules.Modes))
	for _, m := range blob.MatchmakerRules.Modes {
		if m.ID == "" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// parseLegalVersionFromConfig pulls `legal.legal_version` out of
// a game's raw config blob. Returns "" when missing or malformed
// (callers fall back to a compile-time constant).
func parseLegalVersionFromConfig(gc *GameConfig) string {
	if gc == nil || len(gc.Raw) == 0 {
		return ""
	}
	var blob struct {
		Legal struct {
			LegalVersion string `json:"legal_version"`
		} `json:"legal"`
	}
	if err := json.Unmarshal(gc.Raw, &blob); err != nil {
		return ""
	}
	return blob.Legal.LegalVersion
}
