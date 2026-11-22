# CLI overview

`ark` is the command line on top of the library. This page covers what every command shares: the
repository, the password, the global flags, the environment, the output, and the exit codes.

- [Commands](commands.md) describes each command.
- [Profiles](profiles.md) describes profile files and scheduling.
- `ark <command> --help` lists the flags of a command.

Install the binary from a release archive or with Go:

```bash
go install github.com/alexjoedt/ark/cmd/ark@latest
```

`ark version` and `ark --version` print the version.

## Repository

A command takes the repository from the first of these that is set. There is no default.

1. `--repo URI`
2. the `repo` key of a profile
3. `ARK_REPOSITORY`

`ark profile list`, `ark profile show`, and `ark version` need neither a repository nor a
password. `ark docker volumes` opens a repository only when one is configured.

### Locations

| Form | Backend |
|---|---|
| `/srv/ark` or `local:///srv/ark` | a local directory |
| `sftp://host/srv/ark` | a directory over SFTP; user and port from `~/.ssh/config` or the defaults |
| `sftp://user@host/srv/ark` | the same as `user` |
| `sftp://user@host:2222/srv/ark` | user and port in the URI |

- The path after the host is absolute on the server. `sftp://host/srv/ark` is `/srv/ark`, not a
  path below the login directory.
- The scp forms `host:path`, `user@host:path`, and `sftp:host:/path` are rejected.
- A colon before the first `/` makes a location remote. A local path that contains a colon needs
  a slash in front of it: `./host:path` or `/srv/a:b`.

### SFTP options

Options that aren't secret go into the URI as query parameters:

| Parameter | Meaning | Default |
|---|---|---|
| `known_hosts=FILE` | the file the host key is checked against | `~/.ssh/known_hosts` |
| `identity=FILE` | the private key file | see below |
| `insecure=1` | skip the host key check | off |

A user, port, or identity that the URI leaves out comes from the host's entry in `~/.ssh/config`,
then from `/etc/ssh/ssh_config`. ark reads `User`, `Port`, `IdentityFile`, and `HostName` there; a
`HostName` replaces the host. Without an entry, the user is the current user and the port is 22.

ark tries these ways to authenticate, in order:

1. the keys of a running ssh-agent (`SSH_AUTH_SOCK`)
2. the identity file, or without one the default keys `id_rsa`, `id_ecdsa`, and `id_ed25519` in
   `~/.ssh`
3. the password in `ARK_SFTP_PASSWORD`

`ARK_SFTP_KEY_PASSPHRASE` unlocks an encrypted key file. Secrets never go into the URI.

## Password

The password comes from the first source that is set:

| Order | Source | Flag, profile key, or variable |
|---|---|---|
| 1 | a file | `--password-file` |
| 1 | the output of a shell command | `--password-command` |
| 2 | the profile | `password-file`, or `password-env` naming a variable |
| 3 | a file or command named in the environment | `ARK_PASSWORD_FILE`, `ARK_PASSWORD_COMMAND` |
| 4 | the environment | `ARK_PASSWORD` |
| 5 | a prompt, on a terminal only | every command asks once, `ark init` twice |

- A file or command output loses one trailing `\n` or `\r\n`.
- A command runs with `sh -c` and shares stdin and stderr. A non-zero exit fails the ark command.
- A file and a command on the same level conflict. Exit 2.
- Without a source and without a terminal, a command exits 2.

There is no `--password` flag, it would show in the process list.

`ark key passwd` changes the password. See [Commands](commands.md#key-passwd).

### Repositories without a password

`--insecure-no-password`, the profile key `insecure-no-password: true`, and
`ARK_INSECURE_NO_PASSWORD=1` stand for a repository that has no password. Anyone with access to
the storage can read such a repository.

This setting isn't a step in the order above. It conflicts with every password source on every
level, and giving both exits 2.

## Global flags

Global flags go before or after the command name. `ark --json snapshots` and `ark snapshots --json`
are the same.

| Flag | Meaning |
|---|---|
| `--repo URI`, `-r` | the repository; env `ARK_REPOSITORY` |
| `--profile NAME`, `-P` | fill the flags from the profile NAME, see [Profiles](profiles.md) |
| `--profile-file FILE`, `-f` | fill the flags from the profile in FILE; with `-P`, exit 2 |
| `--password-file FILE` | read the password from FILE; env `ARK_PASSWORD_FILE` |
| `--password-command COMMAND` | read the password from the output of `sh -c COMMAND`; env `ARK_PASSWORD_COMMAND` |
| `--insecure-no-password` | the repository has no password; env `ARK_INSECURE_NO_PASSWORD` |
| `--json`, `-j` | machine-readable output on stdout, logs on stderr |
| `--verbose`, `-v` | debug logs and progress per file |
| `--quiet`, `-q` | errors only, no progress; with `--verbose`, exit 2 |
| `--env-file FILE` | load environment variables from FILE before the flags are read; a variable that is already set wins |
| `--lock-wait DURATION` | wait up to DURATION for a repository lock instead of failing at once; default 0 |
| `--concurrency N` | the most repository operations at a time; default twice the CPU count |

## Environment variables

| Variable | Meaning |
|---|---|
| `ARK_REPOSITORY` | the repository, as `--repo` |
| `ARK_PASSWORD` | the password itself |
| `ARK_PASSWORD_FILE` | a file that holds the password, as `--password-file` |
| `ARK_PASSWORD_COMMAND` | a shell command that prints the password, as `--password-command` |
| `ARK_INSECURE_NO_PASSWORD` | `1` for a repository without a password |
| `ARK_NEW_PASSWORD` | the new password for `ark key passwd` |
| `ARK_SFTP_PASSWORD` | the SSH password of an `sftp://` repository |
| `ARK_SFTP_KEY_PASSPHRASE` | the passphrase of an encrypted SSH key file |
| `DOCKER_HOST` | the Docker daemon of `ark docker`, as `--docker-host` |
| `XDG_CONFIG_HOME` | the base of the profile directory; else `~/.config` |
| `TZ` | the time zone `ark forget` counts days, weeks, months, and years in |

Profile hooks see `ARK_PROFILE`, `ARK_COMMAND`, and `ARK_ERROR`. See [Hooks](profiles.md#hooks).

## Output

Without `--json`:

- Results go to stdout as tables and text.
- Progress and logs go to stderr.
- On a terminal, long commands draw a live progress block. `--quiet` turns it off.

With `--json`:

- Results go to stdout as JSON, logs to stderr.
- An error is an object on stdout:

  ```json
  {
    "error": {
      "code": "not_found",
      "message": "restore abcd: ark: snapshot not found: abcd"
    }
  }
  ```

- `ark verify`, `ark check`, and `ark repair index` print their report object instead of an error
  object, and still exit 5 when it lists damage.
- `ark profile list` prints its entries, a broken profile with its `error`. When no profile
  loads, it exits 1 without an error object.

### Confirmation

`forget`, `prune`, and `docker restore` ask for confirmation on a terminal. Without a terminal,
and always with `--json`, they exit 2 unless `--yes` is set.

## Exit codes

| Exit code | JSON `code` | Meaning |
|---|---|---|
| 0 | | success |
| 1 | `error` | any other failure |
| 2 | `usage` | usage error, see below |
| 3 | `locked` | the repository is locked by another process |
| 4 | `not_found`, `ambiguous_id` | snapshot not found, or an ID prefix matches several |
| 5 | `verify_failed` | damage, see below |
| 6 | `wrong_password` | wrong password |

Exit 2 covers:

- a bad flag or argument
- a bad profile
- a conflicting password source
- an empty password from a file, command, or prompt
- passwords that don't match at the prompt
- no password without a terminal
- a forget policy without a keep rule
- a missing confirmation

Exit 5 covers:

- `verify` or `check` found damage
- `repair index` found an unreadable pack
- a read hit a corrupt object or config
- a `restore` or `prune` needs a chunk or tree that isn't stored

[Troubleshooting](../troubleshooting.md) lists the common errors by message.
