// Command bench builds the seeded benchmark dataset that ark is timed against
// restic on: gen writes it, mutate applies the incremental change set in
// place, hash prints a digest of a tree.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: bench gen|mutate|hash -dir DIR [-seed N] [-small]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	dir := fs.String("dir", "", "dataset directory (required)")
	seed := fs.Int64("seed", 1, "seed")
	small := fs.Bool("small", false, "scaled-down dataset (~120 MiB) for a smoke run")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("-dir is required")
	}
	sp := fullSpec
	if *small {
		sp = smallSpec
	}
	switch args[0] {
	case "gen":
		files, n, err := generate(*dir, sp, *seed)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(os.Stdout, "%d files, %.2f GiB\n", files, float64(n)/(1<<30))
	case "mutate":
		return mutate(*dir, *seed)
	case "hash":
		sum, files, n, err := hashTree(*dir)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s  %d files, %d bytes\n", sum, files, n)
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
	return nil
}

func parallel(n int, fn func(i int) error) error {
	var next atomic.Int64
	var failed atomic.Bool
	var mu sync.Mutex
	var first error
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		wg.Go(func() {
			for !failed.Load() {
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
	return files, err
}
