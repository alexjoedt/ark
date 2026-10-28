package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

// verifyJSON is the shape of ark verify --json, frozen for 1.x.
type verifyJSON struct {
	SnapshotID    string   `json:"snapshot_id"`
	ChunksChecked int      `json:"chunks_checked"`
	MissingChunks []string `json:"missing_chunks"`
	CorruptChunks []string `json:"corrupt_chunks"`
	OK            bool     `json:"ok"`
}

func verifyCommand() *cli.Command {
	return &cli.Command{
		Name:      "verify",
		Usage:     "verify one snapshot",
		ArgsUsage: "<id>",
		UsageText: `ark verify <id>

   Reads every chunk of the snapshot and checks hashes. Exit 5 when problems are found.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return exitcode.Usagef("usage: ark verify <id>")
			}
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			pr := p.Progress("verify " + cmd.Args().First())
			res, err := repo.Verify(ctx, cmd.Args().First())
			if err != nil {
				pr.Fail()
				err = fmt.Errorf("verify %s: %w", cmd.Args().First(), err)
				if errors.Is(err, ark.ErrCorruptManifest) || errors.Is(err, ark.ErrCorruptTree) {
					return exitcode.Verify(err)
				}
				return err
			}
			pr.Donef("verified %d chunks of %s", res.ChunksChecked, res.SnapshotID)

			if p.Mode().JSON {
				if err := p.JSON(verifyJSON{
					SnapshotID:    res.SnapshotID,
					ChunksChecked: res.ChunksChecked,
					MissingChunks: nonNil(res.MissingChunks),
					CorruptChunks: nonNil(res.CorruptChunks),
					OK:            res.OK,
				}); err != nil {
					return err
				}
				if !res.OK {
					return exitcode.Reported()
				}
				return nil
			}
			if !res.OK {
				return exitcode.Verify(fmt.Errorf("snapshot %s: %d missing, %d corrupt chunks",
					res.SnapshotID, len(res.MissingChunks), len(res.CorruptChunks)))
			}
			p.Linef("snapshot %s ok, %d chunks", res.SnapshotID, res.ChunksChecked)
			return nil
		},
	}
}

// nonNil keeps an empty list as [] in JSON, never null.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
