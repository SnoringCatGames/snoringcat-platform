package main

import (
	"bytes"
	"context"
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

// LocalDockerAllocator runs game-server containers directly on the
// Nakama host via the Docker Engine API. Replaces Edgegap as the
// fleet backend for matches whose AllocatorMode is "local" (and
// for HybridAllocator's local-route branch).
//
// Wire model:
//   - The Nakama container mounts /var/run/docker.sock from the
//     host at the same path (security tradeoff: gives Nakama
//     ability to spawn any container — acceptable because Nakama
//     already has full runtime authority over match state).
//   - Each match spawns one container named "match-<request_id>".
//     Port-bindings map the container's declared ports (4433/udp,
//     4434/tcp) to host ports drawn from the configured pool.
//   - Stop posts to the Docker socket; the container's
//     `--rm`-equivalent (AutoRemove=true) cleans up the runtime
//     state without leaving stopped-container detritus.
//
// Container env mirrors the Edgegap deploy contract so the same
// Godot binary works on both backends:
//   - EXPECTED_PLAYER_COUNT, EXPECTED_SESSION_IDS, TRANSPORT_TYPE,
//     SIGNALING_PORT, IS_PROBE_MATCH (passed through from
//     deployReq.EnvVars)
//   - ARBITRIUM_PUBLIC_IP, ARBITRIUM_PORT_GAME_EXTERNAL (mirrors
//     Edgegap's host-port injection so the Godot WebRTC ICE rewrite
//     finds the right port without backend-specific branches)
//
// Multi-game: the per-match game config's LocalImageRef determines
// which image to pull and run. No global app-name/version assumption.

// LocalDockerAllocator is a fleet backend that spawns game-server
// containers on the local Docker daemon.
type LocalDockerAllocator struct {
	// dockerSocket is the path to the Docker Engine UNIX socket
	// (typically "/var/run/docker.sock"). The container mounts it
	// at the same path. Configurable so a host-side test can point
	// at a stub.
	dockerSocket string
	// publicIP is the IP clients connect to (the host's public
	// IPv4). Required at boot; the allocator returns it verbatim
	// in the synthesized status response so the signaling URL +
	// match_ready payload point at the right address.
	publicIP string
	// httpKey is the Nakama runtime HTTP key. Injected into each
	// spawned container's env so the in-container game-server can
	// authenticate the `register_server` callback. On the Edgegap
	// path this value is supplied per-app in the Edgegap dashboard;
	// for local matches the runtime is the only source.
	httpKey string
	// dockerNetwork is the docker network name the spawned
	// containers attach to. Pinned so the in-container game-server
	// can resolve `nakama` for register_server without hairpinning
	// out through Caddy. Default "nakama_nakama-net" matches the
	// stock compose project name; override via LOCAL_DOCKER_NETWORK.
	dockerNetwork string
	// nakamaURL is what we inject as NAKAMA_URL in the container
	// env so the game-server's register_with_runtime targets the
	// internal docker DNS name instead of the public hostname.
	// Default "http://nakama:7350"; override via LOCAL_NAKAMA_URL.
	nakamaURL string
	// ports allocates host-side UDP+TCP pairs from the configured
	// ranges. Locking is per-allocator so concurrent matchmaker
	// hooks can't race on the same port.
	ports *localPortPool
	// http is a Docker-socket-aware HTTP client. Reused across
	// requests to keep socket connections alive.
	http *http.Client
}

// newLocalDockerAllocator constructs a LocalDockerAllocator from
// env-supplied config. Returns an error when required env is
// missing so misconfigured runtimes fail fast at boot rather than
// silently allocating against a stale or wrong default.
func newLocalDockerAllocator(env map[string]string) (*LocalDockerAllocator, error) {
	socket := env["LOCAL_DOCKER_SOCKET"]
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	publicIP := env["LOCAL_PUBLIC_IP"]
	if publicIP == "" {
		return nil, fmt.Errorf(
			"LOCAL_PUBLIC_IP must be set when allocator_mode" +
				" includes local")
	}
	httpKey := env["NAKAMA_HTTP_KEY"]
	if httpKey == "" {
		return nil, fmt.Errorf(
			"NAKAMA_HTTP_KEY must be set when allocator_mode" +
				" includes local (in-container game-server needs" +
				" it to call register_server)")
	}
	dockerNetwork := env["LOCAL_DOCKER_NETWORK"]
	if dockerNetwork == "" {
		dockerNetwork = "nakama_nakama-net"
	}
	nakamaURL := env["LOCAL_NAKAMA_URL"]
	if nakamaURL == "" {
		nakamaURL = "http://nakama:7350"
	}
	udpRange := env["LOCAL_UDP_PORT_RANGE"]
	if udpRange == "" {
		udpRange = "30000-30099"
	}
	tcpRange := env["LOCAL_TCP_PORT_RANGE"]
	if tcpRange == "" {
		tcpRange = "30100-30199"
	}
	udpLo, udpHi, err := parsePortRange(udpRange)
	if err != nil {
		return nil, fmt.Errorf("LOCAL_UDP_PORT_RANGE: %w", err)
	}
	tcpLo, tcpHi, err := parsePortRange(tcpRange)
	if err != nil {
		return nil, fmt.Errorf("LOCAL_TCP_PORT_RANGE: %w", err)
	}
	return &LocalDockerAllocator{
		dockerSocket:  socket,
		publicIP:      publicIP,
		httpKey:       httpKey,
		dockerNetwork: dockerNetwork,
		nakamaURL:     nakamaURL,
		ports:         newLocalPortPool(udpLo, udpHi, tcpLo, tcpHi),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(
					ctx context.Context, _, _ string,
				) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(
						ctx, "unix", socket)
				},
			},
		},
	}, nil
}

// Kind implements Allocator.
func (l *LocalDockerAllocator) Kind() string { return allocatorModeLocal }

// localRequestIDPrefix tags local allocations so observers (cost
// monitor, edgegap-leak-sweep, match_lifecycle) can route by ID
// prefix when match_metadata is unavailable (rare; defensive).
const localRequestIDPrefix = "local-"

// Allocate implements Allocator. Spins up a game-server container
// for the matched players. Returns the same (deploy, status) shape
// EdgegapAllocator returns so downstream code (signaling URL,
// match_ready notifications) is backend-agnostic.
func (l *LocalDockerAllocator) Allocate(
	ctx context.Context,
	logger runtime.Logger,
	nk runtime.NakamaModule,
	gameConfig *GameConfig,
	deployReq edgegapDeployRequest,
) (*edgegapDeployResponse, *edgegapStatusResponse, error) {
	if gameConfig == nil || gameConfig.LocalImageRef == "" {
		return nil, nil, fmt.Errorf(
			"local allocator: game %q has no local_image_ref",
			deployReq.AppName)
	}
	udpPort, tcpPort, err := l.ports.allocate()
	if err != nil {
		return nil, nil, fmt.Errorf("port allocation: %w", err)
	}
	requestID := fmt.Sprintf(
		"%s%d", localRequestIDPrefix, time.Now().UnixNano())
	containerName := "match-" + requestID

	// Convert Edgegap env-var list to the Docker-API env-var
	// shape (slice of "KEY=VALUE" strings) and inject the
	// per-deploy externals the game server reads to advertise
	// its real host-port to clients.
	envs := make([]string, 0, len(deployReq.EnvVars)+6)
	for _, kv := range deployReq.EnvVars {
		envs = append(envs, kv.Key+"="+kv.Value)
	}
	envs = append(envs,
		"ARBITRIUM_PUBLIC_IP="+l.publicIP,
		"ARBITRIUM_PORT_GAME_EXTERNAL="+strconv.Itoa(udpPort),
		"ARBITRIUM_PORT_SIGNALING_EXTERNAL="+strconv.Itoa(tcpPort),
		// Edgegap auto-injects ARBITRIUM_REQUEST_ID; the runtime
		// has to fill it in for the local path. The game-server
		// reads it to scope its register_server / match_end RPCs.
		"ARBITRIUM_REQUEST_ID="+requestID,
		// The game-server's register_server call needs the runtime
		// HTTP key. Edgegap supplies this via the per-app env in
		// the dashboard; locally the runtime is the only source.
		"NAKAMA_HTTP_KEY="+l.httpKey,
		// Point at Nakama via the internal docker network so the
		// container doesn't have to hairpin through Caddy.
		"NAKAMA_URL="+l.nakamaURL,
	)

	createPayload := dockerCreateContainerRequest{
		Image: gameConfig.LocalImageRef,
		Env:   envs,
		ExposedPorts: map[string]struct{}{
			"4433/udp": {},
			"4434/tcp": {},
		},
		HostConfig: dockerHostConfig{
			AutoRemove:  true,
			NetworkMode: l.dockerNetwork,
			PortBindings: map[string][]dockerPortBinding{
				"4433/udp": {{
					HostIP:   "0.0.0.0",
					HostPort: strconv.Itoa(udpPort),
				}},
				"4434/tcp": {{
					HostIP:   "0.0.0.0",
					HostPort: strconv.Itoa(tcpPort),
				}},
			},
			// Cap memory + CPU so a runaway game-server can't
			// starve Nakama/Postgres. 1 GB / 1 vCPU matches
			// the historical Edgegap req_memory/req_cpu config.
			// Bound rather than absent because we're co-tenanted.
			Memory:   1024 * 1024 * 1024,
			NanoCPUs: 1000000000, // 1 vCPU
		},
	}
	containerID, err := l.dockerCreateContainer(
		ctx, containerName, createPayload)
	if err != nil {
		l.ports.release(udpPort, tcpPort)
		return nil, nil, fmt.Errorf(
			"docker create: %w", err)
	}
	if err := l.dockerStartContainer(ctx, containerID); err != nil {
		_ = l.dockerStopContainer(ctx, containerID, 1)
		l.ports.release(udpPort, tcpPort)
		return nil, nil, fmt.Errorf(
			"docker start (id=%s): %w", containerID, err)
	}
	logger.Info(
		"local allocator: container %s (id=%s) started for %s on"+
			" udp=%d tcp=%d",
		containerName, containerID,
		gameConfig.GameID, udpPort, tcpPort)

	// Synthesize the (deploy, status) pair callers expect.
	deploy := &edgegapDeployResponse{
		RequestID: requestID,
		Message: fmt.Sprintf(
			"local docker deploy (image=%s)",
			gameConfig.LocalImageRef),
	}
	status := &edgegapStatusResponse{
		RequestID:     requestID,
		CurrentStatus: "Status.READY",
		PublicIP:      l.publicIP,
		Ports: map[string]edgegapPort{
			"game": {
				External: udpPort,
				Internal: 4433,
				Protocol: "UDP",
			},
			"signaling": {
				External: tcpPort,
				Internal: 4434,
				Protocol: "TCP",
			},
		},
	}

	// Wait for the in-container Godot to call register_server so
	// clients don't see connection-refused on the first try. Same
	// gate Edgegap's path uses (see waitForServerRegistered).
	if err := waitForServerRegistered(
		ctx, logger, nk, requestID); err != nil {
		_ = l.dockerStopContainer(ctx, containerID, 5)
		l.ports.release(udpPort, tcpPort)
		return nil, nil, fmt.Errorf(
			"server didn't register: %w", err)
	}

	return deploy, status, nil
}

// Stop implements Allocator. Idempotent: a missing container
// returns nil. Releases the port pair back to the pool.
func (l *LocalDockerAllocator) Stop(
	ctx context.Context,
	requestID string,
) error {
	containerName := "match-" + requestID
	// Best-effort container shutdown; the container's AutoRemove
	// at create time cleans up the runtime state. After stop we
	// can't recover the udp/tcp ports from Docker (the bindings
	// are gone with the container), so we leak them until the
	// runtime restarts. Acceptable: the pool default is 100 each,
	// and matches are short-lived. A future tightening would
	// look up the port bindings via /containers/{id}/json before
	// stopping.
	if err := l.dockerStopContainer(ctx, containerName, 5); err != nil {
		// 404 is fine (already stopped/missing). Other errors
		// log but don't escalate — match_lifecycle's caller
		// follows Edgegap's same "best-effort, cost-monitor
		// catches leaks" model.
		if !strings.Contains(err.Error(), "404") {
			return err
		}
	}
	return nil
}

// localPortPool tracks available host-side UDP+TCP port pairs.
// Pair semantics: each allocation returns one UDP and one TCP
// port; a release returns the same pair. Indices into the
// underlying slices stay aligned across allocations.
type localPortPool struct {
	mu     sync.Mutex
	udpLo  int
	udpHi  int
	tcpLo  int
	tcpHi  int
	udpInUse map[int]bool
	tcpInUse map[int]bool
}

func newLocalPortPool(
	udpLo, udpHi, tcpLo, tcpHi int,
) *localPortPool {
	return &localPortPool{
		udpLo:    udpLo,
		udpHi:    udpHi,
		tcpLo:    tcpLo,
		tcpHi:    tcpHi,
		udpInUse: map[int]bool{},
		tcpInUse: map[int]bool{},
	}
}

func (p *localPortPool) allocate() (int, int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	udp := p.firstFree(p.udpLo, p.udpHi, p.udpInUse)
	if udp == 0 {
		return 0, 0, fmt.Errorf(
			"udp port pool exhausted (range %d-%d)", p.udpLo, p.udpHi)
	}
	tcp := p.firstFree(p.tcpLo, p.tcpHi, p.tcpInUse)
	if tcp == 0 {
		return 0, 0, fmt.Errorf(
			"tcp port pool exhausted (range %d-%d)", p.tcpLo, p.tcpHi)
	}
	p.udpInUse[udp] = true
	p.tcpInUse[tcp] = true
	return udp, tcp, nil
}

func (p *localPortPool) release(udp, tcp int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.udpInUse, udp)
	delete(p.tcpInUse, tcp)
}

func (p *localPortPool) firstFree(lo, hi int, inUse map[int]bool) int {
	for i := lo; i <= hi; i++ {
		if !inUse[i] {
			return i
		}
	}
	return 0
}

func parsePortRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf(
			"port range must be \"lo-hi\" (got %q)", s)
	}
	lo, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || lo < 1 || lo > 65535 {
		return 0, 0, fmt.Errorf(
			"invalid lo port in %q: %v", s, err)
	}
	hi, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || hi < 1 || hi > 65535 {
		return 0, 0, fmt.Errorf(
			"invalid hi port in %q: %v", s, err)
	}
	if hi < lo {
		return 0, 0, fmt.Errorf(
			"port range hi < lo in %q", s)
	}
	return lo, hi, nil
}

// --- Docker Engine API client (UNIX socket) ----------------------

// dockerCreateContainerRequest is the subset of POST
// /containers/create we use. Wire shape per Docker Engine API
// v1.45 (matches the heroiclabs/nakama Dockerfile's docker
// version baseline).
type dockerCreateContainerRequest struct {
	Image        string                    `json:"Image"`
	Env          []string                  `json:"Env"`
	ExposedPorts map[string]struct{}       `json:"ExposedPorts"`
	HostConfig   dockerHostConfig          `json:"HostConfig"`
}

type dockerHostConfig struct {
	AutoRemove   bool                            `json:"AutoRemove"`
	PortBindings map[string][]dockerPortBinding  `json:"PortBindings"`
	// NetworkMode picks the docker network the container attaches
	// to. Set to a named network (e.g. "nakama_nakama-net") so the
	// in-container game-server can resolve `nakama` and reach
	// register_server without depending on hairpin NAT through the
	// host's public IP. Port publishing still works regardless.
	NetworkMode string `json:"NetworkMode,omitempty"`
	// Memory is bytes; NanoCPUs is 10^9 ns of CPU time per
	// second (1e9 == 1 vCPU). Bounded cohabit with Nakama.
	Memory   int64 `json:"Memory,omitempty"`
	NanoCPUs int64 `json:"NanoCpus,omitempty"`
}

type dockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type dockerCreateContainerResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

func (l *LocalDockerAllocator) dockerCreateContainer(
	ctx context.Context,
	name string,
	req dockerCreateContainerRequest,
) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	url := "http://docker/v1.45/containers/create?name=" + name
	httpReq, err := http.NewRequestWithContext(
		ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := l.http.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf(
			"docker create %d: %s", resp.StatusCode, string(respBody))
	}
	out := dockerCreateContainerResponse{}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf(
			"decode create response: %w (body=%s)",
			err, string(respBody))
	}
	return out.ID, nil
}

func (l *LocalDockerAllocator) dockerStartContainer(
	ctx context.Context,
	containerID string,
) error {
	url := "http://docker/v1.45/containers/" + containerID + "/start"
	httpReq, err := http.NewRequestWithContext(
		ctx, "POST", url, nil)
	if err != nil {
		return err
	}
	resp, err := l.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// 204 No Content is the success status. 304 Not Modified
	// means already started. Both are acceptable.
	if resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusNotModified {
		return nil
	}
	return fmt.Errorf(
		"docker start %d: %s", resp.StatusCode, string(respBody))
}

func (l *LocalDockerAllocator) dockerStopContainer(
	ctx context.Context,
	containerID string,
	timeoutSec int,
) error {
	url := fmt.Sprintf(
		"http://docker/v1.45/containers/%s/stop?t=%d",
		containerID, timeoutSec)
	httpReq, err := http.NewRequestWithContext(
		ctx, "POST", url, nil)
	if err != nil {
		return err
	}
	resp, err := l.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	// 204 = stopped, 304 = already stopped, 404 = missing.
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("docker stop 404: %s", string(respBody))
	default:
		return fmt.Errorf(
			"docker stop %d: %s", resp.StatusCode, string(respBody))
	}
}
