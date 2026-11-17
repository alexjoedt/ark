// Command retentiondocs writes the retention guide docs/retention.md from
// ark's own forget selection. Each scenario backs its
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
	pagePath = "docs/retention.md"
	header   = "[//]: # (Generated from scripts/retentiondocs/retention.md.tmpl by task docs:gen. Edit the template.)\n\n"
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
	// Table lists every snapshot of the history, newest first, with its
	// result and the reasons ark forget --json gives a kept one.
	Table string
	// Kept and Removed are the snapshot times, newest first, in the
	// table's format.
	Kept    []string
	Removed []string
	// EmptyDays are the days between the oldest and the newest kept
	// snapshot that have no snapshot, newest first.
	EmptyDays []string
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
		path := filepath.Join(root, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("creating the directory of %s: %w", f.path, err)
		}
		if err := os.WriteFile(path, f.content, 0o644); err != nil { //nolint:gosec // docs are world-readable
			return fmt.Errorf("writing %s: %w", f.path, err)
		}
	}
	return nil
}

// generate returns the files to write and the outcome of every scenario by
// name.
func generate(ctx context.Context, list []scenario) ([]file, map[string]*outcome, error) {
	outcomes := make(map[string]*outcome, len(list))
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
		outcomes[s.name] = describe(s.name, rows)
	}
	page, err := render(outcomes)
	if err != nil {
		return nil, nil, fmt.Errorf("rendering %s: %w", pagePath, err)
	}
	return []file{{path: pagePath, content: page}}, outcomes, nil
}

// forget backs the history up and returns every snapshot with what a dry
// run of the policy does to it, newest first. Periods are cut in UTC.
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
		rows[i] = row{at: h.at, kept: kept[ids[i]], reasons: result.KeepReasons[ids[i]]}
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
		ark.WithForgetDryRun(), ark.WithForgetLocation(time.UTC))
	if err != nil {
		return nil, fmt.Errorf("forget: %w", err)
	}
	return result, nil
}

func describe(name string, rows []row) *outcome {
	layout, dayLayout := "Mon 02 Jan 15:04", "Mon 02 Jan"
	if rows[0].at.Year() != rows[len(rows)-1].at.Year() {
		layout, dayLayout = "Mon 02 Jan 2006 15:04", "Mon 02 Jan 2006"
	}
	o := &outcome{Name: name}
	o.EmptyDays = emptyDays(rows, dayLayout)
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
		"join": strings.Join,
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

// emptyDays returns the days without a snapshot between the oldest and the
// newest kept one, newest first. rows are newest first.
func emptyDays(rows []row, layout string) []string {
	day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()) }
	taken := make(map[time.Time]bool, len(rows))
	var newest, oldest time.Time
	for _, r := range rows {
		d := day(r.at)
		taken[d] = true
		if r.kept {
			if newest.IsZero() {
				newest = d
			}
			oldest = d
		}
	}
	var empty []string
	for d := newest; d.After(oldest); d = d.AddDate(0, 0, -1) {
		if !taken[d] {
			empty = append(empty, d.Format(layout))
		}
	}
	return empty
}
