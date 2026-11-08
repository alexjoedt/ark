package arktest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
	"github.com/alexjoedt/ark/chunker"
)

// RunStreamTests checks a one-file stream Source and its Sink against each
// other through an in-memory repository: what the source read from its
// reader, the sink writes to its writer, byte for byte.
//
//	func TestStreamConformance(t *testing.T) {
//		arktest.RunStreamTests(t,
//			func(name string, r io.Reader) ark.Source { return ark.NewStreamSource(name, r) },
//			func(w io.Writer) ark.Sink { return ark.NewStreamSink(w) },
//		)
//	}
func RunStreamTests(
	t *testing.T,
	newSource func(name string, r io.Reader) ark.Source,
	newSink func(w io.Writer) ark.Sink,
) {
	t.Helper()

	t.Run("Empty", func(t *testing.T) {
		repo := streamRepo(t)
		snap := backupStream(t, repo, newSource("empty", bytes.NewReader(nil)))
		if got := restoreStream(t, repo, snap.ID, newSink); len(got) != 0 {
			t.Errorf("restored %d bytes of an empty stream", len(got))
		}
	})

	t.Run("LargerThanMaxChunk", func(t *testing.T) {
		repo := streamRepo(t)
		data := randomBytes(3*chunker.DefaultMaxChunk + 12345)
		snap := backupStream(t, repo, newSource("big", bytes.NewReader(data)))
		if !bytes.Equal(restoreStream(t, repo, snap.ID, newSink), data) {
			t.Error("restored stream differs from the one backed up")
		}
	})

	t.Run("ChangedStreamReusesChunks", func(t *testing.T) {
		repo := streamRepo(t)
		data := randomBytes(50 << 20)
		snap := backupStream(t, repo, newSource("dump", bytes.NewReader(data)))
		if !bytes.Equal(restoreStream(t, repo, snap.ID, newSink), data) {
			t.Fatal("restored stream differs from the one backed up")
		}

		changed := bytes.Clone(data)
		copy(changed[20<<20:], randomBytes(1<<20))
		snap = backupStream(t, repo, newSource("dump", bytes.NewReader(changed)))
		if snap.Stats.ChunksReused == 0 {
			t.Errorf("ChunksReused = 0 after changing 1 MiB of 50 MiB (ChunksNew %d)", snap.Stats.ChunksNew)
		}
		if !bytes.Equal(restoreStream(t, repo, snap.ID, newSink), changed) {
			t.Error("restored changed stream differs from the one backed up")
		}
	})

	t.Run("ReaderError", func(t *testing.T) {
		repo := streamRepo(t)
		boom := errors.New("arktest: reader failed")
		r := io.MultiReader(bytes.NewReader(randomBytes(1<<20)), errReader{boom})
		_, err := repo.Backup(context.Background(), newSource("broken", r))
		if !errors.Is(err, boom) {
			t.Errorf("Backup = %v, want the reader's error", err)
		}
	})

	t.Run("WriterError", func(t *testing.T) {
		repo := streamRepo(t)
		snap := backupStream(t, repo, newSource("dump", bytes.NewReader(randomBytes(1<<20))))
		boom := errors.New("arktest: writer failed")
		_, err := repo.Restore(context.Background(), snap.ID, newSink(errWriter{boom}))
		if !errors.Is(err, boom) {
			t.Errorf("Restore = %v, want the writer's error", err)
		}
	})

	t.Run("WalkCancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		err := newSource("dump", bytes.NewReader([]byte("x"))).Walk(ctx,
			func(ark.FileInfo, func() (io.ReadCloser, error)) error {
				called = true
				return nil
			})
		if !errors.Is(err, context.Canceled) || called {
			t.Errorf("Walk on a cancelled ctx = %v (fn called %v), want context.Canceled and no call", err, called)
		}
	})
}

func streamRepo(t *testing.T) *ark.Repository {
	t.Helper()
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func backupStream(t *testing.T, repo *ark.Repository, src ark.Source) *ark.Snapshot {
	t.Helper()
	snap, err := repo.Backup(context.Background(), src)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	return snap
}

func restoreStream(t *testing.T, repo *ark.Repository, id string, newSink func(io.Writer) ark.Sink) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := repo.Restore(context.Background(), id, newSink(&buf)); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return buf.Bytes()
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// errWriter fails every write with err.
type errWriter struct{ err error }

func (w errWriter) Write([]byte) (int, error) { return 0, w.err }
