package ark_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
		if err := fn(info, openReader(strings.NewReader(content))); err != nil {
			return err
		}
	}
	return nil
}

// countingBackend records every chunk read, the bytes it asked for and how
// many reads ran at once. Chunks are read as ranges of their data pack, so a
// read counts by pack and offset. delay holds every data pack read, the round
// trip of a remote backend. Whole tree pack reads count apart, by pack.
type countingBackend struct {
	ark.Backend
	delay time.Duration

	mu       sync.Mutex
	gets     map[string]int
	bytes    int64
	treeGets map[string]int
	inABlob  int
	peak     int
}

func newCountingBackend() *countingBackend {
	return &countingBackend{Backend: arkmem.New(), gets: make(map[string]int), treeGets: make(map[string]int)}
}

func (b *countingBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	if h.Type == ark.DataPackFile {
		defer b.enter(h.Name)()
	}
	if h.Type == ark.TreePackFile {
		b.mu.Lock()
		b.treeGets[h.Name]++
		b.mu.Unlock()
	}
	return b.Backend.Get(ctx, h)
}

func (b *countingBackend) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	if h.Type == ark.DataPackFile {
		defer b.enter(fmt.Sprintf("%s@%d", h.Name, offset))()
		b.mu.Lock()
		b.bytes += length
		b.mu.Unlock()
		time.Sleep(b.delay)
	}
	return b.Backend.GetRange(ctx, h, offset, length)
}

// enter counts one chunk read as running until the returned func is called.
func (b *countingBackend) enter(name string) (leave func()) {
	b.mu.Lock()
	b.gets[name]++
	b.inABlob++
	if b.inABlob > b.peak {
		b.peak = b.inABlob
	}
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.inABlob--
		b.mu.Unlock()
	}
}

func (b *countingBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets = make(map[string]int)
	b.bytes = 0
	b.treeGets = make(map[string]int)
	b.peak = 0
}

func (b *countingBackend) readBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytes
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
	meta    ark.SourceMeta
	got     []string
	failOn  string
	written map[string]string
}

func (s *recordingSink) Open(_ context.Context, meta ark.SourceMeta) error {
	s.meta = meta
	return nil
}

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
func chunkyRepo(t *testing.T, b ark.Backend, opts ...ark.Option) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: 16 << 10}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword, opts...)
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

	// Four workers and a 64 KiB budget over 16 KiB chunks: the restore may
	// hold a handful of chunks ahead of the sink, never the snapshot.
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: 16 << 10}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, testPassword, ark.WithConcurrency(4))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer repo.Close()
	ark.SetPrefetchBytes(repo, 64<<10)

	snap, err := repo.Backup(ctx, bigFiles(t))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if unique := snap.Stats.ChunksNew; unique < 20 {
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
	// sink ever saw the first file. A bounded one reads a budget's worth, plus
	// the chunks of other files between those it wants.
	if fetched, stored := b.readBytes(), snap.Stats.StoredSize; fetched > stored/2 {
		t.Errorf("the restore read %d bytes before failing on the first file, snapshot stores %d",
			fetched, stored)
	}
}

// cancellingSink cancels the restore from inside Receive of one path.
type cancellingSink struct {
	recordingSink
	cancelOn string
	cancel   context.CancelFunc
}

func (s *cancellingSink) Receive(ctx context.Context, info ark.FileInfo, r io.Reader) error {
	if info.Path == s.cancelOn {
		s.cancel()
	}
	return s.recordingSink.Receive(ctx, info, r)
}

func TestRestoreStopsItsPrefetchWithoutLeakingGoroutines(t *testing.T) {
	b := newCountingBackend()
	repo := chunkyRepo(t, b)
	snap, err := repo.Backup(context.Background(), bigFiles(t))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	cases := map[string]func(ctx context.Context) (ark.Sink, context.Context){
		"sink failure": func(ctx context.Context) (ark.Sink, context.Context) {
			return &recordingSink{failOn: "a/two.bin"}, ctx
		},
		"context cancelled": func(ctx context.Context) (ark.Sink, context.Context) {
			ctx, cancel := context.WithCancel(ctx)
			t.Cleanup(cancel)
			return &cancellingSink{cancelOn: "a/two.bin", cancel: cancel}, ctx
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			before := runtime.NumGoroutine()
			sink, ctx := setup(context.Background())
			if _, err := repo.Restore(ctx, snap.ID, sink, ark.WithRestoreCache(0)); err == nil {
				t.Fatal("Restore succeeded although it was stopped")
			}
			deadline := time.Now().Add(5 * time.Second)
			for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if after := runtime.NumGoroutine(); after > before {
				t.Errorf("%d goroutines before the restore, %d after it was stopped", before, after)
			}
		})
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
	// One writer stores the chunks in walk order, so the chunks a restore
	// wants next are adjacent and no read covers a chunk it does not want.
	repo := chunkyRepo(t, b, ark.WithConcurrency(1))

	src := bigFiles(t)
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	stored := snap.Stats.StoredSize

	// Two files share their content, and a budget of one chunk has the
	// first file read before the second asks for anything: only a cache
	// turns the second read of every shared chunk into a hit.
	ark.SetPrefetchBytes(repo, 1)
	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}); err != nil {
		t.Fatalf("Restore with the default cache: %v", err)
	}
	cached := b.readBytes()

	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}, ark.WithRestoreCache(0)); err != nil {
		t.Fatalf("Restore without a cache: %v", err)
	}
	uncached := b.readBytes()

	if cached >= uncached {
		t.Errorf("the cache saved nothing: %d bytes read with it, %d without", cached, uncached)
	}
	if cached != stored {
		t.Errorf("the cached restore read %d bytes, the snapshot stores %d", cached, stored)
	}

	// With the budget to want both files at once, and reads slow enough for
	// that to happen, a miss of the second waits for the first one's read
	// instead of repeating it, cache or not.
	ark.SetPrefetchBytes(repo, 0)
	b.delay = 20 * time.Millisecond
	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}, ark.WithRestoreCache(0)); err != nil {
		t.Fatalf("Restore without a cache: %v", err)
	}
	if shared := b.readBytes(); shared != stored {
		t.Errorf("the uncached restore read %d bytes, the snapshot stores %d", shared, stored)
	}
}

func TestRestorePathsPruneTheWalk(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()
	// Chunks in walk order: a read of b/three.bin covers none of a/.
	repo := chunkyRepo(t, b, ark.WithConcurrency(1))

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
	fullReads := b.readBytes()

	b.reset()
	_, err = repo.Restore(ctx, snap.ID, &recordingSink{},
		ark.WithRestorePaths("b"), ark.WithRestoreCache(0))
	if err != nil {
		t.Fatalf("Restore with paths: %v", err)
	}
	if prunedReads := b.readBytes(); prunedReads >= fullReads {
		t.Errorf("restoring one directory read %d bytes, the whole snapshot reads %d", prunedReads, fullReads)
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

// TestRestoreReadsSmallFilesOfOnePackInAFewRanges restores a thousand small
// files whose chunks share one pack from a backend whose every read takes a
// round trip. A read per file would be a thousand round trips.
func TestRestoreReadsSmallFilesOfOnePackInAFewRanges(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()
	repo := chunkyRepo(t, b)
	src := smallFiles("app", 1000)
	snap := mustBackup(t, repo, src)

	b.delay = 20 * time.Millisecond
	b.reset()
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if reads := b.totalGets(); reads > 10 {
		t.Errorf("restoring %d small files of one pack took %d range reads, want at most 10", len(src.files), reads)
	}
	for path, want := range src.files {
		if got := sink.written[path]; got != want {
			t.Fatalf("%s restored as %q, want %q", path, got, want)
		}
	}
}

// TestRestoreReadsEveryTreePackOnce walks a thousand directories whose nodes
// fill a few tree packs: each pack is read whole once and serves every node
// in it.
func TestRestoreReadsEveryTreePackOnce(t *testing.T) {
	ctx := context.Background()
	b := newCountingBackend()
	repo := chunkyRepo(t, b)
	src := multiSource{name: "app", files: map[string]string{}}
	for i := range 1000 {
		dir := fmt.Sprintf("d%04d", i)
		src.dirs = append(src.dirs, dir)
		src.order = append(src.order, dir+"/f")
		src.files[dir+"/f"] = "content of " + dir
	}
	snap := mustBackup(t, repo, src)

	b.reset()
	if _, err := repo.Restore(ctx, snap.ID, &recordingSink{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.treeGets) == 0 {
		t.Fatal("the restore read no tree pack")
	}
	for pack, n := range b.treeGets {
		if n != 1 {
			t.Errorf("tree pack %s read %d times, want once", pack, n)
		}
	}
}
