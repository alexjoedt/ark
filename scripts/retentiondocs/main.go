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
	imageDir = "retention"
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
	// Image is the link of the scenario's timeline, relative to the page.
	Image string
	// Kept and Removed are the snapshot times, newest first, in the
	// table's format.
	Kept    []string
	Removed []string
}

// file is one generated file; scenario is empty for the page.
type file struct {
	path     string
	scenario string
	content  []byte
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
			return err
		}
		if err := os.WriteFile(path, f.content, 0o644); err != nil { //nolint:gosec // docs are world-readable
			return err
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
		outcomes[s.name] = nil
	}
	// Every backup waits out the lock settle delay, so the scenarios run
	// side by side, each in its own repository.
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, s := range list {
		wg.Go(func() {
			rows, err := forget(ctx, s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("scenario %s: %w", s.name, err))
				return
			}
			outcomes[s.name] = describe(s.name, rows)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}
	page, err := render(outcomes)
	if err != nil {
		return nil, nil, err
	}
	return []file{{path: pagePath, content: page}}, outcomes, nil
}

// forget backs the history up and returns every snapshot with what a dry
// run of the policy does to it, newest first. Periods are cut in UTC.
func forget(ctx context.Context, s scenario) ([]row, error) {
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		return nil, err
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		return nil, err
	}
	defer repo.Close()

	ids := make([]string, len(s.history))
	for i, h := range s.history {
		src := ark.NewStreamSource("data", strings.NewReader(h.at.String()))
		snapshot, err := repo.Backup(ctx, src, ark.WithTime(h.at), ark.WithTags(h.tags...))
		if err != nil {
			return nil, fmt.Errorf("backup at %s: %w", h.at, err)
		}
		ids[i] = snapshot.ID
	}

	result, err := repo.Forget(ctx, s.policy, ark.SnapshotFilter{},
		ark.WithForgetDryRun(), ark.WithForgetLocation(time.UTC))
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

func describe(name string, rows []row) *outcome {
	layout := "Mon 02 Jan 15:04"
	if rows[0].at.Year() != rows[len(rows)-1].at.Year() {
		layout = "Mon 02 Jan 2006 15:04"
	}
	o := &outcome{Name: name, Image: imageDir + "/" + name + ".svg"}
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
		return nil, err
	}
	var page bytes.Buffer
	page.WriteString(header)
	if err := tmpl.Execute(&page, nil); err != nil {
		return nil, err
	}
	return page.Bytes(), nil
}
