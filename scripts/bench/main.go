// Command bench builds the seeded benchmark dataset that ark is timed against
// restic on: gen writes it, mutate applies the incremental change set in
// place, hash prints a digest of a tree, run times ark against restic.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:])
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

const usage = "usage: bench gen|mutate|hash|run -dir DIR [-seed N] [-small|-files] [-ark BIN] [-restic BIN] [-sftp-dir DIR]"

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	if !slices.Contains([]string{"gen", "mutate", "hash", "run"}, args[0]) {
		return fmt.Errorf("unknown subcommand %q, %s", args[0], usage)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := fs.String("dir", "", "dataset directory (required)")
	seed := fs.Int64("seed", 1, "seed")
	small := fs.Bool("small", false, "scaled-down dataset (~120 MiB) for a smoke run")
	files := fs.Bool("files", false, "many-small-files dataset (40k files of 1 to 64 KiB, ~600 MiB)")
	sftpDir := fs.String("sftp-dir", "", "run: local directory the sftp URI's path is served from, "+
		"enables the disk ratio of sftp repositories and removes them after each run")
	arkBin := fs.String("ark", "bin/ark", "run: ark binary")
	resticBin := fs.String("restic", "restic", "run: restic binary")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *dir == "" {
		return errors.New("-dir is required")
	}
	sp, need := fullSpec, uint64(minFree)
	switch {
	case *small && *files:
		return errors.New("-small and -files exclude each other")
	case *small:
		sp, need = smallSpec, minFreeSmall
	case *files:
		sp, need = filesSpec, minFreeFiles
	}
	switch args[0] {
	case "gen":
		files, n, err := generate(ctx, *dir, sp, *seed)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(os.Stdout, "%d files, %.2f GiB\n", files, float64(n)/(1<<30))
	case "mutate":
		return mutate(ctx, *dir, *seed)
	case "hash":
		sum, files, n, err := hashTree(ctx, *dir)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s  %d files, %d bytes\n", sum, files, n)
	case "run":
		return runBench(ctx, os.Stdout, *dir, sp, need, *seed, *arkBin, *resticBin, *sftpDir)
	}
	return nil
}

func parallel(ctx context.Context, n int, fn func(i int) error) error {
	var next atomic.Int64
	var failed atomic.Bool
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Go(func() {
			for !failed.Load() && ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				if err := fn(i); err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
					failed.Store(true)
				}
			}
		})
	}
	wg.Wait()
	if first == nil {
		return ctx.Err()
	}
	return first
}

func listFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", root, err)
	}
	return files, nil
}
