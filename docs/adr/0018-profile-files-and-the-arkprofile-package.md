# 0018: Profile files and the arkprofile package

Status: accepted
Date: 2026-11-15

## Context

Users back up the same directory to the same repository with the same tags, excludes and forget
policy again and again, usually from cron or a systemd timer. Every run repeats a dozen flags and
the password source. A stored configuration removes that, but its file format is written by hand
and hard to change once released, and embedders such as a scheduler want the same configuration
without the CLI. Open were the name of the concept, the file layout, the parser, how strict the
decoding is, where passwords come from, and which part lives in the library.

## Decision

A profile is one stored backup configuration, not automation: one YAML file per profile, named
after the file's base name, which must match `[a-z0-9][a-z0-9._-]*`. The CLI looks for profiles in
`$XDG_CONFIG_HOME/ark/profiles/`. The keys are the CLI flag names: `repo`, `password-file`,
`password-env`, `insecure-no-password`, `lock-wait` and `concurrency` at the top, a `backup`
section with `path`, `tag`, `exclude`, `exclude-file`, `exclude-cloud-files`, `host`, `comment` and
`name`, a `forget` section with the `keep-*` flags and `prune`, and `run-before`, `run-after`,
`run-after-fail` and `run-finally` hooks in each section, each one shell command.

The type, the loader and the checks live in a new root package `arkprofile`, standard library plus
`go.yaml.in/yaml/v3`, so a scheduler embeds `Profile` in its own struct with `yaml:",inline"` and
keeps its own keys beside it. Decoding is strict: an unknown key, a key given twice, a second
document, an empty file and a value of the wrong kind fail with the line and the field path, such
as `line 4: backup.pahts: unknown field`. The YAML library gives the line but not the path, so
`Decode` parses into a node tree and walks it against the struct's fields. A `password:` key fails
by name: a profile holds no password, only `password-file` or `password-env`, and `Password`
never falls back to `ARK_*` variables, so the environment of whoever runs the profile cannot
change which password it uses. `Load` refuses a file writable by group or others or owned by
someone other than the caller and root, since a profile names commands the hooks run; there is
no override. The CLI keeps the precedence of flag, profile, environment and default, and fills
the flags of its existing commands from a profile; the library has no orchestration method.

`$VAR`, `${VAR}` and `$$` expand in every value except the hooks, which the shell expands, and a
variable that is not set is an error. `password-file`, `backup.path` and `backup.exclude-file` resolve
`~` to the home directory and a relative path against the profile's directory (`Decode` takes it as
an argument), so cron runs the same profile from any working directory.

- Not one file holding many jobs, because a file per profile needs no name key, and a cron line
  names one profile anyway.
- Not a `run` command chaining backup, forget and prune, because cron chains the existing commands
  with `&&` and their prompts and exit codes stay as they are.
- Not gopkg.in/yaml.v3, because it is archived, nor goccy/go-yaml, because it adds twice the
  binary size and has a single spare-time maintainer.
- Not the profile in `cmd/ark`, because an embedder could not reuse its decoding or its checks.
- Not a lenient decoder that ignores unknown keys, because a misspelt `exclude` would back up
  what the user meant to leave out without a word.

## Consequences

A profile file is a format to keep: renaming a key breaks users' files, and v0 is the only line
where that is allowed. Tests in `arkprofile/profile_test.go` pin the error texts
(`TestDecodeRejects`), the embedding struct (`TestDecodeIntoAStructEmbeddingAProfile`), the file
checks (`TestLoad`, `TestCheckFileRefusesAForeignOwner`) and the password order
(`TestPassword`). Strictness reaches nested structs only; a map or a list of structs added by an
embedder decodes as YAML has it. The parser adds about 344 KiB to the CLI binary.
