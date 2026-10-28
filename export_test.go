package ark_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/alexjoedt/ark/arkmem"
)

// tarNames reads an uncompressed tar and returns its entry names in order.
func tarNames(t *testing.T, archive []byte) []string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		names = append(names, h.Name)
	}
	return names
}

func TestExportWritesPlainTarWithMetadataFirst(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var archive bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Any tar tool reads it: no gzip wrapper.
	if b := archive.Bytes(); len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		t.Fatal("the archive is gzipped, the library must not compress it")
	}

	names := tarNames(t, archive.Bytes())
	if len(names) == 0 {
		t.Fatal("the archive is empty")
	}
	if names[0] != "metadata.json" {
		t.Errorf("the first entry is %q, want metadata.json", names[0])
	}
	for _, name := range names[1:] {
		if len(name) < 5 || name[:5] != "data/" {
			t.Errorf("entry %q is outside data/", name)
		}
	}
}

func TestExportMetadataIsTheSnapshotManifest(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var archive bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}

	tr := tar.NewReader(bytes.NewReader(archive.Bytes()))
	if _, err := tr.Next(); err != nil {
		t.Fatalf("reading the first entry: %v", err)
	}
	var meta struct {
		ID         string `json:"id"`
		SourceName string `json:"source_name"`
	}
	if err := json.NewDecoder(tr).Decode(&meta); err != nil {
		t.Fatalf("metadata.json is not JSON: %v", err)
	}
	if meta.ID != snap.ID {
		t.Errorf("metadata names snapshot %q, want %q", meta.ID, snap.ID)
	}
	if meta.SourceName != "app" {
		t.Errorf("metadata names source %q, want app", meta.SourceName)
	}
}

func TestImportRechunksIntoANewSnapshot(t *testing.T) {
	ctx := context.Background()
	source := openRepo(t, arkmem.New())

	snap, err := source.Backup(ctx, stubSource{name: "app", content: "payload"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	var archive bytes.Buffer
	if err := source.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}

	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.SourceName != "app" {
		t.Errorf("imported source is %q, want app", imported.SourceName)
	}
	if imported.Stats.FileCount != 1 {
		t.Errorf("imported snapshot counts %d files, want 1", imported.Stats.FileCount)
	}
	if imported.Stats.ChunksTotal == 0 {
		t.Error("the imported snapshot recorded no chunks, so it was not re-chunked")
	}

	sink := &recordingSink{}
	if _, err := target.Restore(ctx, imported.ID, sink); err != nil {
		t.Fatalf("Restore of the imported snapshot: %v", err)
	}
	if got := sink.written["data.txt"]; got != "payload" {
		t.Errorf("restored %q, want payload", got)
	}
}

// legacyArchive builds what the released v1.1.0 binary wrote: a gzipped tar
// whose metadata.json is the old backup metadata, not an ark manifest.
func legacyArchive(t *testing.T) []byte {
	t.Helper()

	meta := map[string]any{
		"id":         "01abcdefghijklmnopqrstuvwx",
		"created_at": time.Date(2025, 3, 2, 10, 0, 0, 0, time.UTC),
		"source": map[string]any{
			"volume": "pgdata",
			"labels": map[string]string{"env": "prod"},
		},
		"backup": map[string]any{
			"strategy": "offline",
			"comment":  "nightly",
		},
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal legacy metadata: %v", err)
	}

	var raw bytes.Buffer
	gw := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gw)

	write := func(h *tar.Header, body string) {
		t.Helper()
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("write header %s: %v", h.Name, err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatalf("write body %s: %v", h.Name, err)
			}
		}
	}

	write(&tar.Header{Name: "metadata.json", Mode: 0o644, Size: int64(len(metaJSON))}, string(metaJSON))
	write(&tar.Header{Name: "data/conf", Mode: 0o755, Typeflag: tar.TypeDir}, "")
	write(&tar.Header{
		Name: "data/conf/pg.conf", Mode: 0o644, Size: int64(len("shared_buffers = 128MB")),
		Typeflag: tar.TypeReg,
	}, "shared_buffers = 128MB")

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return raw.Bytes()
}

func TestImportAcceptsAnArchiveFromTheV110Binary(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	imported, err := repo.Import(ctx, bytes.NewReader(legacyArchive(t)))
	if err != nil {
		t.Fatalf("Import of a v1.1.0 archive: %v", err)
	}

	if imported.SourceName != "pgdata" {
		t.Errorf("imported source is %q, want pgdata", imported.SourceName)
	}
	if imported.Comment != "nightly" {
		t.Errorf("imported comment is %q, want nightly", imported.Comment)
	}
	if imported.Stats.FileCount != 1 || imported.Stats.DirCount != 1 {
		t.Errorf("imported stats count %d files and %d dirs, want 1 and 1",
			imported.Stats.FileCount, imported.Stats.DirCount)
	}

	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, imported.ID, sink); err != nil {
		t.Fatalf("Restore of the imported legacy snapshot: %v", err)
	}
	if got := sink.written["conf/pg.conf"]; got != "shared_buffers = 128MB" {
		t.Errorf("restored %q from the legacy archive", got)
	}
}

// A caller that wants compression wraps the stream, and Import takes it back.
func TestImportAcceptsACallerCompressedArchive(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var compressed bytes.Buffer
	gw := gzip.NewWriter(&compressed)
	if err := repo.Export(ctx, snap.ID, gw); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	target := openRepo(t, arkmem.New())
	if _, err := target.Import(ctx, bytes.NewReader(compressed.Bytes())); err != nil {
		t.Fatalf("Import of a gzipped export: %v", err)
	}
}
