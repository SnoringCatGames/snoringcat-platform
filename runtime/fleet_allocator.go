package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// signalingTokenLifetime caps how long a match_ready signaling
// URL is valid. Generous enough to cover slow notification
// delivery + a couple of client-side retries; short enough that
// a leaked URL is useless within a few minutes.
const signalingTokenLifetime = 5 * time.Minute

// signSignalingURL builds a "wss://<domain>/connect/<token>"
// URL where the token is base64url("ip:port:exp:hex-hmac")
// signed with the shared HMAC-SHA256 secret. Must stay in sync
// with infra/remote/signaling-proxy/main.go's decodeAndVerify.
func signSignalingURL(
	domain string,
	secret []byte,
	ip string,
	port int,
	now time.Time,
) string {
	exp := now.Add(signalingTokenLifetime).Unix()
	payload := fmt.Sprintf("%s:%d:%d", ip, port, exp)
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(payload))
	tokenStr := payload + ":" + hex.EncodeToString(h.Sum(nil))
	token := base64.RawURLEncoding.EncodeToString([]byte(tokenStr))
	return fmt.Sprintf("wss://%s/connect/%s", domain, token)
}

// serverRegistrationPollTimeout caps how long the matchmaker
// hook waits for the in-container Godot to call register_server
// after Edgegap reports the container READY. The actual delay
// is dominated by Godot's startup time inside the container —
// typically 1-2s, occasionally more under load. We give it 30s
// before giving up; longer than that means the container is
// genuinely stuck.
const serverRegistrationPollTimeout = 30 * time.Second

// serverRegistrationPollInterval is how often we re-check the
// server_registrations storage row. The first poll happens
// immediately, so a fast server boot is matched in <1 interval.
const serverRegistrationPollInterval = 250 * time.Millisecond

// waitForServerRegistered blocks until the in-container Godot
// posts to the register_server RPC (which writes to the
// server_registrations storage collection keyed by request_id),
// or until serverRegistrationPollTimeout elapses. The storage
// row's existence is a positive readiness signal: the server
// only writes it after server_enable_connections returns, which
// in turn only returns after the WS port has been bound.
//
// Without this gate, match_ready notifications race the server's
// startup and clients see connection-refused on the first few
// retries. The client-side retry-backoff papers over the gap
// but doing it server-side is cleaner — clients connect on the
// first try.
func waitForServerRegistered(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	requestID string,
) error {
	start := time.Now()
	deadline := start.Add(serverRegistrationPollTimeout)
	ticker := time.NewTicker(serverRegistrationPollInterval)
	defer ticker.Stop()
	for {
		rows, err := nk.StorageRead(ctx, []*runtime.StorageRead{{
			Collection: "server_registrations",
			Key:        requestID,
		}})
		if err == nil && len(rows) > 0 {
			logger.Info(
				"server registered after %s: request_id=%s",
				time.Since(start).Round(time.Millisecond),
				requestID)
			return nil
		}
		if err != nil {
			// Treat as transient — Postgres reads can blip.
			logger.Warn("storage read for %s: %v", requestID, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"server didn't register within %s (request=%s)",
				serverRegistrationPollTimeout, requestID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// pickTCPPort scans an Edgegap deploy's port map and returns
// the host-side external port for the first TCP entry, or 0 if
// none. The game-server declares 4433/UDP (game) and 4434/TCP
// (signaling) — we want the TCP one for WebSocket signaling.
func pickTCPPort(ports map[string]edgegapPort) int {
	for _, p := range ports {
		if strings.EqualFold(p.Protocol, "TCP") && p.External > 0 {
			return p.External
		}
	}
	return 0
}

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

// Stop terminates an Edgegap deployment. Without this, deployments
// linger until Edgegap's app-version-level `max_duration` cap (24h
// by default) fires, racking up container-hours we don't need.
// Returns nil on 404 — Edgegap will return that for an already-
// terminated deployment, which we treat as "already done."
func (c *edgegapClient) Stop(ctx context.Context, requestID string) error {
	url := fmt.Sprintf("https://api.edgegap.com/v1/stop/%s", requestID)
	httpReq, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "token "+c.token)
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("edgegap stop failed (%d): %s",
			resp.StatusCode, string(respBody))
	}
	return nil
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
	// signalingDomain is the stable FQDN that fronts the
	// per-deploy WebSocket signaling, e.g.
	// "signaling.snoringcat.games". The runtime hook signs an
	// HMAC token over (deploy IP, port, expiry) using
	// signalingHmacSecret and ships
	// "wss://<signalingDomain>/connect/<token>" to clients in
	// the match_ready payload. Caddy + signaling-proxy on the
	// platform host validate the token and bridge to the
	// upstream game-server. See infra/remote/signaling-proxy/
	// for the proxy. Set via SIGNALING_DOMAIN runtime env.
	signalingDomain string
	// signalingHmacSecret is the shared HMAC-SHA256 secret used
	// to sign signaling_url tokens. Must match the value the
	// signaling-proxy reads from its own SIGNALING_HMAC_SECRET
	// env. Both must be set; the runtime fails fast at boot
	// otherwise.
	signalingHmacSecret []byte
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
	// Collect each player's platform string for transport
	// selection. selectTransportType applies the rule
	// (any web → webrtc, otherwise enet); pulled out so the
	// compliance suite can probe it via the transport_select
	// RPC.
	platforms := make([]string, 0, len(entries))
	for _, e := range entries {
		userID := e.GetPresence().GetUserId()

		localCount := 1
		platform := ""
		if props := e.GetProperties(); props != nil {
			if raw, ok := props["player_count"].(string); ok {
				if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
					localCount = parsed
				}
			}
			if p, ok := props["platform"].(string); ok {
				platform = p
			}
		}
		platforms = append(platforms, platform)
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

	transportType := selectTransportType(platforms)

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
			{
				// Tell the in-container Godot to bind its
				// signaling WebSocket to 4434/TCP (matches
				// the declared container port). With the
				// nginx layer gone, Godot is the only
				// listener on that port.
				Key:   "SIGNALING_PORT",
				Value: "4434",
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
	// Helper: terminate the deploy we just allocated when an
	// allocation-side error makes it impossible to ship a
	// usable match_ready. Without this, the deploy stays alive
	// until Edgegap's 24h max_duration cap.
	stopOnErr := func(reason string) {
		if stopErr := a.edgegap.Stop(ctx, deploy.RequestID); stopErr != nil {
			logger.Warn(
				"failed to stop orphaned deploy %s after %s: %v",
				deploy.RequestID, reason, stopErr)
		} else {
			logger.Info(
				"stopped orphaned deploy %s (%s)",
				deploy.RequestID, reason)
		}
	}

	if status == nil {
		stopOnErr("polling timeout")
		return "", fmt.Errorf("edgegap deployment %s did not become ready in 90s", deploy.RequestID)
	}

	// Build the signed signaling URL clients connect to.
	// Picks the TCP host port (Edgegap's "signaling" entry
	// in status.Ports → container 4434/TCP). Caddy +
	// signaling-proxy on the platform host validate the
	// HMAC token and bridge to that (ip, tcp_port).
	if status.PublicIP == "" {
		stopOnErr("missing PublicIP")
		return "", fmt.Errorf(
			"edgegap status missing PublicIP (request=%s)",
			deploy.RequestID)
	}
	tcpPort := pickTCPPort(status.Ports)
	if tcpPort == 0 {
		stopOnErr("missing TCP port")
		return "", fmt.Errorf(
			"edgegap status has no TCP port (request=%s, ports=%+v)",
			deploy.RequestID, status.Ports)
	}
	signalingURL := signSignalingURL(
		a.signalingDomain, a.signalingHmacSecret,
		status.PublicIP, tcpPort, time.Now())

	// Wait for the in-container Godot to call register_server.
	// Edgegap reports CurrentStatus=READY when the container
	// process starts; the Godot signaling WS doesn't bind for
	// another second or so, and clients that arrive in that
	// window get connection-refused on every retry. The server
	// posts to register_server immediately after binding, so
	// the storage row's existence is the readiness signal.
	if err := waitForServerRegistered(
		ctx, logger, nk, deploy.RequestID); err != nil {
		stopOnErr("server didn't register: " + err.Error())
		return "", err
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
			"ports":          status.Ports,
			"request_id":     status.RequestID,
			"session_ids":    mp.SessionIDs,
			"transport_type": transportType,
			"signaling_url":  signalingURL,
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
