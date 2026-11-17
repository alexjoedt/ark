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
	// policies run on the history one at a time: one policy, or two of one
	// rule each to compare.
	policies []ark.ForgetPolicy
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
	// Shared are the kept snapshots more than one rule keeps, newest first.
	Shared []string
	// Reach is how far back each counted period rule keeps, in rule order:
	// "7 most recent days", "all years".
	Reach []string
	// Only are the snapshots this side of a comparison keeps and the other
	// does not, newest first.
	Only []string
	// Versus are the two sides of a comparison, each described alone.
	Versus []*outcome
	// Rules are the policy's rules, in the order ark forget lists their
	// reasons.
	Rules []ruleOutcome
	// Image is the timeline's path relative to the page.
	Image string
}

// ruleOutcome is what the template sees of one rule of a scenario.
type ruleOutcome struct {
	Flag string
	// Counted is true for a rule with a count.
	Counted bool
	// Kept are the snapshots the rule keeps, newest first.
	Kept []string
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
		if n := len(s.policies); n < 1 || n > 2 {
			return nil, nil, fmt.Errorf("scenario %s has %d policies, want 1 or 2", s.name, n)
		}
		sets, err := forget(ctx, s.history, s.policies...)
		if err != nil {
			return nil, nil, fmt.Errorf("scenario %s: %w", s.name, err)
		}
		o, image, err := outcomeOf(s, sets)
		if err != nil {
			return nil, nil, fmt.Errorf("scenario %s: %w", s.name, err)
		}
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

// outcomeOf describes and draws what the policies of s did to its history,
// one set of rows per policy.
func outcomeOf(s scenario, sets [][]row) (*outcome, []byte, error) {
	var marks []ruleMark
	var sides []*outcome
	for i, p := range s.policies {
		m, err := mark(p, sets[i])
		if err != nil {
			return nil, nil, err
		}
		if len(s.policies) > 1 && len(m) != 1 {
			return nil, nil, fmt.Errorf("a compared policy has %d rules, want 1", len(m))
		}
		marks = append(marks, m...)
		sides = append(sides, describe(s.name, m, sets[i]))
	}
	if len(sides) == 1 {
		image, err := timeline(marks, sets[0])
		if err != nil {
			return nil, nil, fmt.Errorf("drawing: %w", err)
		}
		return sides[0], image, nil
	}
	o := &outcome{Name: s.name, Versus: sides}
	var flags []string
	for i, side := range o.Versus {
		flags = append(flags, side.Policy)
		other := o.Versus[1-i]
		for _, k := range side.Kept {
			if !slices.Contains(other.Kept, k) {
				side.Only = append(side.Only, k)
			}
		}
	}
	o.Policy = strings.Join(flags, " and ")
	o.Table = compareTable(flags, sets)
	image, err := comparison(marks, sets[0])
	if err != nil {
		return nil, nil, fmt.Errorf("drawing: %w", err)
	}
	return o, image, nil
}

// compareTable lists every snapshot with what each policy does to it.
func compareTable(flags []string, sets [][]row) string {
	layout, _ := layouts(sets[0])
	var t strings.Builder
	t.WriteString("| Snapshot |")
	for _, f := range flags {
		t.WriteString(" `" + f + "` |")
	}
	t.WriteString("\n|---|" + strings.Repeat("---|", len(flags)) + "\n")
	for i, r := range sets[0] {
		t.WriteString("| " + r.at.Format(layout) + " |")
		for _, rows := range sets {
			t.WriteString(" " + result(rows[i]) + " |")
		}
		t.WriteString("\n")
	}
	return strings.TrimSuffix(t.String(), "\n")
}

// result is a snapshot's reasons in backticks, or removed.
func result(r row) string {
	if !r.kept {
		return "removed"
	}
	reasons := make([]string, len(r.reasons))
	for i, reason := range r.reasons {
		reasons[i] = "`" + reason + "`"
	}
	return strings.Join(reasons, ", ")
}

// zone is where the dry run cuts its periods and where the page and the
// timelines show and cut every time.
var zone = time.UTC

// forget backs the history up and returns, for each policy, every snapshot
// with what a dry run of the policy does to it, newest first and in zone.
func forget(ctx context.Context, history []snap, policies ...ark.ForgetPolicy) (sets [][]row, err error) {
	b := arkmem.New()
	defer func() {
		if cerr := b.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("closing the repository: %w", cerr))
		}
	}()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		return nil, fmt.Errorf("initializing the repository: %w", err)
	}

	// Every backup waits out the lock settle delay, so the backups run side
	// by side, each through its own Repository like separate processes.
	// Closing one must not empty the shared arkmem backend, nor may a dry
	// run; forget closes it on return.
	ids := make([]string, len(history))
	errs := make([]error, len(history))
	var wg sync.WaitGroup
	for i, h := range history {
		wg.Go(func() { ids[i], errs[i] = backup(ctx, keepOpen{b}, h) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	sets = make([][]row, len(policies))
	for p, policy := range policies {
		result, err := dryRun(ctx, keepOpen{b}, policy)
		if err != nil {
			return nil, err
		}
		kept := make(map[string]bool, len(result.Keep))
		for _, k := range result.Keep {
			kept[k.ID] = true
		}
		rows := make([]row, len(history))
		for i, h := range history {
			rows[i] = row{at: h.at.In(zone), kept: kept[ids[i]], reasons: result.KeepReasons[ids[i]]}
		}
		slices.SortStableFunc(rows, func(a, b row) int { return b.at.Compare(a.at) })
		sets[p] = rows
	}
	return sets, nil
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
		r := ruleOutcome{Flag: m.flag, Counted: m.count != 0}
		if m.cut && m.count != 0 {
			o.Reach = append(o.Reach, reach(m.count, periodWords[m.period].noun))
		}
		if m.window != (ark.Within{}) {
			r.WindowStart = m.winStart.Format(layout)
		}
		for _, p := range slices.Backward(m.noCount) {
			r.Gaps = append(r.Gaps, m.period.name(p, rows))
		}
		if m.byOldest >= 0 {
			r.Oldest = rows[m.byOldest].at.Format(layout)
		}
		for i := range rows {
			if m.keeps[i] {
				r.Kept = append(r.Kept, rows[i].at.Format(layout))
			}
		}
		o.Rules = append(o.Rules, r)
	}
	o.Policy = strings.Join(flags, " ")
	var t strings.Builder
	t.WriteString("| Snapshot | Result | Reason |\n|---|---|---|\n")
	for i, r := range rows {
		when := r.at.Format(layout)
		if !r.kept {
			o.Removed = append(o.Removed, when)
			fmt.Fprintf(&t, "| %s | removed | |\n", when)
			continue
		}
		o.Kept = append(o.Kept, when)
		keptBy := 0
		for _, m := range marks {
			if m.keeps[i] {
				keptBy++
			}
		}
		if keptBy > 1 {
			o.Shared = append(o.Shared, when)
		}
		fmt.Fprintf(&t, "| %s | kept | %s |\n", when, result(r))
	}
	o.Table = strings.TrimSuffix(t.String(), "\n")
	return o
}

// reach is how far back a counted period rule keeps: "7 most recent days".
func reach(count ark.KeepCount, noun string) string {
	switch count {
	case ark.Unlimited:
		return "all " + noun + "s"
	case 1:
		return "most recent " + noun
	}
	return fmt.Sprintf("%d most recent %ss", count, noun)
}

func render(outcomes map[string]*outcome) ([]byte, error) {
	funcs := template.FuncMap{
		"list": func(items []string) string {
			if len(items) == 0 {
				return "nothing"
			}
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
