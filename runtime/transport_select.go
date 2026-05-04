package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// selectTransportType is the runtime's matchmaker transport-
// selection rule, factored out so the compliance suite can
// probe it without standing up a real match. Pure function:
// a list of player platform strings goes in, the chosen
// transport_type comes out.
//
// Rule: any "web" present → "webrtc". Otherwise → "enet".
// Empty / unknown platform strings count as native.
func selectTransportType(platforms []string) string {
	for _, p := range platforms {
		if p == "web" {
			return "webrtc"
		}
	}
	return "enet"
}

type transportSelectArgs struct {
	Platforms []string `json:"platforms"`
}

type transportSelectResponse struct {
	TransportType string `json:"transport_type"`
}

// transportSelectRpc exposes selectTransportType for compliance
// tests. Server-to-server only (HTTP key) — there is no client-
// side use case. Catches regressions where a future refactor
// changes the selection rule (e.g. accidentally degrading
// cross-play to ENet, or flipping the default).
func transportSelectRpc(
	ctx context.Context,
	_ runtime.Logger,
	_ *sql.DB,
	_ runtime.NakamaModule,
	payload string,
) (string, error) {
	args := transportSelectArgs{}
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &args); err != nil {
			return "", runtime.NewError(
				"invalid payload: "+err.Error(), 3)
		}
	}
	resp := transportSelectResponse{
		TransportType: selectTransportType(args.Platforms),
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
