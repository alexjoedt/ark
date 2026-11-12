package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

// cliText names the CLI command where a library message names the method
// that does the same.
func cliText(s string) string {
	return strings.ReplaceAll(s, "RebuildIndex", "ark repair index")
}

func repairCommand() *cli.Command {
	return &cli.Command{
		Name:  "repair",
		Usage: "repair a damaged repository",
		UsageText: `ark repair index

   Repairs what check and prune report.`,
		Commands: []*cli.Command{repairIndexCommand()},
	}
}

func repairIndexCommand() *cli.Command {
	return &cli.Command{
		Name:  "index",
		Usage: "rebuild the index from the stored packs",
		UsageText: `ark repair index

   Reads every stored pack, writes a new index of what the packs say about
   themselves and deletes the old index objects. Run it when check or prune
   report a lost or damaged index object, or a pack no index lists that a
   snapshot needs. A blob whose pack is lost is not brought back; check names
   the snapshots that miss it.

   A pack whose own table does not open is listed and exits 5; the new index
   keeps in it what the old one listed. Takes the exclusive lock, so no backup,
   restore or prune can run meanwhile.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			pr := startProgress(p, "repair index")
			res, err := repo.RebuildIndex(ctx)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("repair index: %w", err)
			}
			pr.Donef("indexed %d packs", res.PacksIndexed)

			if p.Mode().JSON {
				if err := p.JSON(map[string]any{
					"packs_indexed":    res.PacksIndexed,
					"blobs_indexed":    res.BlobsIndexed,
					"unreadable_packs": nonNil(res.UnreadablePacks),
				}); err != nil {
					return err
				}
				if len(res.UnreadablePacks) > 0 {
					return exitcode.Reported()
				}
				return nil
			}
			p.Linef("indexed %d packs, %d blobs", res.PacksIndexed, res.BlobsIndexed)
			for _, name := range res.UnreadablePacks {
				p.Linef("unreadable pack %s", name)
			}
			if len(res.UnreadablePacks) > 0 {
				return exitcode.Verify(fmt.Errorf("%d unreadable packs", len(res.UnreadablePacks)))
			}
			return nil
		},
	}
}

func cliTexts(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = cliText(s)
	}
	return out
}
