# Series

A series is the run of snapshots one backup job writes. Three things work on it: the parent of the
next backup, the keep policy of `ark forget` and `ark snapshots --last`. The reasoning is in
`docs/adr/0021-one-series-key-type-source-host.md`.

## The key

A series is the set of snapshots that share all three of:

| Field | Set by | Example |
|---|---|---|
| source type | the source: `file`, `docker-volume`, `stream` | `file` |
| source name | `--source`, else the absolute path of the directory; the volume name for Docker | `/home/alex` |
| host | `--host`, else the machine's hostname | `laptop` |

The fields are compared verbatim as the manifest stores them. The same path backed up from two
hosts, or the same name as a file source and as a Docker volume, are different series.

## What it drives

- **Parent.** A backup takes the newest snapshot of its series as its parent and reuses the files
  that did not change. A snapshot of another host or source type is never taken on its own.
  `--parent <id>` names one explicitly; its files are used when type and source name match, the host
  is not checked.
- **Forget.** A keep policy (`--keep-daily`, `--keep-last` and the others) applies once per series
  among the snapshots the filters match. `ark forget --keep-last 1` without a filter keeps the
  newest snapshot of every series, so one machine cannot remove another machine's last backup.
- **`--last N`.** `ark snapshots --last N` prints the newest N snapshots of every series.
- **Mount.** `ark mount` gives every series a `latest` link, see [Mount](MOUNT.md).

## The host

The library leaves the host to the embedder. `ark.WithHost` sets it and the default is empty, which
is a series of its own: an embedder that runs where the hostname changes on every run, a pod for
one, would otherwise open a new series each time that no policy ever thins. Set a host when several
machines share a repository.

The `ark` CLI sets the host itself: the machine's hostname for `ark backup`, and for
`ark docker backup` the hostname of a local daemon or the daemon's own name for a remote
`--docker-host`. `--host NAME` overrides it. Profiles use the `backup.host` key.

## Changing the source name

A different `--source` or a profile whose name changed starts a new series, with no parent. Pass
`--parent <id>` for the first backup to reuse the files of the old one.
