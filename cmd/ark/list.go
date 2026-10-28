package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func snapshotsCommand() *cli.Command {
	return &cli.Command{
		Name:  "snapshots",
		Usage: "list snapshots",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Aliases: []string{"N"}, Usage: "source `NAME` (path or volume)"},
			&cli.StringFlag{Name: "type", Usage: "source `TYPE`, file or docker-volume"},
			&cli.StringSliceFlag{
				Name:    "tag",
				Aliases: []string{"t"},
				Usage:   "must carry `TAG`, repeatable, all must match",
			},
			&cli.StringFlag{Name: "host", Usage: "host `NAME`"},
			&cli.IntFlag{Name: "last", Usage: "newest `N` per source name"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{
				Name: cmd.String("name"),
				Type: ark.SourceType(cmd.String("type")),
				Tags: cmd.StringSlice("tag"),
				Host: cmd.String("host"),
			})
			if err != nil {
				return fmt.Errorf("snapshots: %w", err)
			}
			if n := cmd.Int("last"); n > 0 {
				snaps = lastPerSource(snaps, n)
			}

			p := printer(cmd)
			if p.Mode().JSON {
				return p.JSON(out.Snapshots(snaps))
			}
			rows := make([][]string, 0, len(snaps))
			for _, s := range snaps {
				rows = append(rows, []string{
					s.ID, out.Time(s.CreatedAt), s.SourceName, string(s.SourceType), s.Host,
					out.Bytes(s.Stats.TotalSize), strings.Join(s.Tags, ","),
				})
			}
			p.Table([]string{"ID", "TIME", "SOURCE", "TYPE", "HOST", "SIZE", "TAGS"}, rows)
			return nil
		},
	}
}

// lastPerSource keeps the newest n snapshots of every source name. snaps
// arrive newest first and keep that order.
func lastPerSource(snaps []*ark.Snapshot, n int) []*ark.Snapshot {
	seen := make(map[string]int)
	kept := snaps[:0]
	for _, s := range snaps {
		if seen[s.SourceName] < n {
			seen[s.SourceName]++
			kept = append(kept, s)
		}
	}
	return kept
}

func lsCommand() *cli.Command {
	return &cli.Command{
		Name:      "ls",
		Usage:     "list files inside a snapshot",
		ArgsUsage: "<id> [path]",
		UsageText: `ark ls [options] <id> [path]

   Lists entries of snapshot <id>, optionally below [path]. Streams, works on any size.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "long", Aliases: []string{"l"}, Usage: "mode, uid, gid, size, mtime per entry"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() < 1 || cmd.NArg() > 2 {
				return exitcode.Usagef("usage: ark ls [options] <id> [path]")
			}
			id := cmd.Args().Get(0)
			prefix := strings.Trim(cmd.Args().Get(1), "/")
			if prefix != "" {
				prefix += "/"
			}

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			long := cmd.Bool("long")
			err = repo.Walk(ctx, id, func(f ark.FileInfo) error {
				if prefix != "" && !strings.HasPrefix(f.Path, prefix) {
					return nil
				}
				if p.Mode().JSON {
					return p.JSONLine(entryJSON{
						Path: f.Path, Type: string(f.Type), Size: f.Size, Mode: uint32(f.Mode.Perm()),
						UID: f.UID, GID: f.GID, Mtime: f.ModTime, LinkDest: f.LinkDest,
					})
				}
				if long {
					p.Linef("%s %5d %5d %10d %s %s", f.Mode, f.UID, f.GID, f.Size, out.TimeLong(f.ModTime), f.Path)
				} else {
					p.Linef("%s", f.Path)
				}
				return nil
			})
			if err != nil {
				return fmt.Errorf("ls %s: %w", id, err)
			}
			return nil
		},
	}
}

// entryJSON is one line of ark ls --json, frozen for 1.x.
type entryJSON struct {
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Mode     uint32    `json:"mode"`
	UID      int       `json:"uid"`
	GID      int       `json:"gid"`
	Mtime    time.Time `json:"mtime"`
	LinkDest string    `json:"link_dest"`
}

func inspectCommand() *cli.Command {
	return &cli.Command{
		Name:      "inspect",
		Usage:     "show one snapshot in detail",
		ArgsUsage: "<id>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return exitcode.Usagef("usage: ark inspect <id>")
			}
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			s, err := repo.Snapshot(ctx, cmd.Args().First())
			if err != nil {
				return fmt.Errorf("inspect %s: %w", cmd.Args().First(), err)
			}

			p := printer(cmd)
			if p.Mode().JSON {
				return p.JSON(out.NewSnapshot(s))
			}
			st := s.Stats
			files := out.Count(st.FileCount)
			if st.Skipped > 0 {
				files += fmt.Sprintf(" (%s skipped)", out.Count(st.Skipped))
			}
			reused := max(st.TotalSize-st.NewChunksRawSize, 0)
			rows := [][]string{
				{"ID:", s.ID},
				{"Source:", fmt.Sprintf("%s (%s)", s.SourceName, s.SourceType)},
				{"Host:", s.Host},
				{"Time:", out.TimeLong(s.CreatedAt)},
				{"Parent:", orDash(s.ParentID)},
				{"Tags:", orDash(strings.Join(s.Tags, ", "))},
				{"Files:", files},
				{
					"Size:",
					fmt.Sprintf(
						"%s, stored %s new, %s reused",
						out.Bytes(st.TotalSize),
						out.Bytes(st.StoredSize),
						out.Bytes(reused),
					),
				},
				{
					"Ratio:",
					fmt.Sprintf(
						"dedup %s, compression %s",
						out.Ratio(st.DeduplicationRatio),
						out.Ratio(st.CompressionRatio),
					),
				},
				{"Duration:", out.Duration(st.Duration)},
			}
			if s.Comment != "" {
				rows = append(rows, []string{"Comment:", s.Comment})
			}
			p.Table(nil, rows)
			return nil
		},
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
