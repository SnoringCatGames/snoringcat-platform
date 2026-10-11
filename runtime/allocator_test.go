package main

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/heroiclabs/nakama-common/runtime"
)

// testPresence / testMatchmakerEntry are minimal runtime.Presence and
// runtime.MatchmakerEntry fakes. Only the user id and the ticket
// properties matter to the allocator helpers under test.
type testPresence struct{ userID string }

func (testPresence) GetHidden() bool                   { return false }
func (testPresence) GetPersistence() bool              { return false }
func (testPresence) GetUsername() string               { return "" }
func (testPresence) GetStatus() string                 { return "" }
func (testPresence) GetReason() runtime.PresenceReason { return runtime.PresenceReasonUnknown }
func (p testPresence) GetUserId() string               { return p.userID }
func (testPresence) GetSessionId() string              { return "" }
func (testPresence) GetNodeId() string                 { return "" }

type testMatchmakerEntry struct {
	userID string
	props  map[string]interface{}
}

func (e testMatchmakerEntry) GetPresence() runtime.Presence {
	return testPresence{userID: e.userID}
}
func (testMatchmakerEntry) GetTicket() string                       { return "" }
func (e testMatchmakerEntry) GetProperties() map[string]interface{} { return e.props }
func (testMatchmakerEntry) GetPartyId() string                      { return "" }

// entriesWith builds one entry per property map, with distinct user
// ids. A nil map stands for an entry that carries no properties.
func entriesWith(props ...map[string]interface{}) []runtime.MatchmakerEntry {
	out := make([]runtime.MatchmakerEntry, 0, len(props))
	for i, p := range props {
		out = append(out, testMatchmakerEntry{
			userID: fmt.Sprintf("user-%d", i),
			props:  p,
		})
	}
	return out
}

// probeTicket returns the properties the synthetic-match-probe puts
// on its tickets, plus probe_allocator when allocator is non-empty.
func probeTicket(allocator string) map[string]interface{} {
	p := map[string]interface{}{
		"probe":        "true",
		"platform":     "web",
		"player_count": "1",
		"game_id":      "hopnbop",
	}
	if allocator != "" {
		p[probeAllocatorProperty] = allocator
	}
	return p
}

// realTicket returns the properties of an ordinary player ticket.
func realTicket() map[string]interface{} {
	return map[string]interface{}{
		"platform":  "windows",
		"game_id":   "hopnbop",
		"game_mode": "ffa",
	}
}

// recordingLogger captures Warn/Info lines so tests can assert that
// an ignored override was logged, and that a clean path stayed quiet.
type recordingLogger struct {
	testLogger
	warns *[]string
	infos *[]string
}

func newRecordingLogger() recordingLogger {
	return recordingLogger{warns: &[]string{}, infos: &[]string{}}
}

func (l recordingLogger) Warn(format string, v ...interface{}) {
	*l.warns = append(*l.warns, fmt.Sprintf(format, v...))
}

func (l recordingLogger) Info(format string, v ...interface{}) {
	*l.infos = append(*l.infos, fmt.Sprintf(format, v...))
}

func (l recordingLogger) WithField(string, interface{}) runtime.Logger {
	return l
}

func (l recordingLogger) WithFields(map[string]interface{}) runtime.Logger {
	return l
}

// stubGeo classifies IPs from a fixed table; unknown IPs error, which
// sends ipIsNorthAmerica to the static CIDR fallback.
type stubGeo map[string]string

func (g stubGeo) Lookup(ip net.IP) (string, error) {
	if c, ok := g[ip.String()]; ok {
		return c, nil
	}
	return "", fmt.Errorf("no stub entry for %s", ip)
}

func TestIsProbeMatchEntries(t *testing.T) {
	cases := []struct {
		name    string
		entries []runtime.MatchmakerEntry
		want    bool
	}{
		{"no-entries", nil, false},
		{"all-marked", entriesWith(probeTicket(""), probeTicket("")), true},
		{"single-marked", entriesWith(probeTicket("")), true},
		{"one-unmarked", entriesWith(probeTicket(""), realTicket()), false},
		{"all-real", entriesWith(realTicket(), realTicket()), false},
		{"nil-props", entriesWith(probeTicket(""), nil), false},
		{
			"marker-not-true",
			entriesWith(probeTicket(""), map[string]interface{}{
				"probe": "false"}),
			false,
		},
		{
			// Ticket string properties arrive as strings; a
			// numeric/bool marker is not the probe's shape.
			"marker-wrong-type",
			entriesWith(map[string]interface{}{"probe": true}),
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isProbeMatchEntries(c.entries); got != c.want {
				t.Fatalf("isProbeMatchEntries = %v, want %v", got,
					c.want)
			}
		})
	}
}

func TestProbeAllocatorOverride(t *testing.T) {
	cases := []struct {
		name     string
		entries  []runtime.MatchmakerEntry
		want     string
		wantOK   bool
		wantWarn bool
	}{
		{
			name: "edgegap-pinned-by-all",
			entries: entriesWith(
				probeTicket("edgegap"), probeTicket("edgegap")),
			want: allocatorModeEdgegap, wantOK: true,
		},
		{
			name: "local-pinned-by-all",
			entries: entriesWith(
				probeTicket("local"), probeTicket("local")),
			want: allocatorModeLocal, wantOK: true,
		},
		{
			// The everyday probe run sets no pin: no override and
			// nothing worth a warning.
			name:    "probe-without-pin",
			entries: entriesWith(probeTicket(""), probeTicket("")),
		},
		{
			name:    "real-match-without-pin",
			entries: entriesWith(realTicket(), realTicket()),
		},
		{
			name: "entries-disagree",
			entries: entriesWith(
				probeTicket("edgegap"), probeTicket("local")),
			wantWarn: true,
		},
		{
			name: "one-entry-missing-pin",
			entries: entriesWith(
				probeTicket("edgegap"), probeTicket("")),
			wantWarn: true,
		},
		{
			name: "hybrid-is-not-a-backend",
			entries: entriesWith(
				probeTicket("hybrid"), probeTicket("hybrid")),
			wantWarn: true,
		},
		{
			name: "unknown-value",
			entries: entriesWith(
				probeTicket("aws"), probeTicket("aws")),
			wantWarn: true,
		},
		{
			name: "case-sensitive",
			entries: entriesWith(
				probeTicket("Edgegap"), probeTicket("Edgegap")),
			wantWarn: true,
		},
		{
			// A real player can't land in a probe match (targeted
			// query), but if every ticket carried the pin without
			// the probe marker it must still be ignored.
			name: "pinned-but-not-probe-match",
			entries: entriesWith(
				map[string]interface{}{
					probeAllocatorProperty: "edgegap"},
				map[string]interface{}{
					probeAllocatorProperty: "edgegap"}),
			wantWarn: true,
		},
		{
			name: "pinned-probe-mixed-with-real-player",
			entries: entriesWith(
				probeTicket("edgegap"), realTicket()),
			wantWarn: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lg := newRecordingLogger()
			got, ok := probeAllocatorOverride(
				lg, c.entries, isProbeMatchEntries(c.entries))
			if got != c.want || ok != c.wantOK {
				t.Fatalf("probeAllocatorOverride = (%q, %v), want"+
					" (%q, %v)", got, ok, c.want, c.wantOK)
			}
			if gotWarn := len(*lg.warns) > 0; gotWarn != c.wantWarn {
				t.Fatalf("warned=%v, want %v (warns=%q)", gotWarn,
					c.wantWarn, *lg.warns)
			}
			if c.wantWarn && !strings.Contains(
				(*lg.warns)[0], probeAllocatorProperty) {
				t.Errorf("warning should name %s: %q",
					probeAllocatorProperty, (*lg.warns)[0])
			}
		})
	}
}

// TestProbeAllocatorOverrideRequiresProbeMatchFlag locks the gate to
// the caller's isProbeMatch value, not to anything re-derived from
// the entries: even fully pinned probe tickets get no override when
// the caller says the match is not a probe match.
func TestProbeAllocatorOverrideRequiresProbeMatchFlag(t *testing.T) {
	entries := entriesWith(
		probeTicket("edgegap"), probeTicket("edgegap"))
	lg := newRecordingLogger()
	if got, ok := probeAllocatorOverride(lg, entries, false); ok ||
		got != "" {
		t.Fatalf("override applied with isProbeMatch=false: (%q, %v)",
			got, ok)
	}
	if len(*lg.warns) != 1 {
		t.Fatalf("want exactly one warning, got %q", *lg.warns)
	}
}

// legacyResolve is the allocator-mode resolution OnMatchmakerMatched
// ran before the probe override existed, kept verbatim so the test
// below can prove the real-player path is behaviorally identical.
func legacyResolve(
	logger runtime.Logger,
	geo geoIPLookup,
	configured string,
	ipList []string,
) string {
	allocatorMode := configured
	if allocatorMode == allocatorModeHybrid {
		if hybridAllocatorChoice(logger, geo, ipList) {
			allocatorMode = allocatorModeLocal
		} else {
			allocatorMode = allocatorModeEdgegap
		}
	}
	return allocatorMode
}

func TestResolveAllocatorModeMatchesLegacyWithoutOverride(t *testing.T) {
	geo := stubGeo{
		"73.1.2.3":  "US",
		"24.1.2.3":  "CA",
		"85.1.2.3":  "DE",
		"189.1.2.3": "MX",
	}
	ipSets := [][]string{
		nil,
		{"73.1.2.3"},
		{"73.1.2.3", "24.1.2.3"},
		{"73.1.2.3", "85.1.2.3"},
		{"189.1.2.3"},
		{"not-an-ip"},
		// Sidecar miss: falls back to the static CIDR map, which
		// knows 73/8 (ARIN) but not 81/8.
		{"73.9.9.9"},
		{"81.9.9.9"},
	}
	modes := []string{
		"", allocatorModeEdgegap, allocatorModeLocal,
		allocatorModeHybrid,
	}
	for _, geoImpl := range []geoIPLookup{geo, nil} {
		for _, mode := range modes {
			for _, ips := range ipSets {
				want := legacyResolve(testLogger{}, geoImpl, mode, ips)
				got := resolveAllocatorMode(
					testLogger{}, geoImpl, mode, ips, "")
				if got != want {
					t.Errorf("mode=%q ips=%v geo=%v: got %q, legacy"+
						" %q", mode, ips, geoImpl != nil, got, want)
				}
			}
		}
	}
}

func TestResolveAllocatorModeHybridRouting(t *testing.T) {
	geo := stubGeo{"73.1.2.3": "US", "85.1.2.3": "DE"}
	if got := resolveAllocatorMode(testLogger{}, geo,
		allocatorModeHybrid, []string{"73.1.2.3"}, ""); got !=
		allocatorModeLocal {
		t.Errorf("all-NA hybrid match = %q, want local", got)
	}
	if got := resolveAllocatorMode(testLogger{}, geo,
		allocatorModeHybrid, []string{"73.1.2.3", "85.1.2.3"},
		""); got != allocatorModeEdgegap {
		t.Errorf("mixed hybrid match = %q, want edgegap", got)
	}
}

func TestResolveAllocatorModeProbeOverrideWins(t *testing.T) {
	// The probe runs from an NA desktop: under hybrid it would
	// resolve to local. The pin must beat that, and beat any
	// configured mode, in both directions.
	geo := stubGeo{"73.1.2.3": "US", "85.1.2.3": "DE"}
	cases := []struct {
		configured string
		ips        []string
		override   string
	}{
		{allocatorModeHybrid, []string{"73.1.2.3"}, allocatorModeEdgegap},
		{allocatorModeHybrid, []string{"85.1.2.3"}, allocatorModeLocal},
		{allocatorModeLocal, []string{"73.1.2.3"}, allocatorModeEdgegap},
		{allocatorModeEdgegap, []string{"73.1.2.3"}, allocatorModeLocal},
		{"", nil, allocatorModeEdgegap},
	}
	for _, c := range cases {
		lg := newRecordingLogger()
		got := resolveAllocatorMode(
			lg, geo, c.configured, c.ips, c.override)
		if got != c.override {
			t.Errorf("configured=%q ips=%v override=%q: got %q",
				c.configured, c.ips, c.override, got)
		}
		if len(*lg.infos) != 1 || !strings.Contains(
			(*lg.infos)[0], probeAllocatorProperty) {
			t.Errorf("override should log one line naming %s,"+
				" got %q", probeAllocatorProperty, *lg.infos)
		}
	}
}

// TestProbeOverrideEndToEndResolution walks the same two steps
// OnMatchmakerMatched runs (gate, then resolve) for the cases that
// matter operationally.
func TestProbeOverrideEndToEndResolution(t *testing.T) {
	geo := stubGeo{"73.1.2.3": "US"}
	naIPs := []string{"73.1.2.3", "73.1.2.3"}
	resolve := func(entries []runtime.MatchmakerEntry) string {
		lg := newRecordingLogger()
		override, _ := probeAllocatorOverride(
			lg, entries, isProbeMatchEntries(entries))
		return resolveAllocatorMode(
			lg, geo, allocatorModeHybrid, naIPs, override)
	}
	t.Run("weekly-edgegap-probe", func(t *testing.T) {
		got := resolve(entriesWith(
			probeTicket("edgegap"), probeTicket("edgegap")))
		if got != allocatorModeEdgegap {
			t.Fatalf("got %q, want edgegap", got)
		}
	})
	t.Run("daily-probe-follows-hybrid", func(t *testing.T) {
		got := resolve(entriesWith(probeTicket(""), probeTicket("")))
		if got != allocatorModeLocal {
			t.Fatalf("got %q, want local (NA hybrid)", got)
		}
	})
	t.Run("real-players-cannot-pin", func(t *testing.T) {
		a := realTicket()
		a[probeAllocatorProperty] = "edgegap"
		b := realTicket()
		b[probeAllocatorProperty] = "edgegap"
		if got := resolve(entriesWith(a, b)); got != allocatorModeLocal {
			t.Fatalf("got %q, want local: real players must not"+
				" be able to force Edgegap", got)
		}
	})
}

// TestMatchReadyConnection locks the match_ready `connection` shape,
// including the `allocator` field the synthetic-match-probe asserts
// on and the mock flag compliance tests check.
func TestMatchReadyConnection(t *testing.T) {
	status := &edgegapStatusResponse{
		RequestID: "abc123",
		PublicIP:  "1.2.3.4",
		Ports: map[string]edgegapPort{
			"game":      {External: 31000, Internal: 4433, Protocol: "UDP"},
			"signaling": {External: 31001, Internal: 4434, Protocol: "TCP"},
		},
	}
	conn := matchReadyConnection(status, []string{"u_0"}, "webrtc",
		"wss://sig.example/connect/t", allocatorModeEdgegap, false)
	raw, err := json.Marshal(conn)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for key, want := range map[string]string{
		"server_ip":      "1.2.3.4",
		"request_id":     "abc123",
		"transport_type": "webrtc",
		"signaling_url":  "wss://sig.example/connect/t",
		"allocator":      allocatorModeEdgegap,
	} {
		if got, _ := decoded[key].(string); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if _, ok := decoded["ports"].(map[string]any); !ok {
		t.Errorf("ports missing or not an object: %v", decoded["ports"])
	}
	if sids, _ := decoded["session_ids"].([]any); len(sids) != 1 ||
		sids[0] != "u_0" {
		t.Errorf("session_ids = %v, want [u_0]", decoded["session_ids"])
	}
	if _, ok := decoded["mock"]; ok {
		t.Errorf("mock should be absent outside mock mode")
	}

	local := matchReadyConnection(status, nil, "enet", "",
		allocatorModeLocal, true)
	if local["allocator"] != allocatorModeLocal {
		t.Errorf("allocator = %v, want local", local["allocator"])
	}
	if local["mock"] != true {
		t.Errorf("mock should be true in mock mode")
	}
}
