# Mount

`ark mount` shows every snapshot of a repository as a read-only directory tree. You can browse
old versions with `ls`, compare them with `diff`, and copy single files back with `cp`, without
restoring a whole snapshot.

## Mount a repository

Mounting needs FUSE on your machine, see [Platforms](#platforms). Then:

```bash
mkdir -p /mnt/ark
ark mount /mnt/ark
```

The command keeps running and serves the mount until you press Ctrl-C. Work in a second shell:

```bash
ls /mnt/ark/sources/files/laptop/home/alex/latest/
cp /mnt/ark/sources/files/laptop/home/alex/latest/notes.txt ~/notes.txt
```

The mount shows the snapshots that existed when it started. To see newer ones, unmount and mount
again.

## Find a snapshot

The mount offers three ways to reach a snapshot:

```
/mnt/ark/
  ids/<id>/                                  one directory per snapshot
  tags/<tag>/<time>/                         the snapshots with that tag
  sources/<type>/<host>/<source>/
      <time>/                                one directory per snapshot of the series
      latest -> <newest time>
```

`sources/` is usually the one you want. It follows the [series](series.md): pick the type, the
host, and the source, and you get that series' snapshots by time, with `latest` pointing at the
newest.

Take a repository with two directory backups from the host `laptop`, a Docker volume, and a stream
from a program that recorded no host:

```
sources/
  files/
    laptop/
      home/alex/
        2026-11-16 09:00:00/
        2026-11-16 21:00:00/
        latest -> 2026-11-16 21:00:00
      srv/app/
        2026-11-16 12:00:00/
        latest -> 2026-11-16 12:00:00
  docker-volumes/
    laptop/postgres_data/...
  streams/
    _/dump/...
```

- `<type>` is the source type in the plural: `files`, `docker-volumes`, `streams`.
- A source path nests. `/home/alex` becomes `home/alex/`, so `/home` and `/home/alex` share `home/`.
- An empty host shows as `_`.

## Unmount

Press Ctrl-C in the shell that runs `ark mount`.

If a process still uses the mount, for example a shell whose working directory is inside it, ark
keeps serving and tries again every second. Leave the directory, and the unmount goes through. A
second Ctrl-C, or SIGTERM, quits at once with exit code 1 and leaves the mount point for you to
unmount by hand.

## Mount and prune

A mount holds a shared [lock](concepts.md#locks) for as long as it runs. Backups and restores work
as usual, but `ark prune` and `ark forget` exit 3 until you unmount. With `--lock-wait`,
they wait that long first.

## Platforms

| Platform | Requirement |
|---|---|
| Linux | FUSE 3: `/dev/fuse` and `fusermount3`, in the package `fuse3` |
| macOS | [macFUSE](https://macfuse.github.io/); best effort |
| Windows, 32-bit Linux | not supported |

## Details: special files

A fifo or a device node of a snapshot shows up with its type and, for a device, its major and
minor number. A device number the kernel can't hold (on Linux a major above 4095 or a minor above
20 bits) shows as 0, 0. ark mounts with `nodev` on Linux, so opening a device node fails and never
reaches the device of your machine. Opening a fifo blocks: the kernel opens it as a pipe that
nothing writes to. A tool that reads every file, such as `diff -r` or `sha256sum`, hangs on one;
leave fifos out, for example with `find -type f`.

## Details: directory names

Hosts and source names are free text, and not every one of them works as a directory name. ark
adjusts the names that don't:

- A `/` in a host becomes `_`.
- A host or path element gets a `_` in front when it's empty, is named `latest`, looks like a
  time, or starts with `.` or `_`.
- Two snapshots in the same second in one directory, a series or a tag: the second one gets the
  suffix `-1`, the next one `-2`.
- `ids/` uses the short ID. When two snapshots share that prefix, it uses the full ID.

Two names can end up the same after these changes, or a relative source name can collide with an
absolute one. The later one then gets a `-1` or `-2` suffix. Only such names depend on which other
snapshots exist:

| Name | Directory | Becomes, once the other exists |
|---|---|---|
| relative source `home` | `home/` | `home-1/`, when a snapshot of `/home` exists |
| host `a/b` | `a_b/` | `a_b-1/`, when a host named `a_b` exists |
