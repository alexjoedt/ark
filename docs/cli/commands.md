# Commands

This page is the reference for every `ark` command: its arguments, its flags, and what it does.
The [global flags](index.md#global-flags) apply to all of them. `ark <command> --help` prints the
same flags in the terminal.

Conventions on this page:

- `<id>` is a snapshot ID or a unique prefix of one.
- Every command needs a repository and a password, see [CLI overview](index.md). The exceptions
  are `ark profile list`, `ark profile show`, `ark version`, and `ark docker volumes`, which
  opens a repository only when one is configured.

| Command | Purpose |
|---|---|
| [`init`](#init) | create an empty repository |
| [`backup`](#backup) | back up a directory |
| [`restore`](#restore) | restore a snapshot into a directory |
| [`snapshots`](#snapshots) | list snapshots |
| [`ls`](#ls) | list the files in a snapshot |
| [`find`](#find) | find files in snapshots by pattern |
| [`mount`](#mount) | serve every snapshot read-only through FUSE |
| [`inspect`](#inspect) | show one snapshot in detail |
| [`info`](#info) | show what a repository holds |
| [`debug node`](#debug-node) | print a stored tree node as JSON |
| [`forget`](#forget) | remove snapshots by ID or policy |
| [`prune`](#prune) | delete packs that no snapshot references |
| [`repair index`](#repair-index) | rebuild the index from the stored packs |
| [`verify`](#verify) | verify one snapshot |
| [`check`](#check) | verify every snapshot |
| [`export`](#export) | write a snapshot as a tar archive |
| [`import`](#import) | read a tar archive written by `export` as a new snapshot |
| [`unlock`](#unlock) | remove stale repository locks |
| [`docker`](#docker) | list, back up, and restore Docker volumes |
| [`key passwd`](#key-passwd) | change the repository password |
| [`profile`](#profile) | list profiles, show their values, and run their backup |
| `version` | print version information |

## init

```
ark init [--chunk-min SIZE] [--chunk-avg SIZE] [--chunk-max SIZE]
```

Writes the repository config and a key file sealed with the password.

- Fails if the repository directory, local or over SFTP, exists and isn't empty.
- The chunk sizes default to 256KiB, 1MiB, and 4MiB. They are stored in the repository and can't
  be changed later.
- `--chunk-avg` must be a power of two, and `--chunk-max` can be at most 64MiB.

## backup

```
ark backup [options] <path>
```

Backs up the directory at `<path>` as one snapshot. Prints the summary on stderr and the full
snapshot ID on stdout.

| Flag | Meaning |
|---|---|
| `--tag TAG`, `-t` | add a tag; repeatable |
| `--comment TEXT`, `-c` | free text stored with the snapshot |
| `--source NAME`, `-s` | the source name; default: the absolute path, or with `-P` the profile's `backup.source` |
| `--host NAME` | the host; default: the machine's hostname |
| `--label KEY=VALUE` | record a label; repeatable; see [Labels](../labels.md) |
| `--parent ID` | use this snapshot as the parent |
| `--time TIME` | the time of the snapshot instead of now, as `"2006-01-02 15:04:05"` in local time |
| `--exclude PATTERN`, `-e` | leave out paths that match PATTERN; repeatable |
| `--exclude-file FILE` | read exclude patterns from FILE; repeatable |
| `--exclude-cloud-files` | leave out files that exist only in the cloud |

The parent is the newest snapshot of the same [series](../series.md): the same source type, source
name, and host. `--parent` overrides it.

### Backdated snapshots

`--time` dates a backup of data as it was at an earlier time.

- `ark snapshots` shows this time, and `ark forget` counts the snapshot in its hour, day, week,
  month, and year.
- The parent is still the newest snapshot of the series by time, so a snapshot can be dated before
  its parent.
- A time in the future is refused. Exit 2.
- A time that the local clocks skip when daylight saving time starts is refused. Exit 2.
- For an hour that the clocks show twice when daylight saving time ends, either of the two is
  taken. Both count in the same hour.

### Excludes

```bash
ark backup ~ -e node_modules -e '*.tmp' -e /Library --exclude-file ~/.config/ark/excludes
```

- A pattern uses `path.Match` syntax per path component (`*`, `?`, `[...]`), plus `**` for any
  number of components. The separator is `/` on every OS.
- A pattern without a leading `/` matches at any depth. `node_modules` leaves out every directory
  of that name.
- A leading `/` anchors the pattern at the backed-up directory, not at the file system root. In the
  example, `/Library` matches only `~/Library`.
- A matched directory is left out with everything below it.
- A comma stays part of the pattern. `-e 'a,b'` is one pattern.
- An exclude file holds one pattern per line. Surrounding whitespace is trimmed. Blank lines and
  lines that start with `#` are skipped.
- A bad pattern exits 2 before the repository is opened.
- The patterns are stored with the snapshot. `ark inspect` shows them.

`--exclude-cloud-files` skips files that exist only in the cloud, so a backup doesn't download a
whole cloud share. That covers iCloud `.name.icloud` placeholders, and on macOS every dataless File
Provider entry.

Differences from restic: no `!` negation, no `$VAR` expansion in exclude files (`$` stays literal),
and `/` anchors at the backed-up directory instead of the file system root.

### Summary

```
  snapshot   4f9c2a1e
  files      548 974  (530 112 unchanged, 3 skipped)
  size       9.2 GiB
  new        6.7 GiB, stored as 3.4 GiB
```

| Line | Meaning |
|---|---|
| `snapshot` | the short ID of the new snapshot |
| `files` | regular files, plus every further name of a hardlinked file; the note appears when files were taken unchanged from the parent or skipped |
| `size` | the bytes of every regular file in the tree, a file under several names counted once; unchanged files count too |
| `new` | the raw size of the distinct chunks this run wrote |
| `stored as` | what those chunks take in the repository, compressed and encrypted |

- A chunk counts once in `new`, however often it occurs. `new` therefore stays below `size` even
  on an empty repository when several files hold the same content.
- Directory and file metadata count in neither `new` nor `stored as`.

Differences from restic:

- restic's "processed" counts the bytes of a hardlinked file once per name, so on a tree with
  hardlinks, for example a Cargo `target/` directory, it comes out above `size`.
- restic's "Added to the repository" and its stored size include the tree blobs, which are a few
  MiB even for large trees. They don't include pack overhead.
- The chunkers cut at different places (ark: 256 KiB to 4 MiB, restic: 512 KiB to 8 MiB with a
  chunker polynomial picked per repository), so the two find different duplicate chunks. restic's
  "Added" can come out a few percent above or below `new`, and differs between two restic
  repositories of the same tree.

## restore

```
ark restore [options] <id> <dest>
```

Restores a snapshot into `<dest>`. The directory is created if missing. Existing files are
overwritten: an entry already at a restored path is removed and created anew, never written
through, so a symlink there doesn't lead the restore out of `<dest>`. A directory stays where the
snapshot holds a directory.

Owners are restored when the restore may set them, usually as root. An entry other than a
directory whose owner can't be set loses its setuid and setgid bits, as `cp` does, so a restored
program doesn't run as the user who restored it. A directory keeps them.

A device node needs the privilege to create one, usually root, and a fifo or device node needs a
filesystem that holds them; vfat, exFAT and SMB shares don't. Otherwise the restore skips each such
entry with a warning on stderr, restores everything else, and exits 0. `--json` lists the skipped
entries' errors under `skipped`.

| Flag | Meaning |
|---|---|
| `--path PREFIX`, `-p` | restore only the entries under PREFIX; repeatable |
| `--dry-run`, `-n` | list what would be restored, write nothing |
| `--cache SIZE` | the chunk cache during the restore; `0` disables it; default 256MiB |

Docker volume snapshots are refused. Use [`ark docker restore`](#docker), or unpack one into a
directory with `ark export <id> | tar x`.

## snapshots

```
ark snapshots [options]
```

Lists snapshots, newest first.

| Flag | Meaning |
|---|---|
| `--source NAME`, `-s` | only this source name |
| `--type TYPE` | only this source type: `file`, `docker-volume`, or `stream` |
| `--tag TAG`, `-t` | only snapshots with this tag; repeatable, all must match |
| `--host NAME` | only this host |
| `--label KEY=VALUE` | only snapshots with this label; repeatable, all must match |
| `--last N` | the newest N of every series |

With `-P`, only the profile's snapshots are listed. See
[Which snapshots a profile selects](profiles.md#which-snapshots-a-profile-selects).

## ls

```
ark ls [--long] <id> [path]
```

Lists the entries of a snapshot, optionally only those below `path`. It streams, so it works on a
snapshot of any size. `--long`, `-l` adds mode, uid, gid, size, and mtime.

Names are raw bytes. On a terminal, `ls` and `find` write every byte that isn't valid UTF-8 and
every byte of a control or format character (such as a bidi override or a zero-width space) or a
line or paragraph separator as `\xNN`, and every backslash as `\\`. In a pipe they write the bytes
as stored. With `--json`, `path` stays and `path_raw` holds the base64 of the bytes, only for a
name that isn't valid UTF-8; `link_dest_raw` does the same for a link target.

## find

```
ark find [options] <pattern>...
```

Lists every entry whose path matches a pattern, grouped by snapshot, newest snapshot first.

- Patterns follow the rules of [`--exclude`](#excludes): without a `/`, a pattern matches a base
  name at any depth; a leading `/` anchors it at the snapshot root; a matched directory lists
  everything below it.
- Several patterns are alternatives.
- The filters are those of [`ark snapshots`](#snapshots), without `--last`.

## mount

```
ark mount <dir>
```

Serves every snapshot read-only through FUSE at `<dir>` until Ctrl-C. The tree has `ids/<id>/`,
`tags/<tag>/<time>/`, and `sources/<type>/<host>/<source>/<time>/` with a `latest` link per series.

The mount holds a shared lock, so `ark prune` and `ark forget` exit 3 until it's unmounted.
Linux needs FUSE 3, macOS needs macFUSE. See [Mount](../mount.md).

## inspect

```
ark inspect <id>
```

Shows one snapshot: ID, source and type, host, time, parent, tags, file count, size with what was
stored new and what was reused, deduplication and compression ratios, duration, comment, and the
exclude patterns. `--json` adds the labels.

## info

```
ark info [--exact]
```

Shows what a repository holds:

- the number of snapshots
- the total size of the files they cover
- the stored size, split into data packs, tree packs, indexes, snapshots, keys, and other objects
  (config, locks, the lost-pack marker)
- the ratio of total size to stored size

The total size is summed over the snapshots and not deduplicated. A file in ten snapshots counts
ten times, so the ratio grows with every unchanged backup. A ratio below 0.05 prints as `<0.1x`.

Without `--exact`, the command reads the snapshot list and the object sizes that the backend
lists. It reads no data and takes no lock.

`--exact` adds the bytes that no snapshot uses and how many of them a prune frees. It runs a prune
dry run: it reads every tree and index and takes the exclusive lock, so it waits for a running
backup or fails on it, as prune does.

`--json` prints one object:

| Field | Meaning |
|---|---|
| `snapshots` | the number of snapshots |
| `total_size` | the summed size of the files |
| `stored_size` | the size of the repository |
| `stored` | bytes per kind: `datapacks`, `treepacks`, `indexes`, `snapshots`, `keys`, `other` |
| `bytes_freed` | with `--exact`: what a prune deletes |
| `bytes_unused` | with `--exact`: unused bytes that a prune leaves in place |

## forget

```
ark forget [options] [<id>...]
```

Removes snapshots: either the named IDs, or those that a policy doesn't keep. The chunks stay in
the repository until a prune.

[Retention policies](../retention.md) explains what each rule keeps, with timelines. This section
only lists the flags and how the command behaves.

| Flag | Value | Keeps |
|---|---|---|
| `--keep-last` | N | the N newest snapshots |
| `--keep-hourly`, `--keep-daily`, `--keep-weekly`, `--keep-monthly`, `--keep-yearly` | N | the newest snapshot of each of the N most recent hours, days, weeks, months, or years that have one |
| `--keep-within` | DURATION | every snapshot younger than DURATION |
| `--keep-within-hourly`, `--keep-within-daily`, `--keep-within-weekly`, `--keep-within-monthly`, `--keep-within-yearly` | DURATION | the newest snapshot of every hour, day, week, month, or year younger than DURATION |
| `--keep-tag` | TAG[,TAG...] | every snapshot that carries all tags of the list; repeatable |

| Flag | Meaning |
|---|---|
| `--all` | remove every matching snapshot |
| `--source`, `--type`, `--tag`, `--host`, `--label` | filters, as for [`ark snapshots`](#snapshots) |
| `--prune` | run prune afterward |
| `--dry-run`, `-n` | show the keep and remove lists, delete nothing |
| `--yes`, `-y` | skip the confirmation prompt |

```bash
ark forget --keep-last 3 --source /etc --prune
ark forget --keep-daily 7 --keep-weekly 4 --keep-monthly 6
ark forget --keep-within 30d
ark forget --keep-within-daily 14d --keep-within-monthly 1y
ark forget --keep-daily 7 --keep-yearly unlimited
ark forget --keep-last 5 --keep-tag pinned
```

### Values

- N is a whole number or the word `unlimited`. A negative number exits 2.
- DURATION is whole numbers followed by `y`, `m`, `d`, or `h`, each unit at most once and in that
  order: `30d`, `72h`, `1y6m`. `m` means months and goes up to 11. See
  [Durations](../retention.md#durations).
- A `--keep-tag` value is a list of tags separated by commas. `''` stands for snapshots without a
  tag. A list with a stray comma, such as `pinned,`, exits 2.

### Behavior

- **Scope.** A policy applies per [series](../series.md), among the snapshots that the filters
  match. Without a filter, `ark forget --keep-last 1` keeps the newest snapshot of every series.
- **Confirmation.** On a terminal, `forget` lists the snapshots it will remove and asks once. With
  `--prune`, that question covers the prune too. With `--yes`, it asks nothing and lists each
  snapshot after removing it.
- **No rule.** A policy without a keep rule and without IDs exits 2. `--all` removes every
  matching snapshot on purpose.
- **Nothing kept.** When the policy keeps no snapshot of one series, `forget` removes nothing, of
  no series, and exits 1 naming that series. `--dry-run` fails the same way. Only `--keep-tag` can
  cause this; every other rule keeps at least the newest snapshot of a series.
- **Future snapshots.** A snapshot dated after the time of the run is kept, and `forget` warns
  about it on stderr. See
  [Snapshots dated in the future](../retention.md#snapshots-dated-in-the-future).
- **Time zone.** Hours, days, weeks, months, and years are counted in the local time zone of the
  machine that runs `forget`. Set `TZ` to count in another zone.
- **Reasons.** `--json` lists, for every kept snapshot, the reasons it was kept, such as
  `daily snapshot` or `within 30d`.
- **Locking.** `forget` takes the exclusive lock, so it exits 3 while a backup or another forget
  runs. Give a scheduled `forget` that may overlap a backup `--lock-wait`. `--dry-run` takes the
  shared lock and runs beside backups. See
  [Concurrent runs](../retention.md#concurrent-runs).

### With a profile

With `-P`, the profile's `forget` section is the policy, and the profile selects the series. See
[Profiles](profiles.md).

- A flag replaces the profile's value of the same name. `--keep-tag` replaces all of the
  profile's `keep-tag` lists.
- `0` switches off a counted rule that the profile sets.
- An empty value, as in `--keep-within-daily ''`, switches off a duration rule that the profile
  sets.
- Snapshot IDs given on the command line are removed as named, without the profile's policy or
  filter.

## prune

```
ark prune [--dry-run] [--yes]
```

Deletes the packs that hold no chunk or tree a snapshot references. Packs that hold both used and
unused data are rewritten until at most 5% of the repository is unused.

- Takes the exclusive lock.
- `--dry-run`, `-n` shows what would be freed.
- `--yes`, `-y` skips the confirmation prompt.

## repair index

```
ark repair index
```

Reads every stored pack, writes a new index from what the packs say about themselves, and deletes
the old index objects.

- Run it when `check` or `prune` reports a lost or damaged index object, or a pack that no index
  lists and a snapshot needs.
- A blob whose pack is lost isn't brought back. `check` names the snapshots that miss it.
- A pack whose own table doesn't open is listed, and the command exits 5. The new index keeps for
  that pack what the old one listed.
- Takes the exclusive lock, so no backup, restore, or prune can run meanwhile.

## debug node

```
ark debug node <id> [path]
```

Prints the stored tree node of a snapshot as JSON: the root directory, or the directory at `path`.
Every entry shows its name, type, mode (octal), uid, gid, times in nanoseconds since the epoch,
inode, and the fields of its type: chunks, the tree of a directory, the target of a link, the
device numbers, xattrs. A name, link target, or xattr name that isn't valid UTF-8 also appears as
base64 in a `name_raw`, `target_raw`, or xattr `name_raw` field. `path` takes the stored bytes as
they are, so a directory whose name isn't valid UTF-8 opens too.

- Exits 1 when `path` isn't a directory of the snapshot.
- The output is for people, and may change in any release.

## verify

```
ark verify <id>
```

Reads every chunk of one snapshot and checks its hash. Exits 5 when it finds a problem.

## check

```
ark check
```

Verifies every snapshot and reports missing or corrupt objects. Exits 5 when it finds a problem.

Two findings are warnings and don't change the exit code: a pack that no index lists, which an
unfinished backup leaves behind, and an index object that doesn't open. Prune removes both.

## export

```
ark export <id> [file]
```

Writes a snapshot as an uncompressed tar stream to `file` or stdout: `metadata.json` first, then
`data/<path>`. Pipe it through `gzip` to compress it.

## import

```
ark import <file|->
```

Reads a tar archive written by `ark export` from `file` or stdin and stores it as a new snapshot.

## unlock

```
ark unlock [--force]
```

Removes stale locks. `--force` removes every lock, stale or not. Make sure that no ark process is
running before you use it.

## docker

```
ark docker [--docker-host ADDR] volumes [--host NAME]
ark docker [--docker-host ADDR] backup [options] <volume>
ark docker [--docker-host ADDR] restore [options] <id> [volume]
```

Lists, backs up, and restores Docker volumes. [Docker volumes](../docker.md) is the guide.

The daemon comes from `--docker-host`, else a profile's `docker-host`, else `DOCKER_HOST`.

### docker volumes

Lists the volumes with driver and mount point. When a repository is configured, it adds the last
snapshot of each volume. `--host NAME` names the host whose snapshots count.

### docker backup

Stops the running containers that use the volume, backs it up, and starts them again.

| Flag | Meaning |
|---|---|
| `--tag`, `--comment`, `--label`, `--host`, `--parent`, `--time` | as for [`ark backup`](#backup) |
| `--no-stop` | don't stop the containers |
| `--stop-timeout DURATION` | how long a container gets to stop; default 30s |

The host defaults to this machine's hostname, or to the daemon's own name for a remote
`--docker-host`.

### docker restore

Restores a snapshot into `volume`. The default is the volume the snapshot came from. A missing
volume is created. The contents of an existing one are replaced.

| Flag | Meaning |
|---|---|
| `--path PREFIX`, `-p` | restore only the entries under PREFIX; repeatable |
| `--dry-run`, `-n` | show the affected containers and whether data is replaced |
| `--yes`, `-y` | skip the confirmation prompt |
| `--no-stop` | don't stop the containers |
| `--stop-timeout DURATION` | how long a container gets to stop; default 30s |

Refuses a snapshot whose source type isn't `docker-volume`.

### With a profile

For a profile with `backup.volume`:

- `ark docker backup -P NAME` takes no volume argument. It backs up `backup.volume` on the
  profile's `docker-host` with the profile's tags, labels, comment, `backup.host`,
  `backup.no-stop`, and `backup.stop-timeout`, and runs the profile's backup hooks. A flag beats
  the profile's value.
- A volume argument other than `backup.volume` exits 2.
- `ark docker restore -P NAME <id>` restores into `backup.volume` unless a volume is given.
  Without one, it refuses a snapshot of another volume. `<id>` stays required.

See [Volume profiles](profiles.md#volume-profiles).

## key passwd

```
ark key passwd [--new-password-file FILE | --new-insecure-no-password]
```

Opens the repository with the current password, then stores the master key under the new one.

- The new password comes from `--new-password-file`, then `ARK_NEW_PASSWORD`, then a prompt that
  asks twice.
- `--new-insecure-no-password` removes the password.
- The master key stays the same. Whoever could read it before still can.

## profile

```
ark profile list
ark profile show NAME | -f FILE
ark profile backup NAME | -f FILE
```

| Command | Does |
|---|---|
| `list` | prints the name, repository, and backup path or `volume NAME` of every profile in the profile directory; `--json` gives `path` or `volume` |
| `show` | prints every value a command would use with the profile, and where each comes from |
| `backup` | backs up what the profile names, with the profile's values alone; takes no backup flags |

`ark profile backup` runs a `backup.path` as `ark backup -P NAME` does, and a `backup.volume` as
`ark docker backup -P NAME` does. See [Profiles](profiles.md).
