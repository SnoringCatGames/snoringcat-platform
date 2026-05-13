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

// syntheticMatchCollection is the storage collection that flags
// probe-driven matches so match_end skips leaderboard writes and
// match_cancel accepts a client-initiated tear-down. Keyed by
// Edgegap request_id; row exists only while the match is live.
const syntheticMatchCollection = "synthetic_matches"

// matchMetadataCollection holds per-match runtime context the
// game server doesn't supply when it calls match_end (game_id,
// allocation timestamp). Written by fleet_allocator after the
// Edgegap deploy succeeds; read by match_end to scope the
// leaderboard write per game; deleted by match_end/match_cancel
// alongside server_registrations cleanup.
const matchMetadataCollection = "match_metadata"

// matchMetadata is the per-match metadata payload written at
// allocation time. Extend cautiously — every additional field
// turns into a load-bearing read in match_end.
type matchMetadata struct {
	GameID      string `json:"game_id"`
	AllocatedAt int64  `json:"allocated_at"`
}

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
	// games is the per-game config cache. Used by the matchmaker
	// hook to validate the game_id property each matched player's
	// client attached to its ticket, and to fall back gracefully
	// when entries pre-date the client update (no game_id prop).
	games *perGameConfig
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
	// mockDeploy short-circuits the real Edgegap allocate/poll/
	// stop dance for compliance tests so they don't burn paid
	// container-hours. When true, the matchmaker hook synthesizes
	// a canned deploy response (loopback IP, fixed ports), skips
	// the in-container `register_server` wait, and still emits
	// `match_ready` notifications so the addon-side
	// PlatformMatchmakingClient receives a well-formed payload.
	// Enabled via the EDGEGAP_MOCK_DEPLOY=true env var; the
	// runtime logs a loud warning at boot so prod never enables
	// it by accident.
	mockDeploy bool
}

// mockEdgegapRequestIDPrefix tags synthetic deploys so downstream
// tooling (cost monitor, audit-followups skill) can spot mock
// allocations in storage rows without parsing the matchmaker entry
// list. The prefix is also surfaced via the match_ready payload's
// `request_id` field so the addon-side test can sanity-check that
// it ran against a mock-mode runtime.
const mockEdgegapRequestIDPrefix = "mock-"

// mockPublicIP / mockUDPPort / mockTCPPort are the canned Edgegap
// status values mock mode hands back. Loopback so a misconfigured
// production runtime that flipped the env var on by accident would
// fail loudly when clients try to connect to 127.0.0.1.
const (
	mockPublicIP = "127.0.0.1"
	mockUDPPort  = 14433
	mockTCPPort  = 14434
)

// synthesizeMockDeploy returns a canned (deploy, status) pair the
// matchmaker hook uses in EDGEGAP_MOCK_DEPLOY mode. The request_id
// embeds the current unix nanos so two concurrent mock matches
// don't collide on the synthetic_matches / match_metadata storage
// rows. Ports map mirrors Dockerfile.edgegap's container ports
// (4433/UDP for the game, 4434/TCP for signaling) so the
// pickTCPPort path lights up the same code branch as the real
// Edgegap response.
func synthesizeMockDeploy(now time.Time) (
	*edgegapDeployResponse, *edgegapStatusResponse,
) {
	requestID := fmt.Sprintf(
		"%s%d", mockEdgegapRequestIDPrefix, now.UnixNano())
	deploy := &edgegapDeployResponse{
		RequestID: requestID,
		Message:   "mock deploy (EDGEGAP_MOCK_DEPLOY=true)",
	}
	status := &edgegapStatusResponse{
		RequestID:     requestID,
		CurrentStatus: "Status.READY",
		PublicIP:      mockPublicIP,
		Ports: map[string]edgegapPort{
			"game": {
				External: mockUDPPort,
				Internal: 4433,
				Protocol: "UDP",
			},
			"signaling": {
				External: mockTCPPort,
				Internal: 4434,
				Protocol: "TCP",
			},
		},
	}
	return deploy, status
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
	// game_id votes — clients of the same game pass the same
	// value as a string property. Stage 3.6 uses the dominant
	// game_id to scope the leaderboard write at match_end. The
	// matchmaker query stays `*` until Stage 3.8 lands per-game
	// query filters, so mixed-game matches are *possible* in
	// theory; the dominant-vote breaks ties without dropping
	// the match.
	gameIDVotes := map[string]int{}
	// Stage 3.9: per-user client_protocol_version tally for the
	// pre-allocate protocol check. Empty / missing entries are
	// graceful (pre-3.9 clients omit the property; they pass
	// through). The check fires only when at least one entry
	// declares a version AND the dominant game's registered
	// ProtocolVersion is known. Walked once after we resolve
	// matchGameID below.
	protocolByUser := map[string]int{}
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
			if gid, ok := props["game_id"].(string); ok && gid != "" {
				gameIDVotes[gid]++
			}
			if raw, ok := props["client_protocol_version"].(string); ok && raw != "" {
				if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
					protocolByUser[userID] = parsed
				}
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

	// Pick the dominant game_id from the matched players. Mixed-
	// vote matches use the highest-vote winner; pre-update clients
	// (no votes) leave matchGameID empty, in which case match_end
	// falls back to the legacy bare leaderboard ID.
	matchGameID := pickDominantGameID(gameIDVotes, a.games, logger)

	// Stage 3.9: pre-allocate protocol_version check. Each
	// post-3.9 client declares its compile-time protocol_version
	// as a ticket property; we compare against the registered
	// game's ProtocolVersion from the games cache and abort the
	// match before burning an Edgegap deploy if any declared
	// version mismatches. Pre-3.9 clients (no declared property)
	// pass through — the rollout is graceful and the boot-time
	// version_check on the client side is still the primary gate.
	// Skipped when we don't know which game's protocol to compare
	// against (no dominant game_id, or games cache empty in
	// bootstrap).
	if matchGameID != "" && a.games != nil && len(protocolByUser) > 0 {
		if gc, ok := a.games.Get(matchGameID); ok && gc.ProtocolVersion > 0 {
			expected := gc.ProtocolVersion
			mismatched := map[string]int{}
			for uid, got := range protocolByUser {
				if got != expected {
					mismatched[uid] = got
				}
			}
			if len(mismatched) > 0 {
				logger.Warn(
					"protocol_version mismatch in match: game=%s"+
						" expected=%d mismatched=%v;"+
						" aborting before edgegap allocation",
					matchGameID, expected, mismatched)
				abortProtocolMismatch(
					ctx, logger, nk, entries, expected, mismatched)
				return "", nil
			}
		}
	}

	// Stage 3.7: resolve the Edgegap app coordinates per match
	// from the games table when we know the match's game_id, so
	// the runtime no longer needs the EDGEGAP_APP_NAME /
	// EDGEGAP_APP_VERSION env vars bumped by hand after every
	// game-server deploy. Per-game values override; missing
	// fields fall back to the env-var defaults on the allocator
	// struct (a.appName / a.appVersion) so bootstrap deploys
	// still work before any game.yaml has been synced.
	appName := a.appName
	appVersion := a.appVersion
	if matchGameID != "" && a.games != nil {
		if gc, ok := a.games.Get(matchGameID); ok {
			if gc.EdgegapAppSlug != "" {
				appName = gc.EdgegapAppSlug
			}
			if gc.EdgegapAppVersion != "" {
				appVersion = gc.EdgegapAppVersion
			}
		}
	}

	// Synthetic-match detection. The synthetic-match-probe job
	// authenticates two probe identities and tags each ticket with
	// `probe:"true"`; pairing happens against the targeted query
	// `+properties.probe:true`, so a real player never lands in
	// one of these matches. We exclude the resulting match from
	// leaderboard/history writes and tag the Edgegap deploy so
	// downstream tooling (cost monitor, server-side analytics) can
	// distinguish probe traffic. Defense-in-depth: if any entry
	// lacks the marker, treat the match as real (better to over-
	// count than to silently exclude a real player).
	isProbeMatch := len(entries) > 0
	for _, e := range entries {
		marked := false
		if props := e.GetProperties(); props != nil {
			if v, ok := props["probe"].(string); ok && v == "true" {
				marked = true
			}
		}
		if !marked {
			isProbeMatch = false
			break
		}
	}

	deployReq := edgegapDeployRequest{
		AppName:     appName,
		VersionName: appVersion,
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
			{
				Key:   "IS_PROBE_MATCH",
				Value: strconv.FormatBool(isProbeMatch),
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
	// Allocate. Real mode hits Edgegap; mock mode synthesizes the
	// deploy + status response so compliance tests don't burn paid
	// container-hours. Both paths produce the same (deploy, status)
	// shape so every downstream step (storage writes, signaling
	// URL, match_ready fan-out) reads from one branch only.
	var (
		deploy *edgegapDeployResponse
		status *edgegapStatusResponse
	)
	if a.mockDeploy {
		deploy, status = synthesizeMockDeploy(time.Now())
		logger.Warn(
			"EDGEGAP_MOCK_DEPLOY=true: synthesized request_id=%s"+
				" (no real Edgegap allocation, deployReq=%+v)",
			deploy.RequestID, deployReq)
	} else {
		d, err := a.edgegap.Deploy(ctx, deployReq)
		if err != nil {
			logger.Error("edgegap deploy: %v", err)
			return "", err
		}
		logger.Info(
			"edgegap request_id=%s, polling for ready",
			d.RequestID)
		deploy = d
	}

	// Helper: terminate the deploy we just allocated when an
	// allocation-side error makes it impossible to ship a
	// usable match_ready. Without this, the deploy stays alive
	// until Edgegap's 24h max_duration cap. No-op in mock mode
	// (no real deploy to stop).
	stopOnErr := func(reason string) {
		if a.mockDeploy {
			return
		}
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

	// Persist the synthetic flag so match_end / match_cancel can
	// look it up by request_id. The Edgegap env var is also there,
	// but we don't want the cancel path to depend on the game
	// server's continued health to know whether to skip
	// leaderboard writes.
	if isProbeMatch {
		if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
			Collection:      syntheticMatchCollection,
			Key:             deploy.RequestID,
			Value:           `{"synthetic":true}`,
			PermissionRead:  2,
			PermissionWrite: 0,
		}}); err != nil {
			logger.Warn(
				"failed to mark match %s synthetic: %v",
				deploy.RequestID, err)
		}
	}

	// Persist per-match metadata (game_id, allocated_at) so
	// match_end can scope leaderboard writes per game without
	// trusting the game server to know its own game_id.
	metaBytes, _ := json.Marshal(matchMetadata{
		GameID:      matchGameID,
		AllocatedAt: time.Now().Unix(),
	})
	if _, err := nk.StorageWrite(ctx, []*runtime.StorageWrite{{
		Collection:      matchMetadataCollection,
		Key:             deploy.RequestID,
		Value:           string(metaBytes),
		PermissionRead:  2,
		PermissionWrite: 0,
	}}); err != nil {
		logger.Warn(
			"failed to write match metadata for %s: %v",
			deploy.RequestID, err)
	}

	// Poll Edgegap for READY (real mode only). Mock mode already
	// has a synthesized status with CurrentStatus="Status.READY"
	// and the canned ports map, so it skips this loop entirely.
	if !a.mockDeploy {
		pollCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
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
			stopOnErr("polling timeout")
			return "", fmt.Errorf("edgegap deployment %s did not become ready in 90s", deploy.RequestID)
		}
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
	//
	// Mock mode skips this — no real game server is going to
	// register, and a 30 s wait per mock match would defeat the
	// "fast test feedback" goal.
	if !a.mockDeploy {
		if err := waitForServerRegistered(
			ctx, logger, nk, deploy.RequestID); err != nil {
			stopOnErr("server didn't register: " + err.Error())
			return "", err
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
			"ports":          status.Ports,
			"request_id":     status.RequestID,
			"session_ids":    mp.SessionIDs,
			"transport_type": transportType,
			"signaling_url":  signalingURL,
		}
		if a.mockDeploy {
			// Surface mock mode in the payload so compliance
			// tests can sanity-check that they ran against a
			// mock-enabled runtime (and so a misconfigured prod
			// flipping the env on by accident is loudly visible
			// on the client side too).
			connInfo["mock"] = true
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

// abortProtocolMismatch sends a per-player `match_failed`
// notification to every matched user and is called instead of
// allocating an Edgegap deploy when at least one entry's declared
// `client_protocol_version` differs from the game's registered
// ProtocolVersion. Mismatched users get the actionable "your
// version is stale" payload; compatible users get a generic
// abort-shaped payload so they don't sit on a 120s client
// timeout. Best-effort — a failed NotificationSend logs but does
// not abort the abort.
//
// Notification subject `match_failed` is parsed by the addon-side
// `PlatformMatchmakingClient`, which emits `matchmaking_failed`
// with the human-readable message. The game's failure classifier
// surfaces it as a fatal failure (toast + back to lobby).
func abortProtocolMismatch(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	entries []runtime.MatchmakerEntry,
	expected int,
	mismatched map[string]int,
) {
	subject := "match_failed"
	for _, e := range entries {
		userID := e.GetPresence().GetUserId()
		gotProto, isMismatched := mismatched[userID]
		content := map[string]any{
			"reason":   "protocol_mismatch",
			"expected": expected,
		}
		if isMismatched {
			content["got"] = gotProto
			content["message"] = fmt.Sprintf(
				"Your client is out of date (protocol %d,"+
					" server expects %d). Please restart"+
					" the game to update.",
				gotProto, expected)
		} else {
			content["message"] = fmt.Sprintf(
				"Match aborted: another player's client is"+
					" out of date (server expects protocol %d).",
				expected)
		}
		if err := nk.NotificationSend(
			ctx, userID, subject, content, 100, "", true,
		); err != nil {
			logger.Warn(
				"notify match_failed to %s: %v", userID, err)
		}
	}
}

// pickDominantGameID returns the most-voted game_id from
// matchmaker entries. Ties resolve deterministically
// (alphabetical) so a recurring 1-1 tie picks the same winner
// across re-allocations. Unregistered game_ids are dropped — a
// client that lies about its game_id can't write to a game we
// don't recognize. Returns "" when no votes are present or all
// votes are unknown (pre-update clients pre-Stage-3.6).
func pickDominantGameID(
	votes map[string]int,
	games *perGameConfig,
	logger runtime.Logger,
) string {
	if len(votes) == 0 || games == nil {
		return ""
	}
	bestID := ""
	bestCount := 0
	for gid, count := range votes {
		if _, ok := games.Get(gid); !ok {
			logger.Warn(
				"matchmaker entry vote for unknown game_id %q"+
					" dropped",
				gid)
			continue
		}
		if count > bestCount ||
			(count == bestCount && (bestID == "" || gid < bestID)) {
			bestID = gid
			bestCount = count
		}
	}
	if len(votes) > 1 && bestID != "" {
		logger.Info(
			"matchmaker matched mixed-game_id players;"+
				" using dominant=%s votes=%v",
			bestID, votes)
	}
	return bestID
}
