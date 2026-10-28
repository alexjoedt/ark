package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

// lockJSON is the lock shape of ark unlock --json, frozen for 1.x.
type lockJSON struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Exclusive bool      `json:"exclusive"`
	Hostname  string    `json:"hostname"`
	PID       int       `json:"pid"`
	Stale     bool      `json:"stale"`
}

func unlockCommand() *cli.Command {
	return &cli.Command{
		Name:  "unlock",
		Usage: "remove stale repository locks",
		UsageText: `ark unlock [--force]

   Removes stale locks. --force removes every lock; make sure no ark process is running.`,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "force", Usage: "remove every lock, stale or not"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			repo, err := openRepo(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()

			var opts []ark.UnlockOption
			if cmd.Bool("force") {
				opts = append(opts, ark.WithForce())
			}
			removed, err := repo.Unlock(ctx, opts...)
			if err != nil {
				return fmt.Errorf("unlock: %w", err)
			}
			locks, err := repo.Locks(ctx)
			if err != nil {
				return fmt.Errorf("unlock: %w", err)
			}

			p := printer(cmd)
			if p.Mode().JSON {
				list := make([]lockJSON, 0, len(locks))
				for _, l := range locks {
					list = append(list, lockJSON{l.ID, l.Time, l.Exclusive, l.Hostname, l.PID, l.Stale})
				}
				return p.JSON(map[string]any{"removed": removed, "locks": list})
			}
			p.Linef("removed %d locks, %d left", removed, len(locks))
			rows := make([][]string, 0, len(locks))
			for _, l := range locks {
				kind := "shared"
				if l.Exclusive {
					kind = "exclusive"
				}
				rows = append(
					rows,
					[]string{
						l.ID,
						out.Time(l.Time),
						kind,
						l.Hostname,
						strconv.Itoa(l.PID),
						strconv.FormatBool(l.Stale),
					},
				)
			}
			if len(rows) > 0 {
				p.Table([]string{"ID", "TIME", "TYPE", "HOST", "PID", "STALE"}, rows)
			}
			return nil
		},
	}
}
