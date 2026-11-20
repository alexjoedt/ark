# Labels

A label is a key and a value that you store with a snapshot, such as `app=shop` or `env=prod`.
Labels let you tell snapshots apart and select them later.

Tags do something similar, but a tag is a single word. Use a tag for a simple mark such as
`daily`. Use labels when the mark has a value, such as which app or which environment a snapshot
belongs to.

You set labels when you make the backup. They don't change afterward.

## Set labels

Add `--label KEY=VALUE` to a backup, once per label:

```bash
ark backup /srv/app --label app=shop --label env=prod
ark docker backup postgres_data --label app=shop
```

In a [profile](cli/profiles.md), write them as a map:

```yaml
backup:
  path: /srv/app
  label: {app: shop, env: prod}
```

A `--label` on the command line adds to the profile's labels. For a key in both, the command line
wins.

## Select snapshots by label

`ark snapshots`, `ark find`, and `ark forget` take `--label` as a filter:

```bash
ark snapshots --label app=shop
ark find '*.conf' --label env=prod
ark forget --label app=shop --keep-daily 7
```

A snapshot matches when it carries the label with exactly that value. If you give several labels,
all of them have to match.

## Read labels

The table that `ark snapshots` prints doesn't show labels. Use JSON output:

```bash
ark snapshots --json
ark find '*.conf' --json
ark inspect --json <id>
```

Each snapshot has a `labels` object. It's empty when the snapshot has no labels, never `null`.

## Labels that ark sets

ark records a few labels on its own. Their keys start with `ark/`, and you can filter on them like
on any other label:

```bash
ark snapshots --label ark/path=/srv/app
```

| Key | Set on | Value |
|---|---|---|
| `ark/path` | directory backups | the absolute path that was backed up, also when `--source` gives the snapshot another name |
| `ark/docker-host` | Docker volume backups | the address of the Docker daemon; left out when none is set |
| `ark/container.N.name` | Docker volume backups | the name of a container that used the volume; N counts from 0 in the order of the names |
| `ark/container.N.image`, `.image_id`, `.state`, `.compose_project`, `.compose_service` | Docker volume backups | the same container's image, image ID, state, and Compose project and service; empty values are left out |

You can't set a key that starts with `ark/` yourself. A backup that tries exits 2.

## Reference

### Keys and values

| Part | Rule |
|---|---|
| key | an optional prefix and a name, as in `example.com/app` |
| prefix | a lowercase DNS subdomain of at most 253 characters, each part at most 63, followed by `/` |
| name | 1 to 63 letters, digits, `-`, `_`, and `.`; starts and ends with a letter or digit |
| value | UTF-8, at most 1 KiB, no control characters; may be empty |

### The flag

- `--label KEY=VALUE` splits at the first `=`, so a value may contain `=` and commas.
- The same key with two different values exits 2.
- `ark backup` and `ark docker backup` check labels against the rules above before they open the
  repository. A filter only needs the `KEY=VALUE` shape.

### The Docker host label

The value of `ark/docker-host` depends on who talks to the daemon:

- The CLI records `--docker-host`, else the profile's `docker-host`, else `DOCKER_HOST`.
- The library records only the address passed to `arkdocker.NewClient`. It doesn't read
  `DOCKER_HOST` for the label.

### In the library

- `ark.WithLabels` sets labels at backup.
- `Snapshot.Labels` holds the labels of the source together with the caller's.
- `SnapshotFilter.Labels` filters.
- A `Sink` sees the labels in `SourceMeta.Labels` on restore.
- A key that starts with `ark/`, or one that the source records itself, is refused with an error
  that wraps `ark.ErrInvalidLabel`.

### Stored form

The snapshot manifest keeps the labels under the key `source_meta`. See
[Repository format](format.md).
