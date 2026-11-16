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
   the policy is applied per series (source type, source name and host). A policy that
   keeps nothing is refused, use --all to remove every matching snapshot on purpose.
   Chunks stay until prune.`,
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
			lp := profileFrom(ctx)
			ids := cmd.Args().Slice()
			// named ids are removed as named: the profile's policy and
			// snapshot scope do not apply to them
			scope := lp
			if len(ids) > 0 {
				scope = nil
			}
			policy, err := forgetPolicy(cmd, scope)
			if err != nil {
				return err
			}
			filter := ark.SnapshotFilter{
				IDs:  ids,
				Name: cmd.String("name"),
				Type: ark.SourceType(cmd.String("type")),
				Tags: cmd.StringSlice("tag"),
				Host: cmd.String("host"),
			}
			prune := cmd.Bool("prune")
			if scope != nil {
				pf, err := scope.Filter()
				if err != nil {
					return exitcode.Usagef("profile %s: %w", scope.Name, err)
				}
				if !cmd.IsSet("name") {
					filter.Name = pf.Name
				}
				if !cmd.IsSet("host") {
					filter.Host = pf.Host
				}
			}
			if lp != nil && !cmd.IsSet("prune") {
				prune = lp.Forget.Prune
			}
			hasPolicy := policy != (ark.ForgetPolicy{})
			switch {
			case len(ids) > 0 && hasPolicy:
				return exitcode.Usagef("give snapshot ids or a policy, not both")
			case len(ids) > 0 && cmd.Bool("all"):
				return exitcode.Usagef("--all removes by filter, do not name ids")
			}
			run := func(ctx context.Context) error { return runForget(ctx, cmd, policy, filter, prune) }
			// a dry run removes nothing, so the hooks around a removal do not run
			if cmd.Bool("dry-run") {
				return run(ctx)
			}
			return withHooks(ctx, lp, "forget", run)
		},
	}
}

// runForget removes what policy or the ids in filter select, after a
// confirmation, and prunes afterwards when prune is set.
func runForget(
	ctx context.Context,
	cmd *cli.Command,
	policy ark.ForgetPolicy,
	filter ark.SnapshotFilter,
	prune bool,
) error {
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
	pr := startProgress(p, "forget")
	res, err := repo.Forget(ctx, policy, filter)
	if err != nil {
		pr.Fail()
		return fmt.Errorf("forget: %w", err)
	}
	pr.Done()
	if err := printForget(cmd, res); err != nil {
		return err
	}
	if prune {
		return runPrune(ctx, cmd, repo, false)
	}
	return nil
}

// forgetPolicy is the profile's forget policy with every --keep-* flag given
// on the command line put over its rule. A bad duration is a usage error.
func forgetPolicy(cmd *cli.Command, lp *loadedProfile) (ark.ForgetPolicy, error) {
	var policy ark.ForgetPolicy
	if lp != nil {
		var err error
		if policy, err = lp.ForgetPolicy(); err != nil {
			return policy, exitcode.Usagef("profile %s: %w", lp.Name, err)
		}
	}
	for _, f := range []struct {
		name string
		dst  *int
	}{
		{"keep-last", &policy.KeepLast},
		{"keep-hourly", &policy.KeepHourly},
		{"keep-daily", &policy.KeepDaily},
		{"keep-weekly", &policy.KeepWeekly},
		{"keep-monthly", &policy.KeepMonthly},
		{"keep-yearly", &policy.KeepYearly},
	} {
		if cmd.IsSet(f.name) {
			*f.dst = cmd.Int(f.name)
		}
	}
	if cmd.IsSet("keep-within") {
		// an empty value drops the profile's rule
		policy.KeepWithin = 0
		if s := cmd.String("keep-within"); s != "" {
			d, err := out.ParseDuration(s)
			if err != nil {
				return policy, exitcode.Usagef("--keep-within: %v", err)
			}
			policy.KeepWithin = d
		}
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
