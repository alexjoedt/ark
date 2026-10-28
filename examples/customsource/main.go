// Command customsource backs up data that does not come from a filesystem.
// It implements ark.Source over an in-memory set of files, the same shape a
// database dump or an API export would take, and stores it in an in-memory
// repository.
//
//	go run ./customsource
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkmem"
)

// memSource yields a fixed set of files under one directory.
type memSource struct {
	name  string
	files map[string]string
}

func (s memSource) Meta() ark.SourceMeta {
	return ark.SourceMeta{Name: s.name, Type: "memory"}
}

func (s memSource) Walk(ctx context.Context, fn ark.WalkFunc) error {
	now := time.Now()
	// Directories are yielded explicitly, the core only nests what it sees.
	if err := fn(ark.FileInfo{Path: "data", Mode: 0o755, ModTime: now, Type: ark.FileTypeDirectory}, nil); err != nil {
		return err
	}
	for name, content := range s.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		info := ark.FileInfo{
			Path:    "data/" + name,
			Size:    int64(len(content)),
			Mode:    0o644,
			ModTime: now,
			Type:    ark.FileTypeRegular,
		}
		r := io.LimitReader(strings.NewReader(content), info.Size)
		if err := fn(info, r); err != nil {
			return err
		}
	}
	return nil
}

func main() {
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

	src := memSource{name: "settings", files: map[string]string{
		"a.json": `{"theme":"dark"}`,
		"b.json": `{"lang":"en"}`,
	}}
	snap, err := repo.Backup(ctx, src)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("snapshot %s of %q: %d files\n", snap.ID, snap.SourceName, snap.Stats.FileCount)

	err = repo.Walk(ctx, snap.ID, func(e ark.FileInfo) error {
		fmt.Printf("%-12s %4d bytes\n", e.Path, e.Size)
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}
