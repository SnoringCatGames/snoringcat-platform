package main

import (
	"net"

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
// Routing rule: every matched player IP must be classified as NA
// by ipLooksLikeNorthAmerica below. Any missing IP, unparseable
// IP, or non-NA player routes the whole match to Edgegap.
//
// **GeoIP follow-up status (2026-06-02)**: An earlier attempt to
// replace the static CIDR map with a maxminddb-golang lookup
// (DB-IP IP-to-Country Lite) ran into Go-plugin compatibility:
// maxminddb-golang depends on golang.org/x/sys, and the version
// it pulls in is incompatible with the version
// heroiclabs/nakama:3.25.0 was built against, causing the plugin
// to fail to load with "plugin was built with a different
// version of package golang.org/x/sys/unix". The MMDB refresh
// systemd timer + bind-mount are still wired (the file is on
// disk, refreshed monthly), waiting on a no-x/sys-dep GeoIP
// path. Options to revisit:
//   - Parse DB-IP's CSV download with stdlib only (no MMDB
//     parser dep).
//   - Wait for Nakama to upgrade x/sys, then re-add the
//     maxminddb-golang dep.
//   - Move the GeoIP lookup out of the Go plugin entirely —
//     run a sidecar that the runtime calls via HTTP.
// Until that lands, the static CIDR map below is the only
// GeoIP signal — coarse but stable.
func hybridAllocatorChoice(
	logger runtime.Logger,
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
		if !ipLooksLikeNorthAmerica(ip) {
			logger.Info(
				"hybrid allocator: IP %s not classified as NA"+
					" by static CIDR map; routing match to Edgegap",
				raw)
			return false
		}
	}
	return true
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

func ipLooksLikeNorthAmerica(ip net.IP) bool {
	// IPv6: conservative — bail to Edgegap unless we add real
	// geo lookup. The IPv6 deployment surface here is small
	// (most clients still v4-only); revisit when GeoLite lands.
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
