package main

import (
	"context"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfind"
	"github.com/alexjoedt/ark/arkglob"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func findCommand() *cli.Command {
	return &cli.Command{
		Name:      "find",
		Usage:     "find files in snapshots by pattern",
		ArgsUsage: "<pattern>...",
		UsageText: `ark find [options] <pattern>...

   Lists every entry whose path matches a pattern, grouped per snapshot, newest first.
   Patterns follow --exclude: without a "/" a base name anywhere, a leading "/" anchors
   at the snapshot root, a matched directory lists everything below it.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "source", Aliases: []string{"s"}, Usage: "source `NAME` (path or volume)"},
			&cli.StringFlag{Name: "type", Usage: "source `TYPE`, file, docker-volume or stream"},
			&cli.StringSliceFlag{
				Name:    "tag",
				Aliases: []string{"t"},
				Usage:   "must carry `TAG`, repeatable, all must match",
			},
			&cli.StringFlag{Name: "host", Usage: "host `NAME`"},
			labelFlag("must carry the label `KEY=VALUE`, repeatable, all must match"),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			patterns := cmd.Args().Slice()
			if len(patterns) == 0 {
				return exitcode.Usagef("usage: ark find [options] <pattern>...")
			}
			if _, err := arkglob.New(patterns...); err != nil {
				return exitcode.Usagef("find: %w", err)
			}

			labels, err := labelValue(cmd, true)
			if err != nil {
				return err
			}
			filter := ark.SnapshotFilter{
				Source: cmd.String("source"),
				Type:   ark.SourceType(cmd.String("type")),
				Tags:   cmd.StringSlice("tag"),
				Host:   cmd.String("host"),
				Labels: labels,
			}
			if lp := profileFrom(ctx); lp != nil && cmd.String("source") == "" && profileNamesSnapshots(lp) {
				pf, err := lp.Filter()
				if err != nil {
					return exitcode.Usagef("profile %s: %w", lp.Name, err)
				}
				filter.Source = pf.Source
			}

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			m := p.Mode()
			var last ark.SnapshotID
			err = arkfind.Find(ctx, repo, filter, patterns, func(hit arkfind.Match) error {
				s, f := hit.Snapshot, hit.File
				if m.JSON {
					last = s.ID
					return p.JSONLine(matchJSON{
						SnapshotID: s.ID, Path: f.Path, Type: string(f.Type), Size: f.Size, Mtime: f.ModTime,
						Labels: out.Labels(s.Labels),
					})
				}
				if s.ID != last && !m.Quiet {
					if last != "" {
						p.Linef("")
					}
					p.Linef("snapshot %s from %s", out.ShortID(s.ID), out.TimeLong(s.CreatedAt))
				}
				last = s.ID
				p.Linef("%s", f.Path)
				return nil
			})
			if err != nil {
				return err
			}
			if last == "" {
				p.Notef("no matches")
			}
			return nil
		},
	}
}

// matchJSON is one line of ark find --json, frozen for 1.x.
type matchJSON struct {
	SnapshotID ark.SnapshotID    `json:"snapshot_id"`
	Path       string            `json:"path"`
	Type       string            `json:"type"`
	Size       int64             `json:"size"`
	Mtime      time.Time         `json:"mtime"`
	Labels     map[string]string `json:"labels"`
}
