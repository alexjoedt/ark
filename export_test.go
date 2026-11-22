package ark_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

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

// specialSource yields a file, a fifo and both device kinds, the snapshot a
// privileged backup of such a tree stores.
type specialSource struct{}

func (specialSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: "special", Type: ark.SourceTypeFile}
}

func (specialSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	mtime := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	if err := fn(ark.FileInfo{Path: "a.txt", Size: 1, Mode: 0o644, ModTime: mtime, Type: ark.FileTypeRegular},
		openReader(strings.NewReader("a"))); err != nil {
		return err
	}
	for _, fi := range []ark.FileInfo{
		{Path: "pipe", Mode: 0o640, Type: ark.FileTypeFifo},
		{Path: "null", Mode: 0o666, Type: ark.FileTypeCharDevice, DevMajor: 1, DevMinor: 3},
		{Path: "loop", Mode: 0o660, Type: ark.FileTypeBlockDevice, DevMajor: 7, DevMinor: 5},
	} {
		fi.UID, fi.GID, fi.ModTime = 1000, 1000, mtime
		if err := fn(fi, nil); err != nil {
			return err
		}
	}
	return nil
}

// tarHeaders reads an uncompressed tar and returns its headers by name.
func tarHeaders(t *testing.T, archive []byte) map[string]tar.Header {
	t.Helper()
	headers := make(map[string]tar.Header)
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return headers
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		headers[h.Name] = *h
	}
}

func exportSnapshot(t *testing.T, repo *ark.Repository, id ark.SnapshotID) []byte {
	t.Helper()
	var archive bytes.Buffer
	if err := repo.Export(context.Background(), id, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return archive.Bytes()
}

// TestExportCarriesSpecialFiles: a fifo and both device kinds leave as tar's
// own types with their device numbers, and an import of that archive exports
// the same headers again.
func TestExportCarriesSpecialFiles(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	snap, err := repo.Backup(ctx, specialSource{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	archive := exportSnapshot(t, repo, snap.ID)

	got := tarHeaders(t, archive)
	for name, want := range map[string]struct {
		typ          byte
		mode         int64
		major, minor int64
	}{
		"data/pipe": {tar.TypeFifo, 0o640, 0, 0},
		"data/null": {tar.TypeChar, 0o666, 1, 3},
		"data/loop": {tar.TypeBlock, 0o660, 7, 5},
	} {
		h := got[name]
		if h.Typeflag != want.typ || h.Mode != want.mode || h.Devmajor != want.major || h.Devminor != want.minor {
			t.Errorf("%s exported as type %q mode %#o %d,%d, want %q %#o %d,%d", name,
				h.Typeflag, h.Mode, h.Devmajor, h.Devminor, want.typ, want.mode, want.major, want.minor)
		}
	}

	if tarBin, err := exec.LookPath("tar"); err == nil {
		cmd := exec.Command(tarBin, "-tvf", "-")
		cmd.Stdin = bytes.NewReader(archive)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("tar -tv: %v", err)
		}
		for name, kind := range map[string]byte{"data/pipe": 'p', "data/null": 'c', "data/loop": 'b'} {
			found := false
			for line := range strings.SplitSeq(string(out), "\n") {
				if strings.HasSuffix(line, " "+name) && line[0] == kind {
					found = true
				}
			}
			if !found {
				t.Errorf("tar -tv lists no %c entry %s:\n%s", kind, name, out)
			}
		}
	}

	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	again := tarHeaders(t, exportSnapshot(t, target, imported.ID))
	for _, name := range []string{"data/pipe", "data/null", "data/loop"} {
		if !reflect.DeepEqual(again[name], got[name]) {
			t.Errorf("%s exports after an import as\n%+v\nwant\n%+v", name, again[name], got[name])
		}
	}
}

// repackExport copies the entries of an export into a new archive and lets add
// append to it: entries through tw, whole blocks through raw after a
// tw.Flush.
func repackExport(t *testing.T, export []byte, add func(tw *tar.Writer, raw io.Writer)) []byte {
	t.Helper()
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	tr := tar.NewReader(bytes.NewReader(export))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading the export: %v", err)
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatalf("copy: %v", err)
		}
	}
	add(tw, &archive)
	if err := tw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return archive.Bytes()
}

// TestImportSkipsATarTypeArkDoesNotModel: an entry of a tar type the tree has
// no type for is counted as skipped and left out.
func TestImportSkipsATarTypeArkDoesNotModel(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "payload"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	archive := repackExport(t, exportSnapshot(t, repo, snap.ID), func(tw *tar.Writer, _ io.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "data/vendor", Typeflag: 'Z', Mode: 0o644}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
	})

	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.Stats.Skipped != 1 {
		t.Errorf("the import counts %d skipped entries, want 1", imported.Stats.Skipped)
	}
	if _, ok := tarHeaders(t, exportSnapshot(t, target, imported.ID))["data/vendor"]; ok {
		t.Error("the entry of an unknown tar type came back in the export")
	}
}

// oldGNUSparse returns the blocks of a GNU sparse entry, as tar --sparse
// writes it: a file of size bytes whose only data sits at offset.
func oldGNUSparse(t *testing.T, name string, offset, size int64, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Format: tar.FormatGNU}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write([]byte(data)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	block := buf.Bytes()
	block[156] = tar.TypeGNUSparse
	copy(block[386:], fmt.Sprintf("%011o\x00%011o\x00", offset, len(data))) // first sparse map entry
	copy(block[483:], fmt.Sprintf("%011o\x00", size))                       // real size
	copy(block[148:156], "        ")
	sum := 0
	for _, b := range block[:512] {
		sum += int(b)
	}
	copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return block
}

// TestImportReadsContiguousAndSparseEntriesAsFiles: a contiguous file and a
// GNU sparse file are regular files with content, so an import stores them
// instead of counting them as skipped.
func TestImportReadsContiguousAndSparseEntriesAsFiles(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "payload"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	archive := repackExport(t, exportSnapshot(t, repo, snap.ID), func(tw *tar.Writer, raw io.Writer) {
		hdr := &tar.Header{Name: "data/contiguous", Typeflag: tar.TypeCont, Mode: 0o644, Size: 3}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte("abc")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := tw.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		raw.Write(oldGNUSparse(t, "data/sparse", 1024, 4096, "data"))
	})

	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported.Stats.Skipped != 0 {
		t.Errorf("the import counts %d skipped entries, want 0", imported.Stats.Skipped)
	}

	want := map[string]string{
		"data/contiguous": "abc",
		"data/sparse":     strings.Repeat("\x00", 1024) + "data" + strings.Repeat("\x00", 4096-1024-4),
	}
	tr := tar.NewReader(bytes.NewReader(exportSnapshot(t, target, imported.ID)))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the export: %v", err)
		}
		content, ok := want[h.Name]
		if !ok {
			continue
		}
		delete(want, h.Name)
		got, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
		if h.Typeflag != tar.TypeReg || string(got) != content {
			t.Errorf("%s came back as type %q with %d bytes, want a regular file of %d bytes with the same content",
				h.Name, h.Typeflag, len(got), len(content))
		}
	}
	for name := range want {
		t.Errorf("%s is missing from the export of the import", name)
	}
}

// TestExportCarriesALargeDeviceNumberBesideXattrs: a device number beyond the
// header field leaves in a PAX record when the entry has xattrs, which rule
// out the GNU format, and an import reads it back.
func TestExportCarriesALargeDeviceNumberBesideXattrs(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())
	const large = 0x300000
	snap, err := repo.Backup(ctx, entrySource{opened: new(int), entries: []ark.FileInfo{
		{Path: "labelled", Mode: 0o600, Type: ark.FileTypeCharDevice, DevMajor: 1, DevMinor: large,
			Xattrs: map[string][]byte{"user.ark": []byte("value")}},
		{Path: "plain", Mode: 0o600, Type: ark.FileTypeBlockDevice, DevMajor: large, DevMinor: 1},
	}})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	got := tarHeaders(t, exportSnapshot(t, repo, snap.ID))
	if h := got["data/labelled"]; h.Devmajor != 1 || h.PAXRecords["SCHILY.devminor"] != "3145728" ||
		h.PAXRecords["SCHILY.xattr.user.ark"] != "value" {
		t.Errorf("labelled exported as %d,%d with records %v, want major 1, the minor and the xattr in records",
			h.Devmajor, h.Devminor, h.PAXRecords)
	}
	if h := got["data/plain"]; h.Devmajor != large || h.Devminor != 1 {
		t.Errorf("plain exported as %d,%d, want %d,1", h.Devmajor, h.Devminor, large)
	}

	target := openRepo(t, arkmem.New())
	imported, err := target.Import(ctx, bytes.NewReader(exportSnapshot(t, repo, snap.ID)))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	again := tarHeaders(t, exportSnapshot(t, target, imported.ID))
	for _, name := range []string{"data/labelled", "data/plain"} {
		if !reflect.DeepEqual(again[name], got[name]) {
			t.Errorf("%s exports after an import as\n%+v\nwant\n%+v", name, again[name], got[name])
		}
	}
}
