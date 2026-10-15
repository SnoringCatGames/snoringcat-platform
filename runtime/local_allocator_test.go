package main

import "testing"

// SP26: Stop must return the match's port pair to the pool. The
// pool has 100 pairs by default; leaking one pair per local match
// exhausts it after 100 matches since a runtime restart.
func TestLocalPortPoolReleaseForReuse(t *testing.T) {
	a := &LocalDockerAllocator{
		ports: newLocalPortPool(31000, 31000, 31100, 31100),
	}
	udp, tcp, err := a.ports.allocate()
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if _, _, err := a.ports.allocate(); err == nil {
		t.Fatal("second allocate should fail: pool of one is in use")
	}

	a.trackPorts("local-test", udp, tcp)
	if !a.releasePortsFor("local-test") {
		t.Fatal("releasePortsFor should report a tracked pair")
	}
	if _, _, err := a.ports.allocate(); err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
	if a.releasePortsFor("local-test") {
		t.Fatal("double release should report no tracked pair")
	}
	if a.releasePortsFor("local-unknown") {
		t.Fatal("unknown request should report no tracked pair")
	}
}
