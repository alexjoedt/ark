# Profiles

A profile is a YAML file that stores the settings of one backup job: the repository, where the
password comes from, what to back up, and which snapshots to keep. With a profile, a scheduled
backup is one short command, and the same settings apply to `backup`, `forget`, and `snapshots`.

A profile named `docs` lives in `~/.config/ark/profiles/docs.yaml`, or below `$XDG_CONFIG_HOME`
if that is set. Load it with `-P docs`. Load a profile from any other place with `-f FILE`. Giving
both exits 2.

## Write a profile

The keys are the names of the flags they replace:

```yaml
# ~/.config/ark/profiles/docs.yaml
repo: sftp://backup@nas/srv/ark
password-file: ~/.config/ark/docs.pw
backup:
  path: ~/Documents
  tag: [docs, daily]
  exclude: ["*.tmp", .cache]
  run-before: mount /mnt/docs
  run-after-fail: 'notify-send "backup failed: $ARK_ERROR"'
forget:
  keep-daily: 7
  keep-weekly: 4
  prune: true
```

A profile never holds the password itself, only where to find it.

## Use a profile

```bash
ark profile backup docs         # back up ~/Documents with the profile's settings
ark forget -P docs --yes        # apply the forget policy to these snapshots, then prune
ark snapshots -P docs           # list only the snapshots of ~/Documents
```

`ark profile backup` takes no backup flags. To override a setting for one run, use the regular
command with `-P`:

```bash
ark backup -P docs -t manual    # a flag beats the profile; a list flag replaces the profile's list
```

Two commands show what a profile does without running anything:

```bash
ark profile list                # every profile with its repository and its path or volume
ark profile show docs           # every value a command would use, and where it comes from
```

`ark profile show` names the source of each value: `cli`, `profile`, `env`, or `default`. It never
prints the password, only its source.

## Schedule a profile

With cron, use the full path of the binary:

```text
30 2 * * * /usr/local/bin/ark profile backup docs && /usr/local/bin/ark forget -P docs --yes
```

With a systemd user timer:

```ini
# ~/.config/systemd/user/ark-docs.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/ark profile backup docs
ExecStart=/usr/local/bin/ark forget -P docs --yes

# ~/.config/systemd/user/ark-docs.timer
[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
```

`systemctl --user enable --now ark-docs.timer` starts it.

`ark profile backup` handles directory and volume profiles alike, so one loop covers every
profile:

```bash
for p in $(ark profile list --json | jq -r '.[] | select(.error == null) | .name'); do
  ark profile backup "$p" && ark forget -P "$p" --yes
done
```

Neither cron nor a timer has a terminal. The password must come from a file, a command, or the
environment, and `forget` and `prune` need `--yes`.

## Keys

Each key means what the flag of the same name means. See
[Global flags](index.md#global-flags) and [Commands](commands.md).

### Top level

| Key | Value |
|---|---|
| `repo` | the repository, as `--repo` |
| `password-file` | a file that holds the password |
| `password-env` | the name of the environment variable that holds the password |
| `insecure-no-password` | `true` for a repository without a password |
| `lock-wait` | a duration, as `--lock-wait` |
| `concurrency` | a number, as `--concurrency` |
| `docker-host` | the Docker daemon, as `--docker-host` |

### `backup`

| Key | Value |
|---|---|
| `path` | the directory that `ark backup -P` backs up when no path is given |
| `source` | the source name, as `--source` |
| `volume` | a Docker volume to back up instead of a directory |
| `host` | the host, as `--host` |
| `tag` | a list of tags |
| `comment` | text |
| `label` | a map of labels, as in `label: {app: docs}` |
| `exclude` | a list of patterns |
| `exclude-file` | a list of files |
| `exclude-cloud-files` | `true` or `false` |
| `no-stop` | volume profiles: `true` leaves the containers running |
| `stop-timeout` | volume profiles: how long a container gets to stop; default 30s |
| `run-before`, `run-after`, `run-after-fail`, `run-finally` | [hooks](#hooks) |

- A `--label` flag adds to the profile's labels and wins for a key in both. See
  [Labels](../labels.md).
- `ark docker restore -P` doesn't read `no-stop` and `stop-timeout`. A restore stops the
  containers unless `--no-stop` is given.

### `forget`

| Key | Value |
|---|---|
| `keep-last`, `keep-hourly`, `keep-daily`, `keep-weekly`, `keep-monthly`, `keep-yearly` | a whole number or `unlimited` |
| `keep-within`, `keep-within-hourly`, `keep-within-daily`, `keep-within-weekly`, `keep-within-monthly`, `keep-within-yearly` | a duration such as `30d`, `72h`, or `1y6m` |
| `keep-tag` | a list with one entry per tag list |
| `prune` | `true` runs prune after forget |
| `run-before`, `run-after`, `run-after-fail`, `run-finally` | [hooks](#hooks) |

- In a duration, `m` means months. `keep-within: 5m` keeps five months, not five minutes.
- Each `keep-tag` entry is written as `--keep-tag` takes it. `keep-tag: [pinned, "db,prod"]` keeps
  what is tagged `pinned`, and what is tagged both `db` and `prod`. The entry `""` keeps the
  snapshots without a tag.
- A value that the flag would refuse fails the load: a negative count, a bad duration, or a
  `keep-tag` entry with a stray comma, such as `"pinned,"`.

[Retention policies](../retention.md) explains the rules.

## Precedence

A value comes from the first of these that sets it:

1. a flag on the command line
2. the profile
3. the environment
4. the default

A list flag replaces the profile's list; it doesn't add to it. Labels are the exception: they
merge.

A repository without a password is outside this order. `insecure-no-password` on any level
together with a password source on any other level exits 2. Nothing is overridden. See
[Password](index.md#password).

## Which snapshots a profile selects

`ark forget -P`, `ark snapshots -P`, and `ark find -P` work only on the profile's snapshots. The
profile's name plays no part in that: no snapshot is ever named after a profile.

| Profile has | Source name of its snapshots |
|---|---|
| `backup.source` | that name, with the type `file` |
| `backup.path` only | the absolute path, the name a plain `ark backup <path>` stores, with the type `file` |
| `backup.volume` | the volume name, with the type `docker-volume` |

- `forget -P` also narrows to this host's snapshots. `snapshots -P` and `find -P` show every host.
- `--source` replaces the profile's source and type. `--type` and `--host` replace the one they
  name.
- A profile with neither `backup.source`, `backup.path`, nor `backup.volume` selects nothing:
  `ark forget -P` exits 2, and `ark snapshots -P` and `ark find -P` search every snapshot.
- Snapshot IDs given to `ark forget -P` are removed as named, without the profile's policy or
  filter.

See [Series](../series.md) for how source, type, and host form a series.

### Two profiles for one directory

Two profiles with the same `backup.path` and no `backup.source` share one series. Each one's
forget policy removes the other's snapshots, and each one's backup is the other's parent. Give
each profile its own `backup.source`.

### Another path on the command line

`ark backup -P NAME <path>` with a path other than `backup.path` names the snapshot by that path's
absolute form. If the profile sets `backup.source`, the command needs `--source` too, so the
snapshot never lands under the profile's source by accident.

## Volume profiles

A profile can name a Docker volume instead of a directory:

```yaml
# ~/.config/ark/profiles/db.yaml
repo: sftp://backup@nas/srv/ark
password-file: ~/.config/ark/db.pw
docker-host: tcp://db1:2375
backup:
  volume: pgdata
  host: db1
  stop-timeout: 2m              # or no-stop: true, and the containers keep running
forget:
  keep-daily: 7
```

```bash
ark profile backup db              # back up pgdata on db1's daemon, filed under the host db1
ark docker backup -P db --no-stop  # the same, with a flag over the profile
ark snapshots -P db                # the docker-volume snapshots of pgdata, from every host
ark forget -P db --yes             # the policy on the pgdata series of the host db1
ark docker restore -P db <id>      # restore into pgdata
```

- The profile selects the `docker-volume` series of that name, never a file series of the same
  name.
- `ark backup -P` on a volume profile exits 2, and so does `ark docker backup -P` on a directory
  profile. The error names the command to run. `ark profile backup` works for both.
- A profile with neither `backup.path` nor `backup.volume` can't be run with `ark profile backup`.
  Exit 2.

### The host of a volume profile

`ark forget -P` looks at the host that `ark docker backup -P` files the snapshots under:

1. `--host`, if given
2. `backup.host`
3. for a remote daemon named in `DOCKER_HOST`, the daemon's own name, which `forget` asks the
   daemon for
4. this machine's hostname

A profile with neither `backup.path` nor `backup.volume` can still set `backup.host`. It then
applies to `ark docker backup -P NAME <volume>`.

### Keys that exclude each other

The load fails for:

- `backup.volume` next to `backup.path`, `backup.source`, `backup.exclude`, `backup.exclude-file`,
  or `backup.exclude-cloud-files`
- `backup.no-stop` or `backup.stop-timeout` next to `backup.path` or `backup.source`
- a remote `docker-host` without `backup.host`; remote is every address that isn't `unix://` or
  `npipe://`

[Docker volumes](../docker.md) is the guide to volume backups.

## Values

- `$VAR` and `${VAR}` expand in every value except the hooks. An unset variable exits 2.
- In `repo`, `password-file`, `backup.path`, and `backup.exclude-file`, a leading `~` is the home
  directory, and a relative path is relative to the directory of the profile file.

## Validation

A profile that doesn't load exits 2 and names the file and the problem. The load fails for:

- an unknown key or section
- a `password:` key
- a name that doesn't match `[a-z0-9][a-z0-9._-]*`
- a file that is writable by the group or by others
- a file owned by someone other than you or root
- a value that its flag would refuse

`ark profile list` lists a file that doesn't load with its error, and a file that isn't named
`NAME.yaml` as ignored. The command fails only when no profile loads.

## Hooks

A hook is a shell command that runs around `backup` or `forget`. Each section takes four:

| Hook | Runs |
|---|---|
| `run-before` | before the command; if it fails, the command is skipped and `run-after-fail` runs |
| `run-after` | after success; if it fails, the command exits 1 and the snapshot stays |
| `run-after-fail` | after a failure |
| `run-finally` | in every case |

- Hooks run through `sh -c` in the directory of the profile file. Their output goes to stderr.
- They see `ARK_PROFILE` and `ARK_COMMAND`. `run-after-fail` also sees `ARK_ERROR`.
- The hooks of `forget` don't run with `--dry-run`.
