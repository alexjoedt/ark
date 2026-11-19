package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func backupCommand() *cli.Command {
	hostname, _ := os.Hostname()
	return &cli.Command{
		Name:      "backup",
		Usage:     "back up a directory",
		ArgsUsage: "<path>",
		UsageText: `ark backup [options] <path>

   Backs up the directory at <path>. The snapshot's source name is the absolute path.
   The parent is the newest snapshot with the same source type, source name and host
   unless --parent is set.`,
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "add a `TAG`, repeatable"},
			&cli.StringFlag{Name: "comment", Aliases: []string{"c"}, Usage: "free `TEXT` stored with the snapshot"},
			&cli.StringFlag{Name: "host", Usage: "host `NAME` stored with the snapshot", Value: hostname},
			&cli.StringFlag{Name: "parent", Usage: "explicit parent snapshot `ID`"},
			timeFlag(),
			&cli.StringFlag{Name: "source", Aliases: []string{"s"}, Usage: "source `NAME` (default: absolute path)"},
			labelFlag("record the label `KEY=VALUE`, repeatable, the value may hold commas"),
			&listFlag{
				Name:    "exclude",
				Aliases: []string{"e"},
				Usage:   "leave out paths matching `PATTERN`, repeatable, commas stay in the pattern",
			},
			&listFlag{Name: "exclude-file", Usage: "read exclude patterns from `FILE`, one per line, repeatable"},
			&cli.BoolFlag{Name: "exclude-cloud-files", Usage: "leave out files that exist only in the cloud"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			labels, err := labelValue(cmd, false)
			if err != nil {
				return err
			}
			at, err := timeOption(cmd)
			if err != nil {
				return err
			}
			lp := profileFrom(ctx)
			eff := backupProfile(cmd, lp)
			if cmd.NArg() > 1 || eff.Backup.Path == "" {
				return exitcode.Usagef("usage: ark backup [options] <path>, or -P NAME with a backup.path")
			}
			dir := eff.Backup.Path
			if err := checkOtherPath(cmd, lp); err != nil {
				return err
			}
			return withHooks(ctx, lp, "backup", func(ctx context.Context) error {
				src, err := eff.Source()
				if errors.Is(err, path.ErrBadPattern) {
					return exitcode.Usagef("backup: %w", err)
				}
				if err != nil {
					return fmt.Errorf("backup %s: %w", dir, err)
				}

				repo, err := openRepo(ctx, cmd)
				if err != nil {
					return err
				}
				defer repo.Close()

				p := printer(cmd)
				// --label adds to the profile's labels, a key in both takes the flag's value
				bopts := append(
					eff.BackupOptions(),
					ark.WithParent(ark.SnapshotID(cmd.String("parent"))),
					ark.WithLabels(labels),
				)
				bopts = append(bopts, at...)
				// an empty --host or --comment clears the profile's value,
				// which BackupOptions would leave out or default
				if cmd.IsSet("host") {
					bopts = append(bopts, ark.WithHost(cmd.String("host")))
				}
				if cmd.IsSet("comment") {
					bopts = append(bopts, ark.WithComment(cmd.String("comment")))
				}
				// The counting scan only feeds the live display's totals and ETA.
				if p.Live() {
					bopts = append(bopts, ark.WithBackupScan())
				}
				pr := startProgress(p, "backup "+src.Meta().Name)
				snap, err := repo.Backup(ctx, src, bopts...)
				if err != nil {
					pr.Fail()
					return fmt.Errorf("backup %s: %w", dir, err)
				}
				pr.Done(out.BackupRows(snap)...)

				if p.Mode().JSON {
					return p.JSON(out.NewSnapshot(snap))
				}
				p.Result("snapshot", string(snap.ID))
				return nil
			})
		},
	}
}

// backupProfile is the profile's backup section with every flag given on the
// command line put over it; a list flag replaces the profile's list. --label
// is left to the caller, which adds it with ark.WithLabels. Without a profile
// it holds the flags alone.
func backupProfile(cmd *cli.Command, lp *loadedProfile) arkprofile.Profile {
	var eff arkprofile.Profile
	if lp != nil {
		eff = *lp.Profile
	}
	b := &eff.Backup
	if cmd.NArg() == 1 {
		b.Path = cmd.Args().First()
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"comment", &b.Comment}, {"host", &b.Host}} {
		if cmd.IsSet(f.name) {
			*f.dst = cmd.String(f.name)
		}
	}
	// An empty --source counts as not given, as in checkOtherPath and the
	// profile scope of snapshots, find and forget.
	if s := cmd.String("source"); s != "" {
		b.Source = s
	}
	if cmd.IsSet("tag") {
		b.Tags = cmd.StringSlice("tag")
	}
	if cmd.IsSet("exclude") {
		b.Excludes = listValue(cmd, "exclude")
	}
	if cmd.IsSet("exclude-file") {
		b.ExcludeFiles = listValue(cmd, "exclude-file")
	}
	if cmd.IsSet("exclude-cloud-files") {
		b.ExcludeCloudFiles = cmd.Bool("exclude-cloud-files")
	}
	return eff
}

// checkOtherPath refuses a path on the command line that is not the profile's
// backup.path unless --source names its snapshots: filed under the profile's
// backup.source, it would become the parent of the profile's next backup and
// fall under the profile's forget policy. Without backup.source the other path
// names its own snapshots, as a plain backup of it does.
func checkOtherPath(cmd *cli.Command, lp *loadedProfile) error {
	if lp == nil || lp.Backup.Source == "" || cmd.NArg() != 1 || cmd.String("source") != "" {
		return nil
	}
	arg, err := filepath.Abs(cmd.Args().First())
	if err != nil {
		return fmt.Errorf("backup %s: %w", cmd.Args().First(), err)
	}
	if lp.Backup.Path != "" {
		own, err := filepath.Abs(lp.Backup.Path)
		if err != nil {
			return fmt.Errorf("profile %s: backup.path: %w", lp.Name, err)
		}
		if own == arg {
			return nil
		}
	}
	return exitcode.Usagef(
		"backup %s: not the backup.path of profile %s, give --source for its snapshots", arg, lp.Name)
}

// listFlag is a repeatable string flag whose values are never split on
// commas, so a pattern like "a,b" stays one value.
type listFlag = cli.FlagBase[[]string, cli.NoConfig, listCreator]

type listCreator struct{}

func (listCreator) Create(val []string, p *[]string, _ cli.NoConfig) cli.Value {
	*p = slices.Clone(val)
	return &stringList{p: p}
}

func (listCreator) ToString(val []string) string { return strings.Join(val, ", ") }

type stringList struct{ p *[]string }

func (v *stringList) Set(s string) error {
	*v.p = append(*v.p, s)
	return nil
}

func (v *stringList) String() string { return listCreator{}.ToString(*v.p) }

func (v *stringList) Get() any { return *v.p }

func listValue(cmd *cli.Command, name string) []string {
	v, _ := cmd.Value(name).([]string)
	return v
}

// timeFlag is --time of backup and docker backup, in restic's format.
func timeFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "time",
		Usage: "`TIME` of the snapshot instead of now, as \"2006-01-02 15:04:05\" in local time",
	}
}

// timeOption reads --time as local time into ark.WithTime; without the flag
// there is no option. A value that does not parse, a time the local clocks
// skip, or a time the library refuses, is a usage error before anything else
// runs. Of an hour the clocks show twice, Go picks one of the two.
func timeOption(cmd *cli.Command) ([]ark.BackupOption, error) {
	if !cmd.IsSet("time") {
		return nil, nil
	}
	s := cmd.String("time")
	t, err := time.ParseInLocation(time.DateTime, s, time.Local)
	if err != nil {
		return nil, exitcode.Usagef(`--time: %q is not a time such as "2006-01-02 15:04:05"`, s)
	}
	// In the hour skipped when summer time starts, Go moves the time by the
	// shift instead of failing: the clock of t then differs from the one read.
	if wall, _ := time.Parse(time.DateTime, s); t.Format(time.DateTime) != wall.Format(time.DateTime) {
		return nil, exitcode.Usagef("--time: %q does not exist in the local time zone, the clocks skip it", s)
	}
	opt := ark.WithTime(t)
	if err := ark.ValidateBackupOptions(opt); err != nil {
		return nil, exitcode.Usagef("--time: %w", err)
	}
	return []ark.BackupOption{opt}, nil
}

// labelFlag is --label: repeatable, never split on commas.
func labelFlag(usage string) cli.Flag {
	return &listFlag{Name: "label", Usage: usage}
}

// labelValue reads --label KEY=VALUE, split at the first "=". For a backup it
// checks the labels as the library does, so a bad one exits 2 before the
// repository is opened. A filter checks the shape only: it may name any key a
// source records, ark's own ark/ keys and a third-party source's keys in
// another syntax included. A key given twice with two values is refused.
func labelValue(cmd *cli.Command, filter bool) (map[string]string, error) {
	list := listValue(cmd, "label")
	labels := make(map[string]string, len(list))
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, exitcode.Usagef("--label %q: want KEY=VALUE", kv)
		}
		if old, dup := labels[k]; dup && old != v {
			return nil, exitcode.Usagef("--label %q: key %q given twice", kv, k)
		}
		labels[k] = v
	}
	if filter {
		return labels, nil
	}
	if err := ark.ValidateLabels(labels); err != nil {
		return nil, exitcode.Usagef("--label: %w", err)
	}
	return labels, nil
}
