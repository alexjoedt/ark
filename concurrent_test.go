package ark_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkuri"
)

// The concurrent backup test starts concWriters children, the re-exec'd test
// binary, that each open the same repository and run one Backup. Every child
// reports its lock events on fd 3 and, once it holds its shared lock, blocks
// until the parent has seen all children acquire theirs and closes fd 4: all
// shared locks are held at once, so the backups overlap however small they
// are. The sources share most of their files, so the children Put the same
// chunk names concurrently.
//
//	ARK_CONC_SEED  seed of the file contents, printed on every run
const (
	concEnvRepo = "ARK_CONC_CHILD_REPO"
	concEnvSrc  = "ARK_CONC_CHILD_SRC"
	concEnvTag  = "ARK_CONC_CHILD_TAG"
)

const concWriters = 8

func TestConcurrentBackups(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrent backup test spawns processes")
	}
	runConcurrentBackups(t, newKillRepo)
}

func runConcurrentBackups(t *testing.T, newRepo func(context.Context, *testing.T) (string, ark.Backend, func() error)) {
	t.Helper()
	ctx := context.Background()
	ref, raw, remove := newRepo(ctx, t)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("repository kept at %s", ref)
		} else if err := remove(); err != nil {
			t.Errorf("remove %s: %v", ref, err)
		}
		_ = raw.Close()
	})

	bytesSrc, seed := seededChaCha8(t, "ARK_CONC_SEED")
	t.Logf("rerun with ARK_CONC_SEED=%d", seed)
	shared := make([][]byte, 4)
	for i := range shared {
		shared[i] = make([]byte, 96<<10)
		_, _ = bytesSrc.Read(shared[i])
	}
	want := map[string]map[string]string{}
	children := make([]*concChild, concWriters)
	for i := range children {
		src := t.TempDir()
		for j, data := range shared {
			writeTestFile(t, filepath.Join(src, "shared", fmt.Sprintf("s%d.bin", j)), data)
		}
		for j := range 2 {
			data := make([]byte, 64<<10)
			_, _ = bytesSrc.Read(data)
			writeTestFile(t, filepath.Join(src, fmt.Sprintf("own%d.bin", j)), data)
		}
		tag := fmt.Sprintf("conc-%d", i)
		want[tag] = readTree(t, src)
		env := []string{concEnvRepo + "=" + ref, concEnvSrc + "=" + src, concEnvTag + "=" + tag}
		children[i] = startConcChild(t, tag, env)
	}

	events := make(chan concEvent)
	for i, c := range children {
		go c.readEvents(t.Context(), i, events)
	}
	acquired, released := make([]int, concWriters), make([]int, concWriters)
	gateOpen, open := false, concWriters
	timeout := time.After(60 * time.Second)
	for open > 0 {
		var ev concEvent
		select {
		case ev = <-events:
		case <-timeout:
			t.Fatalf("timed out, %d children still running, gate open %v", open, gateOpen)
		}
		switch {
		case ev.event == "":
			open--
			if !gateOpen {
				t.Fatalf("child %d stopped reporting before every child held its lock", ev.child)
			}
			continue
		case ev.exclusive:
			t.Fatalf("child %d reported an exclusive lock: %s", ev.child, ev.event)
		case ev.event == ark.LockAcquired:
			acquired[ev.child]++
		case ev.event == ark.LockReleased:
			if !gateOpen {
				t.Fatalf("child %d released its lock before every child held one", ev.child)
			}
			released[ev.child]++
		default:
			t.Fatalf("child %d reported %s", ev.child, ev.event)
		}
		if !gateOpen && countPositive(acquired) == concWriters {
			gateOpen = true
			for _, c := range children {
				_ = c.gate.Close()
			}
		}
	}
	for i, c := range children {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v", i, err)
		}
		if acquired[i] != 1 || released[i] != 1 {
			t.Fatalf("child %d acquired %d and released %d locks, want 1 each", i, acquired[i], released[i])
		}
	}

	repo, err := ark.Open(ctx, keepOpen{raw}, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = repo.Close() }()
	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if len(snaps) != concWriters {
		t.Fatalf("got %d snapshots, want %d", len(snaps), concWriters)
	}
	seen := map[string]bool{}
	for _, s := range snaps {
		if len(s.Tags) != 1 || want[s.Tags[0]] == nil || seen[s.Tags[0]] {
			t.Fatalf("snapshot %s has unexpected tags %v", s.ID, s.Tags)
		}
		tag := s.Tags[0]
		seen[tag] = true
		dst := t.TempDir()
		sink, err := arkfs.NewSink(dst)
		if err != nil {
			t.Fatalf("NewSink: %v", err)
		}
		if _, err := repo.Restore(ctx, s.ID, sink); err != nil {
			t.Fatalf("Restore %s (%s): %v", s.ID, tag, err)
		}
		if diff := diffTree(want[tag], readTree(t, dst)); diff != "" {
			t.Fatalf("snapshot %s (%s) restores a different tree: %s", s.ID, tag, diff)
		}
		res, err := repo.Verify(ctx, s.ID)
		if err != nil {
			t.Fatalf("Verify %s (%s): %v", s.ID, tag, err)
		}
		if !res.OK {
			t.Fatalf("Verify %s (%s): missing %v, corrupt %v", s.ID, tag, res.MissingChunks, res.CorruptChunks)
		}
	}
}

func countPositive(ns []int) int {
	n := 0
	for _, v := range ns {
		if v > 0 {
			n++
		}
	}
	return n
}

// concChild is a running child: events is the read end of its fd 3, gate the
// write end of its fd 4.
type concChild struct {
	cmd    *exec.Cmd
	out    bytes.Buffer
	events *os.File
	gate   *os.File
}

// concEvent is one line a child reported; an empty event means its fd 3 closed.
type concEvent struct {
	child     int
	event     string
	exclusive bool
}

// startConcChild starts the child named name. A cleanup kills it, waits for
// it and logs its output when the test failed, so a child never outlives the
// test.
func startConcChild(t *testing.T, name string, env []string) *concChild {
	t.Helper()
	evR, evW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	gateR, gateW, err := os.Pipe()
	if err != nil {
		_, _ = evR.Close(), evW.Close()
		t.Fatalf("pipe: %v", err)
	}
	c := &concChild{events: evR, gate: gateW}
	c.cmd, err = startChild("TestConcurrentBackupChild", env, &c.out, evW, gateR)
	if err != nil {
		_, _ = evR.Close(), gateW.Close()
		t.Fatalf("start child %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
		_, _ = evR.Close(), gateW.Close()
		if t.Failed() {
			t.Logf("child %s output:\n%s", name, c.out.Bytes())
		}
	})
	return c
}

// readEvents sends the events of child i until its fd 3 closes or ctx ends.
func (c *concChild) readEvents(ctx context.Context, i int, events chan<- concEvent) {
	send := func(ev concEvent) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	sc := bufio.NewScanner(c.events)
	for sc.Scan() {
		event, kind, _ := strings.Cut(sc.Text(), " ")
		if !send(concEvent{child: i, event: event, exclusive: kind == "exclusive"}) {
			return
		}
	}
	send(concEvent{child: i})
}

// TestConcurrentBackupChild is a child of TestConcurrentBackups: one Backup of
// concEnvSrc into the repository at concEnvRepo.
func TestConcurrentBackupChild(t *testing.T) {
	ref := os.Getenv(concEnvRepo)
	if ref == "" {
		t.Skip("runs only as a child of TestConcurrentBackups")
	}
	ctx := context.Background()
	b, err := arkuri.Open(ctx, ref)
	if err != nil {
		t.Fatalf("open %s: %v", ref, err)
	}
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = repo.Close() }()
	src, err := arkfs.NewSource(os.Getenv(concEnvSrc))
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}

	events, gate := os.NewFile(3, "events"), os.NewFile(4, "gate")
	defer func() { _ = events.Close() }()
	ark.SetLockEvent(func(event string, exclusive bool) {
		kind := "shared"
		if exclusive {
			kind = "exclusive"
		}
		if _, err := fmt.Fprintf(events, "%s %s\n", event, kind); err != nil {
			t.Errorf("report %s: %v", event, err)
		}
		if event == ark.LockAcquired {
			// The read returns EOF once the parent closes the gate, at once
			// on every later call.
			_, _ = gate.Read(make([]byte, 1))
		}
	})
	if _, err := repo.Backup(ctx, src, ark.WithTags(os.Getenv(concEnvTag))); err != nil {
		t.Fatalf("Backup: %v", err)
	}
}
