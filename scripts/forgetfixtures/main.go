// Command forgetfixtures writes the fixtures the forget policy is checked
// against: seeded random snapshot histories and keep policies, each decided
// by restic. It draws the cases, stores every snapshot of a history with
// `restic backup --time`, asks `restic forget --dry-run --json` what the
// policy keeps and writes input and answer as JSON lines.
//
// restic runs here and nowhere else: the test that replays the file needs
// only the file. A run takes about 40 minutes on 12 cores, nearly all of it
// the key derivation of one restic process per snapshot.
//
//	go run ./scripts/forgetfixtures -dir /empty/scratch/dir [-restic BIN] [-seed N] [-cases N]
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// resticVersion is the restic the checked-in fixtures were decided by.
// Another version may decide differently, so it is refused: change the
// constant and regenerate.
const resticVersion = "0.18.1"

// batch is the number of histories that share one restic repository. A
// repository per history would spend its time on init, one for all on
// listing ever more snapshots.
const batch = 25

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "forgetfixtures:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("forgetfixtures", flag.ContinueOnError)
	out := fs.String("out", "testdata/forget/restic.jsonl", "fixture file to write")
	dir := fs.String("dir", "", "empty scratch directory for the restic repositories (required)")
	bin := fs.String("restic", "restic", "restic binary, version "+resticVersion)
	seed := fs.Uint64("seed", 1, "seed of the histories and policies")
	n := fs.Int("cases", 500, "number of cases")
	jobs := fs.Int("jobs", runtime.NumCPU(), "restic processes at a time")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *dir == "" {
		return errors.New("-dir is required")
	}
	scratch, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	if entries, err := os.ReadDir(scratch); err != nil {
		return err
	} else if len(entries) > 0 {
		return fmt.Errorf("-dir %s is not empty", scratch)
	}
	if err := os.Mkdir(filepath.Join(scratch, "src"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scratch, "src", "file"), []byte("forget fixture\n"), 0o644); err != nil {
		return err
	}

	r := restic{bin: *bin, dir: scratch, jobs: *jobs}
	version, err := r.run(ctx, "", "UTC", "version")
	if err != nil {
		return err
	}
	head := header{Restic: strings.TrimSpace(string(version)), Seed: *seed, Cases: *n}
	if !strings.HasPrefix(head.Restic, "restic "+resticVersion+" ") {
		return fmt.Errorf("%s is %q, want restic %s", *bin, head.Restic, resticVersion)
	}

	cases := make([]fixtureCase, *n)
	snapshots := 0
	for i := range cases {
		if cases[i], err = drawCase(*seed, i); err != nil {
			return err
		}
		snapshots += len(cases[i].Times)
	}
	fmt.Fprintf(os.Stderr, "%d cases, %d snapshots\n", len(cases), snapshots)

	for from := 0; from < len(cases); from += batch {
		start := time.Now()
		to := min(from+batch, len(cases))
		if err := r.decide(ctx, from, cases[from:to]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "cases %d to %d decided in %s\n", from, to-1, time.Since(start).Round(time.Second))
	}

	return write(*out, head, cases)
}

func write(name string, head header, cases []fixtureCase) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	err = enc.Encode(head)
	for _, c := range cases {
		err = errors.Join(err, enc.Encode(c))
	}
	return errors.Join(err, w.Flush(), f.Close())
}

// restic runs the binary on repositories below dir.
type restic struct {
	bin, dir string
	jobs     int
}

// run runs restic under the zone tz and returns its standard output. The
// environment is built from scratch: restic reads none of the caller's
// RESTIC_ variables and writes its cache below dir, not into the home
// directory.
func (r restic) run(ctx context.Context, repo, tz string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Dir = r.dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + r.dir,
		"TZ=" + tz,
		"RESTIC_PASSWORD=fixtures",
		"RESTIC_REPOSITORY=" + repo,
		"RESTIC_CACHE_DIR=" + filepath.Join(r.dir, "cache"),
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("restic %s: %w: %s%s", strings.Join(args, " "), err, out, stderr.Bytes())
	}
	return out, nil
}

// decide fills in restic's answer for cases, the first of which is case
// number first. The histories go into one fresh repository, one host each.
func (r restic) decide(ctx context.Context, first int, cases []fixtureCase) error {
	repo := filepath.Join(r.dir, fmt.Sprintf("repo-%d", first))
	if _, err := r.run(ctx, repo, "UTC", "init", "-q"); err != nil {
		return err
	}
	host := func(i int) string { return fmt.Sprintf("case%d", first+i) }

	type backup struct{ c, snapshot int }
	var backups []backup
	for i, c := range cases {
		for j := range c.Times {
			backups = append(backups, backup{i, j})
		}
	}
	err := parallel(ctx, r.jobs, len(backups), func(ctx context.Context, i int) error {
		c := cases[backups[i].c]
		loc, err := time.LoadLocation(c.TZ)
		if err != nil {
			return err
		}
		at := time.Unix(c.Times[backups[i].snapshot], 0).In(loc).Format(timeLayout)
		args := []string{"backup", "-q", "--no-cache", "--force", "--no-scan", "--time", at}
		args = append(args, "--host", host(backups[i].c))
		if c.Tags != nil && c.Tags[backups[i].snapshot] != "" {
			args = append(args, "--tag", c.Tags[backups[i].snapshot])
		}
		_, err = r.run(ctx, repo, c.TZ, append(args, "src")...)
		return err
	})
	if err != nil {
		return err
	}

	err = parallel(ctx, r.jobs, len(cases), func(ctx context.Context, i int) error {
		c := &cases[i]
		args := []string{"forget", "--no-lock", "--no-cache", "--dry-run", "--json", "--host", host(i)}
		for flag, value := range c.Policy {
			args = append(args, "--"+flag, value)
		}
		for _, list := range c.KeepTag {
			args = append(args, "--keep-tag", strings.Join(list, ","))
		}
		out, err := r.run(ctx, repo, c.TZ, args...)
		if err != nil {
			if strings.Contains(err.Error(), "refusing to delete last snapshot") {
				c.Refused = true
				return nil
			}
			return err
		}
		if err := c.answer(out); err != nil {
			return fmt.Errorf("case %d: %w", first+i, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(repo)
}

// answer reads the output of restic forget --json into c, and checks on the
// way that restic stored the history as it was drawn.
func (c *fixtureCase) answer(out []byte) error {
	type snapshot struct {
		Time time.Time `json:"time"`
		Tags []string  `json:"tags"`
	}
	var groups []struct {
		Keep    []snapshot `json:"keep"`
		Remove  []snapshot `json:"remove"`
		Reasons []struct {
			Snapshot snapshot `json:"snapshot"`
			Matches  []string `json:"matches"`
		} `json:"reasons"`
	}
	if err := json.Unmarshal(out, &groups); err != nil {
		return err
	}
	if len(groups) != 1 {
		return fmt.Errorf("restic formed %d groups, want 1", len(groups))
	}
	g := groups[0]
	if len(g.Keep)+len(g.Remove) != len(c.Times) || len(g.Reasons) != len(g.Keep) {
		return fmt.Errorf("restic keeps %d and removes %d with %d reasons, drawn were %d snapshots",
			len(g.Keep), len(g.Remove), len(g.Reasons), len(c.Times))
	}
	index := func(s snapshot) (int, error) {
		i := slices.Index(c.Times, s.Time.Unix())
		if i < 0 {
			return 0, fmt.Errorf("restic has a snapshot at %s, which was not drawn", s.Time)
		}
		if c.Tags != nil && strings.Join(s.Tags, ",") != c.Tags[i] || c.Tags == nil && len(s.Tags) > 0 {
			return 0, fmt.Errorf("restic has tags %q on the snapshot at %s", s.Tags, s.Time)
		}
		return i, nil
	}
	seen := make(map[int]bool)
	for _, s := range g.Remove {
		i, err := index(s)
		if err != nil {
			return err
		}
		seen[i] = true
	}
	for nr, reason := range g.Reasons {
		i, err := index(reason.Snapshot)
		if err != nil {
			return err
		}
		if !g.Keep[nr].Time.Equal(reason.Snapshot.Time) {
			return fmt.Errorf("reason %d is for another snapshot than keep %d", nr, nr)
		}
		seen[i] = true
		c.Keep = append(c.Keep, i)
		c.Reasons = append(c.Reasons, reason.Matches)
	}
	if len(seen) != len(c.Times) {
		return fmt.Errorf("restic lists %d of %d snapshots", len(seen), len(c.Times))
	}
	if !slices.IsSorted(c.Keep) {
		return fmt.Errorf("restic lists the kept snapshots out of order: %v", c.Keep)
	}
	return nil
}

// parallel calls fn for 0 to n-1, jobs at a time, and returns the first
// error, which stops the rest.
func parallel(ctx context.Context, jobs, n int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var wg sync.WaitGroup
	slots := make(chan struct{}, max(jobs, 1))
	for i := 0; i < n && ctx.Err() == nil; i++ {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			if err := fn(ctx, i); err != nil {
				cancel(err)
			}
		})
	}
	wg.Wait()
	return context.Cause(ctx)
}
