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
		Usage: "delete packs no snapshot references",
		UsageText: `ark prune [--dry-run] [--yes]

   Deletes the packs that hold no chunk or tree a snapshot references, and
   rewrites packs that hold both until at most 5% of the repository is unused.
   Takes the exclusive lock.`,
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
	} else if err := p.Confirm("delete unreferenced packs"); err != nil {
		return err
	}
	pr := startProgress(p, "prune")
	res, err := repo.Prune(ctx, opts...)
	if err != nil {
		pr.Fail()
		return fmt.Errorf("prune: %w", err)
	}
	pr.Donef("pruned %d blobs, %s", res.BlobsRemoved, out.Bytes(res.BytesFreed))

	if p.Mode().JSON {
		return p.JSON(map[string]any{
			"blobs_removed": res.BlobsRemoved, "bytes_freed": res.BytesFreed, "dry_run": res.DryRun,
			"packs_deleted": res.PacksDeleted, "bytes_repacked": res.BytesRepacked, "bytes_unused": res.BytesUnused,
		})
	}
	verb, repack := "removed", "repacked"
	if res.DryRun {
		verb, repack = "would remove", "would repack"
	}
	p.Linef("%s %d blobs in %d packs, %s", verb, res.BlobsRemoved, res.PacksDeleted, out.Bytes(res.BytesFreed))
	p.Linef("%s %s, %s unused", repack, out.Bytes(res.BytesRepacked), out.Bytes(res.BytesUnused))
	return nil
}
