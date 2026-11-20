# Series

A series is the run of snapshots that one backup job writes: the same thing, backed up from the
same machine, again and again. You never create a series. ark groups snapshots into series on its
own, and several commands work per series instead of per repository.

You need this page when several machines or several jobs share one repository, or when you rename
a source.

## What makes a series

Two snapshots belong to the same series when they agree in all three of these fields:

| Field | Value | Example |
|---|---|---|
| source type | `file`, `docker-volume`, or `stream` | `file` |
| source name | the absolute path of the directory, or the volume name | `/home/alex` |
| host | the machine's hostname | `laptop` |

So `/home/alex` backed up from `laptop` and from `desktop` are two series. A directory backup
named `data` and a Docker volume named `data` are two series as well.

You can set two of the fields yourself:

- `--source NAME` replaces the path as the source name. Use it when the path may change, or when
  the path says little, such as `/mnt/data`.
- `--host NAME` replaces the hostname. Use it when the hostname isn't stable, for example in a
  container.

In a [profile](cli/profiles.md), the keys are `backup.source` and `backup.host`.

## What a series decides

Four things work per series:

- **The parent of a backup.** A backup takes the newest snapshot of its own series as the parent
  and skips the files that didn't change. It never picks a snapshot from another machine or
  another source.
- **Retention.** `ark forget` applies a policy to each series separately. `ark forget --keep-last 1`
  keeps the newest snapshot of every series, so one machine's policy can't remove another machine's
  last backup.
- **`ark snapshots --last N`** prints the newest N snapshots of every series.
- **`ark mount`** gives every series a `latest` link. See [Mount](mount.md).

## Rename a source

Anything that changes the source name starts a new series:

- a different `--source` or `backup.source`
- a moved directory, when no source name is set
- another volume name

The first backup of the new series has no parent, so it reads and hashes every file again. It
doesn't upload them again: the chunks the old series stored are still in the repository and are
reused.

The old series stays as it is. Its snapshots remain until you remove them, and a retention policy
that keeps the newest snapshot of every series keeps the old one's last snapshot forever. Remove
an abandoned series on purpose:

```bash
ark forget --source /old/path --all
```

## Details

The rules below matter in less common setups.

### The source name with a profile

With `-P`, the source name is `backup.source` if the profile sets it. Otherwise it's the absolute
`backup.path`, the same name a plain `ark backup` of that path stores. For a volume profile, it's
`backup.volume`. The profile's own name never names a snapshot.

### How fields are compared

ark compares the three fields exactly as the snapshot stores them, including case: a `--source`
of `Data` and one of `data` are two series.

### An explicit parent

`--parent ID` names the parent instead of letting ark pick one.

- ark uses the parent's files when its source type and source name match the backup. The host
  isn't checked, so you can continue a series from another machine this way.
- A parent with another source name can't be used. The backup warns, reads every file, and records
  no parent. `--parent` therefore can't bridge a rename.

### The host

The `ark` command sets the host itself:

| Command | Host |
|---|---|
| `ark backup` | the machine's hostname |
| `ark docker backup`, local daemon | the machine's hostname |
| `ark docker backup`, remote `--docker-host` | the daemon's own name |

The library works differently. It records no host unless the program passes `ark.WithHost`, and an
empty host is a series of its own. This default exists for programs that run where the hostname
changes on every start, such as a pod: a new hostname each time would open a new series each time,
and no policy would ever thin them. Set a host when several machines share a repository.

### Time zones

A retention policy counts hours, days, weeks, months, and years in the local time of the machine
that runs `ark forget`, not of the machine that made the backup. See
[Retention policies](retention.md#time-zones).
