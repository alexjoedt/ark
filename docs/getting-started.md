# Getting started

This page takes a repository from nothing to a first backup, a restore and a retention policy. Every command is `ark <command>`; `ark <command> --help` lists all of its flags.

## Install

Download the archive for your platform from the [GitHub releases](https://github.com/alexjoedt/ark/releases) (`ark_Linux_x86_64.tar.gz`, `ark_Darwin_arm64.tar.gz` and so on), unpack it and put `ark` on your `PATH`.

With Go 1.27.4 or newer, build it from source instead:

```bash
go install github.com/alexjoedt/ark/cmd/ark@latest
```

`ark --version` prints the version.

## Choose a repository and a password

Every command needs a repository, taken from `--repo`, a profile's `repo` or `ARK_REPOSITORY`, in this order. There is no default path. A repository is a local directory or an SFTP location:

```bash
export ARK_REPOSITORY=/srv/ark                  # or sftp://backup@nas/srv/ark
```

The password comes from the first source that is set, checked in this order:

| Source | Flag or variable |
|---|---|
| a file, one trailing newline stripped | `--password-file` |
| the output of a shell command | `--password-command` |
| the selected profile's `password-file`, then its `password-env` | profile |
| a file, as above | `ARK_PASSWORD_FILE` |
| the output of a shell command | `ARK_PASSWORD_COMMAND` |
| the environment | `ARK_PASSWORD` |
| a prompt, only on a terminal | none; every command asks, `ark init` twice |

A file and a command on the same level conflict and end in a usage error. So does `--insecure-no-password` (or `ARK_INSECURE_NO_PASSWORD=1`, or the profile's `insecure-no-password`) together with any password source above; it stands for no password at all. Without a password source, every command asks on a terminal and fails without one, for example under cron.

There is no `--password` flag, it would show in the process list. Without a password anyone with access to the backend can read the repository; use `--insecure-no-password` only on purpose. For the examples below:

```bash
export ARK_PASSWORD_FILE=/etc/ark/password
```

## Create the repository

```bash
ark init
```

`ark init` writes the repository config and a key file sealed with the password. It fails if the target directory exists and is not empty. The chunker sizes (`--chunk-min`, `--chunk-avg`, `--chunk-max`) are stored in the repository and cannot be changed later; the defaults suit most data.

## Back up a directory

```bash
ark backup /etc --tag daily
```

The snapshot's [source](SERIES.md) is the absolute path of the directory unless `--source NAME` says otherwise. Later backups of the same source and host use the previous snapshot as parent and read only what changed. `--label KEY=VALUE`, repeatable, stores [labels](LABELS.md) that the listing commands filter on. Leave paths out with `--exclude PATTERN` or `--exclude-file FILE`.

The command ends with a summary of the files, their size and what this run added to the repository.

## List snapshots

```bash
ark snapshots
ark snapshots --source /etc --last 3
ark snapshots --label app=my-app
```

`--source`, `--type`, `--tag`, `--host` and `--label` narrow the list, `--last N` keeps the newest N per series. Use `--json` on any command for machine-readable output.

To see what a snapshot holds, `ark ls <id>` lists its files, and `ark find '*.conf'` finds a name across every snapshot, newest first. An id may be abbreviated to a unique prefix.

## Restore

```bash
ark restore <id> /tmp/etc-restore
```

The destination is created if missing and existing files are overwritten. `--path PREFIX` restores only the entries under a prefix, `--dry-run` lists what would be restored without writing anything. To browse snapshots instead of restoring them, see [Mount](MOUNT.md).

## Forget and prune

```bash
ark forget --keep-daily 7 --keep-weekly 4 --prune
```

`forget` removes snapshots by id or by a retention policy (`--keep-last`, `--keep-hourly`, `--keep-daily`, `--keep-weekly`, `--keep-monthly`, `--keep-yearly`, `--keep-within`). A policy applies per [series](SERIES.md), so each source keeps its own recent snapshots. Days, weeks and the other periods count in local time. The chunks stay in the repository until `prune` frees them; `--prune` runs it straight after.

Try `--dry-run` first to see which snapshots would be kept and which removed. Destructive commands ask for confirmation on a terminal and fail without one unless `--yes` is set.

## Check the repository

```bash
ark check
```

`ark check` reports damage in the repository. Run it now and then, especially before relying on old snapshots.

## Where next

- [Series](SERIES.md): how snapshots group into series and what `--source` and `--host` do.
- [Labels](LABELS.md): tell jobs of one source apart and select them.
- [Mount](MOUNT.md): browse every snapshot read-only through FUSE.
- [CLI reference](cli/index.md): every command, profiles, exit codes.
- [Repository format](FORMAT.md) and [Security](SECURITY.md): the on-disk layout and the threat model.
