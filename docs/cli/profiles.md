# Profiles

A profile stores the flags of one backup: repository, password source, directory, tags, excludes,
forget policy and hooks. It is one YAML file, `$XDG_CONFIG_HOME/ark/profiles/NAME.yaml` (else
`~/.config/ark/profiles/NAME.yaml`), loaded with `-P NAME`, or any file with `-f FILE`; both at once
exit 2. The keys are the flag names:

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

```bash
ark backup -P docs              # source ~/Documents as an absolute path, the profile's tags and excludes
ark forget -P docs --yes        # the policy on this host's ~/Documents snapshots, then prune
ark snapshots -P docs           # only the ~/Documents snapshots
ark backup -P docs -t manual    # a flag beats the profile, a list flag replaces its list
ark profile list                # every profile with its repo and backup path
ark profile show docs           # each effective value and its source: cli, profile, env or default
```

A profile can name a Docker volume instead of a directory. `ark docker backup -P db` backs it up
on the profile's `docker-host`, and `forget`, `snapshots` and `find` select that volume's series:

```yaml
# ~/.config/ark/profiles/db.yaml
repo: sftp://backup@nas/srv/ark
password-file: ~/.config/ark/db.pw
docker-host: tcp://db1:2375
backup:
  volume: pgdata
  host: db1
forget:
  keep-daily: 7
```

```bash
ark docker backup -P db         # backup.volume on db1's daemon, filed under the host db1
ark snapshots -P db             # the docker-volume snapshots of pgdata, from every host
ark forget -P db --yes          # the policy on the pgdata series of host db1
```

## Keys

| Section | Keys |
|---|---|
| top level | `repo`, `password-file`, `password-env`, `insecure-no-password`, `lock-wait`, `concurrency`, `docker-host` |
| `backup` | `path`, `source`, `volume`, `host`, `tag`, `comment`, `label`, `exclude`, `exclude-file`, `exclude-cloud-files`, hooks |
| `forget` | `keep-last`, `keep-hourly`, `keep-daily`, `keep-weekly`, `keep-monthly`, `keep-yearly`, `keep-within`, `keep-within-hourly`, `keep-within-daily`, `keep-within-weekly`, `keep-within-monthly`, `keep-within-yearly`, `keep-tag`, `prune`, hooks |

Each key means what the flag of the same name means, see [CLI](index.md#global-flags) and
[Commands](commands.md). `password-env` names the environment variable that holds the password.
`backup.path` is the directory `ark backup -P` backs up when no path is given.
A keep count is a whole number or `unlimited`, as on the command line: `keep-yearly: unlimited`
keeps the newest snapshot of every year. A negative count fails the load.
`keep-within` is a duration as `--keep-within` takes it, `30d`, `72h` or `1y6m`, where `m` counts
months; one that flag would refuse fails the load. `keep-within-hourly` to `keep-within-yearly`
take the same durations. A profile written when `m` counted minutes
still loads with `1m` to `11m` and then keeps months: see
[`ark forget`](commands.md#forget).
`keep-tag` is a list with one entry per tag list, each written as `--keep-tag` takes it:
`keep-tag: [pinned, "db,prod"]` keeps what is tagged `pinned`, and what is tagged `db` and `prod`;
the entry `""` keeps the untagged snapshots. An entry with a stray comma, such as `"pinned,"`, fails
the load.

## Rules

- A flag given on the command line beats the profile, the profile beats the environment, the
  environment beats the default. No password is the exception: `insecure-no-password` on any layer
  together with a password source on any other exits 2, nothing is overridden, see
  [Password](index.md#password).
- The snapshot's source is the absolute `backup.path`, the name `ark backup <path>` stores, unless
  `backup.source` says otherwise; the profile's name never names snapshots. `ark forget -P`,
  `ark snapshots -P` and `ark find -P` select that source's file snapshots, forget also only this
  host's snapshots; `--source` replaces source and type, `--type` and `--host` the one they name. For
  a profile with neither `backup.source` nor `backup.path` `ark forget -P` exits 2, while
  `ark snapshots -P` and `ark find -P` search every snapshot. Two profiles of the same `backup.path`
  without `backup.source` share one series: each one's forget policy removes the other's snapshots
  and each one's backup is the other's parent, so give each its own `backup.source`. A path on the
  command line other than `backup.path` names its own snapshots by its absolute path; with
  `backup.source` set it needs `--source`, so it never lands under the profile's source. Snapshot ids
  given to `ark forget -P` are removed as named, without the profile's policy or source filter. See
  [Series](../series.md).
- A profile with `backup.volume` selects the docker-volume series of that name, never a file series
  of the same name. `ark forget -P` takes the host `ark docker backup -P` files it under:
  `backup.host`, else this machine's hostname, and for a remote daemon in `$DOCKER_HOST` the
  daemon's own name, which `forget` asks the daemon for unless `--host` names it. `backup.volume` excludes `backup.path` and
  `backup.source`; a remote `docker-host` needs `backup.host`.
- `backup.label` sets labels as a map, `label: {app: docs}`; a `--label` flag adds to them and wins
  for a key in both. See [Labels](../labels.md).
- `$VAR` and `${VAR}` expand in every value but the hooks; an unset variable exits 2. `~` and
  relative paths in `repo`, `password-file`, `backup.path` and `backup.exclude-file` are taken from
  the home directory and the profile's directory. A profile never holds a password: a `password:` key
  exits 2.
- An unknown key or section, a bad name (`[a-z0-9][a-z0-9._-]*`), a file writable by group or
  others or a file owned by someone other than you or root exits 2 naming the file and the problem.

`ark profile list` prints name, repository and backup path of every profile in the profile
directory; a file that does not load is listed with its error, and the command fails only when no
profile loads. `ark profile show NAME` (or `-f FILE`) prints every value a command would use and
where it comes from: cli, profile, env or default. The password is never printed, only its source.

## Hooks

`run-before`, `run-after`, `run-after-fail` and `run-finally` in `backup` and `forget` run through
`sh -c` in the profile's directory with `ARK_PROFILE`, `ARK_COMMAND` and, for `run-after-fail`,
`ARK_ERROR` set; their output goes to stderr.

- A failing `run-before` skips the command and runs `run-after-fail`.
- `run-finally` runs in every case.
- A failing `run-after` exits 1 and keeps the snapshot.
- The hooks of `forget` do not run on `--dry-run`.

## Scheduling

Schedule a profile with cron (use the path `go install` or the release archive put ark at):

```text
30 2 * * * /usr/local/bin/ark backup -P docs && /usr/local/bin/ark forget -P docs --yes
```

or a systemd user timer:

```ini
# ~/.config/systemd/user/ark-docs.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/ark backup -P docs
ExecStart=/usr/local/bin/ark forget -P docs --yes

# ~/.config/systemd/user/ark-docs.timer
[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
```

`systemctl --user enable --now ark-docs.timer` starts it. Neither cron nor a timer has a terminal,
so the password must come from a file, a command or the environment, and `forget` and `prune` need
`--yes`.
