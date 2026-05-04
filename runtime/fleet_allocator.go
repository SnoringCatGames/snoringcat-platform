package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// edgegapClient wraps the Edgegap REST API.
type edgegapClient struct {
	token string
	http  *http.Client
}

func (c *edgegapClient) httpClient() *http.Client {
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	return c.http
}

type edgegapDeployRequest struct {
	AppName     string             `json:"app_name"`
	VersionName string             `json:"version_name"`
	IPList      []string           `json:"ip_list,omitempty"`
	EnvVars     []edgegapEnvKV     `json:"env_vars,omitempty"`
	IsPublicApp bool               `json:"is_public_app,omitempty"`
	Geographies []string           `json:"geographies,omitempty"`
	Filters     []map[string]any   `json:"filters,omitempty"`
}

type edgegapEnvKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type edgegapDeployResponse struct {
	RequestID string `json:"request_id"`
	Message   string `json:"message"`
	// More fields depending on Edgegap API version.
}

type edgegapStatusResponse struct {
	RequestID     string                 `json:"request_id"`
	CurrentStatus string                 `json:"current_status"`
	PublicIP      string                 `json:"public_ip"`
	Ports         map[string]edgegapPort `json:"ports"`
	Fqdn          string                 `json:"fqdn"`
}

type edgegapPort struct {
	External int    `json:"external"`
	Internal int    `json:"internal"`
	Protocol string `json:"protocol"`
}

func (c *edgegapClient) Deploy(ctx context.Context, req edgegapDeployRequest) (*edgegapDeployResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		"https://api.edgegap.com/v1/deploy", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "token "+c.token)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("edgegap deploy failed (%d): %s", resp.StatusCode, string(respBody))
	}
	out := &edgegapDeployResponse{}
	if err := json.Unmarshal(respBody, out); err != nil {
		return nil, fmt.Errorf("decode deploy response: %w (body=%s)", err, string(respBody))
	}
	return out, nil
}

func (c *edgegapClient) Status(ctx context.Context, requestID string) (*edgegapStatusResponse, error) {
	url := fmt.Sprintf("https://api.edgegap.com/v1/status/%s", requestID)
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "token "+c.token)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("edgegap status failed (%d): %s", resp.StatusCode, string(respBody))
	}
	out := &edgegapStatusResponse{}
	if err := json.Unmarshal(respBody, out); err != nil {
		return nil, fmt.Errorf("decode status response: %w (body=%s)", err, string(respBody))
	}
	return out, nil
}

// matchedPlayer pairs a Nakama user with the session IDs the
// runtime issued for that user's local players. For 1:1 matches
// (no couch co-op) SessionIDs has length 1.
type matchedPlayer struct {
	UserID     string
	SessionIDs []string
}

// fleetAllocator hooks into MatchmakerMatched to spin up an Edgegap
// deployment for the matched players.
type fleetAllocator struct {
	edgegap    *edgegapClient
	appName    string
	appVersion string
}

// OnMatchmakerMatched is the Nakama matchmaker hook. Returning a
// non-empty match_id starts a multiplayer match. We use a custom
// flow: we don't return a Nakama match ID; instead we allocate an
// Edgegap deployment, then push a notification with connection
// info to the matched players.
func (a *fleetAllocator) OnMatchmakerMatched(
	ctx context.Context,
	logger runtime.Logger,
	db *sql.DB,
	nk runtime.NakamaModule,
	entries []runtime.MatchmakerEntry,
) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	logger.Info("matchmaker matched %d players, allocating Edgegap deployment", len(entries))

	// Collect player IPs from storage (the client calls
	// `record_client_ip` right before joining the matchmaker —
	// see client_ip.go). Edgegap requires at least one of
	// {ip_list, geo_ip_list, location, filters}, so we fall
	// back to a single fixed geography if no fresh IPs are
	// available (which would only happen if every matched
	// player skipped the pre-matchmaking RPC).
	//
	// While walking entries, also generate per-player session
	// IDs and tally the total expected player count. Each
	// matchmaker entry corresponds to one Nakama user
	// (presence); the optional `player_count` string property
	// indicates couch co-op slots. The IDs flow two ways:
	// (1) via Edgegap deploy EnvVars to the game server, where
	// EdgegapServerProvider validates incoming connections
	// against this allowlist; (2) per-player in the match_ready
	// notification, so each client knows which IDs to declare
	// when handshaking with the server.
	ipList := make([]string, 0, len(entries))
	matchedPlayers := make([]matchedPlayer, 0, len(entries))
	allSessionIDs := make([]string, 0, len(entries))
	totalPlayerCount := 0
	// Transport selection: any web player in the match → WebRTC
	// (UDP-like DataChannels). All-native → ENet. The runtime
	// also passes the choice into the game-server via Edgegap
	// EnvVars so the server starts the right peer.
	transportType := "enet"
	for _, e := range entries {
		userID := e.GetPresence().GetUserId()

		localCount := 1
		if props := e.GetProperties(); props != nil {
			if raw, ok := props["player_count"].(string); ok {
				if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
					localCount = parsed
				}
			}
			if p, ok := props["platform"].(string); ok && p == "web" {
				transportType = "webrtc"
			}
		}
		sessionIDs := make([]string, 0, localCount)
		for i := 0; i < localCount; i++ {
			sid := fmt.Sprintf("%s_%d", userID, i)
			sessionIDs = append(sessionIDs, sid)
			allSessionIDs = append(allSessionIDs, sid)
		}
		matchedPlayers = append(matchedPlayers, matchedPlayer{
			UserID:     userID,
			SessionIDs: sessionIDs,
		})
		totalPlayerCount += localCount

		ip, err := readClientIP(ctx, nk, userID)
		if err != nil {
			logger.Warn("readClientIP for %s: %v", userID, err)
			continue
		}
		if ip != "" {
			ipList = append(ipList, ip)
		} else {
			logger.Warn("no recorded client IP for %s; matchmaker fallback will use geography", userID)
		}
	}

	deployReq := edgegapDeployRequest{
		AppName:     a.appName,
		VersionName: a.appVersion,
		IPList:      ipList,
		EnvVars: []edgegapEnvKV{
			{
				Key:   "EXPECTED_PLAYER_COUNT",
				Value: strconv.Itoa(totalPlayerCount),
			},
			{
				Key:   "EXPECTED_SESSION_IDS",
				Value: strings.Join(allSessionIDs, ","),
			},
			{
				Key:   "TRANSPORT_TYPE",
				Value: transportType,
			},
		},
	}
	if len(ipList) == 0 {
		// `north_america` is a published Edgegap continent tag.
		// This keeps the deploy from 400-ing on missing region
		// hints when nobody recorded an IP, at the cost of
		// potentially-suboptimal placement. If you want a
		// different default, change this list.
		deployReq.Geographies = []string{"north_america"}
	}
	deploy, err := a.edgegap.Deploy(ctx, deployReq)
	if err != nil {
		logger.Error("edgegap deploy: %v", err)
		return "", err
	}
	logger.Info("edgegap request_id=%s, polling for ready", deploy.RequestID)

	// Poll for READY.
	pollCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var status *edgegapStatusResponse
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		s, err := a.edgegap.Status(pollCtx, deploy.RequestID)
		if err != nil {
			logger.Warn("edgegap status check: %v", err)
		} else if s.CurrentStatus == "Status.READY" || s.CurrentStatus == "Ready" {
			status = s
			break
		}
		time.Sleep(2 * time.Second)
	}
	if status == nil {
		return "", fmt.Errorf("edgegap deployment %s did not become ready in 90s", deploy.RequestID)
	}

	// Notify each matched player with connection info. Each
	// player gets only their own session_ids — the server
	// holds the full allowlist via EXPECTED_SESSION_IDS env
	// var. The client uses these IDs in the rollback_netcode
	// player declaration; the server validates them against
	// the allowlist.
	subject := "match_ready"
	for _, mp := range matchedPlayers {
		connInfo := map[string]any{
			"server_ip":      status.PublicIP,
			"server_fqdn":    status.Fqdn,
			"ports":          status.Ports,
			"request_id":     status.RequestID,
			"session_ids":    mp.SessionIDs,
			"transport_type": transportType,
		}
		connInfoJSON, _ := json.Marshal(connInfo)
		if err := nk.NotificationSend(ctx, mp.UserID, subject, map[string]any{
			"connection": string(connInfoJSON),
		}, 100, "", true); err != nil {
			logger.Warn("notify %s: %v", mp.UserID, err)
		}
	}

	// Return empty match ID — the actual realtime match runs on
	// the Edgegap-allocated game server, not inside Nakama.
	return "", nil
}
