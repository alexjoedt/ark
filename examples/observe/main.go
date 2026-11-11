// Command observe runs every repository operation against a local repository
// in a temporary directory and logs what an embedder can watch: the progress
// events of each operation, the lock events and, at the end, the call
// counters of the instrumented backend.
//
//	go run ./observe
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"sync"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(context.Background(), logger); err != nil {
		logger.Error("observe failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger) error {
	dir, err := os.MkdirTemp("", "ark-observe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	local, err := arklocal.New(dir)
	if err != nil {
		return err
	}
	backend := ark.Instrument(local)
	if err := ark.Init(ctx, backend, ark.WithoutPassword()); err != nil {
		return err
	}
	// Per-chunk and per-file events go to Debug; Info gets the first event
	// of each phase and the finalize with the operation's totals.
	type opPhase struct {
		op    ark.Operation
		phase ark.Phase
	}
	var mu sync.Mutex
	seen := map[opPhase]bool{}
	repo, err := ark.Open(ctx, backend, ark.WithoutPassword(),
		ark.WithLogger(logger),
		ark.WithProgress(func(e ark.ProgressEvent) {
			key := opPhase{e.Operation, e.Phase}
			mu.Lock()
			level := slog.LevelDebug
			if !seen[key] {
				seen[key] = true
				level = slog.LevelInfo
			}
			if e.Phase == ark.PhaseFinalize {
				clear(seen)
			}
			mu.Unlock()
			logger.Log(ctx, level, "progress", "op", e.Operation, "phase", e.Phase, "path", e.CurrentPath,
				"files", e.FilesDone, "bytes", e.BytesDone, "chunks", e.ChunksDone, "chunks_total", e.ChunksTotal)
		}),
		ark.WithLockEvent(func(e ark.LockEvent) {
			logger.InfoContext(ctx, "lock", "kind", e.Kind, "exclusive", e.Exclusive, "id", e.LockID,
				"holders", len(e.Holders))
		}))
	if err != nil {
		return err
	}
	defer repo.Close()

	blob := make([]byte, 4<<20)
	_, _ = rand.Read(blob)
	first, err := repo.Backup(ctx, ark.NewStreamSource("dump", bytes.NewReader(blob)))
	if err != nil {
		return err
	}
	var restored bytes.Buffer
	if _, err := repo.Restore(ctx, first.ID, ark.NewStreamSink(&restored)); err != nil {
		return err
	}
	if !bytes.Equal(restored.Bytes(), blob) {
		return errors.New("restored stream differs from the one backed up")
	}

	second, err := repo.Backup(ctx, ark.NewStreamSource("dump", bytes.NewReader(blob[:1<<20])))
	if err != nil {
		return err
	}
	if _, err := repo.Forget(ctx, ark.ForgetPolicy{KeepLast: 1}, ark.SnapshotFilter{}); err != nil {
		return err
	}
	if _, err := repo.Prune(ctx); err != nil {
		return err
	}
	if _, err := repo.Check(ctx); err != nil {
		return err
	}
	if _, err := repo.Verify(ctx, second.ID); err != nil {
		return err
	}

	s := backend.Stats()
	for _, m := range []struct {
		name string
		ark.MethodStats
	}{
		{"put", s.Put}, {"get", s.Get}, {"getrange", s.GetRange}, {"exists", s.Exists},
		{"delete", s.Delete}, {"list", s.List}, {"stat", s.Stat}, {"sync", s.Sync},
	} {
		logger.InfoContext(ctx, "backend", "method", m.name, "calls", m.Calls, "errors", m.Errors,
			"duration", m.Duration, "bytes", m.Bytes)
	}
	return nil
}
