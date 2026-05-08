// signaling-proxy is a stateless WebSocket bridge that fronts
// Edgegap-allocated game-server signaling endpoints behind a
// stable hostname (signaling.snoringcat.games).
//
// Why this exists:
//   The previous design pre-warmed a per-deploy Cloudflare DNS
//   record (s-<dashed-ip>.game.hopnbop.net) seconds before
//   sending match_ready. ISP / home-router DNS resolvers often
//   cache NXDOMAIN for that hostname before the record exists,
//   then keep serving NXDOMAIN until the negative-cache TTL
//   expires — clients see "DNS name does not exist" and the
//   WebSocket fails in <50 ms with no chance to handshake.
//
//   This proxy puts a stable, always-resolvable FQDN in front of
//   the dynamic Edgegap deploy IP. No per-deploy DNS, no
//   per-deploy LE cert. Caddy on Hetzner terminates TLS once;
//   this proxy speaks plain WS to the upstream.
//
// How it works:
//   The Nakama runtime hook signs an HMAC token over the target
//   (ip, port, expiry) using a shared secret, then includes a
//   URL like
//     wss://signaling.snoringcat.games/connect/<base64url-token>
//   in the match_ready notification. The client connects to
//   that URL; this proxy decodes + verifies the token, opens an
//   upstream WS to the encoded (ip, port), and pipes frames
//   bidirectionally until either side closes.
//
// Stateless by design: no registration step, no shared
// in-memory map between Nakama and this binary. The token IS
// the routing decision. Restarting the proxy doesn't break any
// in-flight match (signaling completes in seconds; the actual
// game data goes direct over UDP).
//
// Anti-replay: tokens carry an expiry (default 5 min from
// issuance). Anyone who intercepts a token within the window can
// proxy through to the embedded (ip, port) — but that's only the
// legitimate game server they were already connecting to, so the
// damage is bounded to "use someone else's matchmaking slot",
// which the per-player session_id allowlist on the game server
// already prevents.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultListenAddr = "127.0.0.1:4435"
	tokenPrefix       = "/connect/"
	healthPath        = "/healthz"
	upstreamDialTime  = 5 * time.Second
)

var (
	upgrader = websocket.Upgrader{
		// Caddy fronts us; Origin checking happens at the
		// Caddyfile level if we ever want it. Game-server
		// signaling protocol doesn't trust Origin anyway.
		CheckOrigin:     func(r *http.Request) bool { return true },
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 64 * 1024,
	}
	dialer = websocket.Dialer{
		HandshakeTimeout: upstreamDialTime,
		// No TLS upstream — game-server speaks plain ws over
		// the Edgegap-allocated host port.
	}
)

func main() {
	listenAddr := os.Getenv("SIGNALING_PROXY_LISTEN")
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	secret := []byte(os.Getenv("SIGNALING_HMAC_SECRET"))
	if len(secret) < 16 {
		log.Fatal("SIGNALING_HMAC_SECRET must be set (>=16 bytes)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc(tokenPrefix, func(w http.ResponseWriter, r *http.Request) {
		handleConnect(w, r, secret)
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("signaling-proxy listening on %s", listenAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func handleConnect(w http.ResponseWriter, r *http.Request, secret []byte) {
	token := strings.TrimPrefix(r.URL.Path, tokenPrefix)
	target, err := decodeAndVerify(token, secret)
	if err != nil {
		log.Printf("token reject from %s: %v", r.RemoteAddr, err)
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}

	// Open upstream first. If unreachable, fail with 502 before
	// we upgrade the client (so the client sees the HTTP error
	// rather than an immediate WS close).
	dialCtx, cancel := context.WithTimeout(r.Context(), upstreamDialTime)
	defer cancel()
	upstream, _, err := dialer.DialContext(dialCtx,
		"ws://"+target+"/", nil)
	if err != nil {
		log.Printf("upstream dial %s: %v", target, err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	// Upgrade the client connection.
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("client upgrade %s: %v", r.RemoteAddr, err)
		return
	}
	defer client.Close()

	log.Printf("bridge open: %s <-> %s", r.RemoteAddr, target)
	pipeBidir(client, upstream)
	log.Printf("bridge close: %s <-> %s", r.RemoteAddr, target)
}

// pipeBidir copies WS frames in both directions until either
// side returns an error, then triggers a clean close on the
// peer. Returns once both directions have stopped.
func pipeBidir(a, b *websocket.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyFrames(a, b)
		_ = b.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
	}()
	go func() {
		defer wg.Done()
		copyFrames(b, a)
		_ = a.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
	}()
	wg.Wait()
}

func copyFrames(src, dst *websocket.Conn) {
	for {
		mt, msg, err := src.ReadMessage()
		if err != nil {
			return
		}
		if err := dst.WriteMessage(mt, msg); err != nil {
			return
		}
	}
}

// decodeAndVerify parses base64url("ip:port:exp:hex-hmac"),
// validates the HMAC + expiry, and returns "ip:port" if valid.
//
// Token wire format (UTF-8 bytes, base64url-encoded):
//
//	<ip>:<port>:<unix-exp>:<hex-hmac-sha256>
//
// where the HMAC is computed over "<ip>:<port>:<unix-exp>" with
// the shared SIGNALING_HMAC_SECRET.
func decodeAndVerify(token string, secret []byte) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("base64: %w", err)
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 4 {
		return "", fmt.Errorf("expected 4 parts, got %d", len(parts))
	}
	ip, port, expStr, gotHex := parts[0], parts[1], parts[2], parts[3]

	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("bad ip: %q", ip)
	}
	if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
		return "", fmt.Errorf("bad port: %q", port)
	}

	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return "", fmt.Errorf("bad exp: %w", err)
	}
	if time.Now().Unix() > exp {
		return "", errors.New("token expired")
	}

	want := hmacHex("ip:port:exp", []string{ip, port, expStr}, secret)
	if !hmac.Equal([]byte(want), []byte(gotHex)) {
		return "", errors.New("hmac mismatch")
	}
	return ip + ":" + port, nil
}

// hmacHex computes HMAC-SHA256 over strings.Join(parts, ":"),
// returning the lowercase hex digest. The first arg is a label
// for documentation only — it doesn't enter the digest.
func hmacHex(_ string, parts []string, secret []byte) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(strings.Join(parts, ":")))
	return hex.EncodeToString(h.Sum(nil))
}
