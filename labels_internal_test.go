package ark

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestManifestKeepsLabelsUnderLabels(t *testing.T) {
	data, err := json.Marshal(&manifest{Labels: map[string]string{"app": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"labels":{"app":"x"}`) {
		t.Fatalf("labels not under the key labels: %s", data)
	}
	var m manifest
	if err := decodeStrict("manifest", data, &m); err != nil {
		t.Fatal(err)
	}
	if got := manifestToSnapshot(&m).Labels["app"]; got != "x" {
		t.Fatalf("label reads %q", got)
	}
}

// The ratios follow from the counters: a manifest does not store them, and a
// loaded snapshot has them all the same.
func TestManifestStoresNoRatios(t *testing.T) {
	stats := SnapshotStats{TotalSize: 100, NewChunksRawSize: 50, StoredSize: 10}
	data, err := json.Marshal(&manifest{Stats: storedStats(stats)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "_ratio") {
		t.Fatalf("manifest stores a ratio: %s", data)
	}
	var m manifest
	if err := decodeStrict("manifest", data, &m); err != nil {
		t.Fatal(err)
	}
	got := manifestToSnapshot(&m).Stats
	if got.DeduplicationRatio != 0.5 || got.CompressionRatio != 0.8 {
		t.Errorf("loaded ratios %v and %v, want 0.5 and 0.8", got.DeduplicationRatio, got.CompressionRatio)
	}
}
