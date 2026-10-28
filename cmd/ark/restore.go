package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func restoreCommand() *cli.Command {
	return &cli.Command{
		Name:      "restore",
		Usage:     "restore a snapshot into a directory",
		ArgsUsage: "<id> <dest>",
		UsageText: `ark restore [options] <id> <dest>

   Restores snapshot <id> (full id or unique prefix) into directory <dest>.
   <dest> is created if missing. Existing files are overwritten.
   Docker volume snapshots are refused here: use ark docker restore, or
   ark export <id> | tar x to unpack one into a directory.`,
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:    "path",
				Aliases: []string{"p"},
				Usage:   "restore only entries under `PREFIX`, repeatable",
			},
			&cli.BoolFlag{Name: "dry-run", Aliases: []string{"n"}, Usage: "list what would be restored, write nothing"},
			&cli.StringFlag{Name: "cache", Usage: "chunk cache `SIZE` during restore, 0 disables", Value: "256MiB"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 2 {
				return exitcode.Usagef("usage: ark restore [options] <id> <dest>")
			}
			id, dest := cmd.Args().Get(0), cmd.Args().Get(1)
			cache, err := cacheSize(cmd.String("cache"))
			if err != nil {
				return err
			}
			paths := restorePaths(cmd.StringSlice("path"))

			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			// Resolve the id first so a typo fails before <dest> is created.
			snap, err := repo.Snapshot(ctx, id)
			if err != nil {
				return fmt.Errorf("restore %s: %w", id, err)
			}
			if snap.SourceType == arkdocker.SourceTypeDockerVolume {
				return fmt.Errorf(
					"restore %s: snapshot is a docker volume, use ark docker restore or ark export %s | tar x",
					snap.ID,
					snap.ID,
				)
			}

			p := printer(cmd)
			if cmd.Bool("dry-run") {
				return dryRunRestore(ctx, repo, p, snap.ID, paths)
			}

			sink, err := arkfs.NewSink(dest)
			if err != nil {
				return fmt.Errorf("restore %s: %w", snap.ID, err)
			}
			pr := p.Progress("restore " + snap.ID)
			start := time.Now()
			res, err := repo.Restore(ctx, snap.ID, sink,
				ark.WithRestorePaths(paths...),
				ark.WithRestoreCache(cache),
				ark.WithRestoreProgress(pr.Func()),
			)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("restore %s: %w", snap.ID, err)
			}
			pr.Donef("restored %s into %s: %s files, %s, %s",
				snap.ID, dest, out.Count(res.FilesRestored), out.Bytes(res.BytesRestored),
				out.Duration(time.Since(start)))

			if p.Mode().JSON {
				return p.JSON(map[string]any{
					"snapshot_id":    snap.ID,
					"files_restored": res.FilesRestored,
					"bytes_restored": res.BytesRestored,
				})
			}
			return nil
		},
	}
}

// cacheSize parses --cache. "0" disables the cache, which ParseBytes alone
// would reject.
func cacheSize(s string) (int64, error) {
	if strings.TrimSpace(s) == "0" {
		return 0, nil
	}
	n, err := out.ParseBytes(s)
	if err != nil {
		return 0, exitcode.Usagef("--cache: %v", err)
	}
	return n, nil
}

// restorePaths trims the slashes a shell completion leaves on --path so the
// prefixes match the snapshot's own paths. Empty entries are dropped.
func restorePaths(paths []string) []string {
	kept := make([]string, 0, len(paths))
	for _, s := range paths {
		if s = strings.Trim(strings.TrimPrefix(s, "./"), "/"); s != "" {
			kept = append(kept, s)
		}
	}
	return kept
}

// dryRunRestore lists the entries a restore would write, one per line, with
// the same prefix rule the library applies. No sink is involved.
func dryRunRestore(ctx context.Context, repo *ark.Repository, p *out.Printer, id string, prefixes []string) error {
	keep := func(path string) bool {
		if len(prefixes) == 0 {
			return true
		}
		path = strings.TrimSuffix(path, "/")
		for _, pre := range prefixes {
			if path == pre || strings.HasPrefix(path, pre+"/") {
				return true
			}
		}
		return false
	}
	err := repo.Walk(ctx, id, func(f ark.FileInfo) error {
		if !keep(f.Path) {
			return nil
		}
		if p.Mode().JSON {
			return p.JSONLine(entryJSON{
				Path: f.Path, Type: string(f.Type), Size: f.Size, Mode: uint32(f.Mode.Perm()),
				UID: f.UID, GID: f.GID, Mtime: f.ModTime, LinkDest: f.LinkDest,
			})
		}
		p.Linef("%s", f.Path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("restore %s: %w", id, err)
	}
	return nil
}
