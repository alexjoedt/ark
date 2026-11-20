# Troubleshooting

This page lists the errors you're most likely to meet, what causes them, and what to do. Find your
case by the message `ark` printed or by its exit code. Every error message starts with `ark:`.

| Exit code | Meaning | Section |
|---|---|---|
| 1 | a failure that has no code of its own | depends on the message |
| 2 | wrong usage, or a missing password or confirmation | [Setup](#setup-errors), [Scripts](#cron-and-scripts) |
| 3 | the repository is locked | [Locked repository](#locked-repository) |
| 4 | snapshot not found | [Snapshot IDs](#snapshot-ids) |
| 5 | missing or damaged data | [Check reports problems](#check-reports-problems) |
| 6 | wrong password | [Passwords](#passwords) |

## Setup errors

These show up on the first commands against a repository.

### `no repository given`

ark has no default repository. Set `--repo`, the `repo` key of a profile, or `ARK_REPOSITORY`.

### `repository is not initialized`

The location exists or is reachable, but it holds no repository. Usually the path is mistyped.
Check these:

- For SFTP, the path is absolute on the server. `sftp://nas/srv/ark` is `/srv/ark`, not a
  directory below the login directory.
- A new repository needs `ark init` once.

### `looks like an scp form` or `not a URI`

ark doesn't accept the scp forms `host:path` and `sftp:host:/path`. Write `sftp://host/path`. A
local path that contains a colon needs a slash in front of it, such as `./backup:old`.

### `repository path ... exists and is not empty`

`ark init` only creates a repository in an empty or missing directory. If a repository is already
there, you don't need to initialize it again.

### `unsupported repository format`

The repository was written by a build with another format version. ark doesn't migrate
repositories. Read it with the build that wrote it, or create a new repository and back up again.

## Passwords

### `wrong password or no key file matches`

The password doesn't open the repository. Check where ark takes it from:

```bash
ark profile show NAME        # with a profile: prints the source of every value
env | grep '^ARK_'           # without one
```

A flag beats a profile, and a profile beats the environment, so an old `ARK_PASSWORD` in your
shell doesn't override a `password-file` in a profile, but it does apply when nothing else is set.

### `conflicts with`

You gave two password sources that exclude each other: a file and a command on the same level, or
`--insecure-no-password` together with any password. Remove one. See
[Password](cli/index.md#password) for the order.

### You lost the password

There is no way to recover it and no way around it. The data is encrypted with a key that only the
password unlocks.

If a machine still has the password configured, in a file or a password manager, use that machine
to set a new one with `ark key passwd`.

## Locked repository

```
ark: ... repository is locked by another operation: exclusive lock held by nas pid 4242 for 3m12s
```

Another ark process is using the repository in a way that conflicts with your command. The message
names the machine, the process, and how long it has held the lock.

- **The other process is still running.** Wait for it, or let your command wait with
  `--lock-wait 10m`.
- **An `ark mount` is open.** A mount blocks `ark prune` until you unmount it.
- **The other process crashed.** You don't have to do anything. ark ignores a lock from the same
  machine as soon as its process is gone, and any lock that hasn't been refreshed for 30 minutes.

`ark unlock` removes stale locks right away. `ark unlock --force` removes every lock, also those of
running processes. Use it only when you're sure that no ark process is running anywhere against
this repository. Removing the lock of a running backup while you prune can cost you that backup's
data.

[Locks](concepts.md#locks) explains which commands conflict.

## Snapshot IDs

### `snapshot not found`

No snapshot has this ID or starts with this prefix. `ark snapshots` lists the IDs. Also check that
you're looking at the right repository.

### `snapshot id prefix is ambiguous`

Several snapshots start with the prefix you gave. Add a few more characters.

## Backups

### A backup was interrupted

Run it again. An interrupted backup leaves no snapshot and doesn't touch the existing ones. The
second run uploads only what the first one didn't save.

The interrupted run can leave packs behind that no snapshot uses. `ark check` reports them as a
warning, and the next `ark prune` removes them.

### A backup reads every file again

A backup skips unchanged files only when it has a parent, the newest snapshot of the same
[series](series.md). It reads everything when:

- the source name changed, for example because the directory moved or `--source` is new
- the host changed, for example a new hostname or a container that gets one per start
- you gave a `--parent` with another source name

Set `--source` and `--host` to fixed names if either of them isn't stable. Reading everything
again costs time but little storage, because the chunks are already in the repository.

### A backup tries to download a cloud drive

Files that exist only in iCloud or another cloud drive are downloaded when ark reads them. Add
`--exclude-cloud-files` to leave them out.

## Forget and prune

### `forget policy keeps nothing and no ids given`

`ark forget` needs to know what to keep. Give at least one keep rule, such as `--keep-last 3`, or
name the snapshot IDs to remove. To remove every matching snapshot on purpose, add `--all`.

### `forget policy keeps no snapshot of a series`

The policy would remove every snapshot of the series the message names, and ark removes nothing
at all in that case. It happens with `--keep-tag` alone, when a series has no snapshot with that
tag. Do one of these:

- Add a rule that always keeps something, such as `--keep-last 1`.
- Narrow the filters to the series that carry the tag.
- Remove the series on purpose with a filter and `--all`.

### `confirmation needed, pass --yes`

`forget`, `prune`, and `docker restore` ask before they change anything. Without a terminal there
is nobody to ask, so they stop. Add `--yes`.

### The repository doesn't shrink after forget

`ark forget` only removes snapshots. `ark prune`, or `ark forget --prune`, frees the space. Even
then, up to 5% of the repository may stay unused, and data that another snapshot still uses stays
as well. `ark info --exact` shows how much a prune would free.

## Check reports problems

`ark check` and `ark verify` exit 5 when they find missing or damaged data. The report says what
is wrong. Fix the errors before you prune.

Other commands exit 5 when they run into the damage. A `restore` or `prune` that needs a chunk or
tree that isn't stored fails with `missing chunk` or `missing tree`. Run `ark check` to see which
snapshots are affected.

### Warnings only

Warnings don't affect any snapshot, and the exit code stays 0.

| Warning | Cause | Fix |
|---|---|---|
| `objects no index lists` | a backup is running, or one didn't finish | none needed; the next `ark prune` removes them |
| an index object that doesn't open | damaged storage | the next `ark prune` rewrites the index |
| a pack `is listed by the index and not stored`, as a warning | the pack was lost, but its data has been stored again | `ark prune` or `ark repair index` drops the entry |

### `missing chunk` and the index is damaged

If check reports missing chunks together with packs that no index lists, the data is there but
the index doesn't know it. Rebuild the index:

```bash
ark repair index
ark check
```

`ark repair index` reads every pack, so on a remote repository it downloads all of them.

### `missing chunk` and a pack is gone

```
datapacks object 7c97... is listed by the index and not stored, chunks objects no stored pack holds are lost: ...
snapshot 693a...: missing chunk 63df...
```

A pack was deleted or lost on the storage. The snapshots that check names can't be restored
completely. What you can do depends on whether the original files still exist:

**If the files still exist,** back them up again:

1. Run `ark check`, if you haven't yet. It records in the repository that a pack is lost. Without
   that record, the next backup trusts its parent and skips the unchanged files.
2. Run the backup. It reads the files whose data is missing and stores that data again.
3. Run `ark check` again. If the files haven't changed since the damaged snapshots were made,
   those snapshots are complete again.
4. Run `ark prune` to drop the index entries of the lost pack.

**If the files are gone,** the damaged snapshots stay incomplete:

1. Restore what is left of them. A plain `ark restore` stops at the first file with a missing
   chunk, so restore the intact directories one by one with `--path`.
2. Remove the damaged snapshots with `ark forget <id>`.
3. Run `ark prune`. Until then, `ark check` keeps reporting the lost pack.

### `corrupt` objects

```
snapshot efdb...: corrupt chunk 6375...
```

A pack is still there, but part of it no longer decrypts. The storage changed it: a failing disk,
a bad transfer, or someone with write access. Check the storage before you trust it with new
backups.

A backup doesn't replace the damaged data on its own, because the index still lists the pack. To
repair the snapshots, turn the damaged pack into a lost one:

1. Find the damaged pack. The `sha256sum` command in
   [Check packs without ark](format.md#check-packs-without-ark) names every pack whose content no
   longer matches its name. It runs on the storage host, without ark and without
   the password.
2. Move that pack out of the repository. Don't delete it yet.
3. Follow the steps for [a pack that is gone](#missing-chunk-and-a-pack-is-gone).

Every snapshot that used a chunk from that pack is affected, not only the one check named first.
Snapshots whose original files no longer exist stay incomplete.

## Cron and scripts

A command that works in your shell can fail under cron or systemd, because there is no terminal.

- **No password prompt.** The password must come from a file, a command, or the environment.
  Otherwise the command exits 2 with `no repository password given`.
- **No confirmation prompt.** `forget`, `prune`, and `docker restore` need `--yes`.
- **Another environment.** Cron doesn't load your shell profile. Variables such as
  `ARK_REPOSITORY` aren't set, and `ark` may not be on the `PATH`. Use a
  [profile](cli/profiles.md) and the full path of the binary.

With `--json`, an error is an object on stdout with a `code` field, which is easier to handle in a
script than the message. [Exit codes](cli/index.md#exit-codes) lists the codes.

## Mount

### The mount fails to start

Mounting needs FUSE: the package `fuse3` on Linux, macFUSE on macOS. See
[Platforms](mount.md#platforms).

### Ctrl-C doesn't unmount

Some process still uses the mount, often a shell whose working directory is inside it. Leave the
directory and ark unmounts within a second.

## Docker volumes

### `ark restore` refuses a snapshot

A snapshot of a Docker volume is restored with `ark docker restore`. To unpack one into a
directory, use `ark export <id> | tar x`.

### A profile is refused by `ark backup -P`

A profile that names a volume runs with `ark profile backup NAME` or `ark docker backup -P NAME`.
The error names the command to use.

## Next steps

- [Exit codes](cli/index.md#exit-codes) has the full table with the JSON codes.
- [Security](SECURITY.md) explains what ark detects when the storage is tampered with.
