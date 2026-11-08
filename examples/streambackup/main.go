// Command streambackup pipes the stdout of a shell command into a backup and
// restores it to stdout, the way a database dump would go in without a file
// on disk:
//
//	go run ./streambackup 'pg_dump mydb'
//
// Without an argument it backs up the output of a harmless command. The
// repository is in memory; the summary goes to stderr.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

func main() {
	command := "seq 1 100000"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	ctx := context.Background()

	b := arkmem.New()
	if err := ark.Init(ctx, b, ark.WithoutPassword()); err != nil {
		log.Fatal(err)
	}
	repo, err := ark.Open(ctx, b, ark.WithoutPassword())
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	snap, err := repo.Backup(ctx, ark.NewStreamSource("dump", stdout))
	// Wait closes the pipe, so it runs after the backup read it to EOF.
	if werr := cmd.Wait(); err == nil && werr != nil {
		err = fmt.Errorf("command %q: %w", command, werr)
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "snapshot %s: %d bytes, %d chunks\n", snap.ID, snap.Stats.TotalSize, snap.Stats.ChunksNew)

	if _, err := repo.Restore(ctx, snap.ID, ark.NewStreamSink(os.Stdout)); err != nil {
		log.Fatal(err)
	}
}
