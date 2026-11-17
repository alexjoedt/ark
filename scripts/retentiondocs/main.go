// Command retentiondocs writes the retention guide docs/retention.md and its
// timelines under docs/retention/ from ark's own forget selection. Each scenario backs its
// history up into an in-memory repository, one snapshot per time, and asks
// Repository.Forget in a dry run what the policy keeps. The page is rendered
// whole from retention.md.tmpl, so no outcome in it is written by hand.
//
//	go run ./scripts/retentiondocs [-root DIR]
//
// TestGeneratedFilesAreCurrent fails when a committed file differs from what
// this command writes.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

//go:embed retention.md.tmpl
var pageTemplate string

const (
	pageDir   = "docs"
	pagePath  = pageDir + "/retention.md"
	imageLink = "retention" // the timelines' directory as the page links it
	imageDir  = pageDir + "/" + imageLink
	header    = "[//]: # (Generated from scripts/retentiondocs/retention.md.tmpl by task docs:gen. Edit the template.)\n\n"
)

type scenario struct {
	name    string
	history []snap
	policy  ark.ForgetPolicy
}

type snap struct {
	at   time.Time
	tags []string
}

// row is one snapshot of a scenario's history after the forget.
type row struct {
	at      time.Time
	kept    bool
	reasons []string
}

// outcome is what the template sees of a scenario.
type outcome struct {
	Name string
	// Policy is the policy as ark forget flags, "--keep-daily 5".
	Policy string
	// Table lists every snapshot of the history, newest first, with its
	// result and the reasons ark forget --json gives a kept one.
	Table string
	// Kept and Removed are the snapshot times, newest first, in the
	// table's format.
	Kept    []string
	Removed []string
	// Rules are the policy's rules, in the order ark forget lists their
	// reasons.
	Rules []ruleOutcome
	// Image is the timeline's path relative to the page.
	Image string
}

// ruleOutcome is what the template sees of one rule of a scenario.
type ruleOutcome struct {
	Flag string
	// Gaps are the periods a counted period rule counts across without a
	// snapshot, newest first: "Mon 02 Mar 11:00", "Sat 21 Mar", "May",
	// "2013"; a week by the day it starts.
	Gaps []string
	// Oldest is the snapshot the rule keeps as the oldest of the history,
	// although its period is taken; empty when it keeps none.
	Oldest string
	// WindowStart is where the window of a within rule starts.
	WindowStart string
}

// file is one generated file.
type file struct {
	path    string
	content []byte
}

func main() {
	root := flag.String("root", ".", "repository root the files are written under")
	flag.Parse()
	if err := run(context.Background(), *root); err != nil {
		fmt.Fprintln(os.Stderr, "retentiondocs:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, root string) error {
	files, _, err := generate(ctx, scenarios())
	if err != nil {
		return err
	}
	for _, f := range files {
		dst := filepath.Join(root, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("creating the directory of %s: %w", f.path, err)
		}
		if err := os.WriteFile(dst, f.content, 0o644); err != nil { //nolint:gosec // docs are world-readable
			return fmt.Errorf("writing %s: %w", f.path, err)
		}
	}
	return nil
}

// generate returns the files to write and the outcome of every scenario by
// name.
func generate(ctx context.Context, list []scenario) ([]file, map[string]*outcome, error) {
	outcomes := make(map[string]*outcome, len(list))
	var files []file
	for _, s := range list {
		if _, dup := outcomes[s.name]; dup {
			return nil, nil, fmt.Errorf("scenario %q defined twice", s.name)
		}
		if len(s.history) == 0 {
			return nil, nil, fmt.Errorf("scenario %s has no snapshot", s.name)
		}
		rows, err := forget(ctx, s)
		if err != nil {
			return nil, nil, fmt.Errorf("scenario %s: %w", s.name, err)
		}
		marks, err := mark(s.policy, rows)
		if err != nil {
			return nil, nil, fmt.Errorf("scenario %s: %w", s.name, err)
		}
		image, err := timeline(marks, rows)
		if err != nil {
			return nil, nil, fmt.Errorf("drawing scenario %s: %w", s.name, err)
		}
		o := describe(s.name, marks, rows)
		files = append(files, file{path: path.Join(imageDir, s.name+".svg"), content: image})
		o.Image = path.Join(imageLink, s.name+".svg")
		outcomes[s.name] = o
	}
	page, err := render(outcomes)
	if err != nil {
		return nil, nil, fmt.Errorf("rendering %s: %w", pagePath, err)
	}
	return append([]file{{path: pagePath, content: page}}, files...), outcomes, nil
}

// zone is where the dry run cuts its periods and where the page and the
// timelines show and cut every time.
var zone = time.UTC

// forget backs the history up and returns every snapshot with what a dry
// run of the policy does to it, newest first and in zone.
func forget(ctx context.Context, s scenario) ([]row, error) {
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		return nil, fmt.Errorf("initializing the repository: %w", err)
	}

	// Every backup waits out the lock settle delay, so the backups run side
	// by side, each through its own Repository like separate processes.
	// Closing one must not empty the shared arkmem backend; dryRun closes
	// it at the end.
	ids := make([]string, len(s.history))
	errs := make([]error, len(s.history))
	var wg sync.WaitGroup
	for i, h := range s.history {
		wg.Go(func() { ids[i], errs[i] = backup(ctx, keepOpen{b}, h) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	result, err := dryRun(ctx, b, s.policy)
	if err != nil {
		return nil, err
	}
	kept := make(map[string]bool, len(result.Keep))
	for _, k := range result.Keep {
		kept[k.ID] = true
	}

	rows := make([]row, len(s.history))
	for i, h := range s.history {
		rows[i] = row{at: h.at.In(zone), kept: kept[ids[i]], reasons: result.KeepReasons[ids[i]]}
	}
	slices.SortStableFunc(rows, func(a, b row) int { return b.at.Compare(a.at) })
	return rows, nil
}

// keepOpen is a Backend whose Close leaves the backend as it is.
type keepOpen struct{ ark.Backend }

func (keepOpen) Close() error { return nil }

func backup(ctx context.Context, b ark.Backend, h snap) (id string, err error) {
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		return "", fmt.Errorf("opening the repository: %w", err)
	}
	defer func() {
		if cerr := repo.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("closing the repository: %w", cerr))
		}
	}()
	src := ark.NewStreamSource("data", strings.NewReader(h.at.String()))
	snapshot, err := repo.Backup(ctx, src, ark.WithTime(h.at), ark.WithTags(h.tags...))
	if err != nil {
		return "", fmt.Errorf("backup at %s: %w", h.at, err)
	}
	return snapshot.ID, nil
}

func dryRun(ctx context.Context, b ark.Backend, policy ark.ForgetPolicy) (result *ark.ForgetResult, err error) {
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		return nil, fmt.Errorf("opening the repository: %w", err)
	}
	defer func() {
		if cerr := repo.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("closing the repository: %w", cerr))
		}
	}()
	result, err = repo.Forget(ctx, policy, ark.SnapshotFilter{},
		ark.WithForgetDryRun(), ark.WithForgetLocation(zone))
	if err != nil {
		return nil, fmt.Errorf("forget: %w", err)
	}
	return result, nil
}

// layouts returns the formats of a snapshot time and of a day for rows,
// newest first: with the year only when the history crosses a year turn.
func layouts(rows []row) (timeLayout, dayLayout string) {
	if crossesYear(rows) {
		return "Mon 02 Jan 2006 15:04", "Mon 02 Jan 2006"
	}
	return "Mon 02 Jan 15:04", "Mon 02 Jan"
}

// crossesYear reports whether rows, newest first, cross a year turn.
func crossesYear(rows []row) bool {
	return rows[0].at.Year() != rows[len(rows)-1].at.Year()
}

func describe(name string, marks []ruleMark, rows []row) *outcome {
	layout, _ := layouts(rows)
	o := &outcome{Name: name}
	var flags []string
	for _, m := range marks {
		flags = append(flags, m.flag)
		r := ruleOutcome{Flag: m.flag}
		if m.window != (ark.Within{}) {
			r.WindowStart = m.winStart.Format(layout)
		}
		for _, p := range slices.Backward(m.noCount) {
			r.Gaps = append(r.Gaps, m.period.name(p, rows))
		}
		if m.byOldest >= 0 {
			r.Oldest = rows[m.byOldest].at.Format(layout)
		}
		o.Rules = append(o.Rules, r)
	}
	o.Policy = strings.Join(flags, " ")
	var t strings.Builder
	t.WriteString("| Snapshot | Result | Reason |\n|---|---|---|\n")
	for _, r := range rows {
		when := r.at.Format(layout)
		if !r.kept {
			o.Removed = append(o.Removed, when)
			fmt.Fprintf(&t, "| %s | removed | |\n", when)
			continue
		}
		o.Kept = append(o.Kept, when)
		reasons := make([]string, len(r.reasons))
		for i, reason := range r.reasons {
			reasons[i] = "`" + reason + "`"
		}
		fmt.Fprintf(&t, "| %s | kept | %s |\n", when, strings.Join(reasons, ", "))
	}
	o.Table = strings.TrimSuffix(t.String(), "\n")
	return o
}

func render(outcomes map[string]*outcome) ([]byte, error) {
	funcs := template.FuncMap{
		"list": func(items []string) string {
			if len(items) < 2 {
				return strings.Join(items, "")
			}
			return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
		},
		"plural": func(n int, noun string) string {
			if n == 1 {
				return "1 " + noun
			}
			return fmt.Sprintf("%d %ss", n, noun)
		},
		"scenario": func(name string) (*outcome, error) {
			o, ok := outcomes[name]
			if !ok {
				return nil, errors.New("no scenario named " + name)
			}
			return o, nil
		},
	}
	tmpl, err := template.New("retention.md.tmpl").Funcs(funcs).Option("missingkey=error").Parse(pageTemplate)
	if err != nil {
		return nil, fmt.Errorf("parsing the template: %w", err)
	}
	var page bytes.Buffer
	page.WriteString(header)
	if err := tmpl.Execute(&page, nil); err != nil {
		return nil, fmt.Errorf("executing the template: %w", err)
	}
	return page.Bytes(), nil
}
