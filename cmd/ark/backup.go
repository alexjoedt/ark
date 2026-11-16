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
			&cli.StringFlag{Name: "name", Usage: "source `NAME` override (default: absolute path)"},
			&listFlag{
				Name:    "exclude",
				Aliases: []string{"e"},
				Usage:   "leave out paths matching `PATTERN`, repeatable, commas stay in the pattern",
			},
			&listFlag{Name: "exclude-file", Usage: "read exclude patterns from `FILE`, one per line, repeatable"},
			&cli.BoolFlag{Name: "exclude-cloud-files", Usage: "leave out files that exist only in the cloud"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
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
				bopts := append(eff.BackupOptions(), ark.WithParent(cmd.String("parent")))
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
				p.Result("snapshot", snap.ID)
				return nil
			})
		},
	}
}

// backupProfile is the profile's backup section with every flag given on the
// command line put over it; a list flag replaces the profile's list. Without
// a profile it holds the flags alone.
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
	}{{"comment", &b.Comment}, {"host", &b.Host}, {"name", &b.Name}} {
		if cmd.IsSet(f.name) {
			*f.dst = cmd.String(f.name)
		}
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
// backup.path unless --name names its snapshots: filed under the profile's
// name, it would become the parent of the profile's next backup and fall
// under the profile's forget policy.
func checkOtherPath(cmd *cli.Command, lp *loadedProfile) error {
	if lp == nil || cmd.NArg() != 1 || cmd.String("name") != "" {
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
	return exitcode.Usagef("backup %s: not the backup.path of profile %s, give --name for its snapshots", arg, lp.Name)
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
