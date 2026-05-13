package main

import (
	"encoding/json"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

// testLogger is a no-op runtime.Logger implementation for unit
// tests. Production callers route through Nakama's structured
// logger; tests don't care about log output, only that the helper
// can call any method without panicking.
type testLogger struct{}

func (testLogger) Debug(string, ...interface{})            {}
func (testLogger) Info(string, ...interface{})             {}
func (testLogger) Warn(string, ...interface{})             {}
func (testLogger) Error(string, ...interface{})            {}
func (l testLogger) WithField(string, interface{}) runtime.Logger {
	return l
}
func (l testLogger) WithFields(map[string]interface{}) runtime.Logger {
	return l
}
func (testLogger) Fields() map[string]interface{} { return nil }

// newTestGames constructs an in-memory perGameConfig without a DB.
// The map keys are game_ids; the values are raw JSON blobs as if
// register_game received them. The returned cache is ready for the
// Get/List/GameIDs API used by the production helpers. Tests that
// only need an "empty cache" call newTestGames(t, nil).
//
// We intentionally bypass Refresh/upsert (those require a *sql.DB)
// — every pure helper in the runtime reads through Get/List, so
// pre-populating the map is enough.
func newTestGames(
	t *testing.T,
	configs map[string]string,
) *perGameConfig {
	t.Helper()
	c := &perGameConfig{games: map[string]*GameConfig{}}
	for id, raw := range configs {
		gc := &GameConfig{}
		if err := json.Unmarshal([]byte(raw), gc); err != nil {
			t.Fatalf("newTestGames: parse %q: %v", id, err)
		}
		gc.Raw = json.RawMessage(raw)
		// Honour the map key even if the embedded game_id differs;
		// the registry's logical key is the map key.
		if gc.GameID == "" {
			gc.GameID = id
		}
		c.games[id] = gc
	}
	return c
}
