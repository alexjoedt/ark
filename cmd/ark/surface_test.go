package main

import (
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// TestCommandSurface pins the v1 CLI surface: every command and flag in
// want exists. Extra flags are allowed, removed
// ones are covered by TestRemovedFlagsAndCommands.
func TestCommandSurface(t *testing.T) {
	want := map[string][]string{
		// --version is urfave's own flag, added at run time; TestVersionLine covers it
		"": {
			"repo",
			"json",
			"verbose",
			"quiet",
			"env-file",
			"lock-wait",
			"concurrency",
			"password-file",
			"insecure-no-password",
		},
		"init":      {"chunk-min", "chunk-avg", "chunk-max"},
		"backup":    {"tag", "comment", "host", "parent", "name"},
		"restore":   {"path", "dry-run", "cache"},
		"snapshots": {"name", "type", "tag", "host", "last"},
		"ls":        {"long"},
		"inspect":   {},
		"forget": {
			"keep-last",
			"keep-hourly",
			"keep-daily",
			"keep-weekly",
			"keep-monthly",
			"keep-yearly",
			"keep-within",
			"all",
			"name",
			"type",
			"tag",
			"host",
			"prune",
			"dry-run",
			"yes",
		},
		"prune":          {"dry-run", "yes"},
		"verify":         {},
		"check":          {},
		"export":         {},
		"import":         {},
		"unlock":         {"force"},
		"docker":         {"docker-host"},
		"docker volumes": {},
		"docker backup":  {"tag", "comment", "no-stop", "stop-timeout"},
		"docker restore": {"path", "no-stop", "stop-timeout", "dry-run", "yes"},
		"key":            {},
		"key passwd":     {"new-password-file", "new-insecure-no-password"},
		"serve":          {},
		"version":        {},
	}

	got := map[string]map[string]bool{}
	var walk func(cmd *cli.Command, path string)
	walk = func(cmd *cli.Command, path string) {
		if cmd.Hidden {
			return
		}
		flags := map[string]bool{}
		for _, f := range cmd.Flags {
			if hf, ok := f.(interface{ IsVisible() bool }); ok && !hf.IsVisible() {
				continue
			}
			for _, n := range f.Names() {
				flags[n] = true
			}
		}
		got[path] = flags
		for _, sub := range cmd.Commands {
			walk(sub, strings.TrimSpace(path+" "+sub.Name))
		}
	}
	walk(newApp(), "")

	for path, flags := range want {
		have, ok := got[path]
		if !ok {
			t.Errorf("command %q missing", path)
			continue
		}
		for _, f := range flags {
			if !have[f] {
				t.Errorf("command %q: flag --%s missing", path, f)
			}
		}
	}
	for path := range got {
		if _, ok := want[path]; !ok {
			t.Errorf("command %q is not in the v1 surface", path)
		}
	}
}
