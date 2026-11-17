# Commands

Every command takes the [global flags](index.md#global-flags). All but `ark profile` and
`ark version` need a repository and a password as described in [CLI](index.md). A snapshot `<id>` is the full id or a unique prefix of it.
`ark <command> --help` lists every flag.

```bash
export ARK_REPOSITORY=/srv/ark        # or sftp://backup@nas/srv/ark
export ARK_PASSWORD_FILE=/etc/ark/password

ark init
ark backup /etc --tag daily
ark snapshots
ark ls <id>
ark find '*.conf'           # every snapshot and path whose name matches, newest first
ark restore <id> /tmp/etc-restore
ark forget --keep-daily 7 --keep-weekly 4 --prune
ark check
ark repair index            # after check or prune report a lost or damaged index
```

| Command | Purpose |
|---|---|
| [`init`](#init) | create an empty repository |
| [`backup`](#backup) | back up a directory |
| [`restore`](#restore) | restore a snapshot into a directory |
| [`snapshots`](#snapshots) | list snapshots |
| [`ls`](#ls) | list files inside a snapshot |
| [`find`](#find) | find files in snapshots by pattern |
| [`mount`](#mount) | serve every snapshot read-only through FUSE |
| [`inspect`](#inspect) | show one snapshot in detail |
| [`forget`](#forget) | remove snapshots by id or policy |
| [`prune`](#prune) | delete packs no snapshot references |
| [`repair index`](#repair-index) | rebuild the index from the stored packs |
| [`verify`](#verify) | verify one snapshot |
| [`check`](#check) | verify every snapshot |
| [`export`](#export) | write a snapshot as a tar archive |
| [`import`](#import) | read a tar archive written by export as a new snapshot |
| [`unlock`](#unlock) | remove stale repository locks |
| [`docker`](#docker) | list, back up and restore Docker volumes |
| [`key passwd`](#key-passwd) | change the repository password |
| [`profile`](#profile) | list profiles and show their effective values |
| `version` | print version information |

## init

`ark init [--chunk-min SIZE] [--chunk-avg SIZE] [--chunk-max SIZE]`

Writes the repository config and a key file sealed with the password. Fails if the repository
directory, local or over SFTP, exists and is not empty. The chunker sizes (defaults 256KiB, 1MiB,
4MiB) are stored in the repository and cannot be changed later.

## backup

`ark backup [options] <path>`

Backs up the directory at `<path>` as one snapshot.

| Flag | Meaning |
|---|---|
| `--tag TAG`, `-t` | add a tag, repeatable |
| `--comment TEXT`, `-c` | free text stored with the snapshot |
| `--source NAME`, `-s` | the source name, default the absolute path (with `-P` the profile's name) |
| `--host NAME` | the host, default the machine's hostname |
| `--label KEY=VALUE` | record a label, repeatable, the value may hold commas |
| `--parent ID` | an explicit parent snapshot |
| `--exclude PATTERN`, `-e` | leave out paths matching PATTERN, repeatable |
| `--exclude-file FILE` | read exclude patterns from FILE, repeatable |
| `--exclude-cloud-files` | leave out files that exist only in the cloud |

A backup is one series per source type, source and host; its parent is the newest snapshot of that
series unless `--parent` names one. `ark snapshots`, `ark find` and `ark forget` filter on labels,
and all given labels must match. See [Series](../SERIES.md) and [Labels](../LABELS.md).

### Excludes

`--exclude/-e PATTERN` and `--exclude-file FILE` leave paths out, and `--exclude-cloud-files` skips
files that exist only in the cloud (iCloud `.name.icloud` placeholders, and on macOS every dataless
File Provider entry), so a backup does not download a whole cloud share. The patterns are stored with
the snapshot; `ark inspect` shows them.

```bash
ark backup ~ -e node_modules -e '*.tmp' -e /Library --exclude-file ~/.config/ark/excludes --exclude-cloud-files
```

- Patterns use `path.Match` syntax per path component (`*`, `?`, `[...]`) plus `**` for any number of
  components, with `/` as the separator on every OS.
- A pattern without a leading `/` matches at any depth: `node_modules` leaves out every directory of
  that name. A leading `/` anchors it at the backed-up directory, not at the file system root:
  `/Library` matches only `~/Library` in the example.
- A matched directory is left out with everything below it.
- A comma stays part of the pattern: `-e 'a,b'` is one pattern.
- An exclude file holds one pattern per line; surrounding whitespace is trimmed, blank lines and
  lines starting with `#` are skipped.
- Differences from restic: no `!` negation, no `$VAR` expansion in exclude files (`$` stays literal),
  and `/` anchors at the backed-up directory instead of the file system root. A bad pattern exits
  with the usage code 2 before the repository is opened.

### Summary

`ark backup` ends with a summary of what the snapshot holds and what the run added:

```
  snapshot   4f9c2a1e
  files      548 974  (530 112 unchanged, 3 skipped)
  size       9.2 GiB
  new        6.7 GiB, stored as 3.4 GiB
```

- `snapshot` is the short id of the new snapshot.

- `files` counts regular files and every further name of a hardlinked file. The note in parentheses
  appears when files were taken unchanged from the parent snapshot or skipped.
- `size` is the content of the backed-up tree: the bytes of every regular file, a file under several
  names counted once. Files taken unchanged from the parent snapshot count too.
- `new` is the raw size of the distinct data chunks this run wrote to the repository. A chunk counts
  once however often it occurs, so `new` stays below `size` even on an empty repository when the tree
  holds the same content in several files. `stored as` is what those chunks take in the repository,
  compressed and encrypted.
- Directory and file metadata (the snapshot trees) count in neither `new` nor `stored as`.
- Differences from restic: restic counts the bytes of a hardlinked file once per name, and its "Added
  to the repository" includes the tree metadata and pack overhead, so both its numbers come out a
  little higher. The chunk sizes differ as well (ark: 256 KiB to 4 MiB, restic: 512 KiB to 8 MiB),
  which shifts the deduplication slightly.

## restore

`ark restore [options] <id> <dest>`

Restores a snapshot into `<dest>`, created if missing; existing files are overwritten.

| Flag | Meaning |
|---|---|
| `--path PREFIX`, `-p` | restore only entries under PREFIX, repeatable |
| `--dry-run`, `-n` | list what would be restored, write nothing |
| `--cache SIZE` | chunk cache during restore, `0` disables it (default 256MiB) |

Docker volume snapshots are refused here. Use [`ark docker restore`](#docker), or unpack one into a
directory with `ark export <id> | tar x`.

## snapshots

`ark snapshots [options]`

Lists snapshots, filtered by `--source NAME` (`-s`), `--type file|docker-volume|stream`, `--tag TAG`
(`-t`), `--host NAME` and `--label KEY=VALUE`; repeated tags and labels must all match.
`--last N` keeps the newest N of every series. With `-P` only the profile's snapshots are listed.

## ls

`ark ls [--long] <id> [path]`

Lists the entries of a snapshot, optionally below `path`. It streams, so it works on any size.
`--long` (`-l`) adds mode, uid, gid, size and mtime.

## find

`ark find [options] <pattern>...`

Lists every entry whose path matches a pattern, grouped per snapshot, newest first. Patterns follow
`--exclude`: without a `/` a base name anywhere, a leading `/` anchors at the snapshot root, and a
matched directory lists everything below it. The filters are those of `ark snapshots` without
`--last`.

## mount

`ark mount <dir>`

Serves every snapshot read-only through FUSE until Ctrl-C:

```bash
ark mount /mnt/ark                # in another shell:
ls /mnt/ark/ids/                  # one directory per snapshot
```

The tree has `ids/<id>/`, `tags/<tag>/<time>/` and `sources/<type>/<host>/<source>/<time>/` with a
`latest` link per series, for example `sources/files/myhost/home/alex/latest`. The mount holds a
shared lock, so `ark prune` and `ark forget --prune` exit 3 until it is unmounted. Linux needs
FUSE 3, macOS macFUSE. [Mount](../MOUNT.md) has the full layout, the behaviour on Ctrl-C and the
platforms.

## inspect

`ark inspect <id>`

Shows one snapshot: source and type, host, time, parent, tags, file count, size with what was stored
new and reused, deduplication and compression ratios, duration, comment and the exclude patterns it
was taken with. `--json` adds the labels.

## forget

`ark forget [options] [<id>...]`

Removes snapshots, either the named ids or those a policy does not keep. The policy applies per
series among the snapshots the filters match. Chunks stay in the repository until a prune.

| Flag | Meaning |
|---|---|
| `--keep-last N` | keep the N newest |
| `--keep-hourly N`, `--keep-daily N`, `--keep-weekly N`, `--keep-monthly N`, `--keep-yearly N` | keep the newest of N hours, days, weeks, months, years |
| `--keep-within DURATION` | keep everything younger than DURATION, e.g. `72h`, `30d` |
| `--all` | remove every matching snapshot; a policy that keeps nothing is refused without it |
| `--source`, `--type`, `--tag`, `--host`, `--label` | filters, as for `ark snapshots` |
| `--prune` | run prune afterwards |
| `--dry-run`, `-n` | show the keep and remove lists, delete nothing |
| `--yes`, `-y` | skip the confirmation prompt |

```bash
ark forget --keep-last 3 --source /etc --prune
ark forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6
ark forget --keep-within 30d
```

Hours, days, weeks, months and years are those of the local time zone of the machine `ark forget`
runs on, so a backup at 00:30 belongs to the new day; set `TZ` to count in another zone. A week is
an ISO week and starts on Monday. `--keep-last` and `--keep-within` do not depend on the zone.

`ark forget --keep-last 1` without a filter keeps the newest snapshot of every series, see
[Series](../SERIES.md). With `-P` the profile's `forget` section is the policy, see
[Profiles](profiles.md).

## prune

`ark prune [--dry-run] [--yes]`

Deletes the packs that hold no chunk or tree a snapshot references, and rewrites packs that hold both
until at most 5% of the repository is unused. Takes the exclusive lock. `--dry-run` shows what would
be freed.

## repair index

`ark repair index`

Reads every stored pack, writes a new index of what the packs say about themselves and deletes the
old index objects. Run it when check or prune report a lost or damaged index object, or a pack no
index lists that a snapshot needs. A blob whose pack is lost is not brought back; check names the
snapshots that miss it. A pack whose own table does not open is listed and exits 5; the new index
keeps in it what the old one listed. Takes the exclusive lock.

## verify

`ark verify <id>`

Reads every chunk of one snapshot and checks its hash. Exits 5 when it finds problems.

## check

`ark check`

Verifies every snapshot and reports missing or corrupt objects, exit 5 on problems. A pack no index
lists, which an unfinished backup leaves, and an index object that does not open are warnings; prune
removes both. Run it now and then, especially before relying on old snapshots.

## export

`ark export <id> [file]`

Writes a snapshot as an uncompressed tar stream to `file` or stdout: `metadata.json` first, then
`data/<path>`. Pipe it through gzip if wanted.

## import

`ark import <file|->`

Reads a tar archive written by `ark export` from `file` or stdin and stores it as a new snapshot.

## unlock

`ark unlock [--force]`

Removes stale locks. `--force` removes every lock, stale or not; make sure no ark process is running.

## docker

Docker volumes live under `ark docker`. The daemon comes from `--docker-host ADDR` or `DOCKER_HOST`.

| Command | Purpose |
|---|---|
| `ark docker volumes [--host NAME]` | list the volumes with driver, mountpoint and last snapshot |
| `ark docker backup [options] <volume>` | stop the containers using the volume, back it up, restart them |
| `ark docker restore [options] <id> [volume]` | restore into `volume`, default the snapshot's source name; creates it if missing, replaces its contents if present |

`docker backup` takes `--tag`, `--comment`, `--label`, `--host` and `--parent` like `ark backup`, plus
`--no-stop` (leave the containers running) and `--stop-timeout DURATION` (graceful stop, default
30s). `docker restore` takes `--path`, `--dry-run` (show the affected containers and whether data is
replaced), `--yes`, `--no-stop` and `--stop-timeout`, and refuses snapshots whose source type is not
`docker-volume`.

```bash
ark docker volumes
ark docker backup postgres_data --comment "before upgrade"  # --host NAME overrides the host
ark snapshots --type docker-volume    # only volume snapshots, file backups have type file
ark docker restore <id>                # into the volume it came from
ark docker restore <id> postgres_copy  # or into another one, created if missing
```

A volume is one [series](../SERIES.md) per host: the parent of a backup, `ark forget` and the LAST
SNAPSHOT of `ark docker volumes` only see the snapshots of the same host. The host is this machine's
hostname for a local daemon and the daemon's own name for a remote `--docker-host` (`tcp://`,
`ssh://`), unless `--host` sets it. Volume snapshots written before the host was recorded have none:
`ark docker volumes` shows them until the host has its own, and the first backup after the update has
no parent and reads the volume once, unless `--parent <id>` names the last of them.

## key passwd

`ark key passwd [--new-password-file FILE | --new-insecure-no-password]`

Opens the repository with the current password, then stores the master key under the new one. The
new password comes from `--new-password-file`, then `ARK_NEW_PASSWORD`, then a prompt asked twice.
`--new-insecure-no-password` removes the password. The master key stays the same: whoever could read
it before still can.

## profile

`ark profile list` prints name, repository and backup path of every profile in the profile
directory. `ark profile show NAME` (or `ark profile show -f FILE`) prints every value a command would
use with the profile and where it comes from. See [Profiles](profiles.md).
