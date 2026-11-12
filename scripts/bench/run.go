package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	sftpBudget   = 1.5
	refBudget    = 1.1
	helperImage  = "busybox:stable"
	maxSpread    = 0.10
	benchPass    = "bench"
	minFree      = 45 << 30
	minFreeSmall = 1 << 30
	minFreeFiles = 4 << 30
	sftpRounds   = 3
)

var callerRepoEnv = []string{
	"ARK_REPOSITORY", "ARK_PASSWORD", "ARK_PASSWORD_FILE", "ARK_INSECURE_NO_PASSWORD",
	"RESTIC_REPOSITORY", "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE",
	"RESTIC_PASSWORD_COMMAND", "RESTIC_CACHE_DIR",
}

var phases = [...]string{"initial", "incremental", "restore"}

type times [len(phases)]time.Duration

// tool is one CLI against one kind of repository. repo returns a fresh
// repository location per call and the local directory it lands in, "" when
// that is unknown; known directories are removed after each run. volume
// backs up and restores a Docker volume filled with the dataset instead of
// the dataset directory.
type tool struct {
	name   string
	bin    string
	opts   []string
	ark    bool
	volume bool
	repo   func() (string, string)
}

// result is one run of a tool: its phase times and the repository's bytes on
// disk over its stored bytes after the incremental backup, 0 when unknown.
type result struct {
	t    times
	disk float64
}

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
	need uint64,
	seed int64,
	arkBin, refBin, resticBin, sftpDir string,
) error {
	arkPath, err := exec.LookPath(arkBin)
	if err != nil {
		return fmt.Errorf("ark binary %s not found, build it with task build: %w", arkBin, err)
	}
	refPath := ""
	if refBin != "" {
		if refPath, err = exec.LookPath(refBin); err != nil {
			return fmt.Errorf("reference ark binary %s not found: %w", refBin, err)
		}
	}
	resticPath, err := exec.LookPath(resticBin)
	if err != nil {
		return fmt.Errorf("restic not found (%s), install restic or pass -restic: %w", resticBin, err)
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("run: %s is not empty", dir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("run: reading %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if err := checkFree(dir, need); err != nil {
		return err
	}
	var sftp []tool
	if uri := os.Getenv("ARK_SFTP_TEST_URI"); uri != "" {
		a, r, err := sftpTools(uri, arkPath, resticPath, sftpDir)
		if err != nil {
			return err
		}
		sftp = append(sftp, a)
		if r != nil {
			sftp = append(sftp, *r)
		}
	}
	if sftpDir != "" {
		if err := checkFree(sftpDir, need); err != nil {
			return err
		}
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

	if len(sftp) > 0 && sftpDir != "" {
		if err := b.probeMirror(ctx, sftp[0]); err != nil {
			return err
		}
	}

	header(ctx, w, arkPath, resticPath)
	n := 0
	localRepo := func(name string) func() (string, string) {
		return func() (string, string) {
			n++
			p := filepath.Join(dir, fmt.Sprintf("repo-%s-%d", name, n))
			return p, p
		}
	}
	arkLocal := tool{name: "ark", bin: arkPath, ark: true, repo: localRepo("ark")}
	resticLocal := tool{name: "restic", bin: resticPath, repo: localRepo("restic")}

	tools := []tool{arkLocal, resticLocal}
	runs, err := b.rounds(ctx, w, tools, 5)
	if err != nil {
		return err
	}
	summary(w, "local", tools, runs, budget)

	if len(sftp) > 0 {
		if err := b.sftpRow(ctx, w, sftp, sftpDir != ""); err != nil {
			return err
		}
	}
	if err := b.volumeRows(ctx, w, arkLocal, refPath, sftp, sftpDir != ""); err != nil {
		return err
	}
	ok = true
	return nil
}

// rounds runs every tool once per round until the spread is stable from round
// 3 on, or round last is done, and returns the results per tool. Alternating
// the order keeps drift and the previous run's cleanup from always landing on
// the same tool.
func (b *bench) rounds(ctx context.Context, w io.Writer, tools []tool, last int) ([][]result, error) {
	runs := make([][]result, len(tools))
	for round := 1; ; round++ {
		for k := range tools {
			i := k
			if round%2 == 0 {
				i = len(tools) - 1 - k
			}
			r, err := b.once(ctx, w, tools[i], round)
			if err != nil {
				return nil, err
			}
			runs[i] = append(runs[i], r)
		}
		if round >= 3 && stable(phaseTimes(runs)...) || round >= last {
			return runs, nil
		}
	}
}

// sftpRow times the sftp tools against sftpBudget: sftpRounds rounds when
// mirrored, so each run's repository is removed again, and a single round
// without a verdict otherwise, so only one repository per tool is left on
// the server.
func (b *bench) sftpRow(ctx context.Context, w io.Writer, tools []tool, mirrored bool) error {
	last, title, judge := sftpRounds, "sftp", sftpBudget
	if !mirrored {
		last, title, judge = 1, title+", remote repositories are left on the server, pass -sftp-dir for 3 rounds", 0
	}
	runs, err := b.rounds(ctx, w, tools, last)
	if err != nil {
		return err
	}
	summary(w, title, tools, runs, judge)
	if len(tools) == 1 {
		_, _ = fmt.Fprintln(w, "restic skipped: ARK_SFTP_TEST_URI has no identity")
	}
	return nil
}

// volumeRows times ark docker backup and restore of a volume filled with the
// dataset against the reference ark binary, when there is one, within
// refBudget: on a local repository and, when sftp is set, over sftp with the
// rounds of sftpRow. It runs on the small-files dataset only and is skipped
// with a notice when no Docker daemon answers.
func (b *bench) volumeRows(
	ctx context.Context,
	w io.Writer,
	local tool,
	refBin string,
	sftp []tool,
	mirrored bool,
) error {
	if b.sp != filesSpec {
		_, _ = fmt.Fprintln(w, "\ndocker volume rows skipped: they run with -files")
		return nil
	}
	if err := docker(ctx, "version", "--format", "{{.Server.Version}}"); err != nil {
		_, _ = fmt.Fprintf(w, "\ndocker volume rows skipped: %v\n", err)
		return nil
	}
	pair := func(t tool) []tool {
		t.volume = true
		ts := []tool{t}
		if refBin != "" {
			r := t
			r.name, r.bin = t.name+"-ref", refBin
			ts = append(ts, r)
		}
		return ts
	}
	type row struct {
		title string
		tools []tool
		last  int
		judge float64
	}
	rows := []row{{"docker volume, local", pair(local), 5, refBudget}}
	if len(sftp) > 0 {
		r := row{"docker volume, sftp", pair(sftp[0]), sftpRounds, refBudget}
		if !mirrored {
			r.title, r.last, r.judge = r.title+", remote repositories are left on the server", 1, 0
		}
		rows = append(rows, r)
	}
	for _, r := range rows {
		runs, err := b.rounds(ctx, w, r.tools, r.last)
		if err != nil {
			return err
		}
		summary(w, r.title, r.tools, runs, r.judge)
	}
	if refBin == "" {
		_, _ = fmt.Fprintln(w, "no reference column: pass -ark-ref with the ark binary to compare against")
	}
	return nil
}

// docker runs the docker CLI and returns its output in the error.
func docker(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %w: %s", args[0], err, bytes.TrimSpace(out))
	}
	return nil
}

// fillVolume makes vol hold exactly the dataset, modes and times kept.
func (b *bench) fillVolume(ctx context.Context, vol string) error {
	return docker(ctx, "run", "--rm", "-v", vol+":/data", "-v", b.data+":/src:ro", helperImage,
		"sh", "-c", "find /data -mindepth 1 -delete && cp -a /src/. /data")
}

// copyVolume copies vol into the empty directory dir, owned by the caller.
func copyVolume(ctx context.Context, vol, dir string) error {
	return docker(ctx, "run", "--rm", "-v", vol+":/data:ro", "-v", dir+":/out", helperImage,
		"sh", "-c", fmt.Sprintf("cp -a /data/. /out && chown -R %d:%d /out", os.Getuid(), os.Getgid()))
}

// probeMirror inits an ark repository over sftp and checks that it lands
// below the -sftp-dir directory, readable and removable, before any round
// runs.
func (b *bench) probeMirror(ctx context.Context, t tool) error {
	repo, disk := t.repo()
	if _, _, err := b.exec(ctx, t, "sftp-dir-probe-init", false, "--repo", repo, "init"); err != nil {
		return err
	}
	if _, err := diskRatio(disk); err != nil {
		return fmt.Errorf("run: -sftp-dir does not mirror the sftp URI's path, %s is left on the server: %w", repo, err)
	}
	if err := os.RemoveAll(disk); err != nil {
		return fmt.Errorf("run: -sftp-dir: %w", err)
	}
	return nil
}

// summary prints the medians of tools[0] against tools[1], when present, with
// a verdict against judge when it is set and a warning when the spread is
// above maxSpread.
func summary(w io.Writer, title string, tools []tool, runs [][]result, judge float64) {
	ts := phaseTimes(runs)
	_, _ = fmt.Fprintf(w, "\n%s, median of %d runs\n", title, len(ts[0]))
	if !stable(ts...) {
		parts := make([]string, len(ts))
		for i := range ts {
			parts[i] = fmt.Sprintf("%s %.0f%%", tools[i].name, spread(ts[i])*100)
		}
		what := "the verdict is not stable"
		if judge == 0 {
			what = "the medians are rough"
		}
		_, _ = fmt.Fprintf(w, "warning: spread %s is above %.0f%%, %s\n",
			strings.Join(parts, ", "), maxSpread*100, what)
	}
	var restic times
	var resticDisk float64
	other := "restic"
	if len(runs) > 1 {
		restic, resticDisk, other = median(ts[1]), medianDisk(runs[1]), tools[1].name
	}
	table(w, [2]string{tools[0].name, other}, median(ts[0]), restic, medianDisk(runs[0]), resticDisk, judge)
}

func phaseTimes(runs [][]result) [][]times {
	ts := make([][]times, len(runs))
	for i, rs := range runs {
		for _, r := range rs {
			ts[i] = append(ts[i], r.t)
		}
	}
	return ts
}

func medianDisk(rs []result) float64 {
	d := make([]float64, len(rs))
	for i, r := range rs {
		d[i] = r.disk
	}
	slices.Sort(d)
	return d[len(d)/2]
}

// diskRatio is the space dir takes on disk, directories included, over the
// bytes its regular files hold.
func diskRatio(dir string) (float64, error) {
	var alloc, stored int64
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: no block count", p)
		}
		alloc += st.Blocks * 512
		if fi.Mode().IsRegular() {
			stored += fi.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("disk usage of %s: %w", dir, err)
	}
	if stored == 0 {
		return 0, fmt.Errorf("disk usage of %s: no stored bytes", dir)
	}
	return float64(alloc) / float64(stored), nil
}

// once regenerates the dataset, then times initial backup, incremental backup
// after mutate, and restore of the latest snapshot, and checks the restore
// against the source tree.
func (b *bench) once(ctx context.Context, w io.Writer, t tool, round int) (res result, err error) {
	if err := os.RemoveAll(b.data); err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	if _, _, err := generate(ctx, b.data, b.sp, b.seed); err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	syscall.Sync()
	repo, disk := t.repo()
	if disk != "" {
		defer func() {
			if e := os.RemoveAll(disk); e != nil && err == nil {
				err = fmt.Errorf("run: removing repository: %w", e)
			}
		}()
	}
	defer os.RemoveAll(b.cache)
	tag := fmt.Sprintf("%s-%d", t.name, round)
	vol := ""
	if t.volume {
		vol = fmt.Sprintf("ark-bench-%d", time.Now().UnixNano())
		tag = "docker-" + tag
		defer func() { _ = docker(context.WithoutCancel(ctx), "volume", "rm", "-f", vol) }()
	}

	global := func(a ...string) []string {
		if t.ark {
			return append([]string{"--repo", repo}, a...)
		}
		return append(append([]string{"-r", repo}, t.opts...), a...)
	}
	if _, _, err := b.exec(ctx, t, tag+"-init", false, global("init")...); err != nil {
		return res, err
	}
	var snaps [2]arkSnap
	for i := range 2 {
		if i == 1 {
			if err := mutate(ctx, b.data, b.seed, b.sp.logSizes); err != nil {
				return res, fmt.Errorf("run: %w", err)
			}
			syscall.Sync()
		}
		args := global("backup", b.data)
		switch {
		case vol != "":
			if err := b.fillVolume(ctx, vol); err != nil {
				return res, fmt.Errorf("run: %w", err)
			}
			syscall.Sync()
			args = global("--json", "docker", "backup", vol)
		case t.ark:
			args = global("--json", "backup", b.data)
		}
		d, out, err := b.exec(ctx, t, fmt.Sprintf("%s-%s", tag, phases[i]), t.ark, args...)
		if err != nil {
			return res, err
		}
		res.t[i] = d
		if t.ark {
			if err := json.Unmarshal(out, &snaps[i]); err != nil {
				return res, fmt.Errorf("run: parsing ark backup output: %w", err)
			}
		}
	}
	if disk != "" {
		r, err := diskRatio(disk)
		if err != nil {
			return res, fmt.Errorf("run: %w", err)
		}
		res.disk = r
	}

	if err := os.RemoveAll(b.restore); err != nil {
		return res, fmt.Errorf("run: %w", err)
	}
	defer os.RemoveAll(b.restore)
	args := global("restore", "latest", "--target", b.restore)
	root := filepath.Join(b.restore, b.data)
	switch {
	case vol != "":
		if err := docker(ctx, "volume", "rm", vol); err != nil {
			return res, fmt.Errorf("run: %w", err)
		}
		args = global("docker", "restore", "--yes", snaps[1].ID)
		root = b.restore
	case t.ark:
		args = global("restore", snaps[1].ID, b.restore)
		root = b.restore
	}
	d, _, err := b.exec(ctx, t, tag+"-restore", false, args...)
	if err != nil {
		return res, err
	}
	res.t[2] = d
	if vol != "" {
		if err := os.Mkdir(b.restore, 0o750); err != nil {
			return res, fmt.Errorf("run: %w", err)
		}
		if err := copyVolume(ctx, vol, b.restore); err != nil {
			return res, fmt.Errorf("run: %w", err)
		}
	}

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
		t.name, round, secs(res.t[0]), secs(res.t[1]), secs(res.t[2]))
	if res.disk > 0 {
		_, _ = fmt.Fprintf(w, ", disk/stored %.2f", res.disk)
	}
	if t.ark {
		_, _ = fmt.Fprintf(w, "; chunks new/reused %d/%d, %d/%d",
			snaps[0].Stats.ChunksNew, snaps[0].Stats.ChunksReused,
			snaps[1].Stats.ChunksNew, snaps[1].Stats.ChunksReused)
	}
	_, _ = fmt.Fprintln(w)
	return res, nil
}

// exec runs one command with its output in a log file named after step and
// returns its wall time, including the writeback of what it left dirty, and,
// when capture is set, its stdout.
func (b *bench) exec(
	ctx context.Context,
	t tool,
	step string,
	capture bool,
	args ...string,
) (time.Duration, []byte, error) {
	log, err := os.Create(filepath.Join(b.logs, step+".log"))
	if err != nil {
		return 0, nil, fmt.Errorf("run: %w", err)
	}
	defer log.Close()
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, t.bin, args...)
	cmd.Env = b.env
	cmd.Stdout, cmd.Stderr = log, log
	if capture {
		cmd.Stdout = io.MultiWriter(log, &stdout)
	}
	start := time.Now()
	err = cmd.Run()
	syscall.Sync()
	d := time.Since(start)
	if err != nil {
		return 0, nil, fmt.Errorf("run: %s %s: %w, see %s", t.name, step, err, log.Name())
	}
	return d, stdout.Bytes(), nil
}

// sftpTools derives ark and restic tools from an sftp URI. restic reaches the
// server through ssh built from the URI's port, identity and known_hosts; it
// is nil when the URI has no identity. mirror, when set, is the local
// directory the URI's path is served from.
func sftpTools(uri, arkBin, resticBin, mirror string) (tool, *tool, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "sftp" || u.Host == "" {
		return tool{}, nil, fmt.Errorf("run: ARK_SFTP_TEST_URI %q is not an sftp:// URI", uri)
	}
	fresh := func(prefix string) (string, string) {
		name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
		disk := ""
		if mirror != "" {
			disk = filepath.Join(mirror, name)
		}
		return path.Join("/", u.Path, name), disk
	}
	a := tool{name: "ark-sftp", bin: arkBin, ark: true, repo: func() (string, string) {
		r := *u
		p, disk := fresh("bench-ark")
		r.Path = p
		return r.String(), disk
	}}
	cmd := sftpCommand(u)
	if cmd == "" {
		return a, nil, nil
	}
	// restic's sftp:host:path form cuts at the first colon, so IPv6 hosts need
	// the URL form, where a second leading slash makes the path absolute.
	r := &tool{
		name: "restic-sftp",
		bin:  resticBin,
		opts: []string{"-o", "sftp.command=" + cmd},
		repo: func() (string, string) {
			p, disk := fresh("bench-restic")
			ru := url.URL{Scheme: "sftp", Host: u.Host, Path: "/" + p}
			if user := u.User.Username(); user != "" {
				ru.User = url.User(user)
			}
			return ru.String(), disk
		},
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
	args = append(args, host, "-s", "sftp")
	for i, a := range args {
		args[i] = shellQuote(a)
	}
	return strings.Join(args, " ")
}

// shellQuote quotes a for restic's command splitter, which knows single and
// double quotes but no escapes inside them.
func shellQuote(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n'\"\\") {
		return a
	}
	if !strings.Contains(a, "'") {
		return "'" + a + "'"
	}
	// ponytail: a path with both quote kinds stays unsupported, restic has no escape for it
	return `"` + a + `"`
}

func stable(runs ...[]times) bool {
	for _, r := range runs {
		if spread(r) > maxSpread {
			return false
		}
	}
	return true
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

// verdict is the ratio column and, when judge is set, PASS or FAIL against it.
func verdict(ark, restic time.Duration, judge float64) (string, string) {
	if restic == 0 {
		return "-", ""
	}
	r := float64(ark) / float64(restic)
	if judge == 0 {
		return fmt.Sprintf("%.2f", r), ""
	}
	if r <= judge {
		return fmt.Sprintf("%.2f", r), "PASS"
	}
	return fmt.Sprintf("%.2f", r), "FAIL"
}

func table(w io.Writer, names [2]string, ark, restic times, arkDisk, resticDisk float64, judge float64) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	head := "phase\t" + names[0] + "\t" + names[1] + "\tratio"
	if judge != 0 {
		head += fmt.Sprintf("\tbudget %.1fx", judge)
	}
	_, _ = fmt.Fprintln(tw, head)
	for p, name := range phases {
		ratio, v := verdict(ark[p], restic[p], judge)
		rs := "-"
		if restic[p] > 0 {
			rs = secs(restic[p])
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s", name, secs(ark[p]), rs, ratio)
		if judge != 0 {
			line += "\t" + v
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_, _ = fmt.Fprintf(tw, "disk/stored\t%s\t%s\t-\n", ratio(arkDisk), ratio(resticDisk))
	_ = tw.Flush()
}

func ratio(r float64) string {
	if r == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", r)
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
	// ark's version line carries the commit it was built from.
	_, _ = fmt.Fprintf(w, "ark: %s\n", first(arkBin, "version"))
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

func checkFree(dir string, need uint64) error {
	free, err := freeBytes(dir)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if free < need {
		return fmt.Errorf("run: %s has %.1f GiB free, the benchmark needs %.0f GiB",
			dir, float64(free)/(1<<30), float64(need)/(1<<30))
	}
	return nil
}

func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}
