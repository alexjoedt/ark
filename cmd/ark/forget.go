package main

import (
	"context"
	"fmt"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func forgetCommand() *cli.Command {
	return &cli.Command{
		Name:      "forget",
		Usage:     "remove snapshots by id or policy",
		ArgsUsage: "[<id>...]",
		UsageText: `ark forget [options] [<id>...]
   ark forget --keep-last N [--name NAME] [--prune]
   ark forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6
   ark forget --keep-within 30d

   Removes snapshots. Either name ids, or give a policy that says what to keep;
   the policy is applied per source name. A policy that keeps nothing is refused,
   use --all to remove every matching snapshot on purpose. Chunks stay until prune.`,
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "keep-last", Usage: "keep the `N` newest"},
			&cli.IntFlag{Name: "keep-hourly", Usage: "keep the newest of `N` hours"},
			&cli.IntFlag{Name: "keep-daily", Usage: "keep the newest of `N` days"},
			&cli.IntFlag{Name: "keep-weekly", Usage: "keep the newest of `N` weeks"},
			&cli.IntFlag{Name: "keep-monthly", Usage: "keep the newest of `N` months"},
			&cli.IntFlag{Name: "keep-yearly", Usage: "keep the newest of `N` years"},
			&cli.StringFlag{Name: "keep-within", Usage: "keep everything younger than `DURATION`, e.g. 72h, 30d"},
			&cli.BoolFlag{Name: "all", Usage: "remove every snapshot matching the filter"},
			&cli.StringFlag{Name: "name", Aliases: []string{"N"}, Usage: "filter by source `NAME`"},
			&cli.StringFlag{Name: "type", Usage: "filter by source `TYPE`, file or docker-volume"},
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "filter by `TAG`, repeatable"},
			&cli.StringFlag{Name: "host", Usage: "filter by host `NAME`"},
			&cli.BoolFlag{Name: "prune", Usage: "run prune afterwards"},
			&cli.BoolFlag{Name: "dry-run", Aliases: []string{"n"}, Usage: "show keep and remove lists, delete nothing"},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			policy, err := forgetPolicy(cmd)
			if err != nil {
				return err
			}
			ids := cmd.Args().Slice()
			filter := ark.SnapshotFilter{
				IDs:  ids,
				Name: cmd.String("name"),
				Type: ark.SourceType(cmd.String("type")),
				Tags: cmd.StringSlice("tag"),
				Host: cmd.String("host"),
			}
			hasPolicy := policy != (ark.ForgetPolicy{})
			switch {
			case len(ids) > 0 && hasPolicy:
				return exitcode.Usagef("give snapshot ids or a policy, not both")
			case len(ids) > 0 && cmd.Bool("all"):
				return exitcode.Usagef("--all removes by filter, do not name ids")
			}

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			// --all turns the filter into an id list, the library refuses an empty policy
			if cmd.Bool("all") {
				matches, err := repo.Snapshots(ctx, filter)
				if err != nil {
					return fmt.Errorf("forget: %w", err)
				}
				for _, s := range matches {
					filter.IDs = append(filter.IDs, s.ID)
				}
				if len(filter.IDs) == 0 {
					return printForget(cmd, &ark.ForgetResult{Keep: nil, Remove: nil, DryRun: cmd.Bool("dry-run")})
				}
			}

			// preview first, so the prompt can name what goes
			preview, err := repo.Forget(ctx, policy, filter, ark.WithForgetDryRun())
			if err != nil {
				return fmt.Errorf("forget: %w", err)
			}
			if cmd.Bool("dry-run") || len(preview.Remove) == 0 {
				return printForget(cmd, preview)
			}

			p := printer(cmd)
			if err := p.Confirm(fmt.Sprintf("remove %d snapshots", len(preview.Remove))); err != nil {
				return err
			}
			res, err := repo.Forget(ctx, policy, filter)
			if err != nil {
				return fmt.Errorf("forget: %w", err)
			}
			if err := printForget(cmd, res); err != nil {
				return err
			}
			if cmd.Bool("prune") {
				return runPrune(ctx, cmd, repo, false)
			}
			return nil
		},
	}
}

// forgetPolicy maps the --keep-* flags. A bad duration is a usage error.
func forgetPolicy(cmd *cli.Command) (ark.ForgetPolicy, error) {
	policy := ark.ForgetPolicy{
		KeepLast:    cmd.Int("keep-last"),
		KeepHourly:  cmd.Int("keep-hourly"),
		KeepDaily:   cmd.Int("keep-daily"),
		KeepWeekly:  cmd.Int("keep-weekly"),
		KeepMonthly: cmd.Int("keep-monthly"),
		KeepYearly:  cmd.Int("keep-yearly"),
	}
	if s := cmd.String("keep-within"); s != "" {
		d, err := out.ParseDuration(s)
		if err != nil {
			return policy, exitcode.Usagef("--keep-within: %v", err)
		}
		policy.KeepWithin = d
	}
	return policy, nil
}

func printForget(cmd *cli.Command, res *ark.ForgetResult) error {
	p := printer(cmd)
	if p.Mode().JSON {
		return p.JSON(map[string]any{
			"keep": out.Snapshots(res.Keep), "remove": out.Snapshots(res.Remove), "dry_run": res.DryRun,
		})
	}
	verb := "removed"
	if res.DryRun {
		verb = "would remove"
	}
	for _, s := range res.Remove {
		if s.CreatedAt.IsZero() {
			p.Linef("%s %s (corrupt manifest)", verb, s.ID)
			continue
		}
		p.Linef("%s %s %s (%s)", verb, s.ID, out.Time(s.CreatedAt), s.SourceName)
	}
	p.Linef("%s %d snapshots, kept %d", verb, len(res.Remove), len(res.Keep))
	return nil
}
