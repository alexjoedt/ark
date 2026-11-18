package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func importCommand() *cli.Command {
	return &cli.Command{
		Name:      "import",
		Usage:     "read a tar archive written by export as a new snapshot",
		ArgsUsage: "<file|->",
		UsageText: `ark import <file|->

   Reads a tar archive written by export from <file> or stdin and stores it as a new snapshot.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return exitcode.Usagef("usage: ark import <file|->")
			}
			file := cmd.Args().First()

			var r io.Reader = os.Stdin
			if file != "-" {
				f, err := os.Open(file)
				if err != nil {
					return fmt.Errorf("import: %w", err)
				}
				defer f.Close()
				r = f
			}

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			pr := startProgress(p, "import "+file)
			snap, err := repo.Import(ctx, r)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("import %s: %w", file, err)
			}
			pr.Done(out.R("snapshot", "%s", out.ShortID(snap.ID)),
				out.R("source", "%s", snap.SourceName),
				out.R("files", "%s", out.Count(snap.Stats.FileCount)),
				out.R("size", "%s", out.Bytes(snap.Stats.TotalSize)))

			if snap.CreatedAt.After(time.Now()) {
				warnFuture(p, snap)
			}

			if p.Mode().JSON {
				return p.JSON(out.NewSnapshot(snap))
			}
			p.Result("snapshot", string(snap.ID))
			return nil
		},
	}
}

// warnFuture tells the user that a snapshot is dated after now: while that
// date is still ahead of its clock, forget keeps it and leaves it out of every
// policy rule, so a wrong clock on the machine that wrote it stays visible.
func warnFuture(p *out.Printer, s *ark.Snapshot) {
	p.Warnf("snapshot %s (%s) is dated in the future; until that date has passed, forget keeps it and ignores it in the policy, check the clock of the machine that wrote it",
		out.ShortID(s.ID), timeAndHost(s))
}

// timeAndHost is the creation time of s, with its host when it has one.
func timeAndHost(s *ark.Snapshot) string {
	where := out.Time(s.CreatedAt)
	if s.Host != "" {
		where += ", host " + s.Host
	}
	return where
}
