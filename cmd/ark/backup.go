package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
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
   The parent is the newest snapshot with the same source name unless --parent is set.`,
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
			if cmd.NArg() != 1 {
				return exitcode.Usagef("usage: ark backup [options] <path>")
			}
			dir := cmd.Args().First()

			var opts []arkfs.SourceOption
			if patterns := listValue(cmd, "exclude"); len(patterns) > 0 {
				opts = append(opts, arkfs.WithExclude(patterns...))
			}
			for _, f := range listValue(cmd, "exclude-file") {
				opts = append(opts, arkfs.WithExcludeFile(f))
			}
			if cmd.Bool("exclude-cloud-files") {
				opts = append(opts, arkfs.WithExcludeCloudFiles())
			}
			var src ark.Source
			src, err := arkfs.NewSource(dir, opts...)
			if errors.Is(err, path.ErrBadPattern) {
				return exitcode.Usagef("backup: %w", err)
			}
			if err != nil {
				return fmt.Errorf("backup %s: %w", dir, err)
			}
			if name := cmd.String("name"); name != "" {
				src = namedSource{Source: src, name: name}
			}

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			pr := startProgress(p, "backup "+src.Meta().Name)
			start := time.Now()
			snap, err := repo.Backup(ctx, src,
				ark.WithTags(cmd.StringSlice("tag")...),
				ark.WithComment(cmd.String("comment")),
				ark.WithHost(cmd.String("host")),
				ark.WithParent(cmd.String("parent")),
			)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("backup %s: %w", dir, err)
			}
			pr.Donef("%s", out.BackupSummary("backed up "+src.Meta().Name, snap.Stats, time.Since(start)))

			if p.Mode().JSON {
				return p.JSON(out.NewSnapshot(snap))
			}
			p.Result("snapshot", snap.ID)
			return nil
		},
	}
}

// namedSource overrides the source name, the rest of the Source is untouched.
type namedSource struct {
	ark.Source
	name string
}

func (s namedSource) Meta() ark.SourceMeta {
	m := s.Source.Meta()
	m.Name = s.name
	return m
}

// ConcurrentOpen forwards the wrapped source's opt-in: Walk passes its opens
// through unchanged, so they stay as concurrent as the source made them.
func (s namedSource) ConcurrentOpen() bool {
	cs, ok := s.Source.(ark.ConcurrentSource)
	return ok && cs.ConcurrentOpen()
}

// listFlag is a repeatable string flag whose values are never split on
// commas, so a pattern like "{a,b}" stays one value.
type listFlag = cli.FlagBase[[]string, cli.NoConfig, listCreator]

type listCreator struct{}

func (listCreator) Create(val []string, p *[]string, _ cli.NoConfig) cli.Value {
	*p = append([]string(nil), val...)
	return &stringList{p: p}
}

func (listCreator) ToString(val []string) string { return strings.Join(val, ", ") }

type stringList struct{ p *[]string }

func (v *stringList) Set(s string) error {
	*v.p = append(*v.p, s)
	return nil
}

func (v *stringList) String() string {
	if v.p == nil {
		return ""
	}
	return strings.Join(*v.p, ", ")
}

func (v *stringList) Get() any { return *v.p }

func listValue(cmd *cli.Command, name string) []string {
	v, _ := cmd.Value(name).([]string)
	return v
}
