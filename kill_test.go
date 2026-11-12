package ark_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkuri"
	"github.com/alexjoedt/ark/chunker"
)

// The kill harness runs backup, forget and prune against an arklocal
// repository in a child process, the re-exec'd test binary, and SIGKILLs it
// after a random delay. Backups take a changing tree, forgets remove a few
// snapshots by ID. After every kill the repository must pass Check, every
// snapshot must restore to the tree it was taken of, a snapshot that was seen
// once must stay until a forget names it, and a snapshot a forget removed,
// finished or killed, must not come back. A killed prune is rerun in a fresh
// child and must leave Check clean. Restores run once per snapshot and again in a full pass before the
// repository is replaced; Check covers the rest in between.
//
// The children close a pack at killPackTarget bytes and list their packs
// every killCheckpointBlobs blobs, so a backup of the small tree stores
// several packs and several index objects, and every backup is drawn from one
// of two sources: arkfs, which a backup reads in parallel, or the same tree
// without ConcurrentOpen, which it reads file by file inside the walk as it
// does a tar stream. A child reports each write of a pack, an index object
// and a manifest, so the harness logs where its kills landed (killWindow).
// Half of the kill delays start at the first of those writes instead of at
// the start of the op: an op spends most of its time before it writes, on its
// lock and on reading the index and the snapshots, most of all over sftp, and
// a kill there finds nothing half done.
//
//	ARK_KILL_RUNS  iterations, default 5
//	ARK_KILL_SEED  seed of the ops, tree changes and kill delays, printed on every run
//
// A seed replays the drawn values, not the kills: whether a child dies before
// it finishes depends on timing, and the forget targets and kill ceilings
// depend on earlier kills. A rerun follows the failing run until the first
// kill that lands differently; the repository of a failed run is kept.
const (
	killEnvOp   = "ARK_KILL_CHILD_OP"
	killEnvRepo = "ARK_KILL_CHILD_REPO"
	killEnvSrc  = "ARK_KILL_CHILD_SRC"
	killEnvTag  = "ARK_KILL_CHILD_TAG"
	killEnvIDs  = "ARK_KILL_CHILD_IDS"
	killEnvSeq  = "ARK_KILL_CHILD_SEQ"
)

const (
	// killPackTarget closes the packs of a child: a few chunks to a pack, so
	// the chunks of files that change at different rates share packs.
	killPackTarget = 32 << 10
	// killCheckpointBlobs is how many blobs a child stores before it lists
	// them in the index.
	killCheckpointBlobs = 8
)

// killOps is drawn uniformly, so backup runs at least half of the iterations:
// a forget with fewer than two snapshots to choose from runs as a backup.
var killOps = []string{"backup", "backup", "forget", "prune"}

func TestKillHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("kill harness spawns processes")
	}
	runKillHarness(t, newKillRepo, 0)
}

// TestKillHarnessSFTP is the same run against the sftp server named by
// ARK_SFTP_TEST_URI, one fresh remote directory per repository.
func TestKillHarnessSFTP(t *testing.T) {
	if testing.Short() {
		t.Skip("kill harness spawns processes")
	}
	runKillHarness(t, newSFTPKillRepo(t, "kill"), killSettleSFTP)
}

// newSFTPKillRepo returns a repository factory for the sftp server named by
// ARK_SFTP_TEST_URI, one fresh remote directory per repository. It skips t
// when the variable is unset.
func newSFTPKillRepo(
	t *testing.T, prefix string,
) func(context.Context, *testing.T) (string, ark.Backend, func() error) {
	t.Helper()
	env := os.Getenv("ARK_SFTP_TEST_URI")
	if env == "" {
		t.Skip("ARK_SFTP_TEST_URI is not set, skipping the sftp run")
	}
	base, err := url.Parse(env)
	if err != nil {
		t.Fatalf("parse ARK_SFTP_TEST_URI: %v", err)
	}
	return func(ctx context.Context, t *testing.T) (string, ark.Backend, func() error) {
		t.Helper()
		u := *base
		u.Path = path.Join(base.Path, fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()))
		ref, b := openKillRepo(ctx, t, u.String())
		// ponytail: removal goes through the Backend, so empty directories and
		// the temp files of killed Puts stay on the server.
		return ref, b, func() error {
			for h := range killListing(ctx, t, b) {
				if err := b.Delete(ctx, h); err != nil {
					return err
				}
			}
			return nil
		}
	}
}

// killSettleSFTP is the pause between the listings that settleKill compares
// after a kill over sftp: the server keeps applying the requests a killed
// child had already sent.
// ponytail: a stable listing only shows that renames and deletes stopped
// landing; waiting for the child's session to end on the server would be exact.
const killSettleSFTP = 200 * time.Millisecond

// runKillHarness runs the harness against the repositories newRepo creates.
// newRepo returns the reference the child opens, an open backend that the
// harness closes, and a func that removes the repository once it passed.
// A non-zero settle makes the harness wait after a kill until the repository
// stops changing, see settleKill.
func runKillHarness(
	t *testing.T,
	newRepo func(context.Context, *testing.T) (string, ark.Backend, func() error),
	settle time.Duration,
) {
	t.Helper()
	runs := killEnvInt(t, "ARK_KILL_RUNS", 5)
	bytesSrc, seed := seededChaCha8(t, "ARK_KILL_SEED")
	t.Logf("kill harness: %d runs, rerun with ARK_KILL_SEED=%d", runs, seed)
	rng := rand.New(bytesSrc)

	ctx := context.Background()
	src := t.TempDir()
	for i := range killTreeFiles {
		writeKillFile(t, bytesSrc, src, i, rng.IntN(killFileSize(i)))
	}
	st := &killState{
		want:     map[string]map[string]string{},
		restored: map[string]bool{},
		must:     map[string]bool{},
		gone:     map[string]bool{},
		ids:      map[string]string{},
	}
	ceil := map[string]time.Duration{} // kill delay ceiling per op and start of the delay
	runsOf, kills := map[string]int{}, map[string]int{}
	windows := map[string]int{} // kills by op and killWindow
	repacks := 0                // prunes that stored a pack
	// raw is the open backend of the current repository, b wraps it for
	// ark.Open. A failed run keeps its repository.
	var repoDir string
	var raw, b ark.Backend
	var remove func() error
	t.Cleanup(func() {
		if raw == nil {
			return
		}
		if t.Failed() {
			t.Logf("repository kept at %s", repoDir)
		}
		_ = raw.Close()
	})
	retire := func(point string) {
		st.fullPass(ctx, t, b, point)
		if err := remove(); err != nil {
			t.Fatalf("remove %s: %v", repoDir, err)
		}
		err := raw.Close()
		raw = nil
		if err != nil {
			t.Fatalf("close %s: %v", repoDir, err)
		}
	}

	for i := range runs {
		// ponytail: Check verifies every snapshot, so a fresh repository every
		// killRepoSpan runs keeps the harness linear in runs.
		if i%killRepoSpan == 0 {
			if raw != nil {
				retire(fmt.Sprintf("seed %d before iteration %d, full pass", seed, i))
			}
			repoDir, raw, remove = newRepo(ctx, t)
			b = keepOpen{raw}
			clear(st.must)
		}
		// Every iteration draws the same values from rng whatever the op and
		// whatever earlier kills left behind.
		mutateKillTree(t, rng, bytesSrc, src)
		op := killOps[rng.IntN(len(killOps))]
		frac, pick, sequential := rng.Float64(), rng.Uint64(), rng.IntN(2) == 0
		fromWrite := rng.IntN(2) == 0

		var targets []string
		if op == "forget" {
			if targets = st.forgetTargets(pick); targets == nil {
				op = "backup"
			}
		}
		tag := fmt.Sprintf("kill-%d", i)
		env := []string{killEnvOp + "=" + op, killEnvRepo + "=" + repoDir}
		switch op {
		case "backup":
			st.want[tag] = readTree(t, src)
			env = append(env, killEnvSrc+"="+src, killEnvTag+"="+tag)
			if sequential {
				env = append(env, killEnvSeq+"=1")
			}
		case "forget":
			ids := make([]string, len(targets))
			for j, tg := range targets {
				ids[j] = st.ids[tg]
			}
			env = append(env, killEnvIDs+"="+strings.Join(ids, ","))
		}

		// The first run of each op goes to the end and sets its ceiling. A
		// finished run resets the ceiling to its duration, a killed one raises
		// it by half, so the delays keep reaching the end of an op whose work
		// grows, like a prune that has more to delete than the first one had.
		// A delay that starts at the first write has a ceiling of its own, the
		// time a finished run took from there; a run that wrote nothing was
		// not killed and says nothing about it.
		phase, from := op, "its start"
		if fromWrite {
			phase, from = op+" from its first write", "its first write"
		}
		delay := time.Duration(frac * float64(ceil[phase]))
		run, err := runKillChild(env, delay, fromWrite)
		killed, out, events := run.killed, run.out, run.events
		runsOf[op]++
		point := fmt.Sprintf("seed %d iteration %d, %s exited after %v", seed, i, op, run.took)
		if killed {
			kills[op]++
			window := killWindow(events)
			point = fmt.Sprintf("seed %d iteration %d, %s killed %v after %s, %s", seed, i, op, delay, from, window)
			switch {
			case op == "backup" && sequential:
				windows["backup, sequential source: "+window]++
			case op == "backup":
				windows["backup, arkfs: "+window]++
			default:
				windows[op+": "+window]++
			}
		}
		if err != nil {
			t.Fatalf("%s: child: %v\n%s", point, err, out)
		}
		repacked := op == "prune" && bytes.IndexByte(events, 'P') >= 0
		switch {
		case killed:
			ceil[phase] += ceil[phase] / 2
		case !fromWrite:
			ceil[phase] = run.took
		case len(events) > 0:
			ceil[phase] = run.sinceWrite
		}

		switch op {
		case "backup":
			if !killed {
				st.must[tag] = true
			}
		case "forget":
			for _, tg := range targets {
				delete(st.must, tg)
				if !killed {
					st.gone[tg] = true
				}
			}
		}
		if killed && settle > 0 {
			settleKill(ctx, t, b, settle)
		}
		st.check(ctx, t, b, point)
		switch {
		case op == "forget" && killed:
			for _, tg := range targets {
				if !st.must[tg] {
					st.gone[tg] = true
				}
			}
		case op == "prune" && killed:
			rerun, err := runKillChild(env, 0, false)
			if err != nil {
				t.Fatalf("%s: prune rerun: %v\n%s", point, err, rerun.out)
			}
			repacked = repacked || bytes.IndexByte(rerun.events, 'P') >= 0
			st.check(ctx, t, b, point+", after prune rerun")
		}
		if repacked {
			repacks++
		}
	}

	for _, op := range slices.Compact(slices.Sorted(slices.Values(killOps))) {
		t.Logf("%s: %d runs, %d killed before finishing", op, runsOf[op], kills[op])
	}
	t.Logf("prune: %d of %d runs repacked, the rerun of a killed one included", repacks, runsOf["prune"])
	for _, w := range slices.Sorted(maps.Keys(windows)) {
		t.Logf("killed %s: %d", w, windows[w])
	}
	retire(fmt.Sprintf("seed %d final full pass", seed))
}

// settleKill returns once two listings of b quiet apart agree. A listing over
// sftp takes seconds, which is itself time for the server to drain.
func settleKill(ctx context.Context, t *testing.T, b ark.Backend, quiet time.Duration) {
	t.Helper()
	for prev := killListing(ctx, t, b); ; {
		time.Sleep(quiet)
		cur := killListing(ctx, t, b)
		if maps.Equal(prev, cur) {
			return
		}
		prev = cur
	}
}

// killListing maps every object of b to its size.
func killListing(ctx context.Context, t *testing.T, b ark.Backend) map[ark.Handle]int64 {
	t.Helper()
	objs := map[ark.Handle]int64{}
	for _, typ := range allHandleTypes {
		err := b.List(ctx, typ, func(bi ark.BlobInfo) error {
			objs[ark.Handle{Type: typ, Name: bi.Name}] = bi.Size
			return nil
		})
		if err != nil {
			t.Fatalf("list %s: %v", typ, err)
		}
	}
	return objs
}

// killRepoSpan is the number of runs that share one repository.
const killRepoSpan = 25

// killState is what the parent expects of the repository.
type killState struct {
	want     map[string]map[string]string // tag -> tree the snapshot was taken of
	restored map[string]bool              // snapshot ids already compared
	must     map[string]bool              // tags that have to be present
	gone     map[string]bool              // tags a forget removed
	ids      map[string]string            // tag -> snapshot id, as of the last check
}

// forgetTargets picks one to three tags to forget, always leaving one
// snapshot. It returns nil when fewer than two snapshots exist.
func (st *killState) forgetTargets(pick uint64) []string {
	tags := slices.Sorted(maps.Keys(st.must))
	if len(tags) < 2 {
		return nil
	}
	r := rand.New(rand.NewPCG(pick, pick))
	n := 1 + r.IntN(min(3, len(tags)-1))
	targets := make([]string, n)
	for j, k := range r.Perm(len(tags))[:n] {
		targets[j] = tags[k]
	}
	return targets
}

// newKillRepo initialises a repository in a temporary directory that is only
// removed once the repository passed, so a broken state can be inspected.
func newKillRepo(ctx context.Context, t *testing.T) (string, ark.Backend, func() error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ark-kill-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	ref, b := openKillRepo(ctx, t, dir)
	return ref, b, func() error { return os.RemoveAll(dir) }
}

// openKillRepo opens and initialises the repository at ref, a directory or an
// sftp URI. The caller closes the backend.
func openKillRepo(ctx context.Context, t *testing.T, ref string) (string, ark.Backend) {
	t.Helper()
	b, err := arkuri.Open(ctx, ref)
	if err != nil {
		t.Fatalf("open %s: %v", ref, err)
	}
	cfg := chunker.Config{MinSize: 4 << 10, AvgSize: 8 << 10, MaxSize: scenarioMaxChunk}
	if err := ark.Init(ctx, b, testPassword, ark.WithChunkerConfig(cfg)); err != nil {
		_ = b.Close()
		t.Fatalf("Init %s: %v", ref, err)
	}
	return ref, b
}

// TestKillHarnessChild is the child process of TestKillHarness: one backup,
// forget or prune, named by killEnvOp, against the repository in killEnvRepo.
func TestKillHarnessChild(t *testing.T) {
	repoDir := os.Getenv(killEnvRepo)
	if repoDir == "" {
		t.Skip("runs only as the child of TestKillHarness")
	}
	ctx := context.Background()
	b, err := arkuri.Open(ctx, repoDir)
	if err != nil {
		t.Fatalf("open %s: %v", repoDir, err)
	}
	// fd 3 is the pipe runKillChild reads: the ready byte, then the writes.
	ready := os.NewFile(3, "ready")
	repo, err := ark.Open(ctx, killProbe{b, ready}, testPassword)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = repo.Close() }()
	ark.SetPackLimits(repo, killPackTarget, killCheckpointBlobs)

	var run func() error
	switch op := os.Getenv(killEnvOp); op {
	case "backup":
		src := treeSource(t, os.Getenv(killEnvSrc), os.Getenv(killEnvSeq) != "")
		run = func() error {
			_, err := repo.Backup(ctx, src, ark.WithTags(os.Getenv(killEnvTag)))
			return err
		}
	case "forget":
		ids := strings.Split(os.Getenv(killEnvIDs), ",")
		run = func() error {
			_, err := repo.Forget(ctx, ark.ForgetPolicy{}, ark.SnapshotFilter{IDs: ids})
			return err
		}
	case "prune":
		run = func() error {
			_, err := repo.Prune(ctx)
			return err
		}
	default:
		t.Fatalf("unknown op %q", op)
	}

	// The kill delay starts here.
	if _, err := ready.Write([]byte{killReady}); err != nil {
		t.Fatalf("signal ready: %v", err)
	}
	if err := run(); err != nil {
		t.Fatalf("%s: %v", os.Getenv(killEnvOp), err)
	}
}

// sequentialSource hides the ConcurrentOpen of the source it wraps, so a
// backup reads its files one at a time inside the walk: the path of a source
// that is a stream, such as the tar stream of a docker volume.
type sequentialSource struct{ ark.Source }

// treeSource returns the arkfs source of dir, or the same behind a
// sequentialSource.
func treeSource(t *testing.T, dir string, sequential bool) ark.Source {
	t.Helper()
	src, err := arkfs.NewSource(dir)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	if sequential {
		return sequentialSource{src}
	}
	return src
}

// killReady is the byte a child writes on fd 3 before its op starts. The
// bytes after it are its writes: an upper case letter of killWrites before a
// Put or Delete, the lower case one once it returned.
const killReady = 1

var killWrites = []struct {
	event byte
	name  string
}{
	{'P', "pack put"}, {'I', "index put"}, {'M', "manifest put"},
	{'X', "index delete"}, {'D', "pack delete"}, {'F', "manifest delete"},
}

// killProbe reports the pack, index and manifest writes of a child to w.
type killProbe struct {
	ark.Backend
	w io.Writer
}

func (p killProbe) Put(ctx context.Context, h ark.Handle, r io.Reader) error {
	var event byte
	switch h.Type { //nolint:exhaustive // the other objects are not reported
	case ark.DataPackFile, ark.TreePackFile:
		event = 'P'
	case ark.DataIndexFile, ark.TreeIndexFile:
		event = 'I'
	case ark.SnapshotFile:
		event = 'M'
	}
	return p.report(event, func() error { return p.Backend.Put(ctx, h, r) })
}

func (p killProbe) Delete(ctx context.Context, h ark.Handle) error {
	var event byte
	switch h.Type { //nolint:exhaustive // the other objects are not reported
	case ark.DataPackFile, ark.TreePackFile:
		event = 'D'
	case ark.DataIndexFile, ark.TreeIndexFile:
		event = 'X'
	case ark.SnapshotFile:
		event = 'F'
	}
	return p.report(event, func() error { return p.Backend.Delete(ctx, h) })
}

func (p killProbe) report(event byte, write func() error) error {
	if event == 0 {
		return write()
	}
	_, _ = p.w.Write([]byte{event})
	err := write()
	_, _ = p.w.Write([]byte{event - 'A' + 'a'})
	return err
}

// killWindow names where a kill landed, from the events the child reported
// until then: during a write that had not returned, or after the last one
// that had. A backup killed "during pack put" died in a pack flush, "after
// pack put" between a pack and its index object, "after index put" between
// an index object and the manifest. A prune stores packs only to repack, so
// its kills during or after a pack put landed in a repack; the other three
// steps of a prune are the index put, the index delete and the pack delete.
func killWindow(events []byte) string {
	if len(events) == 0 {
		return "before the first write"
	}
	open := map[byte]int{}
	for _, e := range events {
		if e >= 'a' {
			open[e-'a'+'A']--
		} else {
			open[e]++
		}
	}
	last := events[len(events)-1]
	for _, w := range killWrites {
		if open[w.event] > 0 {
			return "during " + w.name
		}
	}
	for _, w := range killWrites {
		if w.event == last-'a'+'A' {
			return "after " + w.name
		}
	}
	return fmt.Sprintf("after an unknown event %q", last)
}

// killRun is what one child did: took is the time from its ready byte to its
// exit, sinceWrite the time from the first write it reported to its exit,
// zero when it reported none. killed is false when the child exited first.
// events is what the child reported after the ready byte, see killProbe.
type killRun struct {
	took, sinceWrite time.Duration
	killed           bool
	out, events      []byte
}

// runKillChild runs one op in a child process with env added to its
// environment and kills it delay after the child reports it is about to
// start, or with fromWrite delay after it reports its first write; a zero
// delay lets it finish, and so does a child that never writes. err is set
// when the child exited unsuccessfully.
func runKillChild(env []string, delay time.Duration, fromWrite bool) (killRun, error) {
	var buf bytes.Buffer
	ready, w, err := os.Pipe()
	if err != nil {
		return killRun{}, err
	}
	defer func() { _ = ready.Close() }()
	cmd, err := startChild("TestKillHarnessChild", env, &buf, w)
	if err != nil {
		return killRun{}, err
	}
	// The read returns once the child writes its byte, or with EOF once it
	// exits without writing one.
	var b [1]byte
	if _, err := ready.Read(b[:]); err != nil {
		err = cmd.Wait()
		return killRun{out: buf.Bytes()}, fmt.Errorf("child exited before the op started: %w", err)
	}
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// The pipe ends when the child does: nothing else holds its write end.
	// wrote is set before written closes, at the first event.
	var wrote time.Time
	written, reported := make(chan struct{}), make(chan []byte, 1)
	go func() {
		var events []byte
		chunk := make([]byte, 256)
		for {
			n, err := ready.Read(chunk)
			if n > 0 && len(events) == 0 {
				wrote = time.Now()
				close(written)
			}
			events = append(events, chunk[:n]...)
			if err != nil {
				reported <- events
				return
			}
		}
	}()
	finish := func(killed bool) killRun {
		run := killRun{took: time.Since(start), killed: killed, out: buf.Bytes(), events: <-reported}
		if len(run.events) > 0 {
			run.sinceWrite = time.Since(wrote)
		}
		return run
	}

	// armed starts the delay: at once, or at the first write.
	var timeout <-chan time.Time
	armed := written
	if !fromWrite {
		armed = make(chan struct{})
		close(armed)
	}
	for {
		select {
		case err := <-done:
			return finish(false), err
		case <-armed:
			armed = nil
			if delay > 0 {
				timeout = time.After(delay)
			}
		case <-timeout:
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				return killRun{killed: true}, err
			}
			<-done
			return finish(true), nil
		}
	}
}

// startChild re-execs the test binary to run only the test named run, with
// env added to its environment, its output in out and extra as its fds from 3
// on. It closes extra in this process once the child holds them.
func startChild(run string, env []string, out io.Writer, extra ...*os.File) (*exec.Cmd, error) {
	cmd := exec.Command(os.Args[0], "-test.run=^"+run+"$")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = extra
	err := cmd.Start()
	for _, f := range extra {
		_ = f.Close()
	}
	return cmd, err
}

// seededChaCha8 returns a source seeded from the environment variable env, or
// from the clock when it is unset, and the seed that replays it.
func seededChaCha8(t *testing.T, env string) (*rand.ChaCha8, uint64) {
	t.Helper()
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv(env); s != "" {
		var err error
		if seed, err = strconv.ParseUint(s, 10, 64); err != nil {
			t.Fatalf("%s: %v", env, err)
		}
	}
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	return rand.NewChaCha8(key), seed
}

// fullPass restores every snapshot again.
func (st *killState) fullPass(ctx context.Context, t *testing.T, b ark.Backend, point string) {
	t.Helper()
	clear(st.restored)
	st.check(ctx, t, b, point)
}

// check opens the repository the way a fresh process would, runs Check and
// restores every snapshot not restored yet. Every tag in must has to be
// present, none in gone; every present tag joins must.
func (st *killState) check(ctx context.Context, t *testing.T, b ark.Backend, point string) {
	t.Helper()
	repo, err := ark.Open(ctx, b, testPassword)
	if err != nil {
		t.Fatalf("%s: Open: %v", point, err)
	}
	defer func() { _ = repo.Close() }()

	res, err := repo.Check(ctx)
	if err != nil {
		t.Fatalf("%s: Check: %v", point, err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("%s: Check found %d errors: %v", point, len(res.Errors), res.Errors)
	}

	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
	if err != nil {
		t.Fatalf("%s: Snapshots: %v", point, err)
	}
	found := map[string]bool{}
	clear(st.ids)
	for _, s := range snaps {
		if len(s.Tags) != 1 || st.want[s.Tags[0]] == nil {
			t.Fatalf("%s: snapshot %s has unknown tags %v", point, s.ID, s.Tags)
		}
		tag := s.Tags[0]
		if st.gone[tag] {
			t.Fatalf("%s: snapshot %s (%s) was forgotten but is back", point, s.ID, tag)
		}
		found[tag] = true
		st.ids[tag] = s.ID
		if st.restored[s.ID] {
			continue
		}
		dst := t.TempDir()
		sink, err := arkfs.NewSink(dst)
		if err != nil {
			t.Fatalf("NewSink: %v", err)
		}
		if _, err := repo.Restore(ctx, s.ID, sink); err != nil {
			t.Fatalf("%s: Restore %s (%s): %v", point, s.ID, tag, err)
		}
		if diff := diffTree(st.want[tag], readTree(t, dst)); diff != "" {
			t.Fatalf("%s: snapshot %s (%s) restores a different tree: %s", point, s.ID, tag, diff)
		}
		st.restored[s.ID] = true
	}
	for _, tag := range slices.Sorted(maps.Keys(st.must)) {
		if !found[tag] {
			t.Fatalf("%s: snapshot %s is missing", point, tag)
		}
	}
	maps.Copy(st.must, found)
}

// The tree of the harness: killTreeFiles files in killDirs directories. The
// first killHotFiles of them are hot: half of the changes write one of them
// anew, so its chunks die with the few snapshots that hold them, in packs
// they share with the chunks of the other files. A forget then leaves packs
// that are partly dead, which is what a prune repacks; two directories hold no
// hot file, so tree packs keep nodes that outlive their snapshot as well.
const (
	killTreeFiles = 24
	killHotFiles  = 4
	killDirs      = 6
)

func killFilePath(root string, i int) string {
	return filepath.Join(root, fmt.Sprintf("d%d", i%killDirs), fmt.Sprintf("f%02d.bin", i))
}

// killFileSize bounds the size of file i: the hot files are the larger ones,
// so what a forget frees is more than a prune leaves unused.
func killFileSize(i int) int {
	if i < killHotFiles {
		return 128 << 10
	}
	return 64 << 10
}

func writeKillFile(t *testing.T, bytesSrc *rand.ChaCha8, root string, i, size int) {
	t.Helper()
	path := killFilePath(root, i)
	data := make([]byte, size)
	_, _ = bytesSrc.Read(data)
	writeTestFile(t, path, data)
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mutateKillTree changes four files: a hot one, written anew, or any one,
// edited. Either may delete the file instead, and a deleted file is created
// again. An edit replaces one 4 KiB window, so most chunks dedupe against the
// parent.
func mutateKillTree(t *testing.T, rng *rand.Rand, bytesSrc *rand.ChaCha8, root string) {
	t.Helper()
	for range 4 {
		hot, i := rng.IntN(2) == 0, rng.IntN(killTreeFiles)
		if hot {
			i %= killHotFiles
		}
		path := killFilePath(root, i)
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			writeKillFile(t, bytesSrc, root, i, rng.IntN(killFileSize(i)))
			continue
		case err != nil:
		case rng.IntN(5) == 0:
			err = os.Remove(path)
		case hot:
			writeKillFile(t, bytesSrc, root, i, rng.IntN(killFileSize(i)))
		default:
			if len(data) > 0 {
				off := rng.IntN(len(data))
				_, _ = bytesSrc.Read(data[off:min(off+4<<10, len(data))])
			}
			err = os.WriteFile(path, data, 0o644)
		}
		if err != nil {
			t.Fatalf("mutate %s: %v", path, err)
		}
	}
}

// readTree maps every path under root to its type and, for a file, the
// SHA-256 of its content.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		tree[rel] = fmt.Sprintf("file %x", sha256.Sum256(data))
		return err
	})
	if err != nil {
		t.Fatalf("read tree %s: %v", root, err)
	}
	return tree
}

// diffTree names the first path where got differs from want, or returns "".
func diffTree(want, got map[string]string) string {
	for p, w := range want {
		g, ok := got[p]
		switch {
		case !ok:
			return p + " is missing"
		case g != w:
			return fmt.Sprintf("%s is %s, want %s", p, g, w)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			return p + " is extra"
		}
	}
	return ""
}

func killEnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q: want a positive integer", name, s)
	}
	return n
}

// keepOpen lets the parent open and close repositories on one connection:
// Repository.Close closes the backend.
type keepOpen struct{ ark.Backend }

func (keepOpen) Close() error { return nil }
