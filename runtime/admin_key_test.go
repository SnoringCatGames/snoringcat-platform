// Nakama's runtime exposes context keys as plain string constants
// (RUNTIME_CTX_QUERY_PARAMS / RUNTIME_CTX_USER_ID); admin_key.go
// reads through those keys verbatim, so the tests must inject
// values with the same string-typed keys. See auth_test.go.
//
//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

const testAdminKey = "test-admin-key/with+base64=chars"

// ctxWithQuery returns a context carrying the HTTP RPC query
// parameters, as Nakama passes them to an RPC.
func ctxWithQuery(params map[string][]string) context.Context {
	return context.WithValue(
		context.Background(), runtime.RUNTIME_CTX_QUERY_PARAMS, params)
}

// errorCode returns the gRPC code of a runtime.Error, or -1.
func errorCode(err error) int {
	var rtErr *runtime.Error
	if errors.As(err, &rtErr) {
		return rtErr.Code
	}
	return -1
}

func TestAdminGateAcceptsMatchingKey(t *testing.T) {
	gate := newAdminGate(testAdminKey)
	ctx := ctxWithQuery(map[string][]string{
		"admin_key": {testAdminKey},
	})
	if err := gate.authorize(ctx, "register_game"); err != nil {
		t.Fatalf("matching key should pass, got %v", err)
	}
}

func TestAdminGateAcceptsURLValues(t *testing.T) {
	// Nakama declares the parameter as map[string][]string, but a
	// url.Values must work too in case that ever changes.
	gate := newAdminGate(testAdminKey)
	ctx := context.WithValue(
		context.Background(), runtime.RUNTIME_CTX_QUERY_PARAMS,
		url.Values{"admin_key": {testAdminKey}})
	if err := gate.authorize(ctx, "register_game"); err != nil {
		t.Fatalf("url.Values key should pass, got %v", err)
	}
}

func TestAdminGateTrimsConfiguredKey(t *testing.T) {
	// A secret pasted with a trailing newline must still match.
	gate := newAdminGate("  " + testAdminKey + "\r\n")
	ctx := ctxWithQuery(map[string][]string{
		"admin_key": {testAdminKey},
	})
	if err := gate.authorize(ctx, "register_game"); err != nil {
		t.Fatalf("trimmed key should pass, got %v", err)
	}
}

func TestAdminGateRejectsWrongKey(t *testing.T) {
	gate := newAdminGate(testAdminKey)
	cases := map[string]string{
		"different":    "not-the-key",
		"prefix":       testAdminKey[:len(testAdminKey)-1],
		"extended":     testAdminKey + "x",
		"case-changed": strings.ToUpper(testAdminKey),
	}
	for name, provided := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := ctxWithQuery(map[string][]string{
				"admin_key": {provided},
			})
			err := gate.authorize(ctx, "register_game")
			if err == nil {
				t.Fatal("wrong key should be rejected")
			}
			if got := errorCode(err); got != 16 {
				t.Errorf("code = %d, want 16 (UNAUTHENTICATED)", got)
			}
			if strings.Contains(err.Error(), provided) ||
				strings.Contains(err.Error(), testAdminKey) {
				t.Errorf("error echoes a key: %q", err.Error())
			}
		})
	}
}

func TestAdminGateRejectsMissingKey(t *testing.T) {
	gate := newAdminGate(testAdminKey)
	cases := map[string]context.Context{
		"no-query-params": context.Background(),
		"other-params-only": ctxWithQuery(map[string][]string{
			"unwrap": {"true"},
		}),
		"empty-value": ctxWithQuery(map[string][]string{
			"admin_key": {""},
		}),
		"no-values": ctxWithQuery(map[string][]string{
			"admin_key": {},
		}),
	}
	for name, ctx := range cases {
		t.Run(name, func(t *testing.T) {
			err := gate.authorize(ctx, "register_game")
			if err == nil {
				t.Fatal("missing key should be rejected")
			}
			if got := errorCode(err); got != 16 {
				t.Errorf("code = %d, want 16 (UNAUTHENTICATED)", got)
			}
		})
	}
}

func TestAdminGateRefusesEverythingWhenUnset(t *testing.T) {
	// GAME_ADMIN_KEY unset (or blank) must fail closed, even for a
	// caller that sends an empty admin_key, which would otherwise
	// "match" an empty configured key.
	gates := map[string]*adminGate{
		"empty":      newAdminGate(""),
		"whitespace": newAdminGate(" \n"),
		"nil":        nil,
	}
	queries := map[string]map[string][]string{
		"empty-key":  {"admin_key": {""}},
		"some-key":   {"admin_key": {testAdminKey}},
		"no-key-arg": {},
	}
	for gateName, gate := range gates {
		if gate.isConfigured() {
			t.Errorf("%s gate reports configured", gateName)
		}
		for queryName, query := range queries {
			t.Run(gateName+"/"+queryName, func(t *testing.T) {
				err := gate.authorize(
					ctxWithQuery(query), "register_game")
				if err == nil {
					t.Fatal("unset gate should refuse every call")
				}
				if got := errorCode(err); got != 9 {
					t.Errorf(
						"code = %d, want 9 (FAILED_PRECONDITION)", got)
				}
				if !strings.Contains(err.Error(), adminKeyEnvVar) {
					t.Errorf(
						"error should name %s: %q",
						adminKeyEnvVar, err.Error())
				}
			})
		}
	}
}

func TestAdminGateRejectsClientSession(t *testing.T) {
	// A player's session must not reach operator RPCs even with the
	// right admin key in the query string.
	gate := newAdminGate(testAdminKey)
	ctx := context.WithValue(
		ctxWithQuery(map[string][]string{"admin_key": {testAdminKey}}),
		runtime.RUNTIME_CTX_USER_ID, "some-user-id")
	err := gate.authorize(ctx, "register_game")
	if err == nil {
		t.Fatal("client session should be rejected")
	}
	if got := errorCode(err); got != 7 {
		t.Errorf("code = %d, want 7 (PERMISSION_DENIED)", got)
	}
}

// TestRegisterGameRequiresAdminKey drives the real RPC. The store
// has no DB, so a call that got past the gate and the payload
// validation would panic; the payloads below stop before that.
func TestRegisterGameRequiresAdminKey(t *testing.T) {
	// A payload that parses but fails validation. Accepted keys
	// reach validation and come back INVALID_ARGUMENT (3); refused
	// keys never get there.
	const payload = `{"schema_version":1,"game_id":"probe"}`

	t.Run("configured-missing-key", func(t *testing.T) {
		games := &perGameConfig{
			games: map[string]*GameConfig{},
			admin: newAdminGate(testAdminKey),
		}
		_, err := games.RegisterGameRpc(
			context.Background(), testLogger{}, nil, nil, payload)
		if got := errorCode(err); got != 16 {
			t.Fatalf("code = %d (%v), want 16", got, err)
		}
	})

	t.Run("configured-wrong-key", func(t *testing.T) {
		games := &perGameConfig{
			games: map[string]*GameConfig{},
			admin: newAdminGate(testAdminKey),
		}
		ctx := ctxWithQuery(map[string][]string{
			"admin_key": {"wrong"},
		})
		_, err := games.RegisterGameRpc(
			ctx, testLogger{}, nil, nil, payload)
		if got := errorCode(err); got != 16 {
			t.Fatalf("code = %d (%v), want 16", got, err)
		}
	})

	t.Run("configured-right-key-reaches-validation", func(t *testing.T) {
		games := &perGameConfig{
			games: map[string]*GameConfig{},
			admin: newAdminGate(testAdminKey),
		}
		ctx := ctxWithQuery(map[string][]string{
			"admin_key": {testAdminKey},
		})
		_, err := games.RegisterGameRpc(
			ctx, testLogger{}, nil, nil, payload)
		if got := errorCode(err); got != 3 {
			t.Fatalf("code = %d (%v), want 3 (validation)", got, err)
		}
		if !strings.Contains(err.Error(), "display_name") {
			t.Errorf("want the display_name validation error, got %v",
				err)
		}
	})

	t.Run("unset-env-refuses", func(t *testing.T) {
		games := &perGameConfig{
			games: map[string]*GameConfig{},
			admin: newAdminGate(""),
		}
		ctx := ctxWithQuery(map[string][]string{
			"admin_key": {testAdminKey},
		})
		_, err := games.RegisterGameRpc(
			ctx, testLogger{}, nil, nil, payload)
		if got := errorCode(err); got != 9 {
			t.Fatalf("code = %d (%v), want 9", got, err)
		}
	})

	t.Run("no-gate-refuses", func(t *testing.T) {
		games := &perGameConfig{games: map[string]*GameConfig{}}
		ctx := ctxWithQuery(map[string][]string{
			"admin_key": {testAdminKey},
		})
		_, err := games.RegisterGameRpc(
			ctx, testLogger{}, nil, nil, payload)
		if got := errorCode(err); got != 9 {
			t.Fatalf("code = %d (%v), want 9", got, err)
		}
	})
}

// TestBulkImportRequiresAdminKey drives the real RPC with a nil
// NakamaModule; payloads stop before any storage call.
func TestBulkImportRequiresAdminKey(t *testing.T) {
	const payload = `{"type":"no-such-type","records":[]}`
	rpc := bulkImportRpcFactory(newAdminGate(testAdminKey))

	if _, err := rpc(
		context.Background(), testLogger{}, nil, nil, payload,
	); errorCode(err) != 16 {
		t.Errorf("missing key: code = %d (%v), want 16",
			errorCode(err), err)
	}

	wrong := ctxWithQuery(map[string][]string{"admin_key": {"nope"}})
	if _, err := rpc(
		wrong, testLogger{}, nil, nil, payload,
	); errorCode(err) != 16 {
		t.Errorf("wrong key: code = %d (%v), want 16",
			errorCode(err), err)
	}

	right := ctxWithQuery(map[string][]string{
		"admin_key": {testAdminKey},
	})
	_, err := rpc(right, testLogger{}, nil, nil, payload)
	if errorCode(err) != 3 ||
		!strings.Contains(err.Error(), "unknown bulk_import type") {
		t.Errorf("right key should reach type dispatch, got %v", err)
	}

	unset := bulkImportRpcFactory(newAdminGate(""))
	if _, err := unset(
		right, testLogger{}, nil, nil, payload,
	); errorCode(err) != 9 {
		t.Errorf("unset env: code = %d (%v), want 9",
			errorCode(err), err)
	}
}

func TestRuntimeStatusReportsAdminKeyConfigured(t *testing.T) {
	for _, configured := range []bool{true, false} {
		registered := []string{}
		var games *perGameConfig
		status := statusRpcFactory(runtimeStatusConfig{
			AdminKeyConfigured: configured,
			RegisteredRpcs:     &registered,
			Games:              &games,
		})
		out, err := status(
			context.Background(), testLogger{}, nil, nil, "")
		if err != nil {
			t.Fatalf("runtime_status: %v", err)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			t.Fatalf("decode %q: %v", out, err)
		}
		got, ok := resp["admin_key_configured"].(bool)
		if !ok || got != configured {
			t.Errorf("admin_key_configured = %v (present=%t), want %t",
				resp["admin_key_configured"], ok, configured)
		}
	}
}
