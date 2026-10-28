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

func exportCommand() *cli.Command {
	return &cli.Command{
		Name:      "export",
		Usage:     "write a snapshot as a tar archive",
		ArgsUsage: "<id> [file]",
		UsageText: `ark export <id> [file]

   Writes snapshot <id> as an uncompressed tar stream to [file] or stdout.
   Layout: metadata.json first, then data/<path>. Pipe through gzip if wanted.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() < 1 || cmd.NArg() > 2 {
				return exitcode.Usagef("usage: ark export <id> [file]")
			}
			id, file := cmd.Args().Get(0), cmd.Args().Get(1)

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			// Resolve the id first so a typo fails before a file is created.
			snap, err := repo.Snapshot(ctx, id)
			if err != nil {
				return fmt.Errorf("export %s: %w", id, err)
			}

			var w io.Writer = os.Stdout
			var n countingWriter
			if file != "" {
				f, err := os.Create(file)
				if err != nil {
					return fmt.Errorf("export: %w", err)
				}
				defer f.Close()
				w = f
			}
			n.w = w

			p := printer(cmd)
			pr := p.Progress("export " + snap.ID)
			if err := repo.Export(ctx, snap.ID, &n); err != nil {
				pr.Fail()
				if file != "" {
					_ = os.Remove(file)
				}
				return fmt.Errorf("export %s: %w", snap.ID, err)
			}
			pr.Donef("exported %s, %s", snap.ID, out.Bytes(n.n))

			// stdout carries the archive, nothing else may land there
			if file != "" && p.Mode().JSON {
				return p.JSON(map[string]any{"snapshot_id": snap.ID, "file": file, "bytes": n.n})
			}
			return nil
		},
	}
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}
