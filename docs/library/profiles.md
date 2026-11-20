# Profiles

`arkprofile` is the profile format of the CLI as a package. A profile holds one repository, one
directory or Docker volume to back up, and one forget policy. You build it in code or decode it
from YAML, and it gives you the pieces that the `ark` calls need.

The YAML keys are the CLI's flag names. [Profiles](../cli/profiles.md) in the CLI section lists
them.

## Run a profile

```go
p := &arkprofile.Profile{
	Name:        "docs",
	Repo:        "sftp://backup@nas/srv/ark",
	PasswordEnv: "ARK_PASSWORD",
	Backup:      arkprofile.Backup{Path: "/home/me/docs", Tags: []string{"daily"}},
	Forget:      arkprofile.Forget{KeepLast: 3},
}
if err := p.Validate(); err != nil {
	return err
}
pw, err := p.Password()
if err != nil {
	return err
}
repo, err := p.Open(ctx, pw)
if err != nil {
	return err
}
defer repo.Close()

src, err := p.Source()
if err != nil {
	return err
}
if _, err := repo.Backup(ctx, src, p.BackupOptions()...); err != nil {
	return err
}
filter, err := p.Filter()
if err != nil {
	return err
}
if _, err := repo.Forget(ctx, p.ForgetPolicy(), filter); err != nil {
	return err
}
if p.Forget.Prune {
	_, err = repo.Prune(ctx)
}
```

The profile does the translating, and your program makes the calls:

| Method | Returns |
|---|---|
| `Validate` | an error for a profile the CLI would refuse |
| `Password` | the password from the profile's source |
| `Open` | the opened repository |
| `Source` | the `ark.Source` for `Backup.Path` |
| `BackupOptions` | tags, labels, comment, and host as backup options |
| `Filter` | the `ark.SnapshotFilter` that selects the profile's snapshots |
| `ForgetPolicy` | the `ark.ForgetPolicy` of the `Forget` section |

## What the calls leave to you

The methods above don't run a whole job. Two things stay with your program:

- **Prune.** `Forget.Prune` is only a field. Read it and call `repo.Prune`, as in the example.
- **Hooks.** Nothing runs the profile's hooks for you. Wrap each step in `p.Backup.Hooks.Run` or
  `p.Forget.Hooks.Run` with an `arkprofile.HookEnv`, as the CLI does.

A profile with `run-before` or `run-after` hooks otherwise behaves differently in your program
than under `ark`.

## Names and hosts

- `Source` names the snapshots after `Backup.Source`. Without it, the name is the absolute
  `Backup.Path`, the same name `arkfs.NewSource` gives that path.
- `Filter` selects that same source.
- `Name` never names a snapshot.
- Without `Backup.Host`, `BackupOptions` and `Filter` use the machine's hostname. The core library
  alone would record no host.

## Volume profiles

A profile with `Backup.Volume` has no `Source`: the call fails. Back the volume up with an
`arkdocker.Engine` instead:

```go
eng, err := arkdocker.New(repo, arkdocker.WithDockerHost(p.DockerHost))
if err != nil {
	return err
}
defer eng.Close()
_, err = eng.BackupVolume(ctx, p.Backup.Volume, p.BackupOptions()...)
```

Pass `arkdocker.WithDockerHost(p.DockerHost)`. Without it, the engine uses `DOCKER_HOST` or the
local daemon, and backs up another machine's volume under the profile's host.

The snapshot is named after the volume, and `Filter` selects it.

## Load a profile from a file

`arkprofile.Load` reads a profile file as the CLI does.

A program with a configuration of its own, such as a scheduler, can embed a profile in it. Put
`arkprofile.Profile` inline in your struct and decode the whole file with `arkprofile.Decode`:

- It expands `$VAR` and `~`.
- It resolves relative paths against the directory you give it, as `Load` does with the
  directory of the file.

[`examples/profile`](https://github.com/alexjoedt/ark/tree/main/examples/profile) does both.

## Next steps

- [Forget policies](forget.md) explains the policy type that `ForgetPolicy` returns.
- [Profiles](../cli/profiles.md) in the CLI section lists every key and the validation rules.
