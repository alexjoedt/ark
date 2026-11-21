package ark_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"github.com/alexjoedt/ark"
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
	if meta.ID != string(snap.ID) {
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

// TestImportNamesAForeignIDByItsHash: an archive is outside input and may
// carry an ID of any form. The copy is named by a hash of it, so the name is
// hex, shows nothing of the ID, and a second import of the archive collides.
func TestImportNamesAForeignIDByItsHash(t *testing.T) {
	ctx := context.Background()
	const foreign = "01jd3k9s5w0000000000000000"
	meta := `{"version":1,"id":"` + foreign + `","created_at":"2025-03-02T10:00:00Z",` +
		`"source_name":"app","source_type":"file","host":"h","stats":{}}`

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, f := range []struct{ name, body string }{{"metadata.json", meta}, {"data/data.txt", "payload"}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatalf("write header %s: %v", f.name, err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatalf("write body %s: %v", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	repo := openRepo(t, arkmem.New())
	imported, err := repo.Import(ctx, bytes.NewReader(archive.Bytes()))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	sum := sha256.Sum256([]byte(foreign))
	if want := ark.SnapshotID(hex.EncodeToString(sum[:16])); imported.ID != want {
		t.Errorf("imported ID %q, want %q", imported.ID, want)
	}
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, imported.ID, sink); err != nil || sink.written["data.txt"] != "payload" {
		t.Errorf("Restore of the copy: %v, %q", err, sink.written["data.txt"])
	}
	if _, err := repo.Import(ctx, bytes.NewReader(archive.Bytes())); err == nil {
		t.Error("a second import of the same archive succeeded")
	}
	if all, err := repo.Snapshots(ctx, ark.SnapshotFilter{}); err != nil || len(all) != 1 {
		t.Errorf("after two imports Snapshots = %d, %v, want 1", len(all), err)
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
