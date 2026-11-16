package arkfind_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfind"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arkmem"
)

func ExampleFind() {
	ctx := context.Background()
	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		panic(err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		panic(err)
	}
	defer repo.Close()

	dir, err := os.MkdirTemp("", "arkfind-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	src, err := arkfs.NewSource(dir, arkfs.WithName("notes"))
	if err != nil {
		panic(err)
	}

	// Monday holds one note, Tuesday adds a second one in a subdirectory.
	if err := os.WriteFile(filepath.Join(dir, "todo.txt"), []byte("buy milk"), 0o600); err != nil {
		panic(err)
	}
	if _, err := repo.Backup(ctx, src, ark.WithTags("monday")); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "work", "plan.txt"), []byte("ship it"), 0o600); err != nil {
		panic(err)
	}
	if _, err := repo.Backup(ctx, src, ark.WithTags("tuesday")); err != nil {
		panic(err)
	}

	err = arkfind.Find(ctx, repo, ark.SnapshotFilter{Source: "notes"}, []string{"*.txt"}, func(m arkfind.Match) error {
		fmt.Printf("%s %s %d bytes\n", m.Snapshot.Tags[0], m.File.Path, m.File.Size)
		return nil
	})
	if err != nil {
		panic(err)
	}
	// Output:
	// tuesday todo.txt 8 bytes
	// tuesday work/plan.txt 7 bytes
	// monday todo.txt 8 bytes
}
