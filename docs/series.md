# Series

A series is the run of snapshots one backup job writes. Four things work on it: the parent of the
next backup, the keep policy of `ark forget`, `ark snapshots --last` and the `latest` links of
`ark mount`.

## The key

A series is the set of snapshots that share all three of:

| Field | Set by | Example |
|---|---|---|
| source type | the source: `file`, `docker-volume`, `stream` | `file` |
| source name | `--source`, else the absolute path of the directory; with `-P` `backup.source`, else the absolute `backup.path`, the same name a plain backup of it stores; the volume name for Docker, with `-P` `backup.volume` | `/home/alex` |
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
  The hours, days, weeks, months and years of a policy count in the local time of the forget run.
- **`--last N`.** `ark snapshots --last N` prints the newest N snapshots of every series.
- **Mount.** `ark mount` gives every series a `latest` link, see [Mount](mount.md).

## The host

The library leaves the host to the embedder. `ark.WithHost` sets it and the default is empty, which
is a series of its own: an embedder that runs where the hostname changes on every run, a pod for
one, would otherwise open a new series each time that no policy ever thins. Set a host when several
machines share a repository.

The `ark` CLI sets the host itself: the machine's hostname for `ark backup`, and for
`ark docker backup` the hostname of a local daemon or the daemon's own name for a remote
`--docker-host`. `--host NAME` overrides it. Profiles use the `backup.host` key.

## Changing the source name

A different `--source`, a changed `backup.source` or a moved `backup.path` starts a new series, with no parent, so its
first backup reads and hashes every file again. `--parent` cannot bridge the rename: a parent of
another source name is refused with a warning and the backup runs without one. Chunks the old series
stored are still not uploaded twice.
