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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	// AllocatorKind is one of allocatorModeEdgegap /
	// allocatorModeLocal. Persisted so MatchEndRpc dispatches
	// Stop to the right backend without re-running the hybrid
	// geo decision. Empty means "edgegap" (pre-multi-backend
	// matches and the bootstrap path both omit the field).
	AllocatorKind string `json:"allocator_kind,omitempty"`
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
	// SignalingTarget overrides (PublicIP, pickTCPPort(Ports)) for
	// the signaling-URL signing path only. Format "host:port".
	// Edgegap leaves it empty (signaling-proxy hits the deploy via
	// its public IP+host port over the internet). The local allocator
	// sets it to "<container_name>:4434" so the signaling-proxy
	// reaches the in-network container via Docker DNS instead of
	// hairpinning out through Caddy / the host's public IP.
	SignalingTarget string `json:"signaling_target,omitempty"`
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

// inflightAllocation tracks one in-progress Edgegap allocation so a
// matched user can cancel it mid-flight via the
// cancel_matchmaking_allocation RPC. The same struct is shared by
// every user in the match — any one user's cancel propagates to
// all of them (the matched-as-a-group semantic is enforced by the
// matchmaker; if one player bails, the whole match aborts because
// the others are no longer paired against a valid pool).
//
// The struct only holds the cancel func: the OnMatchmakerMatched
// goroutine reads `allocCtx.Err()` at known checkpoints to decide
// between "normal success", "user cancelled", and "all retries
// failed" paths. There's no separate "was this cancelled?" bool
// because ctx.Err() is the source of truth — a parent-ctx
// cancellation and a user-cancel get the same teardown semantics
// (stopDeploy + match_failed fan-out).
type inflightAllocation struct {
	cancel context.CancelFunc
}

// fleetAllocator hooks into MatchmakerMatched to spin up a
// game-server for the matched players. Originally Edgegap-only;
// the local + hybrid backends were added when Edgegap's per-mCPU-
// minute pricing turned out to dominate the platform bill.
type fleetAllocator struct {
	edgegap    *edgegapClient
	// local is the LocalDockerAllocator instance. Nil when no
	// game's allocator_mode is "local" or "hybrid" — keeping
	// the field nil-safe means a bare Edgegap deploy doesn't
	// require docker-socket access just to register the hook.
	local      *LocalDockerAllocator
	// geo is the GeoIP lookup used by hybridAllocatorChoice.
	// Nil when the sidecar URL is "off" — hybrid then falls back
	// entirely to the static CIDR map.
	geo geoIPLookup
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
	// inflightByUserID tracks the active in-progress allocation
	// for each currently-matched user_id. The
	// cancel_matchmaking_allocation RPC looks up the caller's
	// entry and invokes the cancel func to tear down the
	// in-flight allocation (including a best-effort Edgegap Stop
	// on any deploy that already started). Cleaned up on
	// OnMatchmakerMatched exit via defer. Stage 7.2 of
	// MULTI_GAME_ROADMAP.md.
	inflightByUserID sync.Map // map[string]*inflightAllocation
}

// registerInflight associates each matched user_id with the shared
// in-flight allocation handle. Called once at the start of
// OnMatchmakerMatched. A subsequent cancel RPC from any of these
// users will find the same handle and invoke cancel.
func (a *fleetAllocator) registerInflight(
	userIDs []string,
	inflight *inflightAllocation,
) {
	for _, uid := range userIDs {
		if uid == "" {
			continue
		}
		a.inflightByUserID.Store(uid, inflight)
	}
}

// deregisterInflight removes the user_id → inflight mapping for
// each matched user, but only if their current entry still points
// at this inflight. The CompareAndDelete guard is defensive: if a
// later match for the same user_id has already registered a
// different inflight (e.g. the user re-queued before this hook
// returned, somehow), we don't blow away the newer entry.
func (a *fleetAllocator) deregisterInflight(
	userIDs []string,
	inflight *inflightAllocation,
) {
	for _, uid := range userIDs {
		if uid == "" {
			continue
		}
		a.inflightByUserID.CompareAndDelete(uid, inflight)
	}
}

// cancelInflightForUser is the cancel RPC's primary side-effect:
// look up the caller's in-flight allocation and invoke the cancel
// func, which propagates ctx cancellation to OnMatchmakerMatched
// (currently mid-allocation). Returns true when a cancel was
// triggered; false when no in-flight allocation exists for the
// caller (either they're not currently being matched, or the
// allocation already completed past the point of no return).
//
// Idempotent: a second cancel for the same user is a no-op (the
// cancel func is itself idempotent, and the entry is still in the
// map until OnMatchmakerMatched's defer fires). The actual
// teardown (stopDeploy + match_failed fan-out) happens in the
// matchmaker hook goroutine when it observes ctx.Err() — this RPC
// only signals.
func (a *fleetAllocator) cancelInflightForUser(userID string) bool {
	v, ok := a.inflightByUserID.Load(userID)
	if !ok {
		return false
	}
	inflight, ok := v.(*inflightAllocation)
	if !ok || inflight == nil || inflight.cancel == nil {
		return false
	}
	inflight.cancel()
	return true
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

// maxAllocationAttempts caps how many times the matchmaker hook
// will retry an Edgegap allocation that fails to produce a usable
// (deploy, status) pair. 3 = one initial attempt + two retries.
// Beyond that we send `match_failed` to every matched player so
// the client surfaces a clean error instead of sitting on its
// 120 s timeout.
const maxAllocationAttempts = 3

// baseAllocationBackoff is the delay before the first retry. Each
// subsequent retry doubles the previous delay up to
// maxAllocationBackoff. Total worst-case wait across retries
// (excluding the per-attempt 90 s polling budget) is bounded.
const baseAllocationBackoff = 1 * time.Second

// maxAllocationBackoff caps the exponential backoff so deep retry
// chains don't blow past the client's 120 s matchmaker timeout.
const maxAllocationBackoff = 8 * time.Second

// allocationGeographyRotation is the continent fallback list used
// on retry attempts. The first attempt uses the caller-built
// `deployReq` (which honours per-player IP geo-routing when at
// least one client IP was recorded). Retries drop the IP list and
// pin a single continent so Edgegap routes to a region with
// capacity. The rotation order is alpha by region size to bias
// retries toward the busiest regions first.
var allocationGeographyRotation = []string{
	"north_america",
	"europe",
	"asia",
}

// allocationFallbackGeographies returns the Geographies value to
// apply at the given retry attempt index. Index 0 returns nil —
// the caller's existing deployReq stands for the first attempt.
// Index 1+ returns a single-continent slice rotating through
// allocationGeographyRotation. Wraps past the end so call sites
// don't have to bounds-check, even though maxAllocationAttempts
// caps the actual call count.
func allocationFallbackGeographies(attemptIndex int) []string {
	if attemptIndex < 1 {
		return nil
	}
	geo := allocationGeographyRotation[(attemptIndex-1)%len(
		allocationGeographyRotation)]
	return []string{geo}
}

// allocationBackoff returns the sleep duration before the given
// retry attempt index. Index 0 returns 0 (no delay before the
// initial attempt). Index 1 returns baseAllocationBackoff,
// doubling each subsequent index up to maxAllocationBackoff.
func allocationBackoff(attemptIndex int) time.Duration {
	if attemptIndex < 1 {
		return 0
	}
	d := baseAllocationBackoff << (attemptIndex - 1)
	if d <= 0 || d > maxAllocationBackoff {
		d = maxAllocationBackoff
	}
	return d
}

// sleepOrCtxDone blocks for `d` or until ctx is cancelled. Returns
// ctx.Err() on cancellation, nil otherwise. Used by the allocation
// retry loop so a matchmaker context that's cancelled mid-backoff
// doesn't waste cycles before bailing.
func sleepOrCtxDone(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

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
	// Mark the entry point so the post-allocation timer captures
	// the full Edgegap cold-start latency (retry loop + status
	// polling + register-server wait, all of it). Surfaces via
	// the Nakama Prometheus endpoint as `snoringcat_alloc_seconds`;
	// see Stage 7.11.
	matchStart := time.Now()
	logger.Info("matchmaker matched %d players, allocating Edgegap deployment", len(entries))

	// Extract matched user_ids up-front so the inflight tracker
	// can register them before any I/O kicks off. The same list
	// drives the cancel-cleanup notification fan-out below if a
	// matched user invokes cancel_matchmaking_allocation mid-flight.
	matchedUserIDs := make([]string, 0, len(entries))
	for _, e := range entries {
		if uid := e.GetPresence().GetUserId(); uid != "" {
			matchedUserIDs = append(matchedUserIDs, uid)
		}
	}

	// Derive a cancelable child context so the
	// cancel_matchmaking_allocation RPC can abort the allocation
	// by invoking the inflight's cancel func. The child wraps the
	// parent ctx so a parent cancellation (e.g. Nakama shutdown)
	// still propagates, but a user-initiated cancel doesn't kill
	// the parent — we still need a non-cancelled context for the
	// post-cancel teardown notifications.
	allocCtx, allocCancel := context.WithCancel(ctx)
	defer allocCancel()
	inflight := &inflightAllocation{cancel: allocCancel}
	a.registerInflight(matchedUserIDs, inflight)
	defer a.deregisterInflight(matchedUserIDs, inflight)

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

	// Stage 7.4: blocked-pair check. Walk each matched user's
	// BANNED list; if any pair has blocked the other, abort the
	// match before burning an Edgegap deploy. Skipped for 1-player
	// matches (synthetic probes, debug allocations) since blocking
	// is intrinsically a multi-player concern.
	//
	// One FriendsList round trip per matched user. For a 4-player
	// match that's 4 reads, each capped at blockListPageCap pages.
	// Cheap relative to the Edgegap allocation we're protecting
	// (which takes seconds).
	if len(matchedUserIDs) >= 2 {
		blockedBy := map[string]map[string]struct{}{}
		for _, uid := range matchedUserIDs {
			set, err := blockedUserIDSet(ctx, nk, uid)
			if err != nil {
				logger.Warn(
					"blocked-pair check FriendsList failed for"+
						" user=%s: %v; allowing match through",
					uid, err)
				continue
			}
			blockedBy[uid] = set
		}
		blockedPairs := findBlockedPairs(matchedUserIDs, blockedBy)
		if len(blockedPairs) > 0 {
			logger.Warn(
				"blocked pair in match: users=%v;"+
					" aborting before edgegap allocation",
				blockedPairs)
			abortBlockedPair(
				ctx, logger, nk, entries, blockedPairs)
			return "", nil
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
	// Resolve the allocator-mode for this match. Per-game; the
	// default ("") and explicit "edgegap" both go to the existing
	// Edgegap retry loop. "local" goes straight to LocalDocker
	// without retries (a single host has no region to swap into;
	// there's no productive retry semantic). "hybrid" picks per-
	// match based on the matched players' IPs (NA = local,
	// non-NA = edgegap fallback).
	allocatorMode := ""
	var resolvedGameConfig *GameConfig
	if matchGameID != "" && a.games != nil {
		if gc, ok := a.games.Get(matchGameID); ok {
			resolvedGameConfig = gc
			allocatorMode = gc.AllocatorMode
		}
	}
	if allocatorMode == allocatorModeHybrid {
		if hybridAllocatorChoice(logger, a.geo, ipList) {
			allocatorMode = allocatorModeLocal
		} else {
			allocatorMode = allocatorModeEdgegap
		}
	}
	// Allocate. Real mode hits the selected backend; mock mode
	// synthesizes the deploy + status response so compliance
	// tests don't burn paid container-hours. Both paths produce
	// the same (deploy, status) shape so every downstream step
	// (storage writes, signaling URL, match_ready fan-out) reads
	// from one branch only.
	var (
		deploy *edgegapDeployResponse
		status *edgegapStatusResponse
	)
	// allocatorKind is the value match_metadata persists so
	// MatchEndRpc dispatches Stop to the right backend. Captures
	// the post-hybrid-resolution decision (i.e. concrete "local"
	// or "edgegap", never "hybrid").
	allocatorKind := allocatorModeEdgegap
	if a.mockDeploy {
		deploy, status = synthesizeMockDeploy(time.Now())
		logger.Warn(
			"EDGEGAP_MOCK_DEPLOY=true: synthesized request_id=%s"+
				" (no real Edgegap allocation, deployReq=%+v)",
			deploy.RequestID, deployReq)
	} else if allocatorMode == allocatorModeLocal {
		if a.local == nil {
			sendAllocationFailed(
				ctx, logger, nk, entries, 1,
				fmt.Errorf(
					"game %q requested allocator_mode=local but"+
						" runtime has no LocalDockerAllocator"+
						" configured (LOCAL_PUBLIC_IP unset?)",
					matchGameID))
			return "", fmt.Errorf(
				"local allocator not configured for game %s",
				matchGameID)
		}
		d, s, err := a.local.Allocate(
			allocCtx, logger, nk, resolvedGameConfig, deployReq)
		if err != nil {
			if allocCtx.Err() != nil {
				logger.Info(
					"local allocation aborted by user cancel" +
						" (no deploy created)")
				sendMatchCancelled(ctx, logger, nk, entries)
				return "", nil
			}
			sendAllocationFailed(
				ctx, logger, nk, entries, 1, err)
			return "", fmt.Errorf(
				"local allocation failed: %w", err)
		}
		deploy = d
		status = s
		allocatorKind = allocatorModeLocal
		logger.Info(
			"local allocation succeeded: request_id=%s game=%s",
			d.RequestID, matchGameID)
	} else {
		// Retry loop with exponential backoff + region fallback.
		// Each attempt covers Deploy + status poll + register
		// wait; a failure at any stage tears down that attempt's
		// deploy (best-effort Stop) before the next attempt runs.
		// On retry, the IP list is dropped (geo-routing already
		// failed by definition) and the Geographies field rotates
		// through allocationGeographyRotation so a saturated
		// region doesn't trap every attempt.
		//
		// Uses allocCtx (not ctx) so a user-initiated cancel via
		// cancel_matchmaking_allocation propagates through
		// tryAllocate's polling + register wait, and through the
		// inter-attempt backoff sleep.
		var lastErr error
		for attempt := 0; attempt < maxAllocationAttempts; attempt++ {
			if attempt > 0 {
				backoff := allocationBackoff(attempt)
				fallbackGeo := allocationFallbackGeographies(attempt)
				logger.Info(
					"edgegap allocation retry %d/%d after %s"+
						" (geo=%v, prev err: %v)",
					attempt+1, maxAllocationAttempts,
					backoff, fallbackGeo, lastErr)
				if err := sleepOrCtxDone(allocCtx, backoff); err != nil {
					lastErr = err
					break
				}
				deployReq.IPList = nil
				deployReq.Geographies = fallbackGeo
			}
			d, s, err := a.tryAllocate(allocCtx, logger, nk, deployReq)
			if err == nil {
				deploy = d
				status = s
				logger.Info(
					"edgegap allocation succeeded on attempt %d/%d:"+
						" request_id=%s",
					attempt+1, maxAllocationAttempts, d.RequestID)
				break
			}
			lastErr = err
			logger.Warn(
				"edgegap allocation attempt %d/%d failed: %v",
				attempt+1, maxAllocationAttempts, err)
			// Stop retrying if the allocation context was
			// cancelled (user cancel, parent shutdown). Continuing
			// would just produce more cancelled-context errors.
			if allocCtx.Err() != nil {
				break
			}
		}
		if deploy == nil {
			// Distinguish user-initiated cancellation from
			// genuine allocation failure. Cancellation gets the
			// match_failed reason=cancelled fan-out so other
			// matched players see a recoverable "peer cancelled"
			// prompt instead of "allocation failed". Use the
			// parent ctx for notifications (allocCtx is cancelled
			// in this branch by definition).
			if allocCtx.Err() != nil {
				logger.Info(
					"edgegap allocation aborted by user cancel" +
						" (no deploy created)")
				sendMatchCancelled(ctx, logger, nk, entries)
				return "", nil
			}
			sendAllocationFailed(
				ctx, logger, nk, entries,
				maxAllocationAttempts, lastErr)
			return "", fmt.Errorf(
				"edgegap allocation failed after %d attempts: %w",
				maxAllocationAttempts, lastErr)
		}
	}

	// Check whether a cancel arrived between the successful
	// allocation and now. If so, tear down the freshly-allocated
	// deploy + fan out match_failed reason=cancelled and bail
	// before writing storage rows or sending match_ready. Mock
	// mode skips the deploy-stop branch (synthesizeMockDeploy
	// didn't allocate anything real).
	if allocCtx.Err() != nil {
		logger.Info(
			"edgegap allocation aborted by user cancel after"+
				" successful deploy (request_id=%s); tearing down",
			deploy.RequestID)
		a.stopDeploy(
			ctx, logger, deploy.RequestID,
			"user cancelled post-allocation")
		sendMatchCancelled(ctx, logger, nk, entries)
		return "", nil
	}

	// Record allocation cold-start latency for Prometheus
	// scraping (Stage 7.11). Tags isolate game_id and mock-mode
	// so a malformed mock run doesn't pollute real-mode
	// dashboards. Recorded after the post-allocation cancel
	// checkpoint so cancelled allocations don't skew the
	// success-path histogram.
	nk.MetricsTimerRecord(
		"snoringcat_alloc_seconds",
		map[string]string{
			"game_id": matchGameID,
			"mock":    strconv.FormatBool(a.mockDeploy),
		},
		time.Since(matchStart))
	// Active-match gauge + per-backend allocation counter.
	// Mock-mode allocations skip metrics so compliance test
	// runs don't pollute prod dashboards.
	if !a.mockDeploy {
		recordAllocationStart(nk, allocatorKind)
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

	// Persist per-match metadata (game_id, allocated_at,
	// allocator_kind) so match_end can scope leaderboard writes
	// per game without trusting the game server to know its own
	// game_id, and so MatchEndRpc dispatches Stop to the backend
	// that allocated this match (edgegap vs local).
	metaBytes, _ := json.Marshal(matchMetadata{
		GameID:        matchGameID,
		AllocatedAt:   time.Now().Unix(),
		AllocatorKind: allocatorKind,
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

	// Build the signed signaling URL clients connect to.
	// Picks the TCP host port (Edgegap's "signaling" entry
	// in status.Ports → container 4434/TCP). Caddy +
	// signaling-proxy on the platform host validate the
	// HMAC token and bridge to that (ip, tcp_port). The
	// PublicIP / TCP port validity was already enforced by
	// tryAllocate (real mode) or synthesizeMockDeploy (mock
	// mode), so the values are guaranteed non-empty here.
	//
	// SignalingTarget overrides this for backends (like the local
	// docker allocator) where the signaling-proxy can reach the
	// game-server over an internal docker network instead of
	// hairpinning out through the host's public IP.
	signalingHost := status.PublicIP
	signalingPort := pickTCPPort(status.Ports)
	if status.SignalingTarget != "" {
		if h, p, err := net.SplitHostPort(status.SignalingTarget); err == nil {
			if pn, perr := strconv.Atoi(p); perr == nil && pn > 0 {
				signalingHost = h
				signalingPort = pn
			} else {
				logger.Warn(
					"ignoring SignalingTarget %q: unparseable port",
					status.SignalingTarget)
			}
		} else {
			logger.Warn(
				"ignoring SignalingTarget %q: %v",
				status.SignalingTarget, err)
		}
	}
	signalingURL := signSignalingURL(
		a.signalingDomain, a.signalingHmacSecret,
		signalingHost, signalingPort, time.Now())

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

// tryAllocate runs one full Edgegap allocation attempt: Deploy →
// poll Status until READY → validate the resolved IP and TCP port
// → wait for the in-container Godot to register itself. Returns
// the resolved (deploy, status) pair on success. On any failure
// after a successful Deploy, attempts a best-effort Stop on the
// allocated deploy so retries don't leak Edgegap container-hours.
// Callers wrap this in the retry loop in OnMatchmakerMatched.
func (a *fleetAllocator) tryAllocate(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	deployReq edgegapDeployRequest,
) (*edgegapDeployResponse, *edgegapStatusResponse, error) {
	deploy, err := a.edgegap.Deploy(ctx, deployReq)
	if err != nil {
		return nil, nil, fmt.Errorf("edgegap deploy: %w", err)
	}
	logger.Info(
		"edgegap request_id=%s, polling for ready",
		deploy.RequestID)

	// Poll Edgegap for READY. Container start latency is normally
	// 1-2 s but can spike under regional load.
	pollCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	deadline := time.Now().Add(90 * time.Second)
	var status *edgegapStatusResponse
	for time.Now().Before(deadline) {
		s, sErr := a.edgegap.Status(pollCtx, deploy.RequestID)
		if sErr != nil {
			logger.Warn(
				"edgegap status check (request=%s): %v",
				deploy.RequestID, sErr)
		} else if s.CurrentStatus == "Status.READY" ||
			s.CurrentStatus == "Ready" {
			status = s
			break
		}
		if err := sleepOrCtxDone(pollCtx, 2*time.Second); err != nil {
			break
		}
	}
	if status == nil {
		a.stopDeploy(ctx, logger, deploy.RequestID, "polling timeout")
		return nil, nil, fmt.Errorf(
			"edgegap deployment %s did not become ready in 90s",
			deploy.RequestID)
	}

	// Validate the resolved status carries what downstream code
	// needs. A "successful" deploy with missing IP/port is unusable;
	// stop it and surface the failure so the retry loop can swap
	// regions before trying again.
	if status.PublicIP == "" {
		a.stopDeploy(
			ctx, logger, deploy.RequestID, "missing PublicIP")
		return nil, nil, fmt.Errorf(
			"edgegap status missing PublicIP (request=%s)",
			deploy.RequestID)
	}
	if pickTCPPort(status.Ports) == 0 {
		a.stopDeploy(
			ctx, logger, deploy.RequestID, "missing TCP port")
		return nil, nil, fmt.Errorf(
			"edgegap status has no TCP port (request=%s, ports=%+v)",
			deploy.RequestID, status.Ports)
	}

	// Wait for the in-container Godot to call register_server.
	// Edgegap reports CurrentStatus=READY when the container
	// process starts; the Godot signaling WS doesn't bind for
	// another second or so, and clients that arrive in that
	// window get connection-refused on every retry. The server
	// posts to register_server immediately after binding, so
	// the storage row's existence is the readiness signal.
	if err := waitForServerRegistered(
		ctx, logger, nk, deploy.RequestID); err != nil {
		a.stopDeploy(
			ctx, logger, deploy.RequestID,
			"server didn't register: "+err.Error())
		return nil, nil, fmt.Errorf(
			"wait for server registered: %w", err)
	}

	return deploy, status, nil
}

// stopDeploy terminates a deploy that allocated but didn't reach a
// usable state. Best-effort: a Stop failure is logged but doesn't
// escalate — Edgegap's 24h max_duration cap is the safety net.
// No-op in mock mode (no real deploy exists to stop).
func (a *fleetAllocator) stopDeploy(
	ctx context.Context,
	logger runtime.Logger,
	requestID string,
	reason string,
) {
	if a.mockDeploy {
		return
	}
	if err := a.edgegap.Stop(ctx, requestID); err != nil {
		logger.Warn(
			"failed to stop orphaned deploy %s after %s: %v",
			requestID, reason, err)
	} else {
		logger.Info(
			"stopped orphaned deploy %s (%s)", requestID, reason)
	}
}

// sendMatchCancelled sends a `match_failed` notification to every
// matched player after a user-initiated cancel aborted the
// allocation (Stage 7.2 of MULTI_GAME_ROADMAP.md). The reason tag
// is `cancelled` so the game-side
// `_classify_matchmaking_failure` classifier routes it to
// `LOADING.PEER_CANCELLED` (recoverable + retry button on the
// loading screen).
//
// The cancelling user receives this notification too — their
// client side has already cleared `_is_searching` in
// `cancel_matchmaking()`, so their addon-side
// `_handle_match_failed` no-ops on receipt. The notification
// matters for the OTHER matched players, who learn that the match
// was aborted by a peer and can re-queue.
//
// Distinct from `sendAllocationFailed`: that path's "we tried 3
// times, every attempt failed" is a recoverable platform failure
// the user might want to retry against; this path's "another
// player bailed" is intrinsically a peer-level event.
func sendMatchCancelled(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	entries []runtime.MatchmakerEntry,
) {
	subject := "match_failed"
	content := map[string]any{
		"reason":  "cancelled",
		"message": "Match cancelled by another player.",
	}
	for _, e := range entries {
		userID := e.GetPresence().GetUserId()
		if err := nk.NotificationSend(
			ctx, userID, subject, content, 100, "", true,
		); err != nil {
			logger.Warn(
				"notify match_failed (cancelled) to %s: %v",
				userID, err)
		}
	}
}

// cancelAllocationRpcFactory returns the client-session RPC matched
// players use to abort an in-flight Edgegap allocation mid-poll.
// Stage 7.2 of MULTI_GAME_ROADMAP.md.
//
// Lookup is keyed by the caller's user_id (extracted from
// RUNTIME_CTX_USER_ID via requireClientSession). game_id scoping
// is enforced via requireGameID — a misconfigured client without
// game_id can't fire this RPC.
//
// Returns {ok: true, cancelled: bool}. `cancelled=false` means no
// in-flight allocation existed for the caller (the cancel arrived
// before OnMatchmakerMatched fired, or after the deploy completed
// past the point of no return). Treated as a silent success so the
// client doesn't need a "was this too late?" UI branch.
//
// The teardown (stopDeploy + match_failed fan-out) happens in the
// matchmaker hook goroutine when it observes allocCtx.Err() — this
// RPC only signals.
func cancelAllocationRpcFactory(
	alloc *fleetAllocator,
	games *perGameConfig,
) func(
	context.Context, runtime.Logger, *sql.DB,
	runtime.NakamaModule, string,
) (string, error) {
	return func(
		ctx context.Context,
		logger runtime.Logger,
		_ *sql.DB,
		_ runtime.NakamaModule,
		_ string,
	) (string, error) {
		userID, err := requireClientSession(ctx)
		if err != nil {
			return "", err
		}
		if _, err := requireGameID(ctx, games); err != nil {
			return "", err
		}
		cancelled := false
		if alloc != nil {
			cancelled = alloc.cancelInflightForUser(userID)
		}
		if cancelled {
			logger.Info(
				"cancel_matchmaking_allocation: cancelled in-flight"+
					" allocation for user=%s",
				userID)
		}
		resp, _ := json.Marshal(map[string]any{
			"ok":        true,
			"cancelled": cancelled,
		})
		return string(resp), nil
	}
}

// sendAllocationFailed sends a `match_failed` notification to every
// matched player after the retry loop has exhausted all attempts.
// The `message` field includes the "allocation" substring so the
// game-side `_classify_matchmaking_failure` classifier routes it
// to LOADING.ALLOCATION_FAILED — recoverable + retry button on the
// loading screen, not toast-and-bounce.
//
// Distinct from abortProtocolMismatch: that helper tailors per-
// player copy by whether that player's client was the mismatched
// one; allocation failure is uniform — every matched player saw
// the same failure mode (no usable Edgegap deploy emerged).
func sendAllocationFailed(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	entries []runtime.MatchmakerEntry,
	attempts int,
	lastErr error,
) {
	subject := "match_failed"
	content := map[string]any{
		"reason":   "allocation_failed",
		"attempts": attempts,
		"message": fmt.Sprintf(
			"Edgegap allocation failed after %d attempts."+
				" Please try again in a moment.",
			attempts),
	}
	if lastErr != nil {
		content["last_error"] = lastErr.Error()
	}
	for _, e := range entries {
		userID := e.GetPresence().GetUserId()
		if err := nk.NotificationSend(
			ctx, userID, subject, content, 100, "", true,
		); err != nil {
			logger.Warn(
				"notify match_failed (allocation) to %s: %v",
				userID, err)
		}
	}
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

// findBlockedPairs scans the per-user BANNED sets for any
// directed (A → B) relationship where both A and B appear in
// matchedUserIDs. Symmetric: returns each pair once with the
// lower user_id first so the order is stable across re-runs.
// Empty result means no blocked pair was found.
//
// `blockedBy[X]` is the set of user_ids X has blocked. If A is in
// blockedBy[B], B has blocked A. We treat either direction as
// "should not match" — a blocked relationship cuts both ways
// regardless of who initiated.
func findBlockedPairs(
	matchedUserIDs []string,
	blockedBy map[string]map[string]struct{},
) [][2]string {
	seen := map[[2]string]bool{}
	pairs := [][2]string{}
	for _, a := range matchedUserIDs {
		bannedByA := blockedBy[a]
		if len(bannedByA) == 0 {
			continue
		}
		for _, b := range matchedUserIDs {
			if a == b {
				continue
			}
			if _, ok := bannedByA[b]; !ok {
				continue
			}
			pair := [2]string{a, b}
			if pair[0] > pair[1] {
				pair[0], pair[1] = pair[1], pair[0]
			}
			if seen[pair] {
				continue
			}
			seen[pair] = true
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

// abortBlockedPair sends `match_failed reason=blocked_pair` to
// every matched user. Per-player tailoring: users named in any
// blocked pair get the "you and another player have blocked
// each other" framing; bystanders see the generic "match aborted
// because two players have a block relationship" copy. Both
// classify as recoverable on the game side (LOADING.BLOCKED_PAIR)
// — the retry button re-queues and the matchmaker should pair
// them with different players this time.
//
// Mirror of abortProtocolMismatch: same notification subject,
// same notification persistence flag, same best-effort error
// handling. The game-side `_classify_matchmaking_failure`
// matches `"blocked"` substring to route to the right
// LOADING.* code path.
func abortBlockedPair(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	entries []runtime.MatchmakerEntry,
	pairs [][2]string,
) {
	involved := map[string]bool{}
	for _, p := range pairs {
		involved[p[0]] = true
		involved[p[1]] = true
	}
	subject := "match_failed"
	for _, e := range entries {
		userID := e.GetPresence().GetUserId()
		content := map[string]any{
			"reason": "blocked_pair",
		}
		if involved[userID] {
			content["message"] = "Match cancelled: you and" +
				" another matched player have blocked each" +
				" other."
		} else {
			content["message"] = "Match cancelled: two" +
				" matched players have blocked each other."
		}
		if err := nk.NotificationSend(
			ctx, userID, subject, content, 100, "", true,
		); err != nil {
			logger.Warn(
				"notify match_failed (blocked_pair) to %s: %v",
				userID, err)
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
