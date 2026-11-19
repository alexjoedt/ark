# 0029: Volume profiles without the Docker SDK

Status: accepted
Date: 2026-11-19

## Context

A profile backed up one directory (0018). It should also back up a docker volume, with
`backup.volume` beside `backup.path` and a `docker-host` key, so `ark docker backup -P`, `ark
forget -P` and `ark snapshots -P` work from one file. arkprofile must not import the Docker SDK:
an embedder that only reads profiles would pull it in, and `arkdocker` is a module of its own.

`Profile.Filter` selects a profile's snapshots for forget. A volume backup records the host of
the daemon's machine, which for a remote daemon is the name the daemon reports. Asking the daemon
needs the SDK, and a forget should work without the daemon running. A wrong host makes forget
act on another machine's series; a filter without the source type mixes a directory and a volume
backed up under the same name.

## Decision

`Profile` gets `DockerHost` (key `docker-host`, top level, not path-resolved) and `Backup` gets
`Volume` (key `backup.volume`). `backup.volume` excludes `backup.path`, `backup.exclude`,
`backup.exclude-file`, `backup.exclude-cloud-files` and `backup.source`: `Decode` refuses the pair
with both keys and their lines, `Validate` refuses it for a profile built in code. The volume
names the series.

`Filter` keeps its signature and sets `Type`: `docker-volume` for a volume profile, `file` for a
path profile. Its `Host` is `backup.host`, else the machine's hostname, as before. `Validate`
refuses a remote `docker-host` (anything but `unix://` and `npipe://`) without `backup.host`, so
`Filter` never returns the local hostname for a series the daemon's machine names.

`Source` stays arkfs only and fails on a volume profile; the caller builds the volume source
with arkdocker from `backup.volume` and `docker-host`. arkprofile spells the source type
`docker-volume` itself.

- Not a host parameter on `Filter`: every caller would need the daemon, forget included.
- Not leaving `Host` empty for a volume profile: forget would cover every machine's series of the
  volume.
- Not accessors only, without `Filter`: each caller would rebuild the selection and could forget
  the type.

## Consequences

arkprofile still imports no Docker SDK; `TestNoDockerSDK` runs `go list -deps` on it.
`TestVolumeProfileFilterIsOfTheVolumeType` in cmd/ark pins the type string to
`arkdocker.SourceTypeDockerVolume`, and `TestRemoteDaemon` pins the remote rule of `Validate` to
the CLI's. A path profile's filter now selects file snapshots only, so a forget through it leaves
an imported stream of the same name alone. A volume profile on a remote daemon names its machine
by hand in `backup.host`.
