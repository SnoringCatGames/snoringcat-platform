// geoip-sidecar wraps a maxminddb-golang reader behind a tiny HTTP
// API so the Nakama Go plugin can do country lookups without
// depending on golang.org/x/sys directly (the plugin's x/sys
// version has to match what heroiclabs/nakama:3.25.0 was built
// against, and pulling in maxminddb-golang breaks that — see
// runtime/allocator.go for the history).
//
// Wire model:
//   - Container is on the nakama-net docker network. The Nakama
//     runtime reaches it via http://geoip-sidecar:8080/lookup.
//   - The DB-IP MMDB file is bind-mounted read-only from the host
//     path the geoip-refresh systemd timer writes (default
//     /var/lib/snoringcat/geoip/dbip-country-lite.mmdb,
//     overridable via GEOIP_MMDB_PATH).
//   - On a monthly refresh the geoip-refresh script restarts this
//     service so the new file gets mmap'd. No hot-reload here —
//     restart is cheap (~50ms) and at-most-once-per-month.
//
// Endpoints:
//   GET /lookup?ip=<ipv4-or-ipv6>
//     200: {"country":"US","ip":"1.2.3.4"}  (country is the ISO
//          alpha-2 code; "" when the address is unrecognized)
//     400: {"error":"..."}  (unparseable IP)
//   GET /healthz  → 200 "ok" once the MMDB is open.
//   GET /metrics  → Prometheus text format.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/oschwald/maxminddb-golang"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	lookupsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "geoip_lookups_total",
			Help: "Total /lookup calls partitioned by outcome.",
		},
		// result is one of:
		//   hit         — MMDB returned a country code
		//   miss        — MMDB ran, no country (private/unknown IP)
		//   bad_request — unparseable / missing IP param
		//   error       — mmdb lookup itself errored
		[]string{"result"},
	)
	lookupSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name: "geoip_lookup_seconds",
			Help: "Wall-clock latency of /lookup handler.",
			// Tight bucket layout: in-process mmap MMDB lookups
			// land in the tens-of-microseconds range; anything
			// past ~10ms is the smoking-gun for HTTP/network
			// overhead so the runtime's 250ms client timeout
			// fires before saturation.
			Buckets: []float64{
				0.0001, 0.0005, 0.001, 0.005, 0.01,
				0.025, 0.05, 0.1, 0.25, 0.5,
			},
		},
	)
)

func init() {
	prometheus.MustRegister(lookupsTotal, lookupSeconds)
}

type lookupResponse struct {
	Country string `json:"country"`
	IP      string `json:"ip"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type countryRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
}

type server struct {
	db *maxminddb.Reader
}

func (s *server) handleLookup(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		lookupSeconds.Observe(time.Since(start).Seconds())
	}()

	ipStr := r.URL.Query().Get("ip")
	if ipStr == "" {
		lookupsTotal.WithLabelValues("bad_request").Inc()
		writeError(w, http.StatusBadRequest, "missing ip param")
		return
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		lookupsTotal.WithLabelValues("bad_request").Inc()
		writeError(w, http.StatusBadRequest, "unparseable ip")
		return
	}
	rec := countryRecord{}
	if err := s.db.Lookup(ip, &rec); err != nil {
		lookupsTotal.WithLabelValues("error").Inc()
		writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("mmdb lookup: %v", err))
		return
	}
	if rec.Country.ISOCode == "" {
		lookupsTotal.WithLabelValues("miss").Inc()
	} else {
		lookupsTotal.WithLabelValues("hit").Inc()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(lookupResponse{
		Country: rec.Country.ISOCode,
		IP:      ipStr,
	})
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorResponse{Error: msg})
}

func main() {
	mmdbPath := os.Getenv("GEOIP_MMDB_PATH")
	if mmdbPath == "" {
		mmdbPath = "/data/dbip-country-lite.mmdb"
	}
	listenAddr := os.Getenv("GEOIP_LISTEN")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	db, err := maxminddb.Open(mmdbPath)
	if err != nil {
		log.Fatalf("open %s: %v", mmdbPath, err)
	}
	defer db.Close()
	log.Printf("opened mmdb %s (build_epoch=%d, build_time=%s)",
		mmdbPath, db.Metadata.BuildEpoch,
		time.Unix(int64(db.Metadata.BuildEpoch), 0).UTC().Format(time.RFC3339))

	srv := &server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("/lookup", srv.handleLookup)
	mux.HandleFunc("/healthz", srv.handleHealth)
	mux.Handle("/metrics", promhttp.Handler())

	httpServer := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on %s", listenAddr)
	if err := httpServer.ListenAndServe(); err != nil &&
		!errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
