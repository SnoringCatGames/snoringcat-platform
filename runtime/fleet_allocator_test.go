package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestPickTCPPort(t *testing.T) {
	t.Run("first-tcp-wins", func(t *testing.T) {
		ports := map[string]edgegapPort{
			"game": {External: 14433, Protocol: "UDP"},
			"signaling": {
				External: 14434, Protocol: "TCP"},
		}
		if got := pickTCPPort(ports); got != 14434 {
			t.Fatalf("got %d, want 14434", got)
		}
	})

	t.Run("case-insensitive-protocol", func(t *testing.T) {
		ports := map[string]edgegapPort{
			"signaling": {External: 4434, Protocol: "tcp"},
		}
		if got := pickTCPPort(ports); got != 4434 {
			t.Fatalf("lowercase tcp should match, got %d", got)
		}
	})

	t.Run("no-tcp-returns-zero", func(t *testing.T) {
		ports := map[string]edgegapPort{
			"game": {External: 14433, Protocol: "UDP"},
		}
		if got := pickTCPPort(ports); got != 0 {
			t.Fatalf("no TCP port should return 0, got %d", got)
		}
	})

	t.Run("zero-external-skipped", func(t *testing.T) {
		ports := map[string]edgegapPort{
			"signaling": {External: 0, Protocol: "TCP"},
		}
		if got := pickTCPPort(ports); got != 0 {
			t.Fatalf("unallocated TCP should return 0, got %d", got)
		}
	})

	t.Run("nil-map", func(t *testing.T) {
		if got := pickTCPPort(nil); got != 0 {
			t.Fatalf("nil map should return 0, got %d", got)
		}
	})
}

// TestSignSignalingURLDeterministic confirms the URL encoding is
// deterministic for a fixed input + clock — the signaling-proxy on
// the platform host re-derives the HMAC over the same payload
// before bridging the connection.
func TestSignSignalingURLDeterministic(t *testing.T) {
	secret := []byte("mock-hmac-secret")
	now := time.Unix(1715000000, 0)
	a := signSignalingURL("sig.example.com", secret, "1.2.3.4",
		4434, now)
	b := signSignalingURL("sig.example.com", secret, "1.2.3.4",
		4434, now)
	if a != b {
		t.Fatalf("same inputs produced different URLs: %s vs %s",
			a, b)
	}
}

// TestSignSignalingURLShape decodes the URL token back to its
// constituent fields and re-verifies the HMAC. Locks the wire
// shape so a future refactor of the encoding fails loudly here
// rather than at a signaling-proxy mismatch on prod.
func TestSignSignalingURLShape(t *testing.T) {
	secret := []byte("mock-hmac-secret")
	now := time.Unix(1715000000, 0)
	url := signSignalingURL("sig.example.com", secret, "1.2.3.4",
		4434, now)
	const prefix = "wss://sig.example.com/connect/"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("missing prefix; got %q", url)
	}
	token := strings.TrimPrefix(url, prefix)
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("token not base64url-encoded: %v", err)
	}
	// Format: "ip:port:exp:hex-hmac"
	parts := strings.Split(string(raw), ":")
	if len(parts) != 4 {
		t.Fatalf("token has %d colon-segments, want 4: %q",
			len(parts), string(raw))
	}
	if parts[0] != "1.2.3.4" || parts[1] != "4434" {
		t.Errorf("ip/port mismatch: %v", parts[:2])
	}
	wantExp := now.Add(signalingTokenLifetime).Unix()
	if parts[2] != "1715000300" {
		t.Errorf("expiry segment = %q, want %d", parts[2], wantExp)
	}
	// Recompute the HMAC over "ip:port:exp" — this is what the
	// signaling-proxy does on the bridge side.
	payload := strings.Join(parts[:3], ":")
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(payload))
	wantHex := hex.EncodeToString(h.Sum(nil))
	if parts[3] != wantHex {
		t.Errorf("hmac mismatch: got %q, want %q", parts[3],
			wantHex)
	}
}

// TestSignSignalingURLExpiryDistinct — two calls one second apart
// produce different tokens (different `exp` values).
func TestSignSignalingURLExpiryDistinct(t *testing.T) {
	secret := []byte("k")
	a := signSignalingURL("d", secret, "1.1.1.1", 4434,
		time.Unix(1000, 0))
	b := signSignalingURL("d", secret, "1.1.1.1", 4434,
		time.Unix(1001, 0))
	if a == b {
		t.Fatalf("tokens for distinct clocks should differ")
	}
}

func TestSynthesizeMockDeploy(t *testing.T) {
	now := time.Unix(1715000000, 0)
	deploy, status := synthesizeMockDeploy(now)
	if !strings.HasPrefix(deploy.RequestID, mockEdgegapRequestIDPrefix) {
		t.Errorf("request_id %q lacks mock prefix",
			deploy.RequestID)
	}
	if status.PublicIP != mockPublicIP {
		t.Errorf("public_ip = %q, want %q", status.PublicIP,
			mockPublicIP)
	}
	if status.CurrentStatus != "Status.READY" {
		t.Errorf("status = %q, want Status.READY",
			status.CurrentStatus)
	}
	// Ports map mirrors Dockerfile.edgegap (4433/UDP, 4434/TCP).
	if status.Ports["game"].Protocol != "UDP" ||
		status.Ports["game"].Internal != 4433 {
		t.Errorf("game port shape unexpected: %+v",
			status.Ports["game"])
	}
	if status.Ports["signaling"].Protocol != "TCP" ||
		status.Ports["signaling"].Internal != 4434 {
		t.Errorf("signaling port shape unexpected: %+v",
			status.Ports["signaling"])
	}
	// pickTCPPort should successfully pick out the signaling port —
	// this guards against a future change to synthesizeMockDeploy
	// that accidentally breaks the real-mode equivalence.
	if got := pickTCPPort(status.Ports); got != mockTCPPort {
		t.Errorf("pickTCPPort on mock = %d, want %d", got,
			mockTCPPort)
	}
}

// TestSynthesizeMockDeployUniqueRequestIDs — two synthesized
// deploys at distinct UnixNano timestamps have distinct request_ids
// so they don't collide on storage rows.
func TestSynthesizeMockDeployUniqueRequestIDs(t *testing.T) {
	a, _ := synthesizeMockDeploy(time.Unix(0, 1))
	b, _ := synthesizeMockDeploy(time.Unix(0, 2))
	if a.RequestID == b.RequestID {
		t.Fatalf("distinct timestamps produced same request_id: %q",
			a.RequestID)
	}
}

// TestAllocationFallbackGeographies locks the retry rotation. The
// first attempt (index 0) returns nil so the caller's deployReq
// stands; subsequent attempts return single-continent slices that
// cycle through allocationGeographyRotation. Wraps past the end
// so a future bump to maxAllocationAttempts > rotation length
// doesn't panic.
func TestAllocationFallbackGeographies(t *testing.T) {
	t.Run("attempt-0-returns-nil", func(t *testing.T) {
		if got := allocationFallbackGeographies(0); got != nil {
			t.Errorf("attempt 0 should leave caller's deployReq"+
				" alone, got %v", got)
		}
		if got := allocationFallbackGeographies(-1); got != nil {
			t.Errorf("negative index should be nil, got %v", got)
		}
	})

	t.Run("first-retry-uses-rotation-head", func(t *testing.T) {
		got := allocationFallbackGeographies(1)
		want := []string{allocationGeographyRotation[0]}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("attempt 1 = %v, want %v", got, want)
		}
	})

	t.Run("walks-rotation", func(t *testing.T) {
		for i := 1; i <= len(allocationGeographyRotation); i++ {
			got := allocationFallbackGeographies(i)
			want := allocationGeographyRotation[(i-1)%len(
				allocationGeographyRotation)]
			if len(got) != 1 || got[0] != want {
				t.Errorf("attempt %d = %v, want [%s]",
					i, got, want)
			}
		}
	})

	t.Run("wraps-past-rotation-length", func(t *testing.T) {
		// Attempt N where N > rotation length must not panic
		// and must wrap to the rotation head. Locks the wrap
		// contract so a future bump to maxAllocationAttempts
		// beyond the rotation length is safe.
		over := len(allocationGeographyRotation) + 1
		got := allocationFallbackGeographies(over)
		want := allocationGeographyRotation[0]
		if len(got) != 1 || got[0] != want {
			t.Errorf("wrap to head: got %v, want [%s]",
				got, want)
		}
	})

	t.Run("returns-single-continent", func(t *testing.T) {
		// Each retry pins ONE continent — multi-continent
		// fallbacks would defeat the point of rotating away
		// from the failing region.
		for i := 1; i < 6; i++ {
			got := allocationFallbackGeographies(i)
			if len(got) != 1 {
				t.Errorf("attempt %d should return one continent,"+
					" got %v", i, got)
			}
		}
	})
}

// TestAllocationBackoff covers the exponential-up-to-cap policy.
// Attempt 0 = 0 (no delay before the initial attempt); attempt N
// doubles from baseAllocationBackoff up to maxAllocationBackoff
// regardless of how high N climbs.
func TestAllocationBackoff(t *testing.T) {
	t.Run("attempt-0-is-zero", func(t *testing.T) {
		if got := allocationBackoff(0); got != 0 {
			t.Errorf("initial attempt should not sleep, got %v",
				got)
		}
		if got := allocationBackoff(-1); got != 0 {
			t.Errorf("negative index should be zero, got %v", got)
		}
	})

	t.Run("attempt-1-uses-base", func(t *testing.T) {
		if got := allocationBackoff(1); got != baseAllocationBackoff {
			t.Errorf("attempt 1 = %v, want %v", got,
				baseAllocationBackoff)
		}
	})

	t.Run("monotonic-up-to-cap", func(t *testing.T) {
		prev := allocationBackoff(0)
		// First retry is base; each subsequent retry must be
		// >= the previous one (exponential growth or already at
		// the cap).
		for i := 1; i < 8; i++ {
			cur := allocationBackoff(i)
			if cur < prev {
				t.Errorf("attempt %d backoff %v < attempt %d %v",
					i, cur, i-1, prev)
			}
			if cur > maxAllocationBackoff {
				t.Errorf("attempt %d backoff %v > cap %v",
					i, cur, maxAllocationBackoff)
			}
			prev = cur
		}
	})

	t.Run("hits-cap-eventually", func(t *testing.T) {
		// At high attempt indices we should be at the cap. The
		// exact attempt index where this lands depends on the
		// ratio of cap to base; we just assert that a very deep
		// retry chain saturates rather than runs away.
		if got := allocationBackoff(20); got != maxAllocationBackoff {
			t.Errorf("attempt 20 = %v, want cap %v",
				got, maxAllocationBackoff)
		}
	})

	t.Run("exponential-doubles", func(t *testing.T) {
		// Specifically: while we're under the cap, each step
		// doubles. Locks the policy so a future change to the
		// formula trips the test.
		want1 := baseAllocationBackoff
		want2 := 2 * baseAllocationBackoff
		want3 := 4 * baseAllocationBackoff
		if got := allocationBackoff(1); got != want1 {
			t.Errorf("attempt 1 = %v, want %v", got, want1)
		}
		if want2 <= maxAllocationBackoff {
			if got := allocationBackoff(2); got != want2 {
				t.Errorf("attempt 2 = %v, want %v", got, want2)
			}
		}
		if want3 <= maxAllocationBackoff {
			if got := allocationBackoff(3); got != want3 {
				t.Errorf("attempt 3 = %v, want %v", got, want3)
			}
		}
	})
}

// TestSleepOrCtxDone covers both the timer-elapsed and the ctx-
// cancelled branches.
func TestSleepOrCtxDone(t *testing.T) {
	t.Run("zero-duration-returns-immediately", func(t *testing.T) {
		ctx := context.Background()
		start := time.Now()
		if err := sleepOrCtxDone(ctx, 0); err != nil {
			t.Errorf("zero duration should return nil, got %v",
				err)
		}
		if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
			t.Errorf("zero duration took %v, want ~0", elapsed)
		}
	})

	t.Run("negative-duration-returns-immediately", func(t *testing.T) {
		ctx := context.Background()
		if err := sleepOrCtxDone(ctx, -1*time.Second); err != nil {
			t.Errorf("negative duration should return nil, got %v",
				err)
		}
	})

	t.Run("timer-elapses", func(t *testing.T) {
		ctx := context.Background()
		start := time.Now()
		if err := sleepOrCtxDone(ctx, 10*time.Millisecond); err != nil {
			t.Errorf("normal sleep should return nil, got %v",
				err)
		}
		if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
			t.Errorf("returned too early: %v", elapsed)
		}
	})

	t.Run("ctx-cancel-returns-err", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		// Cancel after a short delay; sleepOrCtxDone should
		// observe the cancel and return before the long timer
		// fires.
		go func() {
			time.Sleep(5 * time.Millisecond)
			cancel()
		}()
		start := time.Now()
		err := sleepOrCtxDone(ctx, 10*time.Second)
		if err == nil {
			t.Errorf("cancelled ctx should return non-nil err")
		}
		if elapsed := time.Since(start); elapsed > 1*time.Second {
			t.Errorf("ctx cancel didn't short-circuit: %v",
				elapsed)
		}
	})
}

func TestPickDominantGameID(t *testing.T) {
	games := newTestGames(t, map[string]string{
		"hopnbop": hopnbopConfigJSON,
		"apple":   `{"game_id":"apple","display_name":"A","edgegap_app_slug":"s","protocol_version":1,"display_version":"1"}`,
		"banana":  `{"game_id":"banana","display_name":"B","edgegap_app_slug":"s","protocol_version":1,"display_version":"1"}`,
	})
	lg := testLogger{}

	t.Run("dominant-wins", func(t *testing.T) {
		votes := map[string]int{"hopnbop": 3, "apple": 1}
		if got := pickDominantGameID(votes, games, lg); got != "hopnbop" {
			t.Fatalf("got %q, want hopnbop", got)
		}
	})

	t.Run("tie-resolves-alphabetically", func(t *testing.T) {
		votes := map[string]int{"banana": 2, "apple": 2}
		if got := pickDominantGameID(votes, games, lg); got != "apple" {
			t.Fatalf("got %q, want apple (alpha-first on tie)", got)
		}
	})

	t.Run("unknown-id-dropped", func(t *testing.T) {
		votes := map[string]int{"ghost": 5, "apple": 1}
		if got := pickDominantGameID(votes, games, lg); got != "apple" {
			t.Fatalf("got %q, want apple (ghost not registered)",
				got)
		}
	})

	t.Run("all-unknown-returns-empty", func(t *testing.T) {
		votes := map[string]int{"ghost": 4, "phantom": 1}
		if got := pickDominantGameID(votes, games, lg); got != "" {
			t.Fatalf("all-unknown should return empty, got %q",
				got)
		}
	})

	t.Run("empty-votes-returns-empty", func(t *testing.T) {
		if got := pickDominantGameID(
			nil, games, lg); got != "" {
			t.Fatalf("nil votes should return empty, got %q", got)
		}
		if got := pickDominantGameID(
			map[string]int{}, games, lg); got != "" {
			t.Fatalf("empty map should return empty, got %q", got)
		}
	})

	t.Run("nil-games-returns-empty", func(t *testing.T) {
		votes := map[string]int{"hopnbop": 5}
		if got := pickDominantGameID(votes, nil, lg); got != "" {
			t.Fatalf("nil registry should return empty, got %q",
				got)
		}
	})
}
