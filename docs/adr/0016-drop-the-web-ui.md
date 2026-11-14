# 0016: Drop the web UI

Status: accepted
Date: 2026-11-14

## Context

`ark serve` ran an HTTP server with a React frontend, a job scheduler, a SQLite store and a login.
The UI carried about three quarters of the code of `cmd/ark`: roughly 14,000 lines of TypeScript
against 6,000 lines of Go for the whole CLI, plus 3,800 lines of Go behind the HTTP API. It pulled
in dependencies nothing else used (an HTTP router, sessions, websockets, SQLite and its migrations,
a cron parser), and most dependency update pull requests were frontend ones. The frontend had no
tests and failed its own linter, so it was the part of the repository least able to follow a change
in the library. The builds needed Node besides Go.

ark is a library and a CLI for backing up directories and Docker volumes. A browser UI is not part
of that, and the library's public API is enough for one to be built elsewhere.

## Decision

This repository contains no UI. `ark serve` and everything behind it are removed on the v0 line,
where a command may be dropped without a compatibility step. A UI, if it is wanted again, lives in
its own repository and imports ark through the public API.

- Not a UI kept as an optional build, because it would still need its own tests, dependency
  updates and release steps in every change.
- Not `ark serve` without the frontend, because a scheduler and an HTTP API with a login and a
  database are the larger part of its cost, and their only user was the UI.

## Consequences

The CLI module loses its server, store and frontend dependencies, and `task build` and the release
need Go only. Scheduling is left to cron or systemd timers calling the `ark` CLI; a command that
runs named jobs is planned and replaces what the scheduler did. There is no installer for a
systemd service any more: ark is installed with `go install` or from a release archive.

This supersedes the part of 0006 that names a browser attacker and describes the loopback binding
of `ark serve`. The threat model is otherwise unchanged: the attacker is a passive reader or an
active writer of the backend, or a network attacker between the client and an sftp server.
