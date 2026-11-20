# Getting started

This page takes you from an empty directory to a first backup, a restore, and a retention policy.
It takes about ten minutes. You need a directory to back up and a place for the repository: a local
path or a server you reach over SSH.

## Install ark

Download the archive for your platform from the
[GitHub releases](https://github.com/alexjoedt/ark/releases), for example
`ark_Linux_x86_64.tar.gz` or `ark_Darwin_arm64.tar.gz`. Unpack it and put `ark` on your `PATH`.

If you have Go 1.27.4 or newer, you can build it from source instead:

```bash
go install github.com/alexjoedt/ark/cmd/ark@latest
```

Check that it runs:

```bash
ark --version
```

## Choose a repository and a password

A repository is the directory that holds your backups. ark has no default location, so tell it
where the repository is. The examples on this page use an environment variable:

```bash
export ARK_REPOSITORY=/srv/ark                  # or sftp://backup@nas/srv/ark
```

Every repository is encrypted with a password. On a terminal, ark asks for it. That gets tedious
quickly, so put the password into a file that only you can read and point ark at it:

```bash
mkdir -p ~/.config/ark
(umask 077; openssl rand -base64 24 > ~/.config/ark/password)
export ARK_PASSWORD_FILE=~/.config/ark/password
```

Keep a copy of the password somewhere else, for example in a password manager. Without it, nobody
can read the repository, including you. There is no recovery.

[CLI overview](cli/index.md) lists the other ways to name the repository and to supply the
password.

## Create the repository

```bash
ark init
```

```
repository initialized at /srv/ark
```

`ark init` fails if the directory exists and isn't empty, so it never overwrites anything.

## Back up a directory

```bash
ark backup /srv/app --tag daily
```

```
backup /srv/app  done in 290ms
  snapshot   f3261dd9
  files      3
  size       293.0 KiB
  new        293.0 KiB, stored as 293.1 KiB
```

Each backup creates one snapshot: the state of the directory at that moment. `size` is how much
data the snapshot covers, and `new` is how much of it the repository didn't have yet.

Run the same command again after changing a file:

```
backup /srv/app  done in 265ms
  snapshot   15370171
  files      3
  size       293.0 KiB
  new        12 B, stored as 66 B
```

The second snapshot still covers the whole directory, but only the changed data was stored. ark
found the earlier snapshot of `/srv/app` from this machine, used it as the parent, and read only
the files that changed.

Two flags you'll want early:

- `--exclude PATTERN` leaves paths out, for example `--exclude node_modules`.
- `--tag TAG` marks a snapshot so you can find it later.

## Look at your snapshots

```bash
ark snapshots
```

```
ID        TIME                 SOURCE    TYPE  HOST    SIZE       TAGS
15370171  2026-11-20 22:02:15  /srv/app  file  laptop  293.0 KiB  daily
f3261dd9  2026-11-20 22:02:15  /srv/app  file  laptop  293.0 KiB  daily
```

Wherever a command takes a snapshot ID, the short form from this list is enough.

List the files in a snapshot, or search every snapshot for a name:

```bash
ark ls 15370171
ark find '*.conf'
```

```
snapshot 15370171 from 2026-11-20 22:02:15
conf/app.conf
conf/db.conf

snapshot f3261dd9 from 2026-11-20 22:02:15
conf/app.conf
conf/db.conf
```

## Restore

```bash
ark restore 15370171 /tmp/app-restore
```

```
restore 15370171  done in 208ms
  files   3
  size    293.0 KiB
  into    /tmp/app-restore
```

ark creates the destination if it's missing and overwrites files that already exist there. To get
only part of a snapshot back, add `--path`:

```bash
ark restore --path conf 15370171 /tmp/app-restore
```

`--dry-run` lists what a restore would write without writing anything. If you'd rather browse
snapshots like directories, see [Mount](mount.md).

## Remove old snapshots

Snapshots accumulate. `ark forget` removes the ones a retention policy doesn't keep:

```bash
ark forget --keep-daily 7 --keep-weekly 4 --dry-run
```

This policy keeps the newest snapshot of each of the 7 most recent days that have one, and the
newest of each of the 4 most recent weeks. `--dry-run` shows what would be removed. When the list
looks right, run it for real:

```bash
ark forget --keep-daily 7 --keep-weekly 4 --prune
```

Forgetting a snapshot removes it from the list but leaves its data in the repository. `--prune`
frees the space afterward. You can also run `ark prune` on its own.

On a terminal, ark asks before it removes anything. In a script, add `--yes`.

[Retention policies](retention.md) shows with timelines what each rule keeps.

## Check the repository

```bash
ark check
```

```
check  done in 206ms
  snapshots   1
  errors      0
  warnings    0
1 snapshots ok
```

`ark check` reads every snapshot and reports missing or damaged data. Run it now and then, and
before you rely on an old snapshot. If it reports a problem, see
[Troubleshooting](troubleshooting.md).

## Next steps

- [How ark works](concepts.md) explains snapshots, deduplication, and what forget and prune do.
- [Profiles](cli/profiles.md) store the settings of a backup in a file and show how to schedule it.
- [Docker volumes](docker.md) covers backing up and restoring volumes.
- [Series](series.md) and [Labels](labels.md) matter once several machines or jobs share one
  repository.
- [Commands](cli/commands.md) lists every command and flag.
