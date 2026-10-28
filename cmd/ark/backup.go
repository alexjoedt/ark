package main

import (
	"context"
	"fmt"
	"os"
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
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return exitcode.Usagef("usage: ark backup [options] <path>")
			}
			path := cmd.Args().First()

			var src ark.Source
			src, err := arkfs.NewSource(path)
			if err != nil {
				return fmt.Errorf("backup %s: %w", path, err)
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
			pr := p.Progress("backup " + src.Meta().Name)
			start := time.Now()
			snap, err := repo.Backup(ctx, src,
				ark.WithTags(cmd.StringSlice("tag")...),
				ark.WithComment(cmd.String("comment")),
				ark.WithHost(cmd.String("host")),
				ark.WithParent(cmd.String("parent")),
				ark.WithBackupProgress(pr.Func()),
			)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("backup %s: %w", path, err)
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
