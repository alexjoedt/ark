# Mount

`ark mount DIR` serves every snapshot of the repository read-only through FUSE until Ctrl-C. The
snapshot list is the one at mount start; remount to see newer snapshots.

## Layout

```
DIR/
  ids/<id>/                                  one directory per snapshot
  tags/<tag>/<time>/                         snapshots with that tag
  sources/<type>/<host>/<source path>/
      <time>/                                one directory per snapshot of the series
      latest -> <newest time>
```

For a repository with a directory backup of `/home/alex` from host `laptop`, one of `/srv/app`, a
Docker volume `postgres_data` and a stream `dump` from a host that recorded none:

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

- `<type>` is the source type plus `s`: `files`, `docker-volumes`, `streams`.
- `<source path>` nests: `/home/alex` is `home/alex/`, so `/home` and `/home/alex` share `home/`.
  The sources of one [series](SERIES.md) are one directory, so `latest` is per series.
- An empty host is `_`.
- A host or path element that is empty, `latest`, shaped like a time or starts with `.` or `_` gets
  a `_` in front. A relative source name that clashes with an absolute one gets a `-1` suffix. Names
  never depend on which other snapshots exist.
- A second snapshot in the same second gets a `-1`, `-2` suffix.
- `ids/` uses the short id, the full id when two snapshots share the prefix.

## Behaviour

- The mount holds a shared lock for as long as it runs, so `ark prune` and `ark forget --prune`
  exit 3 (after `--lock-wait`, if set) until it is unmounted.
- Ctrl-C unmounts. While a process still uses the mount it keeps serving and retries every second;
  a second Ctrl-C or SIGTERM quits anyway, with exit 1, and leaves the mountpoint to unmount by hand.

## Platforms

Linux needs FUSE 3: `/dev/fuse` and `fusermount3` (package `fuse3`). macOS needs
[macFUSE](https://macfuse.github.io/) and is best effort. Windows and 32-bit Linux are not
supported.
