package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func dockerCommand() *cli.Command {
	return &cli.Command{
		Name:  "docker",
		Usage: "Docker volume commands",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "docker-host", Usage: "Docker daemon `ADDR`", Sources: cli.EnvVars("DOCKER_HOST")},
		},
		Commands: []*cli.Command{
			dockerVolumesCommand(),
			dockerBackupCommand(),
			dockerRestoreCommand(),
		},
	}
}

// stopFlags are shared by backup and restore: both stop the containers that
// use the volume unless told otherwise.
func stopFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "no-stop", Usage: "do not stop containers"},
		&cli.DurationFlag{Name: "stop-timeout", Usage: "graceful stop `DURATION`", Value: 30 * time.Second},
	}
}

// openDocker opens the repository and an Engine on --docker-host. The
// daemon is not contacted until the first call. The caller closes both.
func openDocker(ctx context.Context, cmd *cli.Command) (*arkdocker.Engine, *ark.Repository, error) {
	repo, err := openRepo(ctx, cmd)
	if err != nil {
		return nil, nil, err
	}
	// volumes has no stop flags, the zero duration keeps the engine default
	opts := []arkdocker.Option{
		arkdocker.WithLogger(slog.Default()),
		arkdocker.WithDockerHost(cmd.String("docker-host")),
		arkdocker.WithStopTimeout(cmd.Duration("stop-timeout")),
	}
	if cmd.Bool("no-stop") {
		opts = append(opts, arkdocker.WithoutStop())
	}
	e, err := arkdocker.New(repo, opts...)
	if err != nil {
		_ = repo.Close()
		return nil, nil, fmt.Errorf("docker: %w", err)
	}
	return e, repo, nil
}

func dockerVolumesCommand() *cli.Command {
	return &cli.Command{
		Name:  "volumes",
		Usage: "list Docker volumes",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 0 {
				return exitcode.Usagef("usage: ark docker volumes")
			}
			e, repo, err := openDocker(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()
			defer e.Close()

			vols, err := e.ListVolumes(ctx)
			if err != nil {
				return fmt.Errorf("docker volumes: %w", err)
			}
			snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{})
			if err != nil {
				return fmt.Errorf("docker volumes: %w", err)
			}
			// snapshots arrive newest first, the first hit per name wins
			last := make(map[string]time.Time)
			for _, s := range snaps {
				if _, ok := last[s.SourceName]; !ok && s.SourceType == arkdocker.SourceTypeDockerVolume {
					last[s.SourceName] = s.CreatedAt
				}
			}

			p := printer(cmd)
			if p.Mode().JSON {
				list := make([]volumeJSON, 0, len(vols))
				for _, v := range vols {
					j := volumeJSON{Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint}
					if t, ok := last[v.Name]; ok {
						j.LastSnapshot = &t
					}
					list = append(list, j)
				}
				return p.JSON(list)
			}
			rows := make([][]string, 0, len(vols))
			for _, v := range vols {
				when := "-"
				if t, ok := last[v.Name]; ok {
					when = out.Time(t)
				}
				rows = append(rows, []string{v.Name, v.Driver, v.Mountpoint, when})
			}
			p.Table([]string{"NAME", "DRIVER", "MOUNTPOINT", "LAST SNAPSHOT"}, rows)
			return nil
		},
	}
}

// volumeJSON is one element of ark docker volumes --json.
type volumeJSON struct {
	Name         string     `json:"name"`
	Driver       string     `json:"driver"`
	Mountpoint   string     `json:"mountpoint"`
	LastSnapshot *time.Time `json:"last_snapshot"`
}

func dockerBackupCommand() *cli.Command {
	return &cli.Command{
		Name:      "backup",
		Usage:     "back up a Docker volume",
		ArgsUsage: "<volume>",
		UsageText: `ark docker backup [options] <volume>

   Stops running containers that use <volume>, backs it up, restarts them.
   The snapshot's source name is the volume name, source type docker-volume.`,
		Flags: append([]cli.Flag{
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "add a `TAG`, repeatable"},
			&cli.StringFlag{Name: "comment", Aliases: []string{"c"}, Usage: "free `TEXT` stored with the snapshot"},
		}, stopFlags()...),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 1 {
				return exitcode.Usagef("usage: ark docker backup [options] <volume>")
			}
			volume := cmd.Args().First()

			e, repo, err := openDocker(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()
			defer e.Close()

			p := printer(cmd)
			pr := p.Progress("backup volume " + volume)
			start := time.Now()
			snap, err := e.BackupVolume(ctx, volume,
				ark.WithTags(cmd.StringSlice("tag")...),
				ark.WithComment(cmd.String("comment")),
				ark.WithBackupProgress(pr.Func()),
			)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("docker backup %s: %w", volume, err)
			}
			pr.Donef("%s", out.BackupSummary("backed up volume "+volume, snap.Stats, time.Since(start)))

			if p.Mode().JSON {
				return p.JSON(out.NewSnapshot(snap))
			}
			p.Result("snapshot", snap.ID)
			return nil
		},
	}
}

func dockerRestoreCommand() *cli.Command {
	return &cli.Command{
		Name:      "restore",
		Usage:     "restore a snapshot into a Docker volume",
		ArgsUsage: "<id> [volume]",
		UsageText: `ark docker restore [options] <id> [volume]

   Restores snapshot <id> into [volume] (default: the snapshot's source name).
   Creates the volume if missing, replaces its contents if present.
   Refuses snapshots whose source type is not docker-volume.`,
		Flags: append([]cli.Flag{
			&cli.StringSliceFlag{
				Name:    "path",
				Aliases: []string{"p"},
				Usage:   "restore only entries under `PREFIX`, repeatable",
			},
			&cli.BoolFlag{
				Name:    "dry-run",
				Aliases: []string{"n"},
				Usage:   "show affected containers and whether data is replaced",
			},
			&cli.BoolFlag{Name: "yes", Aliases: []string{"y"}, Usage: "skip the confirmation prompt"},
		}, stopFlags()...),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() < 1 || cmd.NArg() > 2 {
				return exitcode.Usagef("usage: ark docker restore [options] <id> [volume]")
			}
			id, volume := cmd.Args().Get(0), cmd.Args().Get(1)
			paths := restorePaths(cmd.StringSlice("path"))

			e, repo, err := openDocker(ctx, cmd)
			if err != nil {
				return err
			}
			defer repo.Close()
			defer e.Close()

			// Resolve the id before the daemon is asked anything.
			snap, err := repo.Snapshot(ctx, id)
			if err != nil {
				return fmt.Errorf("docker restore %s: %w", id, err)
			}
			if snap.SourceType != arkdocker.SourceTypeDockerVolume {
				return fmt.Errorf(
					"docker restore %s: snapshot is a %s backup, not a docker volume, use ark restore",
					snap.ID,
					snap.SourceType,
				)
			}
			if volume == "" {
				volume = snap.SourceName
			}

			plan, err := e.DryRunVolume(ctx, snap.ID, volume)
			if err != nil {
				return fmt.Errorf("docker restore %s: %w", snap.ID, err)
			}
			p := printer(cmd)
			if cmd.Bool("dry-run") {
				if p.Mode().JSON {
					return p.JSON(map[string]any{
						"snapshot_id":         snap.ID,
						"volume":              volume,
						"replaces_data":       plan.WillEraseData,
						"containers_affected": nonNil(plan.ContainersAffected),
					})
				}
				p.Linef("%s", describeRestore(snap.ID, volume, plan))
				for _, c := range plan.ContainersAffected {
					p.Linef("  stop and restart %s", c)
				}
				return nil
			}

			if err := p.Confirm(describeRestore(snap.ID, volume, plan)); err != nil {
				return err
			}
			pr := p.Progress("restore " + snap.ID + " into volume " + volume)
			start := time.Now()
			res, err := e.RestoreVolume(ctx, snap.ID, volume,
				ark.WithRestorePaths(paths...),
				ark.WithRestoreProgress(pr.Func()),
			)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("docker restore %s: %w", snap.ID, err)
			}
			pr.Donef("restored %s into volume %s: %s files, %s, %s",
				snap.ID, volume, out.Count(res.FilesRestored), out.Bytes(res.BytesRestored),
				out.Duration(time.Since(start)))

			if p.Mode().JSON {
				return p.JSON(map[string]any{
					"snapshot_id":    snap.ID,
					"volume":         volume,
					"files_restored": res.FilesRestored,
					"bytes_restored": res.BytesRestored,
				})
			}
			return nil
		},
	}
}

// describeRestore is the one line the dry run prints and the prompt asks.
func describeRestore(id, volume string, plan *arkdocker.DryRunVolumeResult) string {
	switch {
	case !plan.WillEraseData:
		return fmt.Sprintf("restore %s into new volume %s", id, volume)
	case len(plan.ContainersAffected) == 0:
		return fmt.Sprintf("restore %s into volume %s, replaces its data", id, volume)
	default:
		return fmt.Sprintf("restore %s into volume %s, replaces its data, stops %d containers",
			id, volume, len(plan.ContainersAffected))
	}
}
