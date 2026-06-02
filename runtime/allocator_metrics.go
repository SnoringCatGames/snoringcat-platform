package main

import (
	"sync/atomic"

	"github.com/heroiclabs/nakama-common/runtime"
)

// Active-match + allocation-counter metrics.
//
// Three signals are exported, surfaced via Nakama's Prometheus
// endpoint and scraped by the prometheus container in
// docker-compose:
//
//   - snoringcat_active_matches (gauge): current number of
//     allocated game-server matches across all backends.
//     Distinguishes local vs edgegap via a label so a Grafana
//     panel can stack them.
//   - snoringcat_allocations_total{kind=local|edgegap}
//     (counter): cumulative allocation count per backend.
//     Combined with the gauge this gives an allocations-per-hour
//     rate and a peak-concurrent measurement.
//   - snoringcat_alloc_seconds (histogram): already shipped in
//     Stage 7.11; the cold-start latency timer. Untouched here.
//
// The CPX21 capacity model in plans/cost-reduction.md predicts
// a peak of 10-15 concurrent matches at 512 mCPU each. Watching
// snoringcat_active_matches{kind=local} over the first week of
// hybrid traffic is the empirical validation.

// activeMatchCounts tracks how many matches each backend is
// currently running. Atomic so allocation and teardown can race
// without needing a mutex. Read from the metrics helper below.
var (
	activeLocalMatches   atomic.Int64
	activeEdgegapMatches atomic.Int64
)

// recordAllocationStart bumps the per-backend gauge and counter
// after a successful allocate. Called from OnMatchmakerMatched
// after the deploy+status pair resolves. kind is the concrete
// post-hybrid choice ("local" or "edgegap"), never "hybrid".
func recordAllocationStart(
	nk runtime.NakamaModule,
	kind string,
) {
	switch kind {
	case allocatorModeLocal:
		activeLocalMatches.Add(1)
	case allocatorModeEdgegap:
		activeEdgegapMatches.Add(1)
	default:
		return
	}
	publishActiveMatchGauges(nk)
	nk.MetricsCounterAdd(
		"snoringcat_allocations_total",
		map[string]string{"kind": kind}, 1)
}

// recordAllocationEnd decrements the per-backend gauge after a
// successful Stop. Called from match_lifecycle's MatchEndRpc and
// MatchCancelRpc. Bounded at zero so a teardown without a
// matched start (replayed cancel, lost metadata) can't drag the
// gauge negative.
func recordAllocationEnd(
	nk runtime.NakamaModule,
	kind string,
) {
	switch kind {
	case allocatorModeLocal:
		decrementBoundedAtZero(&activeLocalMatches)
	case allocatorModeEdgegap, "":
		decrementBoundedAtZero(&activeEdgegapMatches)
	default:
		return
	}
	publishActiveMatchGauges(nk)
}

func decrementBoundedAtZero(c *atomic.Int64) {
	for {
		current := c.Load()
		if current <= 0 {
			return
		}
		if c.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func publishActiveMatchGauges(nk runtime.NakamaModule) {
	nk.MetricsGaugeSet(
		"snoringcat_active_matches",
		map[string]string{"kind": allocatorModeLocal},
		float64(activeLocalMatches.Load()))
	nk.MetricsGaugeSet(
		"snoringcat_active_matches",
		map[string]string{"kind": allocatorModeEdgegap},
		float64(activeEdgegapMatches.Load()))
}
