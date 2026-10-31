package main

import (
	"sync"
	"time"
)

// partyStartWindow is how long a party's start counts as
// the live attempt. The client's roster wait is 20 s (the
// leader gives members that long to join the realtime
// party), so a second start inside the window can only be
// the same attempt firing twice.
const partyStartWindow = 20 * time.Second

// partyStartGuard keeps one live start per party. State is
// in-memory only: a runtime restart forgets recent starts,
// which at worst allows one duplicate fan-out.
type partyStartGuard struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newPartyStartGuard() *partyStartGuard {
	return &partyStartGuard{last: map[string]time.Time{}}
}

// tryStart records a start for the party and reports
// whether it may proceed.
func (g *partyStartGuard) tryStart(
	partyID string, now time.Time,
) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t, ok := g.last[partyID]; ok &&
		now.Sub(t) < partyStartWindow {
		return false
	}
	g.last[partyID] = now
	return true
}
