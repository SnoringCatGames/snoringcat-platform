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
	// serverDNSBase is the apex used to derive the per-deploy
	// hostname `s-<ip-with-dashes>.<serverDNSBase>` we send to
	// matched clients. We POST a Cloudflare A record for that
	// name pointing at the deploy's PublicIP from this hook;
	// the wildcard cert (`*.<serverDNSBase>`) covers any
	// subdomain. Configurable via SERVER_DNS_BASE; defaults
	// to `game.hopnbop.net` when unset.
	//
	// Deprecated. The signalingDomain + HMAC-token flow below
	// replaces the per-deploy DNS approach. Kept temporarily
	// so older client builds without signaling_url support
	// continue to work during rollout.
	serverDNSBase string
	// Cloudflare credentials for per-deploy A record
	// pre-warming. Same deprecation note as serverDNSBase.
	cloudflareDNSToken  string
	cloudflareDNSZoneID string
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
	// env. If either is unset or empty, the hook skips
	// signaling_url emission and clients fall back to the
	// legacy per-deploy DNS path. Set via SIGNALING_HMAC_SECRET
	// runtime env.
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
		},
	}
	// Note: WebRTC signaling sits behind nginx in the container
	// (nginx terminates wss:// on 4434/TCP and proxies to Godot
	// on 4433/TCP; native ws:// is pass-throughed). So we don't
	// override SIGNALING_PORT here — Godot's default (= server
	// port = 4433/TCP) is what nginx forwards to. The cert env
	// vars (TLS_FULLCHAIN / TLS_PRIVKEY) live on the Edgegap
	// app version, not per-deploy.
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

	// Compute the deterministic public hostname from the
	// allocated IP and pre-warm a Cloudflare A record for it.
	//
	// We cannot use Edgegap's status.Fqdn (a `*.pr.edgegap.net`
	// host) for WSS because the wildcard TLS cert covers
	// `*.<dnsBaseDomain>` and the cert chain doesn't match the
	// Edgegap host — browsers reject the handshake.
	//
	// The pre-warm runs here, in the runtime hook, rather than
	// in the container's entrypoint.sh. Two reasons:
	//   1. We have the PublicIP and CLOUDFLARE_DNS_* creds in
	//      Nakama env, so the round-trip Hetzner -> CF is one
	//      step. Doing the same in the container relies on
	//      Edgegap injecting CF creds into the deploy, which
	//      didn't work reliably.
	//   2. Nakama logs are easy to read; Edgegap container
	//      stdout is not exposed via API.
	//
	// On match-end we do not auto-delete; the dns-watchdog
	// systemd timer cleans up s-* records older than its
	// MAX_RECORD_AGE_HOURS threshold (default 4h).
	serverFqdn := status.Fqdn
	if status.PublicIP != "" {
		dnsBase := a.serverDNSBase
		if dnsBase == "" {
			dnsBase = "game.hopnbop.net"
		}
		ipDashed := strings.ReplaceAll(status.PublicIP, ".", "-")
		serverFqdn = fmt.Sprintf("s-%s.%s", ipDashed, dnsBase)

		if a.cloudflareDNSToken != "" && a.cloudflareDNSZoneID != "" {
			if err := a.preWarmDNS(ctx, logger, serverFqdn,
				status.PublicIP, status.RequestID); err != nil {
				logger.Warn("DNS pre-warm failed for %s -> %s: %v",
					serverFqdn, status.PublicIP, err)
				// Continue anyway — clients will fail their
				// WSS handshake but ENet-only pairs survive.
			}
		} else {
			logger.Warn("DNS pre-warm skipped: CLOUDFLARE_DNS_TOKEN" +
				" or CLOUDFLARE_DNS_ZONE_ID not set in runtime env")
		}
	}

	// Build the signaling URL once per allocation. Picks the
	// TCP port (Edgegap's "signaling" entry in status.Ports →
	// container 4434/TCP). When SIGNALING_DOMAIN /
	// SIGNALING_HMAC_SECRET aren't configured, signalingURL
	// stays empty and the client falls back to the legacy
	// per-deploy server_fqdn:port path.
	signalingURL := ""
	if a.signalingDomain != "" && len(a.signalingHmacSecret) > 0 &&
		status.PublicIP != "" {
		tcpPort := pickTCPPort(status.Ports)
		if tcpPort > 0 {
			signalingURL = signSignalingURL(
				a.signalingDomain, a.signalingHmacSecret,
				status.PublicIP, tcpPort, time.Now())
		} else {
			logger.Warn("no TCP port in status.Ports; "+
				"signaling_url omitted (status=%+v)",
				status.Ports)
		}
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
			"server_fqdn":    serverFqdn,
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

// preWarmDNS POSTs a Cloudflare A record for hostname -> publicIP
// with a 60s TTL and a `comment` carrying the deploy ID and a
// `created=<iso>` timestamp the dns-watchdog systemd timer
// (infra/remote/dns-watchdog/) uses to detect stale records.
//
// The matchmaker hook calls this synchronously before sending
// match_ready notifications so clients get a hostname that
// already resolves by the time their browser does the DNS
// lookup. CF DNS propagation typically completes in well under
// the time it takes a client to receive the notification, open
// a socket, and do a fresh resolve.
//
// We do NOT delete the record on match-end here — Edgegap's
// own deploy-cleanup is asynchronous, so a same-IP deploy
// allocated immediately after this match would race against
// our delete. The dns-watchdog timer cleans up records older
// than its MAX_RECORD_AGE_HOURS window (default 4h), which
// is comfortably longer than any plausible match.
func (a *fleetAllocator) preWarmDNS(
	ctx context.Context,
	logger runtime.Logger,
	hostname, publicIP, requestID string,
) error {
	createdISO := time.Now().UTC().Format(time.RFC3339)
	comment := fmt.Sprintf(
		"edgegap deploy=%s created=%s", requestID, createdISO)
	body, err := json.Marshal(map[string]any{
		"type":    "A",
		"name":    hostname,
		"content": publicIP,
		"ttl":     60,
		"proxied": false,
		"comment": comment,
	})
	if err != nil {
		return fmt.Errorf("marshal CF body: %w", err)
	}
	url := fmt.Sprintf(
		"https://api.cloudflare.com/client/v4/zones/%s/dns_records",
		a.cloudflareDNSZoneID)
	req, err := http.NewRequestWithContext(ctx, "POST", url,
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.cloudflareDNSToken)

	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("CF POST: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf(
			"CF POST returned %d: %s",
			resp.StatusCode, string(respBody))
	}
	logger.Info("DNS pre-warm: %s -> %s (deploy=%s)",
		hostname, publicIP, requestID)
	return nil
}
