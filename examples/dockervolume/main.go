// Command dockervolume backs up a named Docker volume into a local repository
// and restores the snapshot into a second volume.
//
//	go run ./dockervolume <repo> <volume> <restore-volume>
//
// Containers using either volume are stopped for the copy and started again
// after. The daemon comes from DOCKER_HOST, or the default socket.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arklocal"
)

func main() {
	if len(os.Args) != 4 {
		log.Fatal("usage: dockervolume <repo> <volume> <restore-volume>")
	}
	repoPath, volume, target := os.Args[1], os.Args[2], os.Args[3]
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

	engine, err := arkdocker.New(repo)
	if err != nil {
		log.Fatal(err)
	}
	defer engine.Close()

	snap, err := engine.BackupVolume(ctx, volume)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("snapshot %s of volume %s: %d files\n", snap.ID, volume, snap.Stats.FileCount)

	if _, err := engine.RestoreVolume(ctx, snap.ID, target); err != nil {
		log.Fatal(err)
	}
	fmt.Println("restored into volume", target)
}
