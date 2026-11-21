package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

func debugCommand() *cli.Command {
	return &cli.Command{
		Name:  "debug",
		Usage: "inspect stored objects; the output may change in any release",
		UsageText: `ark debug node <id> [path]

   Debugging aids for the repository format. Their output is for people and
   may change in any release.`,
		Commands: []*cli.Command{debugNodeCommand()},
	}
}

func debugNodeCommand() *cli.Command {
	return &cli.Command{
		Name:      "node",
		Usage:     "print a stored tree node as JSON; the output may change in any release",
		ArgsUsage: "<id> [path]",
		UsageText: `ark debug node <id> [path]

   Prints the stored tree node of snapshot <id> as JSON: the root directory, or
   the directory at [path]. Every entry shows its name, type, mode, owner,
   times, inode and the fields of its type. A name, link target or xattr name
   that is not valid UTF-8 is also given as base64 in a *_raw field. The
   output is for people and may change in any release.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() < 1 || cmd.NArg() > 2 {
				return exitcode.Usagef("usage: ark debug node <id> [path]")
			}
			id := ark.SnapshotID(cmd.Args().Get(0))
			dir := strings.Trim(cmd.Args().Get(1), "/")
			if dir == "" {
				dir = "."
			}

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()
			rd, err := repo.OpenReader(ctx)
			if err != nil {
				return err
			}
			defer rd.Close()
			fsys, err := rd.FS(id)
			if err != nil {
				return fmt.Errorf("debug node %s: %w", id, err)
			}
			b, err := fsys.DebugNode(dir)
			if err != nil {
				return fmt.Errorf("debug node %s: %w", id, err)
			}
			return printer(cmd).JSON(json.RawMessage(b))
		},
	}
}
