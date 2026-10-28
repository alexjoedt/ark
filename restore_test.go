package ark_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/chunker"
)

// multiSource yields its directories and then its files, in order. The
// directories are not optional: the tree is built from the entries a source
// reports, so a file whose parent never appears lands in no node.
type multiSource struct {
	name  string
	dirs  []string
	files map[string]string
	order []string
}

func (s multiSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: ark.SourceTypeFile}
}

func (s multiSource) Walk(_ context.Context, fn ark.WalkFunc) error {
	for _, dir := range s.dirs {
		info := ark.FileInfo{Path: dir, Mode: 0o755, Type: ark.FileTypeDirectory}
		if err := fn(info, nil); err != nil {
			return err
		}
	}
	for _, path := range s.order {
		content := s.files[path]
		info := ark.FileInfo{
			Path: path,
			Size: int64(len(content)),
			Mode: 0o644,
			Type: ark.FileTypeRegular,
		}
		if err := fn(info, strings.NewReader(content)); err != nil {
			return err
		}
	}
	return nil
}

// countingBackend records every chunk read and how many ran at once.
type countingBackend struct {
	ark.Backend

	mu      sync.Mutex
	gets    map[string]int
	inABlob int
	peak    int
}

func newCountingBackend() *countingBackend {
	return &countingBackend{Backend: arkmem.New(), gets: make(map[string]int)}
}

func (b *countingBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if h.Type != ark.BlobFile {
		return b.Backend.Get(ctx, h)
	}
	b.mu.Lock()
	b.gets[h.Name]++
	b.inABlob++
	if b.inABlob > b.peak {
		b.peak = b.inABlob
	}
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.inABlob--
		b.mu.Unlock()
	}()
	return b.Backend.Get(ctx, h)
}

func (b *countingBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets = make(map[string]int)
	b.peak = 0
}

func (b *countingBackend) totalGets() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, c := range b.gets {
		n += c
	}
	return n
}

func (b *countingBackend) peakConcurrent() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// recordingSink notes the paths it received and can fail on one of them.
type recordingSink struct {
	got     []string
	failOn  string
	written map[string]string
}

func (s *recordingSink) Open(context.Context, ark.SourceMeta) error { return nil }

func (s *recordingSink) Receive(_ context.Context, info ark.FileInfo, r io.Reader) error {
	s.got = append(s.got, info.Path)
	if info.Path == s.failOn {
		return fmt.Errorf("sink refused %s", info.Path)
	}
	if r == nil {
		return nil
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if s.written == nil {
		s.written = make(map[string]string)
	}
	s.written[info.Path] = string(data)
	return nil
}

func (s *recordingSink) Close() error { return nil }

// payload returns half a megabyte of deterministic bytes. The content varies,
// so the chunker finds real boundaries, and the same seed gives the same bytes,
// so two files can share every chunk.
func payload(seed int64) string {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, 512<<10)
	for i := range b {
		b[i] = byte(r.Intn(256))
	}
	return string(b)
}

// chunkyRepo opens a repository whose chunks are small, so half a megabyte
// becomes tens of chunks and "how much did the restore read" is a number with
// something to say.
func chunkyRepo(t *testing.T, b ark.Backend) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: 16 << 10}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// bigFiles is a source whose files span many chunks, with two of them holding
// identical content so the cache has something to serve.
func bigFiles(t *testing.T) multiSource {
	t.Helper()
	shared := payload(1)
	return multiSource{
		name:  "app",
		dirs:  []string{"a", "b"},
		order: []string{"a/one.bin", "a/two.bin", "b/three.bin"},
		files: map[string]string{
			"a/one.bin":   shared,
			"a/two.bin":   shared, // identical content, same chunks
			"b/three.bin": payload(2),
		},
	}
}

func TestRestoreDoesNotFetchThatWholeSnapshotUpFront(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()
	repo := chunkyRepo(t, b)

	snap, err := repo.Backup(ctx, bigFiles(t))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	unique := snap.Stats.ChunksNew
	if unique < 20 {
		t.Fatalf("the fixture produced %d unique chunks, too few to tell streaming apart", unique)
	}

	// The sink rejects the first file. A restore that prefetched the snapshot
	// would already have read every chunk by then.
	b.reset()
	sink := &recordingSink{failOn: "a/one.bin"}
	if _, err := repo.Restore(ctx, snap.ID, sink, ark.WithRestoreCache(0)); err == nil {
		t.Fatal("Restore succeeded although the sink failed")
	}

	// A prefetching restore would have read every unique chunk before the
	// sink ever saw the first file. A streaming one reads a worker's worth.
	if fetched := b.totalGets(); fetched > unique/2 {
		t.Errorf("the restore read %d chunks before failing on the first file, snapshot holds %d unique",
			fetched, unique)
	}
}

func TestRestoreKeepsItsFetchesWithinTheConcurrencyBound(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()

	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: 16 << 10}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword, ark.WithConcurrency(2))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()

	snap, err := repo.Backup(ctx, bigFiles(t))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}, ark.WithRestoreCache(0)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if peak := b.peakConcurrent(); peak > 2 {
		t.Errorf("%d chunk reads ran at once, WithConcurrency(2) allows 2", peak)
	}
}

func TestRestoreCacheServesRepeatedContent(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()
	repo := chunkyRepo(t, b)

	src := bigFiles(t)
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Two files share their content, so a cache turns the second read of
	// every shared chunk into a hit.
	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}); err != nil {
		t.Fatalf("Restore with the default cache: %v", err)
	}
	cached := b.totalGets()

	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}, ark.WithRestoreCache(0)); err != nil {
		t.Fatalf("Restore without a cache: %v", err)
	}
	uncached := b.totalGets()

	if cached >= uncached {
		t.Errorf("the cache saved nothing: %d reads with it, %d without", cached, uncached)
	}
}

func TestRestorePathsPruneTheWalk(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()
	repo := chunkyRepo(t, b)

	snap, err := repo.Backup(ctx, bigFiles(t))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	b.reset()
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink, ark.WithRestorePaths("b")); err != nil {
		t.Fatalf("Restore with paths: %v", err)
	}

	for _, path := range sink.got {
		if strings.HasPrefix(path, "a/") {
			t.Errorf("restored %s, which is outside the requested paths", path)
		}
	}
	if _, ok := sink.written["b/three.bin"]; !ok {
		t.Errorf("b/three.bin was not restored, got %v", sink.got)
	}

	// The pruned files were never read, not merely dropped after reading.
	full := &recordingSink{}
	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, full, ark.WithRestoreCache(0)); err != nil {
		t.Fatalf("full Restore: %v", err)
	}
	fullReads := b.totalGets()

	b.reset()
	_, err = repo.Restore(ctx, snap.ID, &recordingSink{},
		ark.WithRestorePaths("b"), ark.WithRestoreCache(0))
	if err != nil {
		t.Fatalf("Restore with paths: %v", err)
	}
	if prunedReads := b.totalGets(); prunedReads >= fullReads {
		t.Errorf("restoring one directory read %d chunks, the whole snapshot reads %d", prunedReads, fullReads)
	}
}

func TestRestoreContentSurvivesStreaming(t *testing.T) {
	ctx := context.Background()
	repo := chunkyRepo(t, arkmem.New())

	src := bigFiles(t)
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	for _, limit := range []int64{0, 1 << 10, 1 << 30} {
		sink := &recordingSink{}
		if _, err := repo.Restore(ctx, snap.ID, sink, ark.WithRestoreCache(limit)); err != nil {
			t.Fatalf("Restore with cache %d: %v", limit, err)
		}
		for path, want := range src.files {
			if got := sink.written[path]; got != want {
				t.Errorf("cache %d: %s restored %d bytes, want %d", limit, path, len(got), len(want))
			}
		}
	}
}
