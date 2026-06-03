package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Fleet allocator backend selection.
//
// Each game declares an `allocator_mode` in game.yaml that picks
// the game-server backend:
//
//   - "edgegap" (or "" / unset): the historical path — every match
//     allocates an Edgegap deployment. Pay-per-mCPU-minute billing.
//   - "local": every match runs as a Docker container on the Nakama
//     host. Zero marginal per-match cost beyond the host's fixed
//     monthly Hetzner bill.
//   - "hybrid": local-first, Edgegap fallback for matches whose
//     players are too far from the local host's region. Decision
//     made per match by hybridAllocatorChoice based on player IPs.
//
// The dispatch happens in fleet_allocator.go's OnMatchmakerMatched
// after the per-game config resolves. match_metadata then stores
// the chosen kind so MatchEndRpc / MatchCancelRpc can call the
// right Stop without re-running the geo decision.

const (
	allocatorModeEdgegap = "edgegap"
	allocatorModeLocal   = "local"
	allocatorModeHybrid  = "hybrid"
)

// hybridAllocatorChoice decides whether a match should run on the
// local backend or fall back to Edgegap. Returns true to route to
// local. Called per-match from OnMatchmakerMatched when the
// resolved game config's AllocatorMode is "hybrid".
//
// Routing rule: every matched player IP must classify as North
// America. The geoip sidecar (run as a docker-compose service on
// the Nakama host, mmap'd over the DB-IP IP-to-Country Lite MMDB)
// is the primary source. If the sidecar errors or times out, we
// fall back to the static `/8` CIDR map below — a safety net that
// keeps the matchmaker latency budget bounded if the sidecar is
// degraded.
//
// Any missing IP, unparseable IP, or non-NA player routes the
// whole match to Edgegap (safe default — Edgegap will geo-route
// it via its own provider).
func hybridAllocatorChoice(
	logger runtime.Logger,
	geo geoIPLookup,
	matchedIPs []string,
) bool {
	if len(matchedIPs) == 0 {
		// No IPs recorded — fall through to Edgegap's
		// geography-based routing, which is the existing
		// behavior when client_ip RPC didn't get a chance to
		// run pre-matchmaker.
		return false
	}
	for _, raw := range matchedIPs {
		ip := net.ParseIP(raw)
		if ip == nil {
			logger.Warn(
				"hybrid allocator: unparseable IP %q; routing"+
					" match to Edgegap as a safe default",
				raw)
			return false
		}
		if !ipIsNorthAmerica(logger, geo, ip) {
			logger.Info(
				"hybrid allocator: IP %s not classified as NA;"+
					" routing match to Edgegap",
				raw)
			return false
		}
	}
	return true
}

// ipIsNorthAmerica is the policy gate: tries the geoip sidecar
// first, falls back to the static CIDR map on sidecar
// errors/timeouts so a degraded sidecar doesn't black-hole the
// matchmaker. "North America" today means ISO US/CA; we treat
// MX/PR as Edgegap-routed because the local host is in
// us-west and the latency budget for MX clients is worse than
// Edgegap's regional placement would deliver.
//
// The two NA codes match what production Hetzner-hosted Nakama
// can sensibly serve; widen this when the local fleet grows
// beyond a single us-west host.
func ipIsNorthAmerica(
	logger runtime.Logger,
	geo geoIPLookup,
	ip net.IP,
) bool {
	if geo != nil {
		country, err := geo.Lookup(ip)
		if err == nil {
			switch country {
			case "US", "CA":
				return true
			default:
				return false
			}
		}
		logger.Warn(
			"geoip sidecar lookup for %s failed (%v);"+
				" falling back to static CIDR map",
			ip, err)
	}
	return ipLooksLikeNorthAmericaStatic(ip)
}

// geoIPLookup is implemented by the geoip sidecar HTTP client.
// Defined as an interface so the matchmaker hook can run with
// a stub in tests and so a future in-process implementation
// (once nakama and maxminddb agree on x/sys) can drop in.
type geoIPLookup interface {
	Lookup(ip net.IP) (country string, err error)
}

// geoIPHTTPClient calls the sidecar's /lookup endpoint and
// extracts the country field. Per-lookup timeout is tight so the
// matchmaker hook stays responsive even when the sidecar is down.
type geoIPHTTPClient struct {
	baseURL string
	http    *http.Client
}

// newGeoIPClient reads GEOIP_SIDECAR_URL from env (default
// http://geoip-sidecar:8080). Returns nil when set to the literal
// "off", which disables the sidecar path entirely and forces the
// static-CIDR fallback. Useful for the compliance test runner
// where the sidecar isn't deployed.
func newGeoIPClient(env map[string]string) geoIPLookup {
	rawURL := env["GEOIP_SIDECAR_URL"]
	if rawURL == "" {
		rawURL = "http://geoip-sidecar:8080"
	}
	if strings.EqualFold(rawURL, "off") {
		return nil
	}
	return &geoIPHTTPClient{
		baseURL: strings.TrimRight(rawURL, "/"),
		http: &http.Client{
			Timeout: 250 * time.Millisecond,
		},
	}
}

type geoIPLookupResponse struct {
	Country string `json:"country"`
	IP      string `json:"ip"`
}

func (c *geoIPHTTPClient) Lookup(ip net.IP) (string, error) {
	if c == nil || c.http == nil {
		return "", fmt.Errorf("geoip client not initialised")
	}
	u := c.baseURL + "/lookup?ip=" +
		url.QueryEscape(ip.String())
	ctx, cancel := context.WithTimeout(
		context.Background(), c.http.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf(
			"geoip status %d: %s", resp.StatusCode, string(body))
	}
	out := geoIPLookupResponse{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	return out.Country, nil
}


// naCIDRs is a coarse first-pass list of CIDR blocks that cover
// the bulk of US/Canada residential ISPs. Far from exhaustive —
// the proper fix is MaxMind GeoLite2-Country lookup (free,
// updated weekly). This list catches the common case so the
// rollout can validate the hybrid path with real traffic before
// the geo integration lands.
//
// Sourced from public ARIN/CAIDA allocation tables; the
// follow-up GeoIP work replaces this entirely.
var naCIDRs = []string{
	// ARIN-allocated /8 blocks (most US/Canada residential):
	"3.0.0.0/8", "4.0.0.0/8", "6.0.0.0/8", "7.0.0.0/8",
	"8.0.0.0/8", "9.0.0.0/8", "11.0.0.0/8", "12.0.0.0/8",
	"13.0.0.0/8", "15.0.0.0/8", "16.0.0.0/8", "17.0.0.0/8",
	"18.0.0.0/8", "19.0.0.0/8", "20.0.0.0/8", "21.0.0.0/8",
	"22.0.0.0/8", "23.0.0.0/8", "24.0.0.0/8", "26.0.0.0/8",
	"28.0.0.0/8", "29.0.0.0/8", "30.0.0.0/8", "32.0.0.0/8",
	"33.0.0.0/8", "34.0.0.0/8", "35.0.0.0/8", "38.0.0.0/8",
	"40.0.0.0/8", "44.0.0.0/8", "45.0.0.0/8", "47.0.0.0/8",
	"48.0.0.0/8", "50.0.0.0/8", "52.0.0.0/8", "54.0.0.0/8",
	"55.0.0.0/8", "63.0.0.0/8", "64.0.0.0/8", "65.0.0.0/8",
	"66.0.0.0/8", "67.0.0.0/8", "68.0.0.0/8", "69.0.0.0/8",
	"70.0.0.0/8", "71.0.0.0/8", "72.0.0.0/8", "73.0.0.0/8",
	"74.0.0.0/8", "75.0.0.0/8", "76.0.0.0/8", "96.0.0.0/8",
	"97.0.0.0/8", "98.0.0.0/8", "99.0.0.0/8", "100.0.0.0/8",
	"104.0.0.0/8", "107.0.0.0/8", "108.0.0.0/8", "162.0.0.0/8",
	"172.0.0.0/8", "173.0.0.0/8", "174.0.0.0/8", "184.0.0.0/8",
	"199.0.0.0/8", "204.0.0.0/8", "205.0.0.0/8", "206.0.0.0/8",
	"207.0.0.0/8", "208.0.0.0/8", "209.0.0.0/8", "216.0.0.0/8",
}

// parsedNACIDRs caches the parsed forms of naCIDRs so the
// per-match check is a fast in-memory walk.
var parsedNACIDRs = func() []*net.IPNet {
	out := make([]*net.IPNet, 0, len(naCIDRs))
	for _, s := range naCIDRs {
		_, ipnet, err := net.ParseCIDR(s)
		if err == nil {
			out = append(out, ipnet)
		}
	}
	return out
}()

// ipLooksLikeNorthAmericaStatic is the safety-net classifier used
// when the geoip sidecar is unavailable. Coarse first-pass walk
// over the ARIN /8 list above.
func ipLooksLikeNorthAmericaStatic(ip net.IP) bool {
	// IPv6: conservative — bail to Edgegap. The IPv6 surface is
	// small (most clients still v4-only); the sidecar handles v6
	// when it's up, so the fallback only matters when geoip is
	// degraded AND a v6 player matches.
	if ip.To4() == nil {
		return false
	}
	for _, n := range parsedNACIDRs {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
