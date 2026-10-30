//lint:file-ignore SA1029 nakama-common uses bare strings as context keys.
package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/heroiclabs/nakama-common/api"
	"github.com/heroiclabs/nakama-common/runtime"
)

type fakeSweepNk struct {
	runtime.NakamaModule
	objects []*api.StorageObject
	deletes []string
}

func (f *fakeSweepNk) StorageList(
	context.Context, string, string, string, int, string,
) ([]*api.StorageObject, string, error) {
	return f.objects, "", nil
}

func (f *fakeSweepNk) StorageDelete(
	_ context.Context, deletes []*runtime.StorageDelete,
) error {
	for _, d := range deletes {
		f.deletes = append(f.deletes, d.Collection+"/"+d.Key)
	}
	return nil
}

func TestSweepStaleMatchRows(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).Unix()
	fresh := time.Now().Unix()
	nk := &fakeSweepNk{objects: []*api.StorageObject{
		{Collection: matchMetadataCollection, Key: "stale-1",
			Value: fmt.Sprintf(`{"allocated_at":%d}`, old)},
		{Collection: matchMetadataCollection, Key: "live-1",
			Value: fmt.Sprintf(`{"allocated_at":%d}`, fresh)},
		{Collection: matchMetadataCollection, Key: "legacy-1",
			Value: `{}`},
	}}
	sweepStaleMatchRows(context.Background(), testLogger{}, nk)
	joined := strings.Join(nk.deletes, ",")
	for _, want := range []string{
		matchMetadataCollection + "/stale-1",
		"server_registrations/stale-1",
		syntheticMatchCollection + "/stale-1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing delete %s in %v", want, nk.deletes)
		}
	}
	if strings.Contains(joined, "live-1") ||
		strings.Contains(joined, "legacy-1") {
		t.Errorf("swept a live or legacy row: %v", nk.deletes)
	}
}
