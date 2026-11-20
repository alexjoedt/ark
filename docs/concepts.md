# How ark works

This page explains the ideas behind the commands: what a repository holds, why a second backup is
fast and small, and what happens when you remove snapshots. You don't need it to make a backup, but
it helps when you plan one.

## Repository

A repository is a directory, on a local disk or on a server reached over SFTP. Everything ark
knows about your backups lives there. No database or state on the machine that runs the backup
is involved, so any machine with the password can read the repository.

Several machines can back up into the same repository. Data that two of them have in common is
stored once.

## Snapshot

A snapshot records one source at one point in time: the directory tree with every file's content
and metadata, plus the source name, the host, the time, and any tags and labels you set.

Every snapshot is complete. Restoring one never needs another snapshot, and removing one never
damages another. What snapshots share is storage, not structure.

ark backs up three kinds of sources:

| Type | Source | Command |
|---|---|---|
| `file` | a directory | `ark backup` |
| `docker-volume` | a Docker volume | `ark docker backup` |
| `stream` | one stream of bytes, such as a database dump | library only |

## Chunks, packs, and the index

ark doesn't store files. It cuts each file into chunks and stores every distinct chunk once:

```
file ──► chunks ──► compress ──► encrypt ──► packs ──► repository
          │
          └─ a chunk the repository already holds is not stored again
```

- **Chunks.** A chunk is a piece of a file, 1 MiB on average. The cut points follow the content,
  not the position in the file. If you insert a few bytes into a large file, only the chunk
  around the insert changes.
- **Deduplication.** A chunk is named by a keyed hash of its content. Two files with the same
  content, in one snapshot or in different ones, on one machine or on several, share their chunks.
- **Packs.** Storing millions of small objects is slow on most storage, so ark gathers chunks into
  packs of about 16 MiB.
- **Index.** The index records which pack holds which chunk. If it's ever lost,
  `ark repair index` rebuilds it from the packs.

Chunks are compressed with zstd and then encrypted. [Repository format](format.md) has the byte
layout.

## Parents and unchanged files

Deduplication saves storage, but hashing every file again on each backup would still take time. So
a backup looks for a parent: the newest earlier snapshot of the same source from the same host.

For every file, ark compares what the file system reports with what the parent recorded: the size
and the modification time, and where the file system has them, the change time and the inode. A
file that matches isn't read at all. The new snapshot points at the chunks the parent already
listed for it.

This is why the first backup of a directory takes the longest, and the following ones take
roughly as long as it takes to walk the directory.

A source and host together form a [series](series.md). The series decides which snapshot becomes
the parent and how retention policies count.

## Forget and prune

Removing old backups takes two steps, and they do different things:

- **`ark forget`** removes snapshots. It deletes only the small snapshot record, so it's fast. The
  chunks stay in the repository.
- **`ark prune`** frees the space. It finds the chunks no snapshot uses anymore, deletes the packs
  that hold only such chunks, and rewrites packs that hold a mix.

`ark forget --prune` runs both. Prune stops rewriting once at most 5% of the repository is unused,
so a little unused space after a prune is normal.

You choose what to keep with a [retention policy](retention.md), such as "the newest snapshot of
each of the last 7 days."

## Locks

ark coordinates processes that use a repository at the same time with lock objects in the
repository itself. There are two kinds:

| Lock | Taken by | Runs alongside |
|---|---|---|
| shared | `backup`, `restore`, `forget`, `check`, `verify`, `ls`, `find`, `export`, `import`, `mount` | other shared locks |
| exclusive | `prune`, `repair index`, `key passwd`, `info --exact` | nothing |

So several backups can run at once, also from different machines, but a prune waits for all of
them or fails. A command that can't get its lock exits with code 3. `--lock-wait DURATION` makes it
wait instead.

A running command refreshes its lock every few minutes. A lock left behind by a crashed process
doesn't block the repository for long: ark ignores a lock that hasn't been refreshed for 30
minutes, and one from the same machine as soon as its process is gone.

## Interrupted backups

A backup writes its data first and the snapshot last. If it's interrupted, no snapshot appears, and
every existing snapshot is untouched.

The work isn't lost either. A long backup saves its progress about every 10 minutes. When you run
it again, it reads the files again but uploads only what came after the last save. Whatever the
interrupted run left unreferenced is removed by the next prune.

## Encryption

File contents, file names, and snapshot metadata are encrypted and authenticated before they leave
your machine. The storage sees none of them, and an object that someone changed fails to open
instead of restoring wrong data.

The password doesn't encrypt the data directly. It unlocks a random master key that is stored in
the repository. That has two consequences:

- You can change the password with `ark key passwd` without rewriting the repository.
- A lost password is lost data. There is no recovery and no backdoor.

[Security](SECURITY.md) lists what the encryption guarantees, what it doesn't, and what someone
with access to the storage can still see.

## Checking a repository

`ark verify` reads every chunk of one snapshot and checks it. `ark check` does that for every
snapshot and also compares the index with the packs that are stored.

Both read the data they check, so on a remote repository they download it. If a check reports a
problem, [Troubleshooting](troubleshooting.md) tells you what to do.

## Next steps

- [Series](series.md) explains how snapshots group and what `--source` and `--host` do.
- [Retention policies](retention.md) shows what each forget rule keeps.
- [Repository format](format.md) describes the objects in a repository byte by byte.
