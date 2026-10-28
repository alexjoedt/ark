package main

import (
	"context"
	"fmt"

	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/urfave/cli/v3"
)

func checkCommand() *cli.Command {
	return &cli.Command{
		Name:  "check",
		Usage: "verify every snapshot",
		UsageText: `ark check

   Verifies every snapshot and reports missing or corrupt objects. Exit 5 on problems.`,
		Action: func(ctx context.Context, cmd *cli.Command) error {
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			pr := p.Progress("check")
			res, err := repo.Check(ctx)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("check: %w", err)
			}
			pr.Donef("checked %d snapshots", res.SnapshotsChecked)

			if p.Mode().JSON {
				if err := p.JSON(map[string]any{
					"snapshots_checked": res.SnapshotsChecked,
					"errors":            nonNil(res.Errors),
				}); err != nil {
					return err
				}
				if len(res.Errors) > 0 {
					return exitcode.Reported()
				}
				return nil
			}
			for _, e := range res.Errors {
				p.Linef("%s", e)
			}
			if len(res.Errors) > 0 {
				return exitcode.Verify(fmt.Errorf("%d problems in %d snapshots", len(res.Errors), res.SnapshotsChecked))
			}
			p.Linef("%d snapshots ok", res.SnapshotsChecked)
			return nil
		},
	}
}
