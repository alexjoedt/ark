package ark_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

func TestBackupLogNamesSourceAndHost(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var buf bytes.Buffer
	repo, err := ark.Open(ctx, b, ark.WithoutPassword(), ark.WithLogger(slog.New(slog.NewJSONHandler(&buf, nil))))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	if _, err := repo.Backup(ctx, stubSource{name: "app", content: "one"}, ark.WithHost("box")); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var found bool
	for line := range bytes.Lines(buf.Bytes()) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] != "backup completed" {
			continue
		}
		found = true
		if rec["source"] != "app" || rec["host"] != "box" {
			t.Errorf("backup completed logged source=%v host=%v, want app and box", rec["source"], rec["host"])
		}
	}
	if !found {
		t.Fatalf("no backup completed line in %s", buf.String())
	}
}
