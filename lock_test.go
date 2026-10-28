package ark_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// lockRecorder notes the class of every lock an operation writes, which is how
// these tests see which operations lock and how.
type lockRecorder struct {
	ark.Backend
	repo *ark.Repository // set after Open, locks are sealed with its key

	mu    sync.Mutex
	taken []bool // one entry per lock written, true for exclusive
}

func (b *lockRecorder) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	if h.Type != ark.LockFile {
		return b.Backend.Put(ctx, h, r)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	raw, err := ark.UnsealObject(b.repo, h, data)
	if err != nil {
		return err
	}
	var l struct {
		Exclusive bool `json:"exclusive"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return err
	}
	b.mu.Lock()
	b.taken = append(b.taken, l.Exclusive)
	b.mu.Unlock()
	return b.Backend.Put(ctx, h, bytes.NewReader(data))
}

func (b *lockRecorder) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.taken = nil
}

func (b *lockRecorder) classes() []bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]bool(nil), b.taken...)
}

// TestEveryOperationTakesItsLockClass is the check that the lock table is
// actually wired up: prune excludes everyone, the rest share.
func TestEveryOperationTakesItsLockClass(t *testing.T) {
	ctx := context.Background()

	b := &lockRecorder{Backend: arkmem.New()}
	repo := openRepo(t, b)
	b.repo = repo

	snap, err := repo.Backup(ctx, stubSource{name: "app", content: "one"})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	var archive bytes.Buffer
	if err := repo.Export(ctx, snap.ID, &archive); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// A second repository to import into, so Import works on a snapshot the
	// target does not already hold.
	target := &lockRecorder{Backend: arkmem.New()}
	importRepo := openRepo(t, target)
	target.repo = importRepo

	cases := []struct {
		name      string
		exclusive bool
		recorder  *lockRecorder
		run       func() error
	}{
		{"backup", false, b, func() error {
			_, err := repo.Backup(ctx, stubSource{name: "app", content: "two"})
			return err
		}},
		{"restore", false, b, func() error {
			_, err := repo.Restore(ctx, snap.ID, discardSink{})
			return err
		}},
		{"verify", false, b, func() error {
			_, err := repo.Verify(ctx, snap.ID)
			return err
		}},
		{"check", false, b, func() error {
			_, err := repo.Check(ctx)
			return err
		}},
		{"export", false, b, func() error {
			return repo.Export(ctx, snap.ID, io.Discard)
		}},
		{"import", false, target, func() error {
			_, err := importRepo.Import(ctx, bytes.NewReader(archive.Bytes()))
			return err
		}},
		{"forget", false, b, func() error {
			_, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: []string{snap.ID}})
			return err
		}},
		{"prune", true, b, func() error {
			_, err := repo.Prune(ctx)
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.recorder.reset()
			if err := tc.run(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			taken := tc.recorder.classes()
			if len(taken) == 0 {
				t.Fatalf("%s took no lock", tc.name)
			}
			for _, exclusive := range taken {
				if exclusive != tc.exclusive {
					t.Errorf("%s took exclusive=%v, want %v", tc.name, exclusive, tc.exclusive)
				}
			}

			repoUnderTest := repo
			if tc.name == "import" {
				repoUnderTest = importRepo
			}
			locks, err := repoUnderTest.Locks(ctx)
			if err != nil {
				t.Fatalf("Locks: %v", err)
			}
			if len(locks) != 0 {
				t.Errorf("%s left %d locks behind", tc.name, len(locks))
			}
		})
	}
}
