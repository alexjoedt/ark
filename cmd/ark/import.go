package main

import (
	"context"
	"fmt"
	"io"
	"os"

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
			pr := p.Progress("import " + file)
			snap, err := repo.Import(ctx, r)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("import %s: %w", file, err)
			}
			pr.Donef("imported %s as %s, %s files, %s", snap.SourceName, snap.ID,
				out.Count(snap.Stats.FileCount), out.Bytes(snap.Stats.TotalSize))

			if p.Mode().JSON {
				return p.JSON(out.NewSnapshot(snap))
			}
			p.Result("snapshot", snap.ID)
			return nil
		},
	}
}
