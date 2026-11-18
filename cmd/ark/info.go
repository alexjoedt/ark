package main

import (
	"context"
	"fmt"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func infoCommand() *cli.Command {
	return &cli.Command{
		Name:  "info",
		Usage: "show what a repository holds",
		UsageText: `ark info [--exact]

   Counts the snapshots, sums the size of the files they cover (not deduplicated:
   a file in ten snapshots counts ten times) and lists what the repository stores
   per kind of object. Reads the snapshot list and no data. --exact adds the bytes
   no snapshot uses and how many of them a prune frees, from a prune dry run: it
   reads every tree and index and takes the exclusive lock.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "exact", Usage: "add the unused bytes and what a prune frees (prune dry run, exclusive lock)"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			p := printer(cmd)
			var opts []ark.InfoOption
			var pr *out.Progress
			if cmd.Bool("exact") {
				opts = append(opts, ark.WithInfoExact())
				pr = startProgress(p, "info")
			}
			info, err := repo.Info(ctx, opts...)
			if err != nil {
				if pr != nil {
					pr.Fail()
				}
				return fmt.Errorf("info: %w", err)
			}
			if pr != nil {
				pr.Done()
			}
			return printInfo(p, info)
		},
	}
}

func printInfo(p *out.Printer, info *ark.RepositoryInfo) error {
	st := info.Stored
	if p.Mode().JSON {
		v := map[string]any{
			"snapshots":   info.Snapshots,
			"total_size":  info.TotalSize,
			"stored_size": st.Total(),
			"stored": map[string]int64{
				"datapacks": st.DataPacks, "treepacks": st.TreePacks, "indexes": st.Indexes,
				"snapshots": st.Snapshots, "keys": st.Keys, "other": st.Other,
			},
		}
		if info.Exact {
			v["bytes_freed"] = info.BytesFreed
			v["bytes_unused"] = info.BytesUnused
		}
		return p.JSON(v)
	}
	ratio := "-"
	if st.Total() > 0 {
		r := float64(info.TotalSize) / float64(st.Total())
		ratio = fmt.Sprintf("%.1fx", r)
		if r > 0 && r < 0.05 {
			ratio = "<0.1x"
		}
	}
	rows := [][]string{
		{"Snapshots:", out.Count(info.Snapshots)},
		{"Total size:", out.Bytes(info.TotalSize) + " (summed over snapshots, not deduplicated)"},
		{"Stored:", out.Bytes(st.Total())},
		{"  data packs:", out.Bytes(st.DataPacks)},
		{"  tree packs:", out.Bytes(st.TreePacks)},
		{"  indexes:", out.Bytes(st.Indexes)},
		{"  snapshots:", out.Bytes(st.Snapshots)},
		{"  keys:", out.Bytes(st.Keys)},
		{"  other:", out.Bytes(st.Other)},
		{"Ratio:", ratio + " (total size over stored)"},
	}
	if info.Exact {
		rows = append(rows, []string{
			"Unused:",
			out.Bytes(info.BytesFreed+info.BytesUnused) + " (a prune frees " + out.Bytes(info.BytesFreed) + ")",
		})
	}
	p.Table(nil, rows)
	return nil
}
