package arktest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
)

// Entry is one filesystem entry the source and sink suite writes and reads
// back. It is the suite's own shape, close to ark.FileInfo but carrying the
// content instead of a reader.
type Entry struct {
	Path     string
	Type     ark.FileType
	Mode     os.FileMode
	Content  string
	LinkDest string
	Xattrs   map[string][]byte
}

// bogusSourceType is a type no implementation can restore. The suite uses it
// to check that Open rejects what it cannot handle.
const bogusSourceType ark.SourceType = "arktest-not-a-real-source-type"

// The modes the fixtures use. Ordinary permissions, so a sink that adjusts
// them to stay writable does not have to change them.
const (
	modeDir     os.FileMode = 0o755
	modeFile    os.FileMode = 0o644
	modePrivate os.FileMode = 0o600
	modeSymlink os.FileMode = 0o777
)

// RunSourceSinkTests checks one Source and Sink pair against the contract in
// package ark: what the Sink receives, the Source yields again.
//
// typ is the source type the pair produces and accepts; the suite opens the
// Sink under it. newSink returns a Sink over a fresh, empty medium and the
// location that medium can be reopened from; newSource opens a Source over
// such a location. The location is whatever names one medium for the
// implementation, a directory for arkfs and a volume name for arkdocker.
//
//	func TestSourceSinkConformance(t *testing.T) {
//		arktest.RunSourceSinkTests(t, ark.SourceTypeFile,
//			func(t *testing.T, loc string) ark.Source { ... },
//			func(t *testing.T) (ark.Sink, string) { ... },
//		)
//	}
func RunSourceSinkTests(
	t *testing.T,
	typ ark.SourceType,
	newSource func(t *testing.T, location string) ark.Source,
	newSink func(t *testing.T) (ark.Sink, string),
) {
	t.Helper()

	// The sink is opened under the pair's own type; a sink that only accepts
	// its own source type is asked with the one it knows.
	write := func(t *testing.T, entries []Entry) string {
		t.Helper()
		return write(t, newSink, ark.SourceMeta{Name: "arktest", Type: typ}, entries)
	}

	t.Run("RoundTrip", func(t *testing.T) { testSourceSinkRoundTrip(t, newSource, write) })
	t.Run("EmptyFileAndDirectory", func(t *testing.T) { testEmptyEntries(t, newSource, write) })
	t.Run("Hardlink", func(t *testing.T) { testHardlink(t, newSource, write) })
	t.Run("Symlink", func(t *testing.T) { testSymlink(t, newSource, write) })
	t.Run("Xattrs", func(t *testing.T) { testXattrs(t, newSource, write) })
	t.Run("IncompatibleSourceType", func(t *testing.T) { testIncompatibleSourceType(t, newSink) })
}

// writeFunc feeds entries into a fresh sink and returns the location to read back.
type writeFunc func(t *testing.T, entries []Entry) string

// write feeds entries into a fresh sink and returns the location to read back.
// Entries arrive in the order given, which is the order the core uses: parents
// before children, a hardlink after the entry it points at.
func write(t *testing.T, newSink func(t *testing.T) (ark.Sink, string), meta ark.SourceMeta, entries []Entry) string {
	t.Helper()
	ctx := context.Background()

	sink, location := newSink(t)
	if err := sink.Open(ctx, meta); err != nil {
		t.Fatalf("Sink.Open: %v", err)
	}

	for _, e := range entries {
		info := ark.FileInfo{
			Path:     e.Path,
			Size:     int64(len(e.Content)),
			Mode:     e.Mode,
			ModTime:  time.Now().Add(-time.Hour).Truncate(time.Second),
			Type:     e.Type,
			LinkDest: e.LinkDest,
			Xattrs:   e.Xattrs,
		}
		var r io.Reader
		if e.Type == ark.FileTypeRegular {
			r = io.LimitReader(bytes.NewReader([]byte(e.Content)), info.Size)
		}
		if err := sink.Receive(ctx, info, r); err != nil {
			t.Fatalf("Sink.Receive(%s): %v", e.Path, err)
		}
	}

	if err := sink.Close(); err != nil {
		t.Fatalf("Sink.Close: %v", err)
	}
	return location
}

// read walks a source and returns what it yielded, keyed by path.
func read(t *testing.T, newSource func(t *testing.T, location string) ark.Source, location string) map[string]Entry {
	t.Helper()
	ctx := context.Background()

	got := make(map[string]Entry)
	err := newSource(t, location).Walk(ctx, func(info ark.FileInfo, r io.Reader) error {
		e := Entry{
			Path:     info.Path,
			Type:     info.Type,
			Mode:     info.Mode.Perm(),
			LinkDest: info.LinkDest,
			Xattrs:   info.Xattrs,
		}
		content, err := readEntry(t, info, r)
		if err != nil {
			return err
		}
		e.Content = content
		got[info.Path] = e
		return nil
	})
	if err != nil {
		t.Fatalf("Source.Walk: %v", err)
	}
	return got
}

// readEntry consumes the reader a Source handed out, checking the contract
// that only regular files get one.
func readEntry(t *testing.T, info ark.FileInfo, r io.Reader) (string, error) {
	t.Helper()

	if info.Type != ark.FileTypeRegular {
		if r != nil {
			t.Errorf("Source yielded a reader for %s, which is a %s", info.Path, info.Type)
		}
		return "", nil
	}
	if r == nil {
		t.Errorf("Source yielded a nil reader for the regular file %s", info.Path)
		return "", nil
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("arktest: reading %s: %w", info.Path, err)
	}
	return string(data), nil
}

// roundTrip writes entries through a sink and reads them back through a source.
func roundTrip(
	t *testing.T,
	newSource func(t *testing.T, location string) ark.Source,
	write writeFunc,
	entries []Entry,
) map[string]Entry {
	t.Helper()
	return read(t, newSource, write(t, entries))
}

func testSourceSinkRoundTrip(
	t *testing.T,
	newSource func(t *testing.T, location string) ark.Source,
	write writeFunc,
) {
	entries := []Entry{
		{Path: "conf", Type: ark.FileTypeDirectory, Mode: modeDir},
		{Path: "conf/app.toml", Type: ark.FileTypeRegular, Mode: modeFile, Content: "key = 1"},
		{Path: "data.bin", Type: ark.FileTypeRegular, Mode: modePrivate, Content: "binary-ish\x00\xffcontent"},
	}

	got := roundTrip(t, newSource, write, entries)

	for _, want := range entries {
		e, ok := got[want.Path]
		if !ok {
			t.Errorf("%s did not come back, got %v", want.Path, paths(got))
			continue
		}
		if e.Type != want.Type {
			t.Errorf("%s came back as %q, want %q", want.Path, e.Type, want.Type)
		}
		if want.Type == ark.FileTypeRegular {
			if e.Content != want.Content {
				t.Errorf("%s came back with %q, want %q", want.Path, e.Content, want.Content)
			}
			if e.Mode != want.Mode {
				t.Errorf("%s came back with mode %v, want %v", want.Path, e.Mode, want.Mode)
			}
		}
	}
}

func testEmptyEntries(
	t *testing.T,
	newSource func(t *testing.T, location string) ark.Source,
	write writeFunc,
) {
	entries := []Entry{
		{Path: "empty-dir", Type: ark.FileTypeDirectory, Mode: modeDir},
		{Path: "empty-file", Type: ark.FileTypeRegular, Mode: modeFile},
	}

	got := roundTrip(t, newSource, write, entries)

	dir, ok := got["empty-dir"]
	if !ok {
		t.Errorf("the empty directory did not come back, got %v", paths(got))
	} else if dir.Type != ark.FileTypeDirectory {
		t.Errorf("empty-dir came back as %q, want %q", dir.Type, ark.FileTypeDirectory)
	}

	file, ok := got["empty-file"]
	if !ok {
		t.Errorf("the empty file did not come back, got %v", paths(got))
	} else {
		if file.Type != ark.FileTypeRegular {
			t.Errorf("empty-file came back as %q, want %q", file.Type, ark.FileTypeRegular)
		}
		if file.Content != "" {
			t.Errorf("empty-file came back with %q, want no content", file.Content)
		}
	}
}

func testHardlink(
	t *testing.T,
	newSource func(t *testing.T, location string) ark.Source,
	write writeFunc,
) {
	const content = "two names, one inode"
	entries := []Entry{
		{Path: "target.txt", Type: ark.FileTypeRegular, Mode: modeFile, Content: content},
		{Path: "link.txt", Type: ark.FileTypeHardlink, Mode: modeFile, LinkDest: "target.txt"},
	}

	got := roundTrip(t, newSource, write, entries)

	first, second := got["link.txt"], got["target.txt"]
	if first.Path == "" || second.Path == "" {
		t.Fatalf("the hardlink pair did not come back, got %v", paths(got))
	}

	// Which of the two names the source reports as the link depends on the
	// order it walks them, so the suite only asks that exactly one is.
	links := 0
	for _, e := range []Entry{first, second} {
		if e.Type == ark.FileTypeHardlink {
			links++
			if e.LinkDest != "target.txt" && e.LinkDest != "link.txt" {
				t.Errorf("the hardlink points at %q, want the other name of the pair", e.LinkDest)
			}
		}
	}
	if links != 1 {
		t.Errorf("%d of the two names came back as a hardlink, want exactly 1", links)
	}

	for _, e := range []Entry{first, second} {
		if e.Type == ark.FileTypeRegular && e.Content != content {
			t.Errorf("%s came back with %q, want %q", e.Path, e.Content, content)
		}
	}
}

func testSymlink(
	t *testing.T,
	newSource func(t *testing.T, location string) ark.Source,
	write writeFunc,
) {
	entries := []Entry{
		{Path: "target.txt", Type: ark.FileTypeRegular, Mode: modeFile, Content: "pointed at"},
		{Path: "link", Type: ark.FileTypeSymlink, Mode: modeSymlink, LinkDest: "target.txt"},
	}

	got := roundTrip(t, newSource, write, entries)

	link, ok := got["link"]
	if !ok {
		t.Fatalf("the symlink did not come back, got %v", paths(got))
	}
	if link.Type != ark.FileTypeSymlink {
		t.Errorf("link came back as %q, want %q", link.Type, ark.FileTypeSymlink)
	}
	if link.LinkDest != "target.txt" {
		t.Errorf("link points at %q, want target.txt", link.LinkDest)
	}
}

func testXattrs(
	t *testing.T,
	newSource func(t *testing.T, location string) ark.Source,
	write writeFunc,
) {
	want := map[string][]byte{"user.ark": []byte("value")}
	entries := []Entry{
		{Path: "labelled.txt", Type: ark.FileTypeRegular, Mode: modeFile, Content: "has xattrs", Xattrs: want},
	}

	got := roundTrip(t, newSource, write, entries)

	e, ok := got["labelled.txt"]
	if !ok {
		t.Fatalf("the file did not come back, got %v", paths(got))
	}
	if len(e.Xattrs) == 0 {
		t.Skip("this medium carries no extended attributes")
	}
	if string(e.Xattrs["user.ark"]) != "value" {
		t.Errorf("user.ark came back as %q, want %q", e.Xattrs["user.ark"], "value")
	}
}

func testIncompatibleSourceType(t *testing.T, newSink func(t *testing.T) (ark.Sink, string)) {
	sink, _ := newSink(t)

	err := sink.Open(context.Background(), ark.SourceMeta{Name: "arktest", Type: bogusSourceType})
	if err == nil {
		t.Fatalf("Sink.Open accepted the source type %q", bogusSourceType)
	}
	if !errors.Is(err, ark.ErrIncompatibleSourceType) {
		t.Errorf("Sink.Open returned %v, want an error wrapping ErrIncompatibleSourceType", err)
	}
}

// paths lists what came back, for a failure message that says what is there.
func paths(got map[string]Entry) []string {
	out := make([]string, 0, len(got))
	for p := range got {
		out = append(out, p)
	}
	return out
}
