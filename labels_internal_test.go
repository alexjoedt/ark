package ark

import (
	"encoding/json"
	"strings"
	"testing"
)

// The manifest keeps the key source_meta that snapshots written before
// Snapshot.Labels used, so their labels read back unchanged.
func TestManifestLabelsKeepTheSourceMetaKey(t *testing.T) {
	var m manifest
	if err := json.Unmarshal([]byte(`{"version":3,"source_meta":{"docker_host":"tcp://h:2375"}}`), &m); err != nil {
		t.Fatal(err)
	}
	if got := manifestToSnapshot(&m).Labels["docker_host"]; got != "tcp://h:2375" {
		t.Fatalf("old label reads %q", got)
	}
	data, err := json.Marshal(&manifest{Labels: map[string]string{"app": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"source_meta":{"app":"x"}`) {
		t.Fatalf("labels not under source_meta: %s", data)
	}
}
