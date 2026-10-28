package main

import (
	"context"
	"fmt"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func pruneCommand() *cli.Command {
	return &cli.Command{
		Name:  "prune",
		Usage: "delete chunks no snapshot references",
		UsageText: `ark prune [--dry-run] [--yes]

   Deletes chunks and trees no snapshot references. Takes the exclusive lock.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "dry-run", Aliases: []string{"n"}, Usage: "show what would be freed, delete nothing"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()
			return runPrune(ctx, cmd, repo, cmd.Bool("dry-run"))
		},
	}
}

// runPrune is shared with forget --prune. It prompts unless dry-run.
func runPrune(ctx context.Context, cmd *cli.Command, repo *ark.Repository, dryRun bool) error {
	p := printer(cmd)
	var opts []ark.PruneOption
	if dryRun {
		opts = append(opts, ark.WithPruneDryRun())
	} else if err := p.Confirm("delete unreferenced chunks"); err != nil {
		return err
	}
	pr := p.Progress("prune")
	res, err := repo.Prune(ctx, opts...)
	if err != nil {
		pr.Fail()
		return fmt.Errorf("prune: %w", err)
	}
	pr.Donef("pruned %d blobs, %s", res.BlobsRemoved, out.Bytes(res.BytesFreed))

	if p.Mode().JSON {
		return p.JSON(map[string]any{
			"blobs_removed": res.BlobsRemoved, "bytes_freed": res.BytesFreed, "dry_run": res.DryRun,
		})
	}
	verb := "removed"
	if res.DryRun {
		verb = "would remove"
	}
	p.Linef("%s %d blobs, %s", verb, res.BlobsRemoved, out.Bytes(res.BytesFreed))
	return nil
}
