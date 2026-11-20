# Docker volumes

ark backs up a Docker volume the way it backs up a directory: one snapshot per backup,
deduplicated and encrypted, in the same repository as everything else. This page shows how to list
volumes, back one up, and restore it, on the local Docker daemon or a remote one.

All volume commands live under `ark docker`.

## Before you start

You need a repository and a password, as in [Getting started](getting-started.md), and access to
the Docker daemon. By default, ark uses the local daemon. To pick another one, see
[Use a remote daemon](#use-a-remote-daemon).

ark reads and writes a volume through a short-lived helper container that runs the image
`busybox:stable`. The daemon pulls the image on first use, so it needs access to a registry once.

## List volumes

```bash
ark docker volumes
```

The list shows every volume of the daemon with its driver and mount point. When a repository is
configured, it also shows each volume's last snapshot, so you see at a glance which volumes have
no backup.

## Back up a volume

```bash
ark docker backup postgres_data
```

ark does three things:

1. It stops the running containers that use the volume, so the data is consistent.
2. It backs up the volume.
3. It starts the containers again.

The snapshot's source name is the volume name, and its type is `docker-volume`:

```bash
ark snapshots --type docker-volume
```

`ark docker backup` takes the flags you know from `ark backup`: `--tag`, `--comment`, `--label`,
`--host`, `--parent`, and `--time`. Two flags control the containers:

| Flag | Effect |
|---|---|
| `--stop-timeout DURATION` | how long a container gets to stop gracefully before it's killed; default 30s |
| `--no-stop` | leave the containers running |

Use `--no-stop` only for data that tolerates being copied while it changes. A database usually
doesn't.

## Restore a volume

```bash
ark docker restore <id>                  # into the volume the snapshot came from
ark docker restore <id> postgres_copy    # into another volume
```

A restore creates the volume if it's missing. If the volume exists, the restore **replaces its
contents**, and it stops the containers that use the volume while it runs. Restore into a new
volume name unless replacing the data is what you want.

Check first what a restore would do:

```bash
ark docker restore --dry-run <id>
```

The dry run names the containers that would be stopped and tells you whether existing data would
be replaced. On a terminal, the real restore asks for confirmation. In a script, add `--yes`.

`--path PREFIX` restores only part of the snapshot. `--no-stop` and `--stop-timeout` work as for a
backup.

### Restore into a directory instead

`ark restore` refuses a volume snapshot. To get the files of a volume snapshot into a plain
directory, export it:

```bash
ark export <id> | tar x
```

You can also browse volume snapshots with [`ark mount`](mount.md).

## Use a remote daemon

ark picks the daemon from the first of these that is set:

1. `--docker-host ADDR`, for example `tcp://db1:2375`
2. the `docker-host` key of a profile
3. the `DOCKER_HOST` environment variable

```bash
ark docker --docker-host tcp://db1:2375 backup postgres_data
```

The repository is opened from the machine that runs ark, not from the remote one. The volume data
travels from the daemon to ark and from there into the repository.

## Volumes and hosts

A volume name is only unique per daemon. Two machines can each have a `postgres_data`, and their
backups must not be mixed up. That's why a volume forms one [series](series.md) per host:

- With the local daemon, the host is this machine's hostname.
- With a remote daemon, the host is the daemon's own name.
- `--host NAME` overrides both.

The parent of a backup, `ark forget`, and the last snapshot in `ark docker volumes` all look only
at the snapshots of the same volume and host.

## Schedule volume backups with a profile

A [profile](cli/profiles.md) can name a volume instead of a directory:

```yaml
# ~/.config/ark/profiles/db.yaml
repo: sftp://backup@nas/srv/ark
password-file: ~/.config/ark/db.pw
docker-host: tcp://db1:2375
backup:
  volume: pgdata
  host: db1
  stop-timeout: 2m
forget:
  keep-daily: 7
```

```bash
ark profile backup db            # back up pgdata on db1's daemon
ark forget -P db --yes           # apply the policy to this volume's series
ark docker restore -P db <id>    # restore into pgdata
```

A profile with a remote `docker-host` must set `backup.host`. See
[Volume profiles](cli/profiles.md#volume-profiles) for the rules.

## Details

- **What ark records.** Each volume snapshot carries labels with the Docker host and with the
  name, image, and state of every container that used the volume. See
  [Labels that ark sets](labels.md#labels-that-ark-sets).
- **No excludes.** A volume is always backed up whole. `ark docker backup` has no `--exclude`.
- **Without a parent.** When there is no earlier snapshot of the same volume and host, and
  `--parent` names none, the backup reads the whole volume.
- **Snapshots without a host.** A volume snapshot that records no host, as a program using the
  library can write it, is never picked as a parent. `ark docker volumes` shows it as the last
  snapshot only while the host has no snapshot of its own.
- **`ark docker volumes --json`.** `last_snapshot` is `null` for a volume that was never backed
  up. The field is absent when no repository is configured. A repository that is configured must
  open, or the command fails.
- **Restoring another type.** `ark docker restore` refuses a snapshot whose source type isn't
  `docker-volume`.
