package ark_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/arktest"
)

func TestStreamConformance(t *testing.T) {
	arktest.RunStreamTests(t,
		func(name string, r io.Reader) ark.Source { return ark.NewStreamSource(name, r) },
		func(w io.Writer) ark.Sink { return ark.NewStreamSink(w) },
	)
}

func TestStreamSourceRejectsNamesThatAreNotBaseNames(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "/abs", "dir/"} {
		err := ark.NewStreamSource(name, strings.NewReader("x")).Walk(context.Background(),
			func(ark.FileInfo, func() (io.ReadCloser, error)) error { return nil })
		if err == nil {
			t.Errorf("Walk with name %q = nil, want an error", name)
		}
	}
}

// closeTracker records a Close it should never get.
type closeTracker struct {
	io.Reader
	io.Writer
	closed bool
}

func (c *closeTracker) Close() error { c.closed = true; return nil }

func TestStreamSnapshotHasZeroModTimeAndIsReadEveryRun(t *testing.T) {
	ctx := context.Background()
	repo := openRepo(t, arkmem.New())

	for run := range 2 {
		r := &closeTracker{Reader: strings.NewReader("same bytes every run")}
		snap, err := repo.Backup(ctx, ark.NewStreamSource("dump.sql", r))
		if err != nil {
			t.Fatalf("Backup %d: %v", run, err)
		}
		if r.closed {
			t.Error("Backup closed the caller's reader")
		}
		if snap.SourceType != ark.SourceTypeStream || snap.SourceName != "dump.sql" {
			t.Errorf("snapshot source %s/%s, want %s/dump.sql", snap.SourceType, snap.SourceName, ark.SourceTypeStream)
		}
		if snap.Stats.FileCount != 1 || snap.Stats.FilesUnchanged != 0 {
			t.Errorf("run %d: FileCount %d, FilesUnchanged %d, want 1 and 0",
				run, snap.Stats.FileCount, snap.Stats.FilesUnchanged)
		}
		var entries []ark.FileInfo
		if err := repo.Walk(ctx, snap.ID, func(e ark.FileInfo) error {
			entries = append(entries, e)
			return nil
		}); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		var files int
		for _, e := range entries {
			if e.Type != ark.FileTypeRegular {
				continue
			}
			files++
			if e.Path != "dump.sql" || !e.ModTime.IsZero() || e.Mode.Perm() != 0o600 {
				t.Errorf("entry %s mtime %v mode %v, want dump.sql, zero, 0600", e.Path, e.ModTime, e.Mode.Perm())
			}
		}
		if files != 1 {
			t.Errorf("snapshot holds %d regular files, want 1", files)
		}

		w := &closeTracker{Writer: &bytes.Buffer{}}
		if _, err := repo.Restore(ctx, snap.ID, ark.NewStreamSink(w)); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if w.closed {
			t.Error("Restore closed the caller's writer")
		}
	}
}

func TestStreamSinkTakesOneRegularFile(t *testing.T) {
	ctx := context.Background()
	meta := ark.SourceMeta{Name: "dump", Type: ark.SourceTypeStream}
	receive := func(t *testing.T, entries ...ark.FileInfo) (string, error) {
		t.Helper()
		var buf bytes.Buffer
		sink := ark.NewStreamSink(&buf)
		if err := sink.Open(ctx, meta); err != nil {
			t.Fatalf("Open: %v", err)
		}
		for _, e := range entries {
			var r io.Reader
			if e.Type == ark.FileTypeRegular {
				r = strings.NewReader(e.Path)
			}
			if err := sink.Receive(ctx, e, r); err != nil {
				return buf.String(), err
			}
		}
		return buf.String(), sink.Close()
	}
	dir := ark.FileInfo{Path: "d", Type: ark.FileTypeDirectory}
	file := ark.FileInfo{Path: "first", Type: ark.FileTypeRegular}

	got, err := receive(t, dir, file, dir)
	if err != nil || got != "first" {
		t.Errorf("dir, file, dir = %q, %v; want the file's bytes and no error", got, err)
	}

	for _, bad := range []ark.FileInfo{
		{Path: "second", Type: ark.FileTypeRegular},
		{Path: "link", Type: ark.FileTypeSymlink, LinkDest: "first"},
		{Path: "hard", Type: ark.FileTypeHardlink, LinkDest: "first"},
	} {
		_, err := receive(t, file, bad)
		if err == nil || !strings.Contains(err.Error(), bad.Path) {
			t.Errorf("%s after a file = %v, want an error naming it", bad.Type, err)
		}
	}

	err = ark.NewStreamSink(io.Discard).Open(ctx, ark.SourceMeta{Type: ark.SourceTypeFile})
	if !errors.Is(err, ark.ErrIncompatibleSourceType) {
		t.Errorf("Open of a file snapshot = %v, want ErrIncompatibleSourceType", err)
	}
}
