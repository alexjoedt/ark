package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	ark "github.com/alexjoedt/ark"
	"github.com/alexjoedt/ark/arkdocker"
	"github.com/alexjoedt/ark/arkprofile"
	"github.com/alexjoedt/ark/cmd/ark/internal/exitcode"
	"github.com/alexjoedt/ark/cmd/ark/internal/out"
	"github.com/urfave/cli/v3"
)

func dockerCommand() *cli.Command {
	return &cli.Command{
		Name:  "docker",
		Usage: "Docker volume commands",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "docker-host",
				Usage: "Docker daemon `ADDR` (default: a profile's docker-host, else $DOCKER_HOST)",
			},
		},
		Commands: []*cli.Command{
			dockerVolumesCommand(),
			dockerBackupCommand(),
			dockerRestoreCommand(),
		},
	}
}

// defaultStopTimeout is arkdocker's own default, shown by --help and ark
// profile show.
const defaultStopTimeout = 30 * time.Second

// stopOptions are --no-stop and --stop-timeout, each over b's no-stop and
// stop-timeout when not given: the backup section of a profile, or none for a
// restore. A timeout of 0 is arkdocker's default.
func stopOptions(cmd *cli.Command, b arkprofile.Backup) []arkdocker.Option {
	timeout, noStop := stopSettings(cmd, b)
	opts := []arkdocker.Option{arkdocker.WithStopTimeout(timeout)}
	if noStop {
		opts = append(opts, arkdocker.WithoutStop())
	}
	return opts
}

func stopSettings(cmd *cli.Command, b arkprofile.Backup) (time.Duration, bool) {
	timeout, noStop := b.StopTimeout, b.NoStop
	if cmd.IsSet("stop-timeout") {
		timeout = cmd.Duration("stop-timeout")
	}
	if cmd.IsSet("no-stop") {
		noStop = cmd.Bool("no-stop")
	}
	return timeout, noStop
}

// stopFlags are shared by backup and restore: both stop the containers that
// use the volume unless told otherwise.
func stopFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "no-stop", Usage: "do not stop containers"},
		&cli.DurationFlag{Name: "stop-timeout", Usage: "graceful stop `DURATION`", Value: defaultStopTimeout},
	}
}

// dockerHost is --docker-host, else the profile's docker-host, else
// DOCKER_HOST: the profile beats the variable, as for every other key.
func dockerHost(ctx context.Context, cmd *cli.Command) string {
	if cmd.IsSet("docker-host") {
		return cmd.String("docker-host")
	}
	if lp := profileFrom(ctx); lp != nil && lp.DockerHost != "" {
		return lp.DockerHost
	}
	return os.Getenv("DOCKER_HOST")
}

// openDocker opens the repository and an Engine on the daemon dockerHost
// names, stopping containers as stop says. The daemon is not contacted until
// the first call. The caller closes both.
func openDocker(
	ctx context.Context,
	cmd *cli.Command,
	stop []arkdocker.Option,
) (*arkdocker.Engine, *ark.Repository, error) {
	repo, err := openRepo(ctx, cmd)
	if err != nil {
		return nil, nil, err
	}
	opts := append([]arkdocker.Option{
		arkdocker.WithLogger(slog.Default()),
		arkdocker.WithDockerHost(dockerHost(ctx, cmd)),
	}, stop...)
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
		Flags: []cli.Flag{hostFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if cmd.NArg() != 0 {
				return exitcode.Usagef("usage: ark docker volumes")
			}
			vols, last, err := listVolumes(ctx, cmd)
			if err != nil {
				return err
			}
			return printVolumes(printer(cmd), vols, last)
		},
	}
}

// listVolumes asks the daemon for the volumes. The repository feeds only the
// last snapshot map and is optional: without one configured the map is nil.
// A configured one still has to open, password included.
func listVolumes(ctx context.Context, cmd *cli.Command) ([]arkdocker.VolumeInfo, map[string]time.Time, error) {
	var repo *ark.Repository
	if uri, _ := repoSetting(cmd.Root(), profileFrom(ctx)); uri != "" {
		var err error
		if repo, err = openRepo(ctx, cmd); err != nil {
			return nil, nil, err
		}
		defer repo.Close()
	}
	c, err := arkdocker.NewClient(dockerHost(ctx, cmd))
	if err != nil {
		return nil, nil, fmt.Errorf("docker volumes: %w", err)
	}
	defer c.Close()

	host, err := dockerSeriesHost(ctx, cmd, c.DaemonName)
	if err != nil {
		return nil, nil, fmt.Errorf("docker volumes: %w", err)
	}
	vols, err := c.ListVolumes(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("docker volumes: %w", err)
	}
	if repo == nil {
		return vols, nil, nil
	}
	snaps, err := repo.Snapshots(ctx, ark.SnapshotFilter{Type: arkdocker.SourceTypeDockerVolume})
	if err != nil {
		return nil, nil, fmt.Errorf("docker volumes: %w", err)
	}
	return vols, lastVolumeSnapshots(snaps, host), nil
}

// printVolumes lists vols. last is nil without a repository: the LAST
// SNAPSHOT column and the last_snapshot key are then left out, so a volume
// never backed up (null) reads apart from no repository to ask (absent).
func printVolumes(p *out.Printer, vols []arkdocker.VolumeInfo, last map[string]time.Time) error {
	if p.Mode().JSON {
		plain := make([]volumeJSON, 0, len(vols))
		for _, v := range vols {
			plain = append(plain, volumeJSON{Name: v.Name, Driver: v.Driver, Mountpoint: v.Mountpoint})
		}
		if last == nil {
			return p.JSON(plain)
		}
		list := make([]volumeSnapshotJSON, 0, len(vols))
		for _, j := range plain {
			s := volumeSnapshotJSON{volumeJSON: j}
			if t, ok := last[j.Name]; ok {
				s.LastSnapshot = &t
			}
			list = append(list, s)
		}
		return p.JSON(list)
	}
	header := []string{"NAME", "DRIVER", "MOUNTPOINT"}
	if last != nil {
		header = append(header, "LAST SNAPSHOT")
	}
	rows := make([][]string, 0, len(vols))
	for _, v := range vols {
		row := []string{v.Name, v.Driver, v.Mountpoint}
		if last != nil {
			when := "-"
			if t, ok := last[v.Name]; ok {
				when = out.TimeLong(t)
			}
			row = append(row, when)
		}
		rows = append(rows, row)
	}
	p.Table(header, rows)
	return nil
}

// hostFlag is --host of the docker commands. Its default is computed by
// dockerSeriesHost, which needs the daemon.
func hostFlag() cli.Flag {
	return &cli.StringFlag{
		Name:  "host",
		Usage: "host `NAME` of the snapshots (default: a volume profile's backup.host, else this machine's hostname, or a remote daemon's name)",
	}
}

// dockerSeriesHost is the host docker snapshots are filed under: --host if
// set, else the backup.host of a profile that backs up no path, else the
// hostname of the machine the volumes live on. That is this machine for a
// local daemon and the daemon's own name for a remote one, so the same
// volume name on two daemons is two series. A local daemon is not asked,
// Docker Desktop names itself docker-desktop on every machine; daemonName is
// called only for a remote one.
func dockerSeriesHost(
	ctx context.Context,
	cmd *cli.Command,
	daemonName func(context.Context) (string, error),
) (string, error) {
	if cmd.IsSet("host") {
		return cmd.String("host"), checkHostFlag(cmd)
	}
	if lp := profileFrom(ctx); lp != nil && lp.Backup.Host != "" && !pathProfile(lp) {
		return lp.Backup.Host, nil
	}
	if remoteDaemon(dockerHost(ctx, cmd)) {
		return daemonName(ctx)
	}
	h, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("hostname: %w, set --host", err)
	}
	return h, nil
}

// checkHostFlag refuses a daemon address in --host, meant for --docker-host.
func checkHostFlag(cmd *cli.Command) error {
	if h := cmd.String("host"); strings.Contains(h, "://") {
		return exitcode.Usagef("--host %q is a daemon address, the daemon is set with --docker-host", h)
	}
	return nil
}

// pathProfile reports whether the profile backs up a path, not a volume.
func pathProfile(lp *loadedProfile) bool {
	return lp.Backup.Path != "" || lp.Backup.Source != ""
}

// remoteDaemon reports whether the daemon address is on another machine:
// tcp:// or ssh://, not a local socket or pipe and not the default.
func remoteDaemon(addr string) bool {
	return addr != "" && !strings.HasPrefix(addr, "unix://") && !strings.HasPrefix(addr, "npipe://")
}

// lastVolumeSnapshots maps a volume name to the time of its newest snapshot
// recorded for host: the series key of the library, so another machine's
// backup of the same volume name does not show here. A volume with none
// falls back to its snapshots without a host, written before the host was
// recorded. snaps are volume snapshots, newest first.
func lastVolumeSnapshots(snaps []*ark.Snapshot, host string) map[string]time.Time {
	last := make(map[string]time.Time)
	legacy := make(map[string]time.Time)
	for _, s := range snaps {
		m := last
		switch s.Host {
		case host:
		case "":
			m = legacy
		default:
			continue
		}
		if _, ok := m[s.SourceName]; !ok {
			m[s.SourceName] = s.CreatedAt
		}
	}
	for name, t := range legacy {
		if _, ok := last[name]; !ok {
			last[name] = t
		}
	}
	return last
}

// volumeJSON is one element of ark docker volumes --json without a
// repository, volumeSnapshotJSON one with a repository.
type volumeJSON struct {
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Mountpoint string `json:"mountpoint"`
}

type volumeSnapshotJSON struct {
	volumeJSON
	LastSnapshot *time.Time `json:"last_snapshot"`
}

func dockerBackupCommand() *cli.Command {
	return &cli.Command{
		Name:      "backup",
		Usage:     "back up a Docker volume",
		ArgsUsage: "<volume>",
		UsageText: `ark docker backup [options] <volume>
   ark docker backup -P NAME [options]

   Stops running containers that use <volume>, backs it up, restarts them.
   The snapshot's source name is the volume name, source type docker-volume. The
   host is this machine's hostname, or the daemon's name when --docker-host is a
   remote daemon, unless --host is set. The parent is the newest snapshot of the
   same volume and host unless --parent is set. With a profile, <volume> defaults
   to its backup.volume, any other volume is refused; its tags, labels, comment
   and host apply unless a flag sets them, and its backup hooks run around it.`,
		Flags: append([]cli.Flag{
			hostFlag(),
			&cli.StringFlag{Name: "parent", Usage: "explicit parent snapshot `ID`"},
			timeFlag(),
			&cli.StringSliceFlag{Name: "tag", Aliases: []string{"t"}, Usage: "add a `TAG`, repeatable"},
			&cli.StringFlag{Name: "comment", Aliases: []string{"c"}, Usage: "free `TEXT` stored with the snapshot"},
			labelFlag("record the label `KEY=VALUE`, repeatable, the value may hold commas"),
		}, stopFlags()...),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			lp := profileFrom(ctx)
			volume, err := backupVolume(cmd, lp)
			if err != nil {
				return err
			}
			return runVolumeBackup(ctx, cmd, lp, volume, backupProfile(cmd, lp).Backup)
		},
	}
}

// runVolumeBackup backs up volume with the tags, comment, labels and stop
// settings of b and the remaining flags of cmd; a cmd without them, ark
// profile backup, takes b alone. The backup hooks of lp run around it.
func runVolumeBackup(
	ctx context.Context,
	cmd *cli.Command,
	lp *loadedProfile,
	volume string,
	b arkprofile.Backup,
) error {
	labels, err := labelValue(cmd, false)
	if err != nil {
		return err
	}
	at, err := timeOption(cmd)
	if err != nil {
		return err
	}
	// before the hooks: a usage error must not follow their side effects
	if err := checkHostFlag(cmd); err != nil {
		return err
	}

	return withHooks(ctx, lp, "backup", func(ctx context.Context) error {
		e, repo, err := openDocker(ctx, cmd, stopOptions(cmd, b))
		if err != nil {
			return err
		}
		defer repo.Close()
		defer e.Close()

		host, err := dockerSeriesHost(ctx, cmd, e.Client().DaemonName)
		if err != nil {
			return fmt.Errorf("docker backup %s: %w", volume, err)
		}
		p := printer(cmd)
		pr := startProgress(p, "backup volume "+volume)
		// --label adds to the profile's labels, a key in both takes the flag's value
		snap, err := e.BackupVolume(ctx, volume, append(at,
			ark.WithTags(b.Tags...),
			ark.WithComment(b.Comment),
			ark.WithHost(host),
			ark.WithParent(ark.SnapshotID(cmd.String("parent"))),
			ark.WithLabels(b.Labels),
			ark.WithLabels(labels),
		)...)
		if err != nil {
			pr.Fail()
			return fmt.Errorf("docker backup %s: %w", volume, err)
		}
		pr.Done(out.BackupRows(snap)...)

		if p.Mode().JSON {
			return p.JSON(out.NewSnapshot(snap))
		}
		p.Result("snapshot", string(snap.ID))
		return nil
	})
}

func dockerRestoreCommand() *cli.Command {
	return &cli.Command{
		Name:      "restore",
		Usage:     "restore a snapshot into a Docker volume",
		ArgsUsage: "<id> [volume]",
		UsageText: `ark docker restore [options] <id> [volume]

   Restores snapshot <id> into [volume] (default: a profile's backup.volume,
   which refuses a snapshot of another volume, else the snapshot's source name).
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
			id, volume := ark.SnapshotID(cmd.Args().Get(0)), cmd.Args().Get(1)
			paths := restorePaths(cmd.StringSlice("path"))

			e, repo, err := openDocker(ctx, cmd, stopOptions(cmd, arkprofile.Backup{}))
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
			if lp := profileFrom(ctx); volume == "" && lp != nil && lp.Backup.Volume != "" {
				if snap.SourceName != lp.Backup.Volume {
					return exitcode.Usagef(
						"docker restore %s: snapshot of volume %s, not the backup.volume %s of profile %s, name the target volume",
						snap.ID,
						snap.SourceName,
						lp.Backup.Volume,
						lp.Name,
					)
				}
				volume = lp.Backup.Volume
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
			pr := startProgress(p, "restore "+out.ShortID(snap.ID)+" into volume "+volume)
			res, err := e.RestoreVolume(ctx, snap.ID, volume,
				ark.WithRestorePaths(paths...),
			)
			if err != nil {
				pr.Fail()
				return fmt.Errorf("docker restore %s: %w", snap.ID, err)
			}
			pr.Done(out.R("files", "%s", out.Count(res.FilesRestored)),
				out.R("size", "%s", out.Bytes(res.BytesRestored)))
			skipped := warnSkipped(p, res.Skipped)

			if p.Mode().JSON {
				return p.JSON(map[string]any{
					"snapshot_id":    snap.ID,
					"volume":         volume,
					"files_restored": res.FilesRestored,
					"bytes_restored": res.BytesRestored,
					"skipped":        skipped,
				})
			}
			return nil
		},
	}
}

// backupVolume is the volume docker backup works on: the argument, else the
// profile's backup.volume. A profile of a path, or another volume than the
// profile's, is refused: its snapshots would not be the profile's series.
func backupVolume(cmd *cli.Command, lp *loadedProfile) (string, error) {
	const usage = "usage: ark docker backup [options] <volume>, or -P NAME with a backup.volume"
	if cmd.NArg() > 1 {
		return "", exitcode.Usagef(usage)
	}
	arg := cmd.Args().First()
	if lp == nil {
		if arg == "" {
			return "", exitcode.Usagef(usage)
		}
		return arg, nil
	}
	if pathProfile(lp) {
		return "", exitcode.Usagef("profile %s backs up a path, not a docker volume, use %s or ark backup %s",
			lp.Name, profileBackupCommand(cmd), profileArg(cmd))
	}
	switch {
	case lp.Backup.Volume == "" && arg == "":
		return "", exitcode.Usagef(usage)
	case lp.Backup.Volume == "":
		return arg, nil
	case arg != "" && arg != lp.Backup.Volume:
		return "", exitcode.Usagef("docker backup %s: not the backup.volume of profile %s", arg, lp.Name)
	}
	return lp.Backup.Volume, nil
}

// describeRestore is the one line the dry run prints and the prompt asks.
func describeRestore(id ark.SnapshotID, volume string, plan *arkdocker.DryRunVolumeResult) string {
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
