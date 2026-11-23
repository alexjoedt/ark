// Command profile runs a backup and a forget from a profile built in code,
// then decodes the configuration of a scheduler that embeds a profile.
//
//	EXAMPLE_PASSWORD=secret go run ./profile <repo> <dir>
//
// The repository is created on the first run and reused after that.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/arkuri"
)

// job is a scheduler's configuration: a profile plus a key of its own.
type job struct {
	arkprofile.Profile `yaml:",inline"`

	Schedule string `yaml:"schedule"`
}

const jobFile = `
schedule: daily 03:00
repo: sftp://backup@nas/srv/ark
password-env: EXAMPLE_PASSWORD
backup:
  path: ~/Documents
  tag: [docs]
forget:
  keep-daily: 7
  prune: true
`

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: EXAMPLE_PASSWORD=secret profile <repo> <dir>")
	}
	ctx := context.Background()

	p := &arkprofile.Profile{
		Name:        "docs",
		Repo:        os.Args[1],
		PasswordEnv: "EXAMPLE_PASSWORD",
		Backup:      arkprofile.Backup{Path: os.Args[2], Tags: []string{"example"}},
		Forget:      arkprofile.Forget{KeepLast: 3},
	}
	if err := p.Validate(); err != nil {
		log.Fatal(err)
	}
	if err := run(ctx, p); err != nil {
		log.Fatal(err)
	}

	var j job
	if err := arkprofile.Decode(strings.NewReader(jobFile), &j, ""); err != nil {
		log.Fatal(err)
	}
	j.Name = "docs"
	fmt.Printf("scheduler: %s runs profile %s against %s\n", j.Schedule, j.Name, j.Repo)
}

func run(ctx context.Context, p *arkprofile.Profile) error {
	pw, err := p.Password()
	if err != nil {
		return err
	}
	repo, err := p.Open(ctx, pw)
	if errors.Is(err, ark.ErrNotInitialized) || errors.Is(err, fs.ErrNotExist) {
		if err := initRepo(ctx, p.Repo, pw); err != nil {
			return err
		}
		repo, err = p.Open(ctx, pw)
	}
	if err != nil {
		return err
	}
	defer repo.Close()

	src, err := p.Source()
	if err != nil {
		return err
	}
	snap, err := repo.Backup(ctx, src, p.BackupOptions()...)
	if err != nil {
		return err
	}
	fmt.Printf("snapshot %s of %s: %d files\n", snap.ID, snap.SourceName, snap.Stats.FileCount)

	filter, err := p.Filter()
	if err != nil {
		return err
	}
	res, err := repo.Forget(ctx, p.ForgetPolicy(), filter)
	if err != nil {
		return err
	}
	fmt.Printf("forget: kept %d, removed %d\n", len(res.Keep), len(res.Remove))
	if p.Forget.Prune {
		if _, err := repo.Prune(ctx); err != nil {
			return err
		}
	}
	return nil
}

func initRepo(ctx context.Context, uri string, pw ark.Password) error {
	// A local backend opens a directory, it does not create one.
	if dir, ok := arkuri.LocalPath(uri); ok {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	b, err := arkuri.Open(ctx, uri)
	if err != nil {
		return err
	}
	defer b.Close()
	return ark.Init(ctx, b, pw)
}
