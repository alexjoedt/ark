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
			"profile",
			"profile-file",
			"json",
			"verbose",
			"quiet",
			"env-file",
			"lock-wait",
			"concurrency",
			"password-file",
			"password-command",
			"insecure-no-password",
		},
		"init": {"chunk-min", "chunk-avg", "chunk-max"},
		"backup": {
			"tag", "comment", "host", "parent", "time", "source", "exclude", "exclude-file", "exclude-cloud-files",
		},
		"restore":   {"path", "dry-run", "cache"},
		"snapshots": {"source", "type", "tag", "host", "last"},
		"ls":        {"long"},
		"find":      {"source", "type", "tag", "host"},
		"inspect":   {},
		"mount":     {},
		"forget": {
			"keep-last",
			"keep-hourly",
			"keep-daily",
			"keep-weekly",
			"keep-monthly",
			"keep-yearly",
			"keep-within",
			"all",
			"source",
			"type",
			"tag",
			"host",
			"prune",
			"dry-run",
			"yes",
		},
		"prune":          {"dry-run", "yes"},
		"repair":         {},
		"repair index":   {},
		"verify":         {},
		"check":          {},
		"export":         {},
		"import":         {},
		"unlock":         {"force"},
		"docker":         {"docker-host"},
		"docker volumes": {},
		"docker backup":  {"tag", "comment", "time", "no-stop", "stop-timeout"},
		"docker restore": {"path", "no-stop", "stop-timeout", "dry-run", "yes"},
		"key":            {},
		"key passwd":     {"new-password-file", "new-insecure-no-password"},
		"profile":        {},
		"profile list":   {},
		"profile show":   {},
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
