package ark_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// faultBackend fails writes of one handle type once it is armed: the nth Put
// or Delete since then, or every one. Every other request goes through, so
// the run that meets a single fault goes on to whatever it does about a
// failed write.
type faultBackend struct {
	ark.Backend
	mu      sync.Mutex
	op      string
	typ     ark.HandleType
	n, seen int
}

// arm makes the nth op ("put" or "delete", "" for both) of typ from now on
// fail, every one when n is negative.
func (b *faultBackend) arm(op string, typ ark.HandleType, n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.op, b.typ, b.n, b.seen = op, typ, n, 0
}

// disarm reports whether an armed write came, and fails nothing from now on.
func (b *faultBackend) disarm() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	fired := b.n != 0 && b.seen > 0 && b.seen >= b.n
	b.n = 0
	return fired
}

func (b *faultBackend) fail(op string, h ark.Handle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.n == 0 || (b.op != "" && op != b.op) || h.Type != b.typ {
		return nil
	}
	if b.seen++; b.n < 0 || b.seen == b.n {
		return fmt.Errorf("%s %s %d: %w", op, h.Type, b.seen, errInjected)
	}
	return nil
}

func (b *faultBackend) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if err := b.fail("put", h); err != nil {
		return err
	}
	return b.Backend.Put(ctx, h, r)
}

func (b *faultBackend) Delete(ctx context.Context, h ark.Handle) error {
	if err := b.fail("delete", h); err != nil {
		return err
	}
	return b.Backend.Delete(ctx, h)
}

// faultRepo returns a repository over an faultBackend whose packs close at
// packBytes and whose runs list their packs every eight blobs, so a small
// fixture stores several packs and several index objects. It stores one
// object at a time: what shares a pack follows from the order of the source.
func faultRepo(t *testing.T, packBytes int) (*ark.Repository, *faultBackend) {
	t.Helper()
	ctx := context.Background()
	b := &faultBackend{Backend: arkmem.New()}
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword(), ark.WithConcurrency(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	ark.SetPackLimits(repo, packBytes, 8)
	return repo, b
}

// oneFileDirs returns a source with a directory per content, in order. A
// directory holds one file of the same name in each, so two such sources
// share the tree node of every content both hold, wherever it lies.
func oneFileDirs(name string, contents []string) multiSource {
	dirs := make([]dirOf, len(contents))
	for i, c := range contents {
		dirs[i] = dirOf{fmt.Sprintf("d%03d", i), []string{c}}
	}
	return treeOf(name, dirs...)
}

// interleave returns a[0], b[0], a[1], b[1] and so on: stored in that order,
// the chunks of a and of b share every pack.
func interleave(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for i := range a {
		out = append(out, a[i], b[i])
	}
	return out
}

// assertNoErrors fails the test when Check reports an error, and when
// warnings is false also when it reports a warning.
func assertNoErrors(t *testing.T, repo *ark.Repository, point string, warnings bool) {
	t.Helper()
	check := mustCheck(t, repo)
	if len(check.Errors) != 0 || !warnings && len(check.Warnings) != 0 {
		t.Fatalf("%s: Check = %+v", point, check)
	}
}

// TestBackupWithOneFailedWriteIsMadeUpByTheNext fails each write of a backup
// in turn: a pack Put, which leaves the packs before it without an index
// object or with the checkpoints stored until then, an index Put, which
// leaves every pack and no entry for the last ones, and the manifest Put,
// which leaves a complete index and no snapshot. Whatever the backup left,
// the repository has no error, the snapshot before it restores, the same
// backup then finishes, and a prune leaves nothing to report.
func TestBackupWithOneFailedWriteIsMadeUpByTheNext(t *testing.T) {
	ctx := context.Background()
	a := contents("a", 12)
	base := treeOf("app", dirOf{"a", a})
	next := treeOf("app", dirOf{"a", a}, dirOf{"b", contents("b", 16)}, dirOf{"c", contents("c", 16)})
	// atLeast is how many such writes the backup has to issue, so that the
	// fixture keeps reaching every window: a pack with packs before it, an
	// index object with index objects before it.
	windows := []struct {
		typ     ark.HandleType
		atLeast int
	}{
		{ark.DataPackFile, 4}, {ark.TreePackFile, 2}, {ark.DataIndexFile, 2}, {ark.TreeIndexFile, 1},
		{ark.SnapshotFile, 1},
	}
	for _, w := range windows {
		typ, atLeast := w.typ, w.atLeast
		t.Run(typ.String(), func(t *testing.T) {
			for n := 1; ; n++ {
				repo, b := faultRepo(t, 300)
				first := mustBackup(t, repo, base)

				b.arm("put", typ, n)
				_, err := repo.Backup(ctx, next)
				if !b.disarm() {
					if err != nil || n <= atLeast {
						t.Fatalf("the backup issued %d Puts of %s and returned %v, want at least %d",
							n-1, typ, err, atLeast)
					}
					return
				}
				point := fmt.Sprintf("Put %d of %s failed", n, typ)
				if !errors.Is(err, errInjected) {
					t.Fatalf("%s: Backup = %v, want the fault", point, err)
				}
				assertNoErrors(t, repo, point, true)
				snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
				if err != nil || len(snaps) != 1 || snaps[0].ID != first.ID {
					t.Fatalf("%s: Snapshots = %v, %v; want the first one alone", point, snaps, err)
				}
				assertRestores(t, repo, first, base)

				second := mustBackup(t, repo, next)
				assertRestores(t, repo, second, next)
				if _, err := repo.Prune(ctx); err != nil {
					t.Fatalf("%s: Prune: %v", point, err)
				}
				assertNoErrors(t, repo, point+", after the prune", false)
				assertRestores(t, repo, first, base)
				assertRestores(t, repo, second, next)
				if t.Failed() {
					t.Fatal(point)
				}
			}
		})
	}
}

// TestPruneWithOneFailedWriteIsMadeUpByTheNext fails each write of a prune
// that repacks in turn, in each of its four steps: the Put of a new pack, the
// Put of an index object, of the repack or of the rewrite, the Delete of an
// old index object and the Delete of an old pack. A failed write ends the
// prune, except a pack that does not delete, which stays for the next one.
// Whatever the prune left, the repository has no error, every kept snapshot
// restores, and the next prune leaves nothing for a third.
func TestPruneWithOneFailedWriteIsMadeUpByTheNext(t *testing.T) {
	ctx := context.Background()
	a, c := contents("a", 12), contents("c", 12)
	tail := treeOf("tail", dirOf{"t", contents("t", 5)})
	keep := oneFileDirs("keep", slices.Concat(a, c))
	// The forgotten snapshots hold every other chunk and tree node with
	// keep, so each of their packs is half dead.
	one := oneFileDirs("one", interleave(a, contents("b", 12)))
	two := oneFileDirs("two", interleave(c, contents("d", 12)))
	windows := []struct {
		op      string
		typ     ark.HandleType
		atLeast int
		// logged says the prune goes on after the failed write.
		logged bool
	}{
		{op: "put", typ: ark.DataPackFile, atLeast: 2},
		{op: "put", typ: ark.TreePackFile, atLeast: 2},
		{op: "put", typ: ark.DataIndexFile, atLeast: 2},
		{op: "put", typ: ark.TreeIndexFile, atLeast: 2},
		{op: "delete", typ: ark.DataIndexFile, atLeast: 2},
		{op: "delete", typ: ark.TreeIndexFile, atLeast: 2},
		{op: "delete", typ: ark.DataPackFile, atLeast: 2, logged: true},
		{op: "delete", typ: ark.TreePackFile, atLeast: 2, logged: true},
	}
	for _, w := range windows {
		t.Run(w.op+" "+w.typ.String(), func(t *testing.T) {
			for n := 1; ; n++ {
				repo, b := faultRepo(t, 600)
				goneOne, goneTwo := mustBackup(t, repo, one), mustBackup(t, repo, two)
				keptTail, kept := mustBackup(t, repo, tail), mustBackup(t, repo, keep)
				mustForget(t, repo, goneOne, goneTwo)

				b.arm(w.op, w.typ, n)
				res, err := repo.Prune(ctx)
				if !b.disarm() {
					if err != nil || n <= w.atLeast || res.BytesRepacked == 0 {
						t.Fatalf("the prune issued %d %ss of %s and returned %+v, %v; want a repack and at least %d",
							n-1, w.op, w.typ, res, err, w.atLeast)
					}
					return
				}
				point := fmt.Sprintf("%s %d of %s failed", w.op, n, w.typ)
				if w.logged && err != nil || !w.logged && !errors.Is(err, errInjected) {
					t.Fatalf("%s: Prune = %v", point, err)
				}
				assertNoErrors(t, repo, point, true)
				assertRestores(t, repo, kept, keep)
				assertRestores(t, repo, keptTail, tail)

				if _, err := repo.Prune(ctx); err != nil {
					t.Fatalf("%s: second Prune: %v", point, err)
				}
				assertNoErrors(t, repo, point+", after the second prune", false)
				assertRestores(t, repo, kept, keep)
				assertRestores(t, repo, keptTail, tail)
				res, err = repo.Prune(ctx)
				if err != nil || res.PacksDeleted != 0 || res.BytesRepacked != 0 {
					t.Fatalf("%s: third Prune = %+v, %v; want nothing left to do", point, res, err)
				}
				if t.Failed() {
					t.Fatal(point)
				}
			}
		})
	}
}

// TestTornIndexObjectIsSkippedByBackupAndRemovedByPrune: a power loss may cut
// short an index object that a killed backup had stored for packs no snapshot
// references yet. The next backup reads the index around it and stores only
// what is new, and a prune deletes it. The torn object that lists a
// referenced blob is TestPruneAbortsWhenTheIndexHidesALiveBlob.
func TestTornIndexObjectIsSkippedByBackupAndRemovedByPrune(t *testing.T) {
	ctx := context.Background()
	b := arkmem.New()
	repo := openRepo(t, b)
	a := contents("a", 5)
	base := treeOf("app", dirOf{"a", a})
	next := treeOf("app", dirOf{"a", a}, dirOf{"b", contents("b", 5)})
	first := mustBackup(t, repo, base)

	torn := []ark.Handle{
		{Type: ark.DataIndexFile, Name: strings.Repeat("0c", 32)},
		{Type: ark.TreeIndexFile, Name: strings.Repeat("0d", 32)},
	}
	for _, h := range torn {
		obj := readObject(t, b, h.Type, objectNames(t, b, h.Type)[0])
		putObject(t, b, h.Type, h.Name, obj[:len(obj)/2])
	}

	second := mustBackup(t, repo, next)
	if second.Stats.ChunksNew != 5 {
		t.Errorf("the backup beside the torn index objects stored %d chunks, want the 5 new ones",
			second.Stats.ChunksNew)
	}
	assertNoErrors(t, repo, "beside the torn index objects", true)
	assertRestores(t, repo, first, base)
	assertRestores(t, repo, second, next)

	if _, err := repo.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, h := range torn {
		if slices.Contains(objectNames(t, b, h.Type), h.Name) {
			t.Errorf("the prune left the torn %s object", h.Type)
		}
	}
	assertNoErrors(t, repo, "after the prune", false)
	assertRestores(t, repo, second, next)
}
