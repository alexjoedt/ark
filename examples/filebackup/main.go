// Command filebackup backs up a directory into a local repository and
// restores it to a second directory.
//
//	go run ./filebackup <repo> <dir> <restore-to>
//
// The repository is created on the first run and reused after that.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkfs"
	"github.com/alexjoedt/ark/arklocal"
)

func main() {
	if len(os.Args) != 4 {
		log.Fatal("usage: filebackup <repo> <dir> <restore-to>")
	}
	repoPath, dir, dest := os.Args[1], os.Args[2], os.Args[3]
	ctx := context.Background()

	b, err := arklocal.New(repoPath)
	if err != nil {
		log.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if errors.Is(err, ark.ErrNotInitialized) {
		if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
			log.Fatal(err)
		}
		repo, err = ark.Open(ctx, b, ark.WithoutPassword())
	}
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()

	snap, err := arkfs.Backup(ctx, repo, dir, ark.WithTags("example"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("snapshot %s: %d files, %d bytes stored\n", snap.ID, snap.Stats.FileCount, snap.Stats.StoredSize)

	if _, err := arkfs.Restore(ctx, repo, snap.ID, dest); err != nil {
		log.Fatal(err)
	}
	fmt.Println("restored to", dest)
}
