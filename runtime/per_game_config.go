package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Per-game configuration store.
//
// Each consuming game ships a `game.yaml` at its repo root. On
// release, the game's CI calls the `register_game` RPC (server-
// to-server, HTTP-key gated) with the YAML content converted to
// JSON; the runtime upserts the row into the `games` table and
// refreshes its in-process cache. Downstream RPCs (presence,
// matchmaking, leaderboards, version_check) consume the cached
// values via the perGameConfig.Get/List API.
//
// The `games` table is platform-shared global state, not user-
// scoped, so it lives in raw Postgres rather than Nakama's
// storage primitives.
//
// Cache strategy: load-all at module init, full refresh on every
// `register_game` write. There's no TTL — the cache is the
// authoritative read source between writes. A future addition
// could be a 60s background refresh to pick up out-of-band
// edits (manual `UPDATE games ...`), but until that's a real
// need keep the model simple.

// gamesTableDDL is run at module init. `IF NOT EXISTS` keeps it
// safe to run on every plugin load. Mirrors the schema in
// PLATFORM_ARCHITECTURE.md §"Per-game configuration".
const gamesTableDDL = `
CREATE TABLE IF NOT EXISTS games (
	game_id TEXT PRIMARY KEY,
	display_name TEXT NOT NULL,
	edgegap_app_slug TEXT NOT NULL,
	protocol_version INTEGER NOT NULL,
	display_version TEXT NOT NULL,
	config JSONB NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
)
`

// GameConfig holds the typed fields the runtime reads directly
// from game.yaml. The full original document is also retained
// in Raw so downstream consumers can read arbitrary keys
// (matchmaker_rules, leaderboards, legal, ...) without
// per-key plumbing.
type GameConfig struct {
	SchemaVersion   int    `json:"schema_version"`
	GameID          string `json:"game_id"`
	DisplayName     string `json:"display_name"`
	EdgegapAppSlug  string `json:"edgegap_app_slug"`
	ProtocolVersion int    `json:"protocol_version"`
	DisplayVersion  string `json:"display_version"`

	// Raw is the verbatim JSON of the original registration
	// payload (i.e. the source game.yaml converted to JSON).
	// Populated when the config is loaded from the DB or
	// upserted via register_game. Not part of the wire JSON
	// shape: marshalling a GameConfig would include the raw
	// document under a `Raw` key which would round-trip
	// double-nested; downstream uses pass `Raw` separately.
	Raw json.RawMessage `json:"-"`
}

// perGameConfig is the in-process cache + Postgres-backed
// registry of all known games. Created once at module init,
// mutated by `register_game` writes, read by RPCs that need
// per-game config.
type perGameConfig struct {
	db    *sql.DB
	mu    sync.RWMutex
	games map[string]*GameConfig
}

// newPerGameConfig ensures the table exists and warms the cache
// from whatever's already in Postgres. Returns a usable store
// even when the table is empty.
func newPerGameConfig(
	ctx context.Context,
	db *sql.DB,
) (*perGameConfig, error) {
	if _, err := db.ExecContext(ctx, gamesTableDDL); err != nil {
		return nil, fmt.Errorf("ensure games table: %w", err)
	}
	c := &perGameConfig{
		db:    db,
		games: map[string]*GameConfig{},
	}
	if err := c.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("warm games cache: %w", err)
	}
	return c, nil
}

// Refresh re-reads every row from `games` and replaces the
// in-process cache atomically. Called at init and after every
// successful `register_game` upsert.
func (c *perGameConfig) Refresh(ctx context.Context) error {
	rows, err := c.db.QueryContext(
		ctx, `SELECT game_id, config FROM games`)
	if err != nil {
		return err
	}
	defer rows.Close()
	next := map[string]*GameConfig{}
	for rows.Next() {
		var (
			gameID string
			cfgRaw []byte
		)
		if err := rows.Scan(&gameID, &cfgRaw); err != nil {
			return err
		}
		gc := &GameConfig{}
		if err := json.Unmarshal(cfgRaw, gc); err != nil {
			return fmt.Errorf(
				"decode game %q: %w", gameID, err)
		}
		// Copy the original bytes so callers can read
		// arbitrary keys. append-to-nil-slice ensures we
		// don't alias the scan buffer.
		gc.Raw = append(json.RawMessage(nil), cfgRaw...)
		next[gameID] = gc
	}
	if err := rows.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.games = next
	c.mu.Unlock()
	return nil
}

// Get returns the cached config for a game, or (nil, false) if
// unknown.
func (c *perGameConfig) Get(gameID string) (*GameConfig, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	gc, ok := c.games[gameID]
	return gc, ok
}

// List returns every known game, sorted by game_id for
// deterministic output (logs, status RPCs).
func (c *perGameConfig) List() []*GameConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*GameConfig, 0, len(c.games))
	for _, gc := range c.games {
		out = append(out, gc)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GameID < out[j].GameID
	})
	return out
}

// GameIDs returns the sorted list of known game IDs. Handy for
// status RPCs and log lines.
func (c *perGameConfig) GameIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.games))
	for id := range c.games {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// upsert writes the row to Postgres and updates the cache. The
// caller has already validated `gc`. `raw` is the original
// registration JSON, retained verbatim in the `config` column.
func (c *perGameConfig) upsert(
	ctx context.Context,
	gc *GameConfig,
	raw []byte,
) error {
	const stmt = `
INSERT INTO games (
	game_id, display_name, edgegap_app_slug,
	protocol_version, display_version, config,
	created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, now(), now())
ON CONFLICT (game_id) DO UPDATE SET
	display_name     = EXCLUDED.display_name,
	edgegap_app_slug = EXCLUDED.edgegap_app_slug,
	protocol_version = EXCLUDED.protocol_version,
	display_version  = EXCLUDED.display_version,
	config           = EXCLUDED.config,
	updated_at       = now()
`
	if _, err := c.db.ExecContext(
		ctx, stmt,
		gc.GameID, gc.DisplayName, gc.EdgegapAppSlug,
		gc.ProtocolVersion, gc.DisplayVersion, raw,
	); err != nil {
		return err
	}
	// Update the in-memory cache to match. Refresh-from-DB
	// would also work but we already have the authoritative
	// row in hand; skip the round trip.
	cached := *gc
	cached.Raw = append(json.RawMessage(nil), raw...)
	c.mu.Lock()
	c.games[gc.GameID] = &cached
	c.mu.Unlock()
	return nil
}

// validateGameConfig enforces the required-field invariants the
// rest of the runtime depends on. Schema-level checks (unknown
// keys, type mismatches) are handled by encoding/json's
// unmarshal step.
func validateGameConfig(gc *GameConfig) error {
	if gc.SchemaVersion <= 0 {
		return fmt.Errorf("schema_version must be positive")
	}
	if gc.GameID == "" {
		return fmt.Errorf("game_id required")
	}
	if gc.DisplayName == "" {
		return fmt.Errorf("display_name required")
	}
	if gc.EdgegapAppSlug == "" {
		return fmt.Errorf("edgegap_app_slug required")
	}
	if gc.ProtocolVersion <= 0 {
		return fmt.Errorf("protocol_version must be positive")
	}
	if gc.DisplayVersion == "" {
		return fmt.Errorf("display_version required")
	}
	return nil
}

// registerGameResponse is the JSON returned to the sync script
// after a successful upsert. The caller (typically a CI step)
// uses this to confirm the runtime saw the new version.
type registerGameResponse struct {
	GameID          string `json:"game_id"`
	ProtocolVersion int    `json:"protocol_version"`
	DisplayVersion  string `json:"display_version"`
	UpdatedAt       int64  `json:"updated_at"`
}

// RegisterGameRpc accepts a game.yaml-shaped JSON payload and
// upserts it into the `games` table. Server-to-server only —
// game registration is part of the platform's deploy surface,
// not a client capability.
func (c *perGameConfig) RegisterGameRpc(
	ctx context.Context,
	logger runtime.Logger,
	_ *sql.DB,
	_ runtime.NakamaModule,
	payload string,
) (string, error) {
	if err := requireServerToServer(ctx); err != nil {
		return "", err
	}
	if payload == "" {
		return "", runtime.NewError(
			"payload required (JSON-encoded game config)", 3)
	}
	gc := &GameConfig{}
	if err := json.Unmarshal([]byte(payload), gc); err != nil {
		return "", runtime.NewError(
			"invalid payload: "+err.Error(), 3)
	}
	if err := validateGameConfig(gc); err != nil {
		return "", runtime.NewError(err.Error(), 3)
	}
	if err := c.upsert(ctx, gc, []byte(payload)); err != nil {
		logger.Error(
			"register_game upsert failed for %q: %v",
			gc.GameID, err)
		return "", runtime.NewError(
			"upsert failed: "+err.Error(), 13)
	}
	logger.Info(
		"register_game: %s protocol=%d display=%s",
		gc.GameID, gc.ProtocolVersion, gc.DisplayVersion)
	out, err := json.Marshal(registerGameResponse{
		GameID:          gc.GameID,
		ProtocolVersion: gc.ProtocolVersion,
		DisplayVersion:  gc.DisplayVersion,
		UpdatedAt:       time.Now().Unix(),
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// getGameConfigArgs lets a client ask for a specific game's
// public config; empty means "the game the caller is signed
// into" (resolved once game_id JWT claims land in Stage 2.5).
type getGameConfigArgs struct {
	GameID string `json:"game_id,omitempty"`
}

// getGameConfigResponse is the client-visible projection of a
// game's config. Mirrors the architecture spec's
// `Platform.get_game_config` RPC and intentionally excludes
// server-internal fields (none today; reserved for future
// additions like rate-limit thresholds).
type getGameConfigResponse struct {
	GameID          string          `json:"game_id"`
	DisplayName     string          `json:"display_name"`
	ProtocolVersion int             `json:"protocol_version"`
	DisplayVersion  string          `json:"display_version"`
	Config          json.RawMessage `json:"config"`
}

// GetGameConfigRpc returns a single game's public config to a
// client session. Unauthenticated callers are rejected; a future
// pre-auth variant (for the version-check screen) would call a
// separate `get_protocol_version` RPC.
//
// The game_id resolves in this order: the payload's explicit
// `game_id` (lets a client browse other games' public config),
// then the session's game_id var (set by the
// BeforeAuthenticate* hook). Missing in both is an error.
func (c *perGameConfig) GetGameConfigRpc(
	ctx context.Context,
	_ runtime.Logger,
	_ *sql.DB,
	_ runtime.NakamaModule,
	payload string,
) (string, error) {
	if _, err := requireClientSession(ctx); err != nil {
		return "", err
	}
	args := getGameConfigArgs{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return "", runtime.NewError(
				"invalid payload: "+err.Error(), 3)
		}
	}
	if args.GameID == "" {
		sessionGameID, err := requireGameID(ctx, c)
		if err != nil {
			return "", err
		}
		if sessionGameID == "" {
			return "", runtime.NewError(
				"game_id required (no payload override and"+
					" session vars carry no game_id —"+
					" re-authenticate with vars set)", 3)
		}
		args.GameID = sessionGameID
	}
	gc, ok := c.Get(args.GameID)
	if !ok {
		return "", runtime.NewError(
			"unknown game_id: "+args.GameID, 5)
	}
	out, err := json.Marshal(getGameConfigResponse{
		GameID:          gc.GameID,
		DisplayName:     gc.DisplayName,
		ProtocolVersion: gc.ProtocolVersion,
		DisplayVersion:  gc.DisplayVersion,
		Config:          gc.Raw,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}
