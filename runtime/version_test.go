package main

import (
	"context"
	"encoding/json"
	"testing"
)

const hopnbopConfigJSON = `{
	"schema_version": 1,
	"game_id": "hopnbop",
	"display_name": "Hop 'n Bop",
	"edgegap_app_slug": "hopnbop-server",
	"edgegap_app_version": "v27",
	"protocol_version": 2,
	"display_version": "0.39.0",
	"legal": {"legal_version": "1.1"},
	"matchmaker_rules": {
		"min_players": 2,
		"max_players": 4,
		"query": "+properties.game_id:hopnbop",
		"modes": [
			{
				"id": "ffa",
				"display_name_key": "MODE.FFA_NAME",
				"description_key": "MODE.FFA_DESC",
				"min_players": 2,
				"max_players": 4,
				"query": "+properties.game_mode:ffa",
				"is_default": true
			},
			{
				"id": "duo",
				"display_name_key": "MODE.DUO_NAME",
				"description_key": "MODE.DUO_DESC",
				"min_players": 2,
				"max_players": 2,
				"query": "+properties.game_mode:duo"
			}
		]
	}
}`

func TestParseLegalVersionFromConfig(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		games := newTestGames(t,
			map[string]string{"hopnbop": hopnbopConfigJSON})
		gc, _ := games.Get("hopnbop")
		got := parseLegalVersionFromConfig(gc)
		if got != "1.1" {
			t.Fatalf("got %q, want %q", got, "1.1")
		}
	})
	t.Run("nil-config", func(t *testing.T) {
		if got := parseLegalVersionFromConfig(nil); got != "" {
			t.Fatalf("nil should return empty, got %q", got)
		}
	})
	t.Run("empty-raw", func(t *testing.T) {
		got := parseLegalVersionFromConfig(&GameConfig{})
		if got != "" {
			t.Fatalf("empty Raw should return empty, got %q", got)
		}
	})
	t.Run("missing-legal-block", func(t *testing.T) {
		gc := &GameConfig{
			Raw: json.RawMessage(`{"game_id":"x"}`),
		}
		if got := parseLegalVersionFromConfig(gc); got != "" {
			t.Fatalf("no legal block should return empty, got %q",
				got)
		}
	})
	t.Run("malformed-raw", func(t *testing.T) {
		gc := &GameConfig{Raw: json.RawMessage(`{not json`)}
		if got := parseLegalVersionFromConfig(gc); got != "" {
			t.Fatalf("malformed should return empty, got %q", got)
		}
	})
}

func TestParseMatchmakerRulesFromConfig(t *testing.T) {
	games := newTestGames(t,
		map[string]string{"hopnbop": hopnbopConfigJSON})
	gc, _ := games.Get("hopnbop")
	minP, maxP, query := parseMatchmakerRulesFromConfig(gc)
	if minP != 2 {
		t.Errorf("min_players = %d, want 2", minP)
	}
	if maxP != 4 {
		t.Errorf("max_players = %d, want 4", maxP)
	}
	if query != "+properties.game_id:hopnbop" {
		t.Errorf("query = %q, want %q", query,
			"+properties.game_id:hopnbop")
	}

	t.Run("nil", func(t *testing.T) {
		min2, max2, q2 := parseMatchmakerRulesFromConfig(nil)
		if min2 != 0 || max2 != 0 || q2 != "" {
			t.Fatalf("nil should return zero values, got"+
				" (%d, %d, %q)", min2, max2, q2)
		}
	})
	t.Run("missing-block", func(t *testing.T) {
		gc := &GameConfig{Raw: json.RawMessage(`{}`)}
		min2, max2, q2 := parseMatchmakerRulesFromConfig(gc)
		if min2 != 0 || max2 != 0 || q2 != "" {
			t.Fatalf("missing block should return zero values, "+
				"got (%d, %d, %q)", min2, max2, q2)
		}
	})
	t.Run("malformed", func(t *testing.T) {
		gc := &GameConfig{Raw: json.RawMessage(`{bad`)}
		min2, max2, q2 := parseMatchmakerRulesFromConfig(gc)
		if min2 != 0 || max2 != 0 || q2 != "" {
			t.Fatalf("malformed should return zero values, "+
				"got (%d, %d, %q)", min2, max2, q2)
		}
	})
}

func TestParseModesFromConfig(t *testing.T) {
	games := newTestGames(t,
		map[string]string{"hopnbop": hopnbopConfigJSON})
	gc, _ := games.Get("hopnbop")
	modes := parseModesFromConfig(gc)
	if len(modes) != 2 {
		t.Fatalf("got %d modes, want 2: %+v", len(modes), modes)
	}
	if modes[0].ID != "ffa" || !modes[0].IsDefault {
		t.Errorf("expected mode[0]={id:ffa, is_default:true}, "+
			"got %+v", modes[0])
	}
	if modes[1].ID != "duo" || modes[1].MaxPlayers != 2 {
		t.Errorf("expected mode[1]={id:duo, max_players:2}, "+
			"got %+v", modes[1])
	}

	t.Run("drop-empty-id", func(t *testing.T) {
		gc := &GameConfig{
			Raw: json.RawMessage(`{"matchmaker_rules":{"modes":[
				{"id":""},
				{"id":"valid"}
			]}}`),
		}
		modes := parseModesFromConfig(gc)
		if len(modes) != 1 || modes[0].ID != "valid" {
			t.Fatalf("expected one valid mode, got %+v", modes)
		}
	})
	t.Run("nil", func(t *testing.T) {
		if got := parseModesFromConfig(nil); got != nil {
			t.Fatalf("nil should return nil, got %+v", got)
		}
	})
	t.Run("no-modes", func(t *testing.T) {
		gc := &GameConfig{
			Raw: json.RawMessage(`{"matchmaker_rules":{}}`),
		}
		if got := parseModesFromConfig(gc); got != nil {
			t.Fatalf("missing modes should return nil, got %+v",
				got)
		}
	})
	t.Run("empty-list", func(t *testing.T) {
		gc := &GameConfig{
			Raw: json.RawMessage(
				`{"matchmaker_rules":{"modes":[]}}`),
		}
		if got := parseModesFromConfig(gc); got != nil {
			t.Fatalf("empty list should return nil, got %+v",
				got)
		}
	})
}

// TestVersionCheckRpcCompatibilityMatrix walks the client vs server
// protocol_version combinations and asserts the IsCompatible flag.
// Empty client version means "unknown / pre-protocol-version
// client"; we treat it as compatible to keep the rollout graceful.
func TestVersionCheckRpcCompatibilityMatrix(t *testing.T) {
	games := newTestGames(t,
		map[string]string{"hopnbop": hopnbopConfigJSON})
	rpc := versionCheckRpcFactory(
		versionConfig{
			GameVersion:     "fallback-1.0.0",
			ProtocolVersion: 99,
		}, games)
	cases := []struct {
		name           string
		clientProtocol int
		gameID         string
		wantCompatible bool
		wantProtocol   int
	}{
		{"matching-per-game", 2, "hopnbop", true, 2},
		{"mismatched-per-game", 3, "hopnbop", false, 2},
		{"zero-client-passes-through", 0, "hopnbop", true, 2},
		{"unknown-game-uses-env-fallback", 99, "ghost", true, 99},
		{"unknown-game-mismatch", 5, "ghost", false, 99},
		{"no-game-id-uses-env-fallback", 99, "", true, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, _ := json.Marshal(versionCheckArgs{
				ClientProtocolVersion: tc.clientProtocol,
				GameID:                tc.gameID,
			})
			out, err := rpc(
				context.Background(), nil, nil, nil, string(payload))
			if err != nil {
				t.Fatalf("rpc error: %v", err)
			}
			var resp versionCheckResponse
			if err := json.Unmarshal([]byte(out), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.IsCompatible != tc.wantCompatible {
				t.Errorf("is_compatible = %t, want %t (resp=%+v)",
					resp.IsCompatible, tc.wantCompatible, resp)
			}
			if resp.ProtocolVersion != tc.wantProtocol {
				t.Errorf("protocol_version = %d, want %d",
					resp.ProtocolVersion, tc.wantProtocol)
			}
		})
	}
}

// TestVersionCheckRpcSurfacesPerGameConfig confirms that legal /
// matchmaker / mode values flow through into the response when a
// known game_id is supplied. Guards against accidentally dropping
// the per-game projection during a future refactor.
func TestVersionCheckRpcSurfacesPerGameConfig(t *testing.T) {
	games := newTestGames(t,
		map[string]string{"hopnbop": hopnbopConfigJSON})
	rpc := versionCheckRpcFactory(
		versionConfig{}, games)
	payload, _ := json.Marshal(versionCheckArgs{GameID: "hopnbop"})
	out, err := rpc(
		context.Background(), nil, nil, nil, string(payload))
	if err != nil {
		t.Fatalf("rpc error: %v", err)
	}
	var resp versionCheckResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LegalVersion != "1.1" {
		t.Errorf("legal_version = %q, want 1.1", resp.LegalVersion)
	}
	if resp.MatchmakerMinPlayers != 2 ||
		resp.MatchmakerMaxPlayers != 4 ||
		resp.MatchmakerQuery != "+properties.game_id:hopnbop" {
		t.Errorf("matchmaker rules not surfaced: %+v", resp)
	}
	if len(resp.MatchmakerModes) != 2 {
		t.Errorf("expected 2 modes, got %d (%+v)",
			len(resp.MatchmakerModes), resp.MatchmakerModes)
	}
	if resp.GameVersion != "0.39.0" {
		t.Errorf("game_version = %q, want 0.39.0", resp.GameVersion)
	}
}

// TestVersionCheckRpcEmptyPayload — clients without a payload (the
// HTTP-key probe path uses this) still get a sensible response.
func TestVersionCheckRpcEmptyPayload(t *testing.T) {
	games := newTestGames(t, nil)
	rpc := versionCheckRpcFactory(
		versionConfig{
			GameVersion:     "0.39.0",
			ProtocolVersion: 2,
		}, games)
	out, err := rpc(context.Background(), nil, nil, nil, "")
	if err != nil {
		t.Fatalf("rpc error: %v", err)
	}
	var resp versionCheckResponse
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ProtocolVersion != 2 || resp.GameVersion != "0.39.0" {
		t.Errorf("got %+v, want protocol=2 version=0.39.0", resp)
	}
	if !resp.IsCompatible {
		t.Error("client_protocol_version=0 should be compatible")
	}
}
