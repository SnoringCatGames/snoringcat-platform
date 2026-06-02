package main

import (
	"net"
	"sync/atomic"
	"time"

	"github.com/heroiclabs/nakama-common/runtime"
	"github.com/oschwald/maxminddb-golang"
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

// geoIPLookup hides the MaxMind/DB-IP MMDB behind a small
// interface so the runtime degrades gracefully when the DB
// isn't on disk (initial-boot window before the host's monthly
// refresh has run). When db is nil, lookups fall through to the
// static-CIDR helper below; otherwise they hit the MMDB. The
// MMDB schema both MaxMind GeoLite2-Country and DB-IP IP-to-
// Country Lite expose carries a {country: {iso_code: "US"}}
// node, which is all we need for the "is this player in NA"
// decision.
type geoIPLookup struct {
	db atomic.Pointer[maxminddb.Reader]
}

// load opens an MMDB at the given path and atomically swaps it
// into the lookup. Safe to call concurrently with reads. Returns
// nil on success, the underlying err on failure (caller logs and
// continues with whatever previous db was loaded).
func (g *geoIPLookup) load(path string) error {
	reader, err := maxminddb.Open(path)
	if err != nil {
		return err
	}
	old := g.db.Swap(reader)
	if old != nil {
		// Reader hot-swap: defer the close so any in-flight
		// Lookup call against the previous reader can finish.
		// 5s is a generous upper bound on a single Lookup.
		go func() {
			closeReaderAfterDelay(old)
		}()
	}
	return nil
}

// countryRecord matches the MMDB country node we care about.
// Field tags are ASCII to match the MMDB's actual key strings.
type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

// lookupCountry returns the ISO-3166 two-letter country code for
// the given IP, or "" when the DB isn't loaded or the IP isn't in
// the DB. Callers that want NA-routing semantics should compare
// against the naCountryCodes set below.
func (g *geoIPLookup) lookupCountry(ip net.IP) string {
	r := g.db.Load()
	if r == nil {
		return ""
	}
	rec := countryRecord{}
	if err := r.Lookup(ip, &rec); err != nil {
		return ""
	}
	return rec.Country.ISOCode
}

// naCountryCodes is the set of ISO-3166 country codes treated as
// North American for routing. Hillsboro is in OR (US Pacific
// time); US + Canada players have <100ms RTT on a typical
// residential link. Mexico is included because Edgegap's North
// America region also covers it; routing MX players local rather
// than to Edgegap's NA region is a slight RTT improvement (CA
// vs MX-via-NA-edge) but not a regression. PR included as a US
// territory. Any code outside this set routes to Edgegap.
var naCountryCodes = map[string]struct{}{
	"US": {}, "CA": {}, "MX": {}, "PR": {},
}

// closeReaderAfterDelay closes a swapped-out reader after a
// short delay so in-flight Lookup calls against the previous
// pointer finish without segfault. Called as a goroutine from
// geoIPLookup.load.
func closeReaderAfterDelay(r *maxminddb.Reader) {
	// 5s grace > any sane single-lookup latency; the readers we
	// swap are <10MB each so the brief overlap costs no real
	// memory.
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	<-timer.C
	_ = r.Close()
}

// initGeoIPFromEnv loads the MMDB at the env-supplied path if
// present, into the package-global activeGeoIP. Called once at
// runtime startup from main.go. Missing file = silent skip; the
// runtime still boots and hybridAllocatorChoice falls back to the
// static CIDR map. Bad-format file or read error = warning log
// but still continues.
func initGeoIPFromEnv(
	logger runtime.Logger,
	env map[string]string,
) {
	path := env["GEOIP_MMDB_PATH"]
	if path == "" {
		// Default location matches the host-side fetch script
		// (infra/remote/geoip-refresh/) so a vanilla deploy
		// picks it up without env override.
		path = "/var/lib/snoringcat/geoip/dbip-country-lite.mmdb"
	}
	if err := activeGeoIP.load(path); err != nil {
		logger.Warn(
			"geoip: MMDB load failed at %s: %v; hybrid allocator"+
				" will use the static ARIN CIDR fallback. Re-run"+
				" the geoip-refresh systemd timer to populate.",
			path, err)
		return
	}
	logger.Info(
		"geoip: loaded MMDB from %s", path)
}

// activeGeoIP is the runtime-shared lookup loaded at startup
// (and refreshed monthly by an out-of-band systemd timer). Nil-
// db means hybridAllocatorChoice falls through to the static
// CIDR map below.
var activeGeoIP = &geoIPLookup{}

// hybridAllocatorChoice decides whether a match should run on the
// local backend or fall back to Edgegap. Returns true to route to
// local. Called per-match from OnMatchmakerMatched when the
// resolved game config's AllocatorMode is "hybrid".
//
// Two paths:
//   1. MMDB loaded → lookup each IP's country, route local only
//      when every IP is in naCountryCodes.
//   2. MMDB missing → fall through to the static ARIN /8 map
//      (ipLooksLikeNorthAmerica). Coarse but boot-safe.
//
// Any missing IP, unparseable IP, or non-NA player routes the
// whole match to Edgegap — better to over-spend on egress than
// to serve a poor experience to a remote player.
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
		// MMDB path. Empty country (db not loaded, or IP not in
		// db) falls through to the static fallback for this IP
		// only — partial coverage during the monthly refresh
		// window stays safe.
		if cc := activeGeoIP.lookupCountry(ip); cc != "" {
			if _, ok := naCountryCodes[cc]; !ok {
				logger.Info(
					"hybrid allocator: IP %s country=%s;"+
						" routing match to Edgegap",
					raw, cc)
				return false
			}
			continue
		}
		// Static fallback.
		if !ipLooksLikeNorthAmerica(ip) {
			logger.Info(
				"hybrid allocator: IP %s not classified as NA"+
					" by static map (mmdb miss); routing match"+
					" to Edgegap",
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
