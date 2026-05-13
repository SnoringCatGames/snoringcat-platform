// Nakama's runtime exposes context keys as plain string constants
// (RUNTIME_CTX_VARS / RUNTIME_CTX_USER_ID); the production code in
// auth.go reads through those keys verbatim. Tests must inject
// values using the same string-typed keys so the production
// lookups find them — staticcheck's SA1029 ("don't use built-in
// string as a context key") flags the writes but a wrapper type
// would defeat the round-trip. Suppress for this file only.
//
//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

func TestGameIDFromVars(t *testing.T) {
	if got := gameIDFromVars(nil); got != "" {
		t.Errorf("nil vars should return empty, got %q", got)
	}
	if got := gameIDFromVars(map[string]string{}); got != "" {
		t.Errorf("empty map should return empty, got %q", got)
	}
	got := gameIDFromVars(map[string]string{"game_id": "hopnbop"})
	if got != "hopnbop" {
		t.Errorf("got %q, want hopnbop", got)
	}
}

// ctxWithVars wraps context.WithValue with the Nakama vars key so
// requireGameID's `ctx.Value(...)` lookup finds it.
func ctxWithVars(vars map[string]string) context.Context {
	return context.WithValue(
		context.Background(), runtime.RUNTIME_CTX_VARS, vars)
}

func TestRequireGameIDBootstrapPassthrough(t *testing.T) {
	games := newTestGames(t, nil)

	t.Run("empty-cache-empty-vars", func(t *testing.T) {
		got, err := requireGameID(ctxWithVars(nil), games)
		if err != nil {
			t.Fatalf("bootstrap should pass through, got err: %v",
				err)
		}
		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})

	t.Run("empty-cache-with-vars", func(t *testing.T) {
		got, err := requireGameID(
			ctxWithVars(map[string]string{"game_id": "anything"}),
			games)
		if err != nil {
			t.Fatalf("bootstrap should pass through, got err: %v",
				err)
		}
		if got != "anything" {
			t.Errorf("bootstrap should echo whatever client sent,"+
				" got %q", got)
		}
	})
}

func TestRequireGameIDStrictMode(t *testing.T) {
	games := newTestGames(t,
		map[string]string{"hopnbop": hopnbopConfigJSON})

	t.Run("valid", func(t *testing.T) {
		got, err := requireGameID(
			ctxWithVars(map[string]string{"game_id": "hopnbop"}),
			games)
		if err != nil {
			t.Fatalf("valid game_id should pass, got err: %v", err)
		}
		if got != "hopnbop" {
			t.Errorf("got %q, want hopnbop", got)
		}
	})

	t.Run("missing", func(t *testing.T) {
		_, err := requireGameID(ctxWithVars(nil), games)
		if err == nil {
			t.Fatal("missing game_id should error in strict mode")
		}
	})

	t.Run("unknown", func(t *testing.T) {
		_, err := requireGameID(
			ctxWithVars(map[string]string{"game_id": "ghost"}),
			games)
		if err == nil {
			t.Fatal("unknown game_id should error")
		}
	})

	t.Run("empty-string", func(t *testing.T) {
		_, err := requireGameID(
			ctxWithVars(map[string]string{"game_id": ""}), games)
		if err == nil {
			t.Fatal("empty game_id should error in strict mode")
		}
	})
}

func TestValidateGameIDInVarsBootstrapPassthrough(t *testing.T) {
	games := newTestGames(t, nil)
	if err := validateGameIDInVars(nil, games); err != nil {
		t.Errorf("empty cache + nil vars should pass, got %v", err)
	}
	if err := validateGameIDInVars(
		map[string]string{}, games); err != nil {
		t.Errorf("empty cache + empty vars should pass, got %v",
			err)
	}
	if err := validateGameIDInVars(
		map[string]string{"game_id": "anything"}, games); err != nil {
		t.Errorf("empty cache + any value should pass, got %v",
			err)
	}
}

func TestValidateGameIDInVarsStrictMode(t *testing.T) {
	games := newTestGames(t,
		map[string]string{"hopnbop": hopnbopConfigJSON})

	if err := validateGameIDInVars(
		map[string]string{"game_id": "hopnbop"}, games); err != nil {
		t.Errorf("valid id should pass, got %v", err)
	}
	if err := validateGameIDInVars(nil, games); err == nil {
		t.Error("nil vars should fail in strict mode")
	}
	if err := validateGameIDInVars(
		map[string]string{"game_id": ""}, games); err == nil {
		t.Error("empty game_id should fail in strict mode")
	}
	if err := validateGameIDInVars(
		map[string]string{"game_id": "ghost"}, games); err == nil {
		t.Error("unknown game_id should fail")
	}
}

// TestRequireServerToServer covers the auth helper used by every
// server-to-server RPC (register_game, match lifecycle, etc.). A
// client session populates RUNTIME_CTX_USER_ID; the helper rejects
// any caller whose user_id is non-empty.
func TestRequireServerToServer(t *testing.T) {
	t.Run("no-user-passes", func(t *testing.T) {
		if err := requireServerToServer(
			context.Background()); err != nil {
			t.Fatalf("server-to-server caller should pass, got %v",
				err)
		}
	})

	t.Run("client-rejected", func(t *testing.T) {
		ctx := context.WithValue(
			context.Background(),
			runtime.RUNTIME_CTX_USER_ID,
			"some-user-id")
		if err := requireServerToServer(ctx); err == nil {
			t.Fatal("client caller should be rejected")
		}
	})
}

// TestRequireClientSession is the mirror: client RPCs require a
// non-empty user_id; server-to-server callers are rejected.
func TestRequireClientSession(t *testing.T) {
	t.Run("client-passes", func(t *testing.T) {
		ctx := context.WithValue(
			context.Background(),
			runtime.RUNTIME_CTX_USER_ID,
			"alice")
		got, err := requireClientSession(ctx)
		if err != nil {
			t.Fatalf("client caller should pass, got %v", err)
		}
		if got != "alice" {
			t.Errorf("user_id = %q, want alice", got)
		}
	})

	t.Run("missing-rejected", func(t *testing.T) {
		_, err := requireClientSession(context.Background())
		if err == nil {
			t.Fatal("missing session should be rejected")
		}
	})
}
