package main

import (
	"context"
	"testing"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

// fakeReadyNk answers the ready gate's batched read from a
// fixed set of ready user ids. Any other method panics
// through the nil embedded interface.
type fakeReadyNk struct {
	runtime.NakamaModule

	ready map[string]bool
}

func (f *fakeReadyNk) StorageRead(
	_ context.Context, reads []*runtime.StorageRead,
) ([]*api.StorageObject, error) {
	out := []*api.StorageObject{}
	for _, r := range reads {
		if f.ready[r.UserID] {
			out = append(out, &api.StorageObject{
				Collection: r.Collection,
				Key:        r.Key,
				UserId:     r.UserID,
				Value:      `{"ready":true}`,
			})
		}
	}
	return out, nil
}

func TestAllMembersReady(t *testing.T) {
	nk := &fakeReadyNk{ready: map[string]bool{"u1": true, "u2": true}}
	ready, err := allMembersReady(
		context.Background(), nk, "party-1", []string{"u1", "u2"})
	if err != nil || !ready {
		t.Fatalf("all ready: ready=%v err=%v", ready, err)
	}
}

func TestOneUnreadyBlocksStart(t *testing.T) {
	nk := &fakeReadyNk{ready: map[string]bool{"u1": true}}
	ready, err := allMembersReady(
		context.Background(), nk, "party-1", []string{"u1", "u2"})
	if err != nil || ready {
		t.Fatalf("one unready: ready=%v err=%v", ready, err)
	}
}
