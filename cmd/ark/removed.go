package main

import (
	"context"
	"strings"

	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

// removedFlags lists the flags v1 dropped, by command path ("" is the
// root), each with the replacement the usage error names. v1 chose
// this over a compatibility layer: they are hidden and fail on sight.
var removedFlags = map[string][]struct{ name, bool, replacement string }{
	"": {
		{"docker-socket", "", "ark docker --docker-host"},
		{"stop-timeout", "", "ark docker backup|restore --stop-timeout"},
	},
	"snapshots":      {{"volume", "", "--name"}},
	"forget":         {{"volume", "", "--name"}, {"until", "", "--keep-within"}, {"force", "bool", "--yes"}},
	"prune":          {{"force", "bool", "--yes"}},
	"restore":        {{"force", "bool", "--yes"}, {"to", "", "ark docker restore <id> [volume]"}},
	"docker restore": {{"force", "bool", "--yes"}},
}

// removedCommands maps dropped commands to their replacement.
var removedCommands = map[string]string{
	"volumes": "ark docker volumes",
}

// addRemoved attaches the hidden flags and commands to the tree.
func addRemoved(root *cli.Command) {
	var walk func(cmd *cli.Command, path string)
	walk = func(cmd *cli.Command, path string) {
		for _, f := range removedFlags[path] {
			cmd.Flags = append(cmd.Flags, removedFlag(f.name, f.bool != "", f.replacement))
		}
		for _, sub := range cmd.Commands {
			walk(sub, strings.TrimSpace(path+" "+sub.Name))
		}
	}
	walk(root, "")
	for name, replacement := range removedCommands {
		root.Commands = append(root.Commands, &cli.Command{
			Name:   name,
			Hidden: true,
			Action: func(_ context.Context, _ *cli.Command) error {
				return exitcode.Usagef("ark %s was removed, use %s", name, replacement)
			},
		})
	}
}

// removedFlag builds a hidden flag whose only job is to fail with the
// replacement. Bool flags take no value, everything else swallows one.
func removedFlag(name string, isBool bool, replacement string) cli.Flag {
	fail := func() error { return exitcode.Usagef("--%s was removed, use %s", name, replacement) }
	if isBool {
		return &cli.BoolFlag{Name: name, Hidden: true,
			Action: func(_ context.Context, _ *cli.Command, _ bool) error { return fail() }}
	}
	return &cli.StringFlag{Name: name, Hidden: true,
		Action: func(_ context.Context, _ *cli.Command, _ string) error { return fail() }}
}
