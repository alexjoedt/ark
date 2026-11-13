package ark_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// opCountingBackend counts every request by method and handle type and runs
// onPut before each Put, whose error fails that Put.
type opCountingBackend struct {
	ark.Backend

	mu    sync.Mutex
	n     map[string]int
	onPut func(h ark.Handle) error
}

func newOpCountingBackend() *opCountingBackend {
	return &opCountingBackend{Backend: arkmem.New(), n: map[string]int{}}
}

func (b *opCountingBackend) count(op string, t ark.HandleType) func(ark.Handle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n[op+" "+t.String()]++
	return b.onPut
}

// take returns the requests since the last call, keyed "method namespace",
// without the lock a run takes and releases.
func (b *opCountingBackend) take() map[string]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.n
	b.n = map[string]int{}
	maps.DeleteFunc(n, func(k string, _ int) bool { return strings.HasSuffix(k, " "+ark.LockFile.String()) })
	return n
}

func (b *opCountingBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if onPut := b.count("put", h.Type); onPut != nil {
		if err := onPut(h); err != nil {
			return err
		}
	}
	return b.Backend.Put(ctx, h, r)
}

func (b *opCountingBackend) Get(ctx context.Context, h ark.Handle) (io.ReadCloser, error) {
	b.count("get", h.Type)
	return b.Backend.Get(ctx, h)
}

func (b *opCountingBackend) GetRange(ctx context.Context, h ark.Handle, offset, length int64) ([]byte, error) {
	b.count("range", h.Type)
	return b.Backend.GetRange(ctx, h, offset, length)
}

func (b *opCountingBackend) List(ctx context.Context, t ark.HandleType, fn func(ark.BlobInfo) error) error {
	b.count("list", t)
	return b.Backend.List(ctx, t, fn)
}

func (b *opCountingBackend) Exists(ctx context.Context, h ark.Handle) (bool, error) {
	b.count("exists", h.Type)
	return b.Backend.Exists(ctx, h)
}

func (b *opCountingBackend) Delete(ctx context.Context, h ark.Handle) error {
	b.count("delete", h.Type)
	return b.Backend.Delete(ctx, h)
}

// TestBackupOfSmallFilesWritesAFewObjects is the point of the packs: ten
// thousand small files are ten thousand chunks and a handful of objects.
func TestBackupOfSmallFilesWritesAFewObjects(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)

	src := multiSource{name: "small", files: map[string]string{}}
	for d := range 100 {
		src.dirs = append(src.dirs, fmt.Sprintf("d%02d", d))
		for f := range 100 {
			p := fmt.Sprintf("d%02d/f%02d", d, f)
			src.files[p] = fmt.Sprintf("content of file %d in directory %d", f, d)
			src.order = append(src.order, p)
		}
	}
	b.take()
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if snap.Stats.ChunksNew != len(src.files) {
		t.Fatalf("backup stored %d chunks, want one per file, %d", snap.Stats.ChunksNew, len(src.files))
	}
	got := b.take()
	puts := 0
	for op, n := range got {
		if strings.HasPrefix(op, "put ") {
			puts += n
		}
	}
	if puts >= 100 || got["put datapacks"] == 0 || got["put chunks"] != 0 || got["put trees"] != 0 {
		t.Errorf("backup of %d files issued %d Puts, want fewer than 100, all packs, index and manifest: %v",
			len(src.files), puts, got)
	}

	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !maps.Equal(sink.written, src.files) {
		t.Errorf("restore wrote %d files that differ from the %d backed up", len(sink.written), len(src.files))
	}
}

// TestUnchangedBackupReadsNoDataIndex pins what an incremental backup with no
// changed file costs: the tree index, loaded once, the parent's tree pack,
// fetched whole and once for all of its nodes, and one request for the
// lost-pack marker. It stores no chunk, so nothing asks the data index and no
// data pack is read.
func TestUnchangedBackupReadsNoDataIndex(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	ib := ark.Instrument(b)
	repo := openRepo(t, ib)

	t0 := time.Date(2026, 1, 2, 3, 4, 5, 600, time.UTC)
	files := map[string]statFile{}
	for d := range 20 {
		for f := range 3 {
			files[fmt.Sprintf("d%02d/f%d.txt", d, f)] = statFile{
				content: fmt.Sprintf("file %d of %d", f, d), mtime: t0, ctime: t0, inode: uint64(1 + d*3 + f),
			}
		}
	}
	if _, err := repo.Backup(ctx, newStatSource("app", files)); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	if n := len(blobNames(t, repo, ark.TreeFile)); n != 21 {
		t.Fatalf("first backup stored %d tree nodes, want 21", n)
	}

	b.take()
	before := ib.Stats()
	src := newStatSource("app", maps.Clone(files))
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if exists := ib.Stats().Exists.Calls - before.Exists.Calls; exists != 2 {
		t.Errorf("unchanged backup issued %d Exists, want 2 for the lost-pack marker, before the walk and "+
			"before the trees", exists)
	}
	if snap.Stats.FilesUnchanged != len(files) || len(src.opens) != 0 {
		t.Fatalf("second backup took %d of %d files unread, opened %v",
			snap.Stats.FilesUnchanged, len(files), src.opens)
	}
	got := b.take()
	for _, op := range []string{
		"list dataindex", "get dataindex", "list datapacks", "get datapacks", "range datapacks",
	} {
		if got[op] != 0 {
			t.Errorf("unchanged backup issued %d %q, want none: %v", got[op], op, got)
		}
	}
	if got["exists lostpacks"] != 2 {
		t.Errorf("unchanged backup asked for the lost-pack marker %d times, want twice: %v",
			got["exists lostpacks"], got)
	}
	if got["list treeindex"] != 1 || got["get treeindex"] != 1 {
		t.Errorf("unchanged backup loaded the tree index with %d List and %d Get, want 1 and 1: %v",
			got["list treeindex"], got["get treeindex"], got)
	}
	if got["get treepacks"] != 1 || got["range treepacks"] != 0 {
		t.Errorf("unchanged backup read the parent's 21 tree nodes with %d Get and %d range reads, "+
			"want its one tree pack fetched whole: %v", got["get treepacks"], got["range treepacks"], got)
	}
	puts := maps.Collect(func(yield func(string, int) bool) {
		for op, n := range got {
			if strings.HasPrefix(op, "put ") && !yield(op, n) {
				return
			}
		}
	})
	if len(puts) != 1 || puts["put snapshots"] != 1 {
		t.Errorf("unchanged backup stored %v, want only the manifest", puts)
	}
}

// TestImportWritesInBackupOrder: an import makes its objects present in the
// order a backup does.
func TestImportWritesInBackupOrder(t *testing.T) {
	ctx := context.Background()
	from := openRepo(t, arkmem.New())
	snap, err := from.Backup(ctx, stubSource{name: "app", content: victimContent})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	var archive bytes.Buffer
	if err := from.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}

	b := &syncRecordingBackend{Backend: arkmem.New()}
	repo := openRepo(t, b)
	b.takeEvents()
	imported, err := repo.Import(ctx, &archive)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if indexes, manifests := assertWriteOrder(t, repo, b.takeEvents()); indexes != 2 || manifests != 1 {
		t.Errorf("Import stored %d index objects and %d manifests, want 2 and 1", indexes, manifests)
	}
	if got, err := restoreData(t, repo, imported.ID); err != nil || got != victimContent {
		t.Errorf("Restore = %q, %v; want %q", got, err, victimContent)
	}
}

// TestBackupCheckpointsItsIndex interrupts a backup that crossed the
// checkpoint interval: the packs it had stored by then are listed in an index
// object written before its end, and the rerun stores none of their chunks
// again.
func TestBackupCheckpointsItsIndex(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)
	var ahead atomic.Int64
	ark.SetClock(repo, func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) })

	// Random bytes for three data packs, chunked to about 1 MiB each.
	data := make([]byte, 50<<20)
	_, _ = rand.Read(data)
	src := multiSource{name: "big", files: map[string]string{}}
	for i := range 5 {
		p := fmt.Sprintf("f%d", i)
		src.files[p] = string(data[i*10<<20 : (i+1)*10<<20])
		src.order = append(src.order, p)
	}

	errInterrupted := errors.New("interrupted")
	indexed := make(chan struct{})
	var checkpoint ark.Handle
	var once sync.Once
	var packs atomic.Int32
	b.onPut = func(h ark.Handle) error {
		switch h.Type { //nolint:exhaustive // only the objects the checkpoint orders
		case ark.DataIndexFile:
			once.Do(func() {
				checkpoint = h
				close(indexed)
			})
		case ark.DataPackFile:
			if packs.Add(1) == 1 {
				// Ten minutes pass while the first pack uploads.
				ahead.Store(int64(11 * time.Minute))
				return nil
			}
			// No later pack arrives, and it fails only once the checkpoint
			// of the first one is stored.
			select {
			case <-indexed:
			case <-time.After(10 * time.Second):
				return errors.New("no index object followed the first pack")
			}
			return errInterrupted
		}
		return nil
	}
	b.take()
	if _, err := repo.Backup(ctx, src); !errors.Is(err, errInterrupted) {
		t.Fatalf("Backup = %v, want the interrupt", err)
	}
	got := b.take()
	if got["put dataindex"] != 1 || got["put snapshots"] != 0 {
		t.Fatalf("interrupted backup stored %v, want one data index object and no manifest", got)
	}
	listed, blobs, err := ark.IndexedPacks(ctx, repo, checkpoint)
	if err != nil || len(listed) != 1 || blobs == 0 {
		t.Fatalf("checkpoint lists packs %v with %d chunks, %v; want the first pack", listed, blobs, err)
	}

	b.mu.Lock()
	b.onPut = nil
	b.mu.Unlock()
	ahead.Store(0)
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	// Every chunk is distinct, so the new ones are exactly those the
	// checkpoint did not list.
	if snap.Stats.ChunksNew != snap.Stats.ChunksTotal-blobs {
		t.Errorf("rerun stored %d of %d chunks, want all but the %d the checkpoint lists",
			snap.Stats.ChunksNew, snap.Stats.ChunksTotal, blobs)
	}
	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, snap.ID, sink); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !maps.Equal(sink.written, src.files) {
		t.Error("the restore of the rerun differs from the source")
	}
}

// packObjects returns the name and size of every pack and index object in b.
func packObjects(t *testing.T, b ark.Backend) map[ark.Handle]int64 {
	t.Helper()
	objects := map[ark.Handle]int64{}
	for _, typ := range []ark.HandleType{ark.DataPackFile, ark.TreePackFile, ark.DataIndexFile, ark.TreeIndexFile} {
		err := b.List(context.Background(), typ, func(info ark.BlobInfo) error {
			objects[ark.Handle{Type: typ, Name: info.Name}] = info.Size
			return nil
		})
		if err != nil {
			t.Fatalf("list %s: %v", typ, err)
		}
	}
	return objects
}

// TestPruneDeletesDeadAndUnlistedPacks: after a forget, prune deletes the
// packs that hold nothing a snapshot references, the packs a failed backup
// left without an index entry, and the index entries of what it deleted.
// The dry run reports the same without changing anything.
func TestPruneDeletesDeadAndUnlistedPacks(t *testing.T) {
	ctx := context.Background()
	b := newOpCountingBackend()
	repo := openRepo(t, b)

	gone, err := repo.Backup(ctx, stubSource{name: "gone", content: victimContent})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	goneObjects := packObjects(t, b)
	kept, err := repo.Backup(ctx, stubSource{name: "kept", content: otherContent})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	keptPacks := packObjects(t, b)
	maps.DeleteFunc(keptPacks, func(h ark.Handle, _ int64) bool {
		_, old := goneObjects[h]
		return old || h.Type == ark.DataIndexFile || h.Type == ark.TreeIndexFile
	})

	// A backup that dies between its packs and its index leaves packs no
	// index lists.
	errInterrupted := errors.New("interrupted")
	b.onPut = func(h ark.Handle) error {
		if h.Type == ark.DataIndexFile || h.Type == ark.TreeIndexFile {
			return errInterrupted
		}
		return nil
	}
	if _, err := repo.Backup(ctx, stubSource{name: "torn", content: "never indexed"}); !errors.Is(err, errInterrupted) {
		t.Fatalf("Backup = %v, want the interrupt", err)
	}
	b.mu.Lock()
	b.onPut = nil
	b.mu.Unlock()

	if _, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{gone.ID}}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	before := packObjects(t, b)
	var wantFreed int64
	for h, size := range before {
		if _, keep := keptPacks[h]; !keep && (h.Type == ark.DataPackFile || h.Type == ark.TreePackFile) {
			wantFreed += size
		}
	}
	if len(before) != 10 || len(keptPacks) != 2 {
		t.Fatalf("%d pack and index objects before the prune, %d packs to keep; want 6 packs with 4 index objects, 2",
			len(before), len(keptPacks))
	}

	b.take()
	dry, err := repo.Prune(ctx, ark.WithPruneDryRun())
	if err != nil {
		t.Fatalf("Prune dry run: %v", err)
	}
	if got := b.take(); !maps.Equal(packObjects(t, b), before) || got["put dataindex"]+got["put treeindex"] != 0 {
		t.Fatalf("the dry run changed the repository: %v", got)
	}
	res, err := repo.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// The forgotten snapshot's chunk and tree node; the unlisted packs count
	// with their bytes only.
	if res.BlobsRemoved != 2 || res.BytesFreed != wantFreed || dry.BlobsRemoved != 2 || dry.BytesFreed != wantFreed {
		t.Errorf("Prune removed %d blobs / %d bytes, the dry run %d / %d; want 2 / %d",
			res.BlobsRemoved, res.BytesFreed, dry.BlobsRemoved, dry.BytesFreed, wantFreed)
	}

	after := packObjects(t, b)
	indexObjects := 0
	for h := range after {
		if _, keep := keptPacks[h]; keep {
			continue
		}
		if h.Type == ark.DataPackFile || h.Type == ark.TreePackFile {
			t.Errorf("prune left %s/%s, which holds nothing referenced", h.Type, h.Name)
		}
		indexObjects++
	}
	if len(after) != 4 || indexObjects != 2 {
		t.Errorf("after the prune: %d objects, %d of them index objects; want the 2 kept packs and one index each",
			len(after), indexObjects)
	}
	wantChunk := []string{ark.ObjectName(repo, []byte(otherContent))}
	if got := blobNames(t, repo, ark.BlobFile); !slices.Equal(got, wantChunk) {
		t.Errorf("the data index lists %v after the prune, want only the kept chunk %v", got, wantChunk)
	}
	if n := len(blobNames(t, repo, ark.TreeFile)); n != 1 {
		t.Errorf("the tree index lists %d nodes after the prune, want 1", n)
	}

	sink := &recordingSink{}
	if _, err := repo.Restore(ctx, kept.ID, sink); err != nil || sink.written["data.txt"] != otherContent {
		t.Errorf("Restore of the kept snapshot = %q, %v", sink.written["data.txt"], err)
	}
	if check, err := repo.Check(ctx); err != nil || len(check.Errors) != 0 || len(check.Warnings) != 0 {
		t.Errorf("Check = %+v, %v; want no findings", check, err)
	}

	// Nothing left to do: a second prune deletes and rewrites nothing.
	b.take()
	if res, err := repo.Prune(ctx); err != nil || res.BlobsRemoved != 0 || res.BytesFreed != 0 {
		t.Errorf("second Prune = %+v, %v; want nothing removed", res, err)
	}
	for op, n := range b.take() {
		if strings.HasPrefix(op, "put ") || strings.HasPrefix(op, "delete ") {
			t.Errorf("second prune issued %d %q", n, op)
		}
	}
}

// TestPruneAbortsWhenTheIndexHidesALiveBlob cuts an index object short, as a
// power loss may: the blobs it listed are still referenced, and a prune that
// trusted the remaining index would delete their pack as unlisted. It deletes
// nothing and says how to get the index back.
func TestPruneAbortsWhenTheIndexHidesALiveBlob(t *testing.T) {
	ctx := context.Background()
	for _, typ := range []ark.HandleType{ark.DataIndexFile, ark.TreeIndexFile} {
		t.Run(typ.String(), func(t *testing.T) {
			b := newOpCountingBackend()
			repo := openRepo(t, b)
			snap, err := repo.Backup(ctx, stubSource{name: "app", content: victimContent})
			if err != nil {
				t.Fatalf("Backup: %v", err)
			}
			name := objectNames(t, b, typ)[0]
			obj := readObject(t, b, typ, name)
			putObject(t, b, typ, name, obj[:len(obj)/2])
			before := packObjects(t, b)

			b.take()
			for _, opts := range [][]ark.PruneOption{{ark.WithPruneDryRun()}, nil} {
				_, err = repo.Prune(ctx, opts...)
				if !errors.Is(err, ark.ErrNotFound) || !strings.Contains(fmt.Sprint(err), "index rebuild") {
					t.Fatalf("Prune = %v, want ErrNotFound advising an index rebuild", err)
				}
			}
			// The skipped index object hides what it listed from a backup's
			// lookups too: the prune stores the lost-pack marker and nothing else.
			got := b.take()
			if want := map[ark.HandleType]int{ark.DataIndexFile: 1}[typ]; got["put lostpacks"] != want {
				t.Errorf("the aborted prune stored the lost-pack marker %d times, want %d", got["put lostpacks"], want)
			}
			delete(got, "put lostpacks")
			for op, n := range got {
				if strings.HasPrefix(op, "delete ") || strings.HasPrefix(op, "put ") {
					t.Errorf("the aborted prune issued %d %q", n, op)
				}
			}
			if !maps.Equal(packObjects(t, b), before) {
				t.Error("the aborted prune changed the packs or the index")
			}

			// With the index object back, the same prune has nothing to do
			// and the snapshot restores.
			putObject(t, b, typ, name, obj)
			if res, err := repo.Prune(ctx); err != nil || res.BytesFreed != 0 {
				t.Fatalf("Prune with the index restored = %+v, %v", res, err)
			}
			if got, err := restoreData(t, repo, snap.ID); err != nil || got != victimContent {
				t.Errorf("Restore = %q, %v; want %q", got, err, victimContent)
			}
		})
	}
}
