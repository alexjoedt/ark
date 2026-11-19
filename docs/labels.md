# Labels

A label is a key and a value stored with a snapshot. Tags are plain strings; a label carries a
value, so `app=db` and `app=web` tell two jobs of the same source apart. Labels are set at backup
and never change afterwards.

## Rules

- A key is an optional prefix and a name: `example.com/app`. The prefix is a lower-case DNS
  subdomain of at most 253 characters, parts of at most 63, followed by a `/`. The name is 1 to 63
  letters, digits, `-`, `_` and `.`, starting and ending with a letter or digit.
- A value is UTF-8 of at most 1 KiB without control characters, and may be empty.
- Keys that start with `ark/` are reserved for ark's own sources. A backup that sets one exits 2.
  The library refuses such a key too, and also a key the source records itself; both errors wrap
  `ark.ErrInvalidLabel`.

## Reserved keys

| Key | Recorded by | Value |
|---|---|---|
| `ark/path` | directory backups | the absolute path that was backed up, also when `--source` names it differently |
| `ark/docker-host` | Docker volume backups | the Docker host the engine talks to (`--docker-host`, a profile's `docker-host` or `DOCKER_HOST`), left out when none is set |
| `ark/container.N.name`, `.image`, `.image_id`, `.state`, `.compose_project`, `.compose_service` | Docker volume backups | the containers that used the volume, N counts from 0 in the order of their names; empty values are left out |

## Set and filter

```bash
ark backup /srv/app --label app=shop --label env=prod
ark docker backup postgres_data --label app=shop
ark snapshots --label app=shop            # every --label must match: AND, exact value
ark find '*.conf' --label env=prod
ark forget --label app=shop --keep-daily 7
ark snapshots --label ark/path=/srv/app   # a filter may name ark/ keys
```

`--label KEY=VALUE` splits at the first `=`, so a value may hold `=` and commas. A key given twice
with two values exits 2. On `backup` and `docker backup` the labels are checked against the rules
above before the repository is opened; a filter only needs the `KEY=VALUE` shape.

A profile sets labels under `backup.label`; a `--label` on the command line adds to them and wins
for a key in both:

```yaml
backup:
  path: /srv/app
  label: {app: shop, env: prod}
```

## Where to read them

`ark snapshots --json` and `ark find --json` print `labels` as an object, never `null`. Text output
does not show labels. In the library, `Snapshot.Labels` holds the source's labels with the caller's
added, `ark.WithLabels` sets them at backup and `SnapshotFilter.Labels` filters. A `Sink` sees them
in `SourceMeta.Labels` on restore.

## Stored form

The manifest keeps the labels under the key `source_meta`, see [format.md](format.md).
