// Command v1fixture writes the stored repository of format version 1 under
// testdata/repo-v1 from the synthetic source in internal/v1fixture: Init with
// the fixture's password and chunker, then a backup of each snapshot, then a
// check. The repository is written once and committed; the tests that read it
// and write it again pin the bytes version 1 stores. After the format freeze
// it is never written again: a build that can no longer read it, or that
// writes other names for the same input, breaks every repository out there.
//
//	go run ./scripts/v1fixture [-out testdata/repo-v1]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arklocal"
	"github.com/alexjoedt/ark/internal/v1fixture"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "v1fixture:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("v1fixture", flag.ContinueOnError)
	out := fs.String("out", v1fixture.Dir, "directory to write the repository to; must not exist")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if _, err := os.Lstat(*out); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s exists: the fixture is written once, remove it only before the format freeze", *out)
	}

	b, err := arklocal.New(*out)
	if err != nil {
		return err
	}
	defer b.Close()
	pw := ark.NewPassword(v1fixture.Password)
	if err := ark.Init(ctx, b, pw, ark.WithChunkerConfig(v1fixture.Chunker)); err != nil {
		return err
	}
	repo, err := ark.Open(ctx, b, pw)
	if err != nil {
		return err
	}
	defer repo.Close()

	for _, s := range v1fixture.Snapshots() {
		snap, err := repo.Backup(ctx, s.Source(), s.Options()...)
		if err != nil {
			return err
		}
		fmt.Fprintf(
			os.Stdout,
			"snapshot %s at %s: %d files\n",
			snap.ID,
			snap.CreatedAt.UTC().Format(time.RFC3339Nano),
			len(s.Files),
		)
	}
	res, err := repo.Check(ctx)
	if err != nil {
		return err
	}
	if len(res.Errors)+len(res.Warnings) > 0 || res.SnapshotsChecked != len(v1fixture.Snapshots()) {
		return fmt.Errorf("check of the new repository: %+v", res)
	}
	return nil
}
