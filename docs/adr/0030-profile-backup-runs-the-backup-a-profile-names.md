# 0030: ark profile backup runs the backup a profile names

Status: accepted
Date: 2026-11-20

## Context

A profile backs up a directory (`backup.path`, 0018) or a docker volume (`backup.volume`, 0029).
`-P` fills the flags of an existing command, so a directory profile runs through `ark backup -P`
and a volume profile through `ark docker backup -P`, and each command refuses the other kind. A
script that backs up every profile has to look at each file to pick the command. A volume profile
also could not say how its containers stop: `--no-stop` and `--stop-timeout` existed only as flags,
so an unattended backup of a database that must keep running carried them in the cron line.

Letting `ark backup -P` run a volume profile was tried and dropped: `ark backup` would stop
containers, its help would describe docker volumes, and its exclude and source flags would need a
refusal for one kind of profile.

## Decision

`ark profile backup NAME` (or `-f FILE`, or `-P NAME`) backs up what the profile names with the
profile's values alone: a `backup.path` as `ark backup -P NAME` does, a `backup.volume` as
`ark docker backup -P NAME` does, the backup hooks around either. It takes no backup flags; a
one-off override goes to `ark backup -P` or `ark docker backup -P`, which keep working as before.
`ark backup -P` on a volume profile and `ark docker backup -P` on a path profile stay usage errors
and name `ark profile backup`. A profile naming neither exits 2.

`Backup` gets `NoStop` (`backup.no-stop`) and `StopTimeout` (`backup.stop-timeout`), the flags of
`ark docker backup`, with a flag over the profile. Both conflict with `backup.path` and
`backup.source` through the `conflicts` tag, so a directory profile cannot carry them; a profile
naming neither may set them for a volume given on the command line. `ark docker restore -P` does
not read them: a restore replaces the volume's data and stops the containers unless `--no-stop` is
given.

`ark profile list` shows `volume NAME` in its `BACKUP` column and `volume` beside `path` in JSON,
so a script can tell the kinds apart without reading the files.

- Not `ark backup -P` for both kinds, for the reasons above.
- Not a `run` or `exec` command chaining backup, forget and prune (0018): `ark profile backup`
  runs the backup only, cron still chains `ark forget -P`, whose prompts and exit codes stay.
- Not stop keys on a directory profile, ignored: a profile that sets a key that does nothing
  misleads its reader.

## Consequences

The CLI has one command per profile for scripts and schedulers, and `ark backup` stays about
directories. `TestProfileBackupCommand` and `TestDockerProfileWithoutDaemon` in cmd/ark pin the
dispatch, the refusals and the hooks, `TestStopSettings` the order of flag, profile and default.
The library keeps no orchestration method: `ark profile backup` lives in cmd/ark, an embedder
dispatches on `Backup.Volume` itself.
