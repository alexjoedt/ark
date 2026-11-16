# CLI

`ark` is the command line on top of the library. This page covers what every command shares: the
repository, the password, the global flags, the environment, the output and the exit codes.
[Commands](commands.md) describes each command, [Profiles](profiles.md) the profile files and
scheduling. `ark <command> --help` lists every flag of a command.

```bash
go install github.com/alexjoedt/ark/cmd/ark@latest
```

A release archive holds the same binary. `ark version` (or `ark --version`) prints the version.

## Repository

Every command needs a repository, from `--repo`, a profile's `repo` or `ARK_REPOSITORY`, in this
order. There is no default path.

| Form | Backend |
|---|---|
| `/srv/ark` or `local:///srv/ark` | a local directory |
| `sftp://user@host:22/srv/ark` | a directory over SFTP |

An SFTP repository takes its non-secret options as query parameters: `known_hosts=FILE` (default
`~/.ssh/known_hosts`), `identity=FILE` and `insecure=1`, which skips the host key check. Secrets never
go into the URI. ark authenticates with the keys of a running ssh-agent (`SSH_AUTH_SOCK`), then the
`identity` file or the default keys in `~/.ssh` (`id_rsa`, `id_ecdsa`, `id_ed25519`), then the
password in `ARK_SFTP_PASSWORD`. An encrypted key file is unlocked with `ARK_SFTP_KEY_PASSPHRASE`.

## Password

The password comes from the first source that is set, in this order:

| Source | Flag, profile key or variable |
|---|---|
| a file, one trailing newline stripped | `--password-file` |
| the output of a shell command, same stripping | `--password-command` |
| the profile | `password-file`, or `password-env` naming the variable |
| a file or command named in the environment | `ARK_PASSWORD_FILE`, `ARK_PASSWORD_COMMAND` |
| the environment | `ARK_PASSWORD` |
| a prompt, only on a terminal | every command asks, `ark init` twice |
| no password at all | `--insecure-no-password`, profile `insecure-no-password: true` or `ARK_INSECURE_NO_PASSWORD=1` |

No password conflicts with any password source: giving both exits 2. So do a file and a command on
the same layer. The command runs with `sh -c`, shares stdin and stderr, and a non-zero exit fails the
command.

There is no `--password` flag, it would show in the process list. `ark key passwd` changes the
password, see [Commands](commands.md#key-passwd).

## Global flags

Global flags go before or after the command name: `ark --json snapshots` and `ark snapshots --json`
are the same.

| Flag | Meaning |
|---|---|
| `--repo URI`, `-r` | the repository, see above; env `ARK_REPOSITORY` |
| `--profile NAME`, `-P` | fill the flags from profile NAME, see [Profiles](profiles.md) |
| `--profile-file FILE`, `-f` | fill the flags from the profile in FILE; both `-P` and `-f` exit 2 |
| `--password-file FILE` | read the password from FILE; env `ARK_PASSWORD_FILE` |
| `--password-command COMMAND` | read the password from the output of `sh -c COMMAND`; env `ARK_PASSWORD_COMMAND` |
| `--insecure-no-password` | the repository has no password; env `ARK_INSECURE_NO_PASSWORD` |
| `--json`, `-j` | machine readable output on stdout, logs on stderr |
| `--verbose`, `-v` | debug logs and per file progress |
| `--quiet`, `-q` | errors only, no progress; with `--verbose` exits 2 |
| `--env-file FILE` | load environment variables from FILE before the flags are read; a variable already set wins |
| `--lock-wait DURATION` | wait up to DURATION for a repository lock instead of failing at once (default 0) |
| `--concurrency N` | max concurrent repository operations (default twice the CPU count) |

## Environment variables

| Variable | Meaning |
|---|---|
| `ARK_REPOSITORY` | the repository, as `--repo` |
| `ARK_PASSWORD` | the password itself |
| `ARK_PASSWORD_FILE` | a file holding the password, as `--password-file` |
| `ARK_PASSWORD_COMMAND` | a shell command printing the password, as `--password-command` |
| `ARK_INSECURE_NO_PASSWORD` | `1` for a repository without a password |
| `ARK_NEW_PASSWORD` | the new password for `ark key passwd` |
| `ARK_SFTP_PASSWORD` | the SSH password of an `sftp://` repository |
| `ARK_SFTP_KEY_PASSPHRASE` | the passphrase of an encrypted SSH key file |
| `DOCKER_HOST` | the Docker daemon of `ark docker`, as `--docker-host` |
| `XDG_CONFIG_HOME` | the base of the profile directory, else `~/.config` |

Profile hooks see `ARK_PROFILE`, `ARK_COMMAND` and `ARK_ERROR`, see [Profiles](profiles.md#hooks).

## Output

Without `--json`, results go to stdout as tables and text, progress and logs to stderr. On a
terminal, long commands draw a live progress block; `--quiet` turns it off and prints errors only.

`--json` prints results as JSON on stdout and logs on stderr. An error is an object on stdout:

```json
{
  "error": {
    "code": "not_found",
    "message": "..."
  }
}
```

`ark verify`, `ark check` and `ark repair index` print their report object instead and still exit 5
when it lists damage.

Destructive commands (`forget`, `prune`, `docker restore`) ask for confirmation on a terminal.
Without one, and always with `--json`, they exit 2 unless `--yes` is set.

## Exit codes

| Exit code | JSON `code` | Meaning |
|---|---|---|
| 0 | | success |
| 1 | `error` | any other failure |
| 2 | `usage` | usage error: a bad flag or argument, a conflicting password source, a forget policy that keeps nothing, a missing confirmation |
| 3 | `locked` | the repository is locked by another process |
| 4 | `not_found`, `ambiguous_id` | snapshot not found, or an id prefix matches several |
| 5 | `verify_failed` | verify or check found damage, `repair index` found an unreadable pack, or a read hit a corrupt object or config |
| 6 | `wrong_password` | wrong password |

## Upgrading from `ark serve`

Builds that had `ark serve` ran a web UI with its own scheduler. It is gone, and a unit still running
`ark serve` fails on every start. Run `systemctl disable --now ark`, remove
`/etc/systemd/system/ark.service`, `/etc/ark/daemon.json` and `/var/lib/ark`, then write each job as
a [profile](profiles.md). The jobs live only in the SQLite file under `/var/lib/ark`; note them down
before deleting it. Repositories are unaffected.
