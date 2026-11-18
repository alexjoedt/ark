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
   ark forget --keep-last N [--source NAME] [--prune]
   ark forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6
   ark forget --keep-within 30d
   ark forget --keep-within-daily 14d --keep-within-monthly 1y
   ark forget --keep-daily 7 --keep-yearly unlimited
   ark forget --keep-last 5 --keep-tag pinned

   Removes snapshots. Either name ids, or give a policy that says what to keep;
   the policy is applied per series (source type, source name and host). A policy that
   keeps nothing is refused, use --all to remove every matching snapshot on purpose.
   --keep-tag keeps what carries all tags of a list and is the one rule that can keep
   nothing of a series: the run then fails and removes nothing.
   Hours, days, weeks, months and years count in local time (TZ).
   A period rule with count left at the end of a series keeps its oldest snapshot too.
   --keep-within-hourly to --keep-within-yearly are the period rules bounded by a
   DURATION instead of a count: the newest of every period younger than it.
   Chunks stay until prune.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "keep-last", Usage: "keep the `N` newest, unlimited for all"},
			&cli.StringFlag{Name: "keep-hourly", Usage: "keep the newest of `N` hours, unlimited for all"},
			&cli.StringFlag{Name: "keep-daily", Usage: "keep the newest of `N` days, unlimited for all"},
			&cli.StringFlag{Name: "keep-weekly", Usage: "keep the newest of `N` weeks, unlimited for all"},
			&cli.StringFlag{Name: "keep-monthly", Usage: "keep the newest of `N` months, unlimited for all"},
			&cli.StringFlag{Name: "keep-yearly", Usage: "keep the newest of `N` years, unlimited for all"},
			&cli.StringFlag{
				Name:  "keep-within",
				Usage: "keep everything younger than `DURATION` in years, months, days and hours, e.g. 30d, 72h, 1y6m",
			},
			&cli.StringFlag{Name: "keep-within-hourly", Usage: "keep the newest of every hour within `DURATION`"},
			&cli.StringFlag{Name: "keep-within-daily", Usage: "keep the newest of every day within `DURATION`"},
			&cli.StringFlag{Name: "keep-within-weekly", Usage: "keep the newest of every week within `DURATION`"},
			&cli.StringFlag{Name: "keep-within-monthly", Usage: "keep the newest of every month within `DURATION`"},
			&cli.StringFlag{Name: "keep-within-yearly", Usage: "keep the newest of every year within `DURATION`"},
			&listFlag{
				Name:  "keep-tag",
				Usage: "keep what carries all of `TAG[,TAG...]`, '' for untagged, repeatable: any list keeps",
			},
			&cli.BoolFlag{Name: "all", Usage: "remove every snapshot matching the filter"},
			&cli.StringFlag{Name: "source", Aliases: []string{"s"}, Usage: "filter by source `NAME`"},
			&cli.StringFlag{Name: "type", Usage: "filter by source `TYPE`, file, docker-volume or stream"},
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "filter by `TAG`, repeatable"},
			&cli.StringFlag{Name: "host", Usage: "filter by host `NAME`"},
			labelFlag("must carry the label `KEY=VALUE`, repeatable, all must match"),
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
			labels, err := labelValue(cmd, true)
			if err != nil {
				return err
			}
			filter := ark.SnapshotFilter{
				Labels: labels,
				IDs:    ids,
				Source: cmd.String("source"),
				Type:   ark.SourceType(cmd.String("type")),
				Tags:   cmd.StringSlice("tag"),
				Host:   cmd.String("host"),
			}
			prune := cmd.Bool("prune")
			if scope != nil {
				pf, err := scope.Filter()
				if err != nil {
					return exitcode.Usagef("profile %s: %w", scope.Name, err)
				}
				if cmd.String("source") == "" {
					filter.Source = pf.Source
				}
				if !cmd.IsSet("host") {
					filter.Host = pf.Host
				}
			}
			if lp != nil && !cmd.IsSet("prune") {
				prune = lp.Forget.Prune
			}
			hasPolicy := !policy.Empty()
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
	for _, s := range preview.Future {
		printer(
			cmd,
		).Warnf("snapshot %s (%s) is dated in the future; kept and ignored by the policy, check that machine's clock",
			out.ShortID(s.ID), timeAndHost(s))
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
// on the command line put over its rule; --keep-tag replaces the profile's
// lists. A bad count or duration is a usage error.
func forgetPolicy(cmd *cli.Command, lp *loadedProfile) (ark.ForgetPolicy, error) {
	var policy ark.ForgetPolicy
	if lp != nil {
		policy = lp.ForgetPolicy()
	}
	for _, f := range []struct {
		name string
		dst  *ark.KeepCount
	}{
		{"keep-last", &policy.KeepLast},
		{"keep-hourly", &policy.KeepHourly},
		{"keep-daily", &policy.KeepDaily},
		{"keep-weekly", &policy.KeepWeekly},
		{"keep-monthly", &policy.KeepMonthly},
		{"keep-yearly", &policy.KeepYearly},
	} {
		if cmd.IsSet(f.name) {
			n, err := ark.ParseKeepCount(cmd.String(f.name))
			if err != nil {
				return policy, exitcode.Usagef("--%s: %v", f.name, err)
			}
			*f.dst = n
		}
	}
	for _, f := range []struct {
		name string
		dst  *ark.Within
	}{
		{"keep-within", &policy.KeepWithin},
		{"keep-within-hourly", &policy.KeepWithinHourly},
		{"keep-within-daily", &policy.KeepWithinDaily},
		{"keep-within-weekly", &policy.KeepWithinWeekly},
		{"keep-within-monthly", &policy.KeepWithinMonthly},
		{"keep-within-yearly", &policy.KeepWithinYearly},
	} {
		if !cmd.IsSet(f.name) {
			continue
		}
		// an empty value drops the profile's rule
		*f.dst = ark.Within{}
		if s := cmd.String(f.name); s != "" {
			w, err := ark.ParseWithin(s)
			if err != nil {
				return policy, exitcode.Usagef("--%s: %v", f.name, err)
			}
			*f.dst = w
		}
	}
	if cmd.IsSet("keep-tag") {
		policy.KeepTags = nil
		for _, list := range listValue(cmd, "keep-tag") {
			policy.KeepTags = append(policy.KeepTags, ark.ParseTagList(list))
		}
	}
	return policy, nil
}

func printForget(cmd *cli.Command, res *ark.ForgetResult) error {
	p := printer(cmd)
	if p.Mode().JSON {
		return p.JSON(map[string]any{
			"keep":    out.KeptSnapshots(res.Keep, res.KeepReasons),
			"remove":  out.Snapshots(res.Remove),
			"dry_run": res.DryRun,
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
