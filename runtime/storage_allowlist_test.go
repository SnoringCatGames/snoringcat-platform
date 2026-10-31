package main

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

func testStorageGames(t *testing.T) *perGameConfig {
	t.Helper()
	return newTestGames(t, map[string]string{
		"hopnbop": `{"game_id":"hopnbop"}`,
	})
}

func TestValidateClientStorageWrite(t *testing.T) {
	games := testStorageGames(t)
	good := []struct{ collection, key string }{
		{"settings", "global"},
		{"settings", "game/hopnbop"},
		{"settings", "game/unknown"},
	}
	for _, tc := range good {
		if err := validateClientStorageWrite(
			games, tc.collection, tc.key, `{"volume":3}`,
		); err != nil {
			t.Errorf(
				"%s/%s: err = %v, want nil",
				tc.collection, tc.key, err)
		}
	}
	bad := []struct {
		collection, key string
		code            int
	}{
		{"presence", "current", 7},
		{"recent_players", "someone", 7},
		{"settings", "junk-00000000", 7},
		{"settings", "game/other-game", 7},
		{"settings", "", 7},
	}
	for _, tc := range bad {
		if err := validateClientStorageWrite(
			games, tc.collection, tc.key, `{}`,
		); errorCode(err) != tc.code {
			t.Errorf(
				"%s/%s: err = %v, want code %d",
				tc.collection, tc.key, err, tc.code)
		}
	}
	big := strings.Repeat("x", storageWriteMaxBytes+1)
	if err := validateClientStorageWrite(
		games, "settings", "global", big,
	); errorCode(err) != 3 {
		t.Fatalf("oversize write: err = %v, want code 3", err)
	}
}

// capturingInitializer records the storage hook so the
// test can call it the way Nakama would. Dropping the
// registration in main.go leaves storageFn nil and fails
// here.
type capturingInitializer struct {
	runtime.Initializer

	storageFn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.WriteStorageObjectsRequest,
	) (*api.WriteStorageObjectsRequest, error)
}

func (c *capturingInitializer) RegisterBeforeWriteStorageObjects(
	fn func(
		context.Context, runtime.Logger, *sql.DB,
		runtime.NakamaModule, *api.WriteStorageObjectsRequest,
	) (*api.WriteStorageObjectsRequest, error),
) error {
	c.storageFn = fn
	return nil
}

func TestStorageAllowlistHookIsRegistered(t *testing.T) {
	initializer := &capturingInitializer{}
	if err := registerStorageAllowlistHook(
		initializer, testStorageGames(t)); err != nil {
		t.Fatalf("register: err = %v", err)
	}
	if initializer.storageFn == nil {
		t.Fatal("no storage hook was registered")
	}
	write := func(
		objects ...*api.WriteStorageObject,
	) (*api.WriteStorageObjectsRequest, error) {
		return initializer.storageFn(
			context.Background(), testLogger{}, nil, nil,
			&api.WriteStorageObjectsRequest{Objects: objects})
	}
	obj := func(collection, key, value string) *api.WriteStorageObject {
		return &api.WriteStorageObject{
			Collection: collection, Key: key, Value: value}
	}
	if _, err := write(
		obj("settings", "global", `{}`)); err != nil {
		t.Fatalf("allowed write: err = %v, want nil", err)
	}
	if _, err := write(
		obj("presence", "current", `{}`),
	); errorCode(err) != 7 {
		t.Fatalf("presence write: err = %v, want code 7", err)
	}
	if _, err := write(
		obj("settings", "junk-00000000", `{}`),
	); errorCode(err) != 7 {
		t.Fatalf("junk key write: err = %v, want code 7", err)
	}
	many := make([]*api.WriteStorageObject, 0, storageWriteMaxObjects+1)
	for range storageWriteMaxObjects + 1 {
		many = append(many, obj("settings", "global", `{}`))
	}
	if _, err := write(
		many...); errorCode(err) != 8 {
		t.Fatalf("many objects: err = %v, want code 8", err)
	}
}
