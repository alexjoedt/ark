package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

const (
	budget       = 1.2
	maxSpread    = 0.10
	benchPass    = "bench"
	minFree      = 45 << 30
	minFreeSmall = 1 << 30
)

var callerRepoEnv = []string{
	"ARK_REPOSITORY", "ARK_PASSWORD", "ARK_PASSWORD_FILE", "ARK_INSECURE_NO_PASSWORD",
	"RESTIC_REPOSITORY", "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE",
	"RESTIC_PASSWORD_COMMAND", "RESTIC_CACHE_DIR",
}

var phases = [...]string{"initial", "incremental", "restore"}

type times [len(phases)]time.Duration

// tool is one CLI against one kind of repository. repo returns a fresh
// repository location per call; local ones are removed after the run.
type tool struct {
	name  string
	bin   string
	opts  []string
	local bool
	repo  func() string
}

func (t tool) ark() bool { return strings.HasPrefix(t.name, "ark") }

type arkSnap struct {
	ID    string `json:"id"`
	Stats struct {
		ChunksNew    int `json:"chunks_new"`
		ChunksReused int `json:"chunks_reused"`
	} `json:"stats"`
}

type bench struct {
	data, restore, cache, logs string
	sp                         spec
	seed                       int64
	env                        []string
}

// runBench times ark against restic on the generated dataset, interleaved
// with a fresh dataset and repository per run, and prints a table per phase.
func runBench(
	ctx context.Context,
	w io.Writer,
	dir string,
	sp spec,
	small bool,
	seed int64,
	arkBin, resticBin string,
) error {
	arkPath, err := exec.LookPath(arkBin)
	if err != nil {
		return fmt.Errorf("ark binary %s not found, build it with task build: %w", arkBin, err)
	}
	resticPath, err := exec.LookPath(resticBin)
	if err != nil {
		return fmt.Errorf("restic not found (%s), install restic or pass -restic: %w", resticBin, err)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("run: %s is not empty", dir)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	need := uint64(minFree)
	if small {
		need = minFreeSmall
	}
	free, err := freeBytes(dir)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if free < need {
		return fmt.Errorf("run: %s has %.1f GiB free, the benchmark needs %.0f GiB",
			dir, float64(free)/(1<<30), float64(need)/(1<<30))
	}

	b := &bench{
		data:    filepath.Join(dir, "data"),
		restore: filepath.Join(dir, "restore"),
		cache:   filepath.Join(dir, "restic-cache"),
		logs:    filepath.Join(dir, "logs"),
		sp:      sp,
		seed:    seed,
	}
	// Repository and password settings of the caller would override the
	// bench's own; ARK_SFTP_KEY_PASSPHRASE and the like pass through.
	b.env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(callerRepoEnv, k)
	})
	b.env = append(b.env, "ARK_PASSWORD="+benchPass, "RESTIC_PASSWORD="+benchPass, "RESTIC_CACHE_DIR="+b.cache)
	if err := os.Mkdir(b.logs, 0o750); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	ok := false
	defer func() {
		for _, p := range []string{b.data, b.restore, b.cache} {
			_ = os.RemoveAll(p)
		}
		if ok {
			_ = os.RemoveAll(b.logs)
		} else {
			_, _ = fmt.Fprintf(w, "logs kept in %s\n", b.logs)
		}
	}()

	header(ctx, w, arkPath, resticPath)
	n := 0
	localRepo := func(name string) func() string {
		return func() string { n++; return filepath.Join(dir, fmt.Sprintf("repo-%s-%d", name, n)) }
	}
	arkLocal := tool{name: "ark", bin: arkPath, local: true}
	arkLocal.repo = localRepo("ark")
	resticLocal := tool{name: "restic", bin: resticPath, local: true}
	resticLocal.repo = localRepo("restic")

	var arkRuns, resticRuns []times
	for round := 1; ; round++ {
		at, err := b.once(ctx, w, arkLocal, round)
		if err != nil {
			return err
		}
		rt, err := b.once(ctx, w, resticLocal, round)
		if err != nil {
			return err
		}
		arkRuns, resticRuns = append(arkRuns, at), append(resticRuns, rt)
		if round == 5 || round == 3 && spread(arkRuns) <= maxSpread && spread(resticRuns) <= maxSpread {
			break
		}
	}
	_, _ = fmt.Fprintf(w, "\nlocal, median of %d runs\n", len(arkRuns))
	table(w, median(arkRuns), median(resticRuns), true)

	if uri := os.Getenv("ARK_SFTP_TEST_URI"); uri != "" {
		if err := b.sftpRow(ctx, w, uri, arkPath, resticPath); err != nil {
			return err
		}
	}
	ok = true
	return nil
}

// sftpRow times one ark and, when the URI names an identity, one restic run
// against the sftp server and prints them without a verdict.
func (b *bench) sftpRow(ctx context.Context, w io.Writer, uri, arkPath, resticPath string) error {
	arkSFTP, resticSFTP, err := sftpTools(uri, arkPath, resticPath)
	if err != nil {
		return err
	}
	at, err := b.once(ctx, w, arkSFTP, 1)
	if err != nil {
		return err
	}
	var rt times
	if resticSFTP != nil {
		if rt, err = b.once(ctx, w, *resticSFTP, 1); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprintln(w, "\nsftp, informational, one run, remote repositories are left on the server")
	if resticSFTP == nil {
		_, _ = fmt.Fprintln(w, "restic skipped: ARK_SFTP_TEST_URI has no identity")
	}
	table(w, at, rt, false)
	return nil
}

// once regenerates the dataset, then times initial backup, incremental backup
// after mutate, and restore of the latest snapshot, and checks the restore
// against the source tree.
func (b *bench) once(ctx context.Context, w io.Writer, t tool, round int) (times, error) {
	var res times
	if err := os.RemoveAll(b.data); err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	if _, _, err := generate(ctx, b.data, b.sp, b.seed); err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	syscall.Sync()
	repo := t.repo()
	if t.local {
		defer os.RemoveAll(repo)
	}
	defer os.RemoveAll(b.cache)
	tag := fmt.Sprintf("%s-%d", t.name, round)

	global := func(a ...string) []string {
		if t.ark() {
			return append([]string{"--repo", repo}, a...)
		}
		return append(append([]string{"-r", repo}, t.opts...), a...)
	}
	if _, _, err := b.exec(ctx, t, tag+"-init", global("init")...); err != nil {
		return res, err
	}
	var snaps [2]arkSnap
	for i := range 2 {
		if i == 1 {
			if err := mutate(ctx, b.data, b.seed); err != nil {
				return res, fmt.Errorf("run: %w", err)
			}
			syscall.Sync()
		}
		args := global("backup", b.data)
		if t.ark() {
			args = global("--json", "backup", b.data)
		}
		d, out, err := b.exec(ctx, t, fmt.Sprintf("%s-%s", tag, phases[i]), args...)
		if err != nil {
			return res, err
		}
		res[i] = d
		if t.ark() {
			if err := json.Unmarshal(out, &snaps[i]); err != nil {
				return res, fmt.Errorf("run: parsing ark backup output: %w", err)
			}
		}
		syscall.Sync()
	}

	if err := os.RemoveAll(b.restore); err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	defer os.RemoveAll(b.restore)
	args := global("restore", "latest", "--target", b.restore)
	root := filepath.Join(b.restore, b.data)
	if t.ark() {
		args = global("restore", snaps[1].ID, b.restore)
		root = b.restore
	}
	d, _, err := b.exec(ctx, t, tag+"-restore", args...)
	if err != nil {
		return res, err
	}
	res[2] = d
	syscall.Sync()

	want, _, _, err := hashTree(ctx, b.data)
	if err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	got, _, _, err := hashTree(ctx, root)
	if err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	if got != want {
		return res, fmt.Errorf("run: %s restore differs from the source tree: %s, want %s", tag, got, want)
	}

	_, _ = fmt.Fprintf(w, "%-12s run %d: initial %s, incremental %s, restore %s",
		t.name, round, secs(res[0]), secs(res[1]), secs(res[2]))
	if t.ark() {
		_, _ = fmt.Fprintf(w, "; chunks new/reused %d/%d, %d/%d",
			snaps[0].Stats.ChunksNew, snaps[0].Stats.ChunksReused,
			snaps[1].Stats.ChunksNew, snaps[1].Stats.ChunksReused)
	}
	_, _ = fmt.Fprintln(w)
	return res, nil
}

// exec runs one command with its stderr, and its stdout unless it is
// returned, in a log file named after step, and returns its wall time.
func (b *bench) exec(ctx context.Context, t tool, step string, args ...string) (time.Duration, []byte, error) {
	log, err := os.Create(filepath.Join(b.logs, step+".log"))
	if err != nil {
		return 0, nil, fmt.Errorf("run: %w", err)
	}
	defer log.Close()
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, t.bin, args...)
	cmd.Env = b.env
	cmd.Stdout, cmd.Stderr = log, log
	if t.ark() && slices.Contains(args, "--json") {
		cmd.Stdout = &stdout
	}
	start := time.Now()
	err = cmd.Run()
	d := time.Since(start)
	if err != nil {
		return 0, nil, fmt.Errorf("run: %s %s: %w, see %s", t.name, step, err, log.Name())
	}
	return d, stdout.Bytes(), nil
}

// sftpTools derives ark and restic tools from an sftp URI. restic reaches the
// server through ssh built from the URI's port, identity and known_hosts; it
// is nil when the URI has no identity.
func sftpTools(uri, arkBin, resticBin string) (tool, *tool, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "sftp" || u.Host == "" {
		return tool{}, nil, fmt.Errorf("run: ARK_SFTP_TEST_URI %q is not an sftp:// URI", uri)
	}
	fresh := func(prefix string) string {
		return path.Join(u.Path, fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()))
	}
	a := tool{name: "ark-sftp", bin: arkBin, repo: func() string {
		r := *u
		r.Path = fresh("bench-ark")
		return r.String()
	}}
	cmd := sftpCommand(u)
	if cmd == "" {
		return a, nil, nil
	}
	host := u.Hostname()
	if user := u.User.Username(); user != "" {
		host = user + "@" + host
	}
	r := &tool{
		name: "restic-sftp",
		bin:  resticBin,
		opts: []string{"-o", "sftp.command=" + cmd},
		repo: func() string { return "sftp:" + host + ":" + fresh("bench-restic") },
	}
	return a, r, nil
}

// sftpCommand is the ssh command line restic runs for u, or "" when u names
// no identity.
func sftpCommand(u *url.URL) string {
	q := u.Query()
	id := q.Get("identity")
	if id == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	args := []string{"ssh", "-p", port, "-i", id, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes"}
	if kh := q.Get("known_hosts"); kh != "" {
		args = append(args, "-o", "UserKnownHostsFile="+kh)
	}
	if q.Get("insecure") == "1" {
		args = append(args, "-o", "StrictHostKeyChecking=no")
	}
	host := u.Hostname()
	if user := u.User.Username(); user != "" {
		host = user + "@" + host
	}
	return strings.Join(append(args, host, "-s", "sftp"), " ")
}

func spread(runs []times) float64 {
	m := median(runs)
	var worst float64
	for p := range phases {
		lo, hi := runs[0][p], runs[0][p]
		for _, r := range runs {
			lo, hi = min(lo, r[p]), max(hi, r[p])
		}
		if m[p] > 0 {
			worst = max(worst, float64(hi-lo)/float64(m[p]))
		}
	}
	return worst
}

func median(runs []times) times {
	var m times
	for p := range phases {
		col := make([]time.Duration, len(runs))
		for i, r := range runs {
			col[i] = r[p]
		}
		slices.Sort(col)
		m[p] = col[len(col)/2]
	}
	return m
}

// verdict is the ratio column and, when judged, PASS or FAIL against budget.
func verdict(ark, restic time.Duration, judged bool) (string, string) {
	if restic == 0 {
		return "-", ""
	}
	r := float64(ark) / float64(restic)
	if !judged {
		return fmt.Sprintf("%.2f", r), ""
	}
	if r <= budget {
		return fmt.Sprintf("%.2f", r), "PASS"
	}
	return fmt.Sprintf("%.2f", r), "FAIL"
}

func table(w io.Writer, ark, restic times, judged bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "phase\tark\trestic\tratio"
	if judged {
		head += fmt.Sprintf("\tbudget %.1fx", budget)
	}
	_, _ = fmt.Fprintln(tw, head)
	for p, name := range phases {
		ratio, v := verdict(ark[p], restic[p], judged)
		rs := "-"
		if restic[p] > 0 {
			rs = secs(restic[p])
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s", name, secs(ark[p]), rs, ratio)
		if judged {
			line += "\t" + v
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

func secs(d time.Duration) string { return fmt.Sprintf("%.1fs", d.Seconds()) }

func header(ctx context.Context, w io.Writer, arkBin, resticBin string) {
	first := func(name string, args ...string) string {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		if err != nil {
			return "unknown"
		}
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return line
	}
	host, _ := os.Hostname()
	_, _ = fmt.Fprintf(
		w,
		"machine: %s %s/%s, %d CPUs, %s RAM\n",
		host,
		runtime.GOOS,
		runtime.GOARCH,
		runtime.NumCPU(),
		memTotal(),
	)
	_, _ = fmt.Fprintf(
		w,
		"ark: %s (commit %s)\n",
		first(arkBin, "version"),
		first("git", "rev-parse", "--short", "HEAD"),
	)
	_, _ = fmt.Fprintf(w, "restic: %s\n", first(resticBin, "version"))
}

func memTotal() string {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return "unknown"
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "MemTotal:"); ok {
			var kb int64
			if _, err := fmt.Sscan(strings.TrimSuffix(strings.TrimSpace(v), " kB"), &kb); err == nil {
				return fmt.Sprintf("%.1f GiB", float64(kb)/(1<<20))
			}
		}
	}
	return "unknown"
}

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}
