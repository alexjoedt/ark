package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkuri"
	"github.com/alexjoedt/ark/chunker"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func initCommand() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "create an empty repository at --repo",
		UsageText: `ark init [--chunk-min SIZE] [--chunk-avg SIZE] [--chunk-max SIZE]

   Writes the repository config and a key file sealed with the password,
   asked twice at a prompt. Fails if the repository already exists.
   Chunker sizes are stored in the repository and cannot be changed later.`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "chunk-min", Usage: "minimum chunk `SIZE`", Value: "256KiB"},
			&cli.StringFlag{Name: "chunk-avg", Usage: "average chunk `SIZE`", Value: "1MiB"},
			&cli.StringFlag{Name: "chunk-max", Usage: "maximum chunk `SIZE`", Value: "4MiB"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 0 {
				return exitcode.Usagef("ark init takes no arguments, set the repository with --repo or ARK_REPOSITORY")
			}
			uri := cmd.Root().String("repo")
			if uri == "" {
				return exitcode.Usagef("no repository given, set --repo or ARK_REPOSITORY")
			}
			cfg, err := chunkerConfig(cmd)
			if err != nil {
				return err
			}
			pw, err := repoPassword.read(cmd.Root(), true)
			if err != nil {
				return err
			}

			b, err := arkuri.Open(ctx, uri)
			if err != nil {
				return fmt.Errorf("open repository %s: %w", uri, err)
			}
			defer b.Close()

			err = ark.Init(ctx, b, pw, ark.WithChunkerConfig(cfg))
			switch {
			case errors.Is(err, ark.ErrAlreadyInitialized):
				return fmt.Errorf("repository %s: %w", uri, err)
			case err != nil:
				return fmt.Errorf("initialize repository %s: %w", uri, err)
			}

			p := printer(cmd)
			if p.Mode().JSON {
				return p.JSON(map[string]any{
					"repository": uri,
					"chunker":    map[string]int{"min": cfg.MinSize, "avg": cfg.AvgSize, "max": cfg.MaxSize},
				})
			}
			p.Linef("repository initialized at %s", uri)
			return nil
		},
	}
}

// chunkerConfig parses the three --chunk-* flags. Sizes out of order are a
// usage error, the library would refuse them as well.
func chunkerConfig(cmd *cli.Command) (chunker.Config, error) {
	var cfg chunker.Config
	for _, f := range []struct {
		name string
		dst  *int
	}{{"chunk-min", &cfg.MinSize}, {"chunk-avg", &cfg.AvgSize}, {"chunk-max", &cfg.MaxSize}} {
		n, err := out.ParseBytes(cmd.String(f.name))
		if err != nil {
			return cfg, exitcode.Usagef("--%s: %v", f.name, err)
		}
		*f.dst = int(n)
	}
	if cfg.MinSize > cfg.AvgSize || cfg.AvgSize > cfg.MaxSize {
		return cfg, exitcode.Usagef("chunk sizes must satisfy min <= avg <= max, got %s, %s, %s",
			out.Bytes(int64(cfg.MinSize)), out.Bytes(int64(cfg.AvgSize)), out.Bytes(int64(cfg.MaxSize)))
	}
	return cfg, nil
}
