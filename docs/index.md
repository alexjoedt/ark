---
layout: home

landing:
  eyebrow: [Go library, CLI, local and SFTP, directories and Docker volumes]
  title: Every chunk, stored once.
  tagline: ark keeps deduplicated, compressed and encrypted backups of directories and Docker volumes in a local or SFTP repository, as a Go library with a CLI on top. A Go program can plug in its own Source or Sink.
  install: go install github.com/alexjoedt/ark/cmd/ark@latest
  actions:
    - text: Get started
      link: /getting-started
    - text: Repository format
      link: /format
  steps:
    - title: Split
      details: FastCDC cuts content into chunks at boundaries that follow the data, so an insert moves one chunk and not the whole file.
    - title: Store once
      details: A chunk is named by the HMAC-SHA256 of its plaintext. A name the repository already holds is not written again.
    - title: Compress and encrypt
      details: Chunks are compressed with zstd and encrypted before they leave the machine.
    - title: Pack
      details: Blobs are gathered into packs named by their SHA-256, in a local directory or over SFTP.

features:
  - title: Getting started
    details: From an empty directory to a first backup, a restore, and a retention policy.
    link: /getting-started
  - title: How ark works
    details: Snapshots, deduplication, series, and what forget and prune do.
    link: /concepts
  - title: CLI
    details: Every command, the global flags, profiles, and exit codes.
    link: /cli/
  - title: Library
    details: Backends, sources and sinks, forget policies, profiles, and progress hooks for Go programs.
    link: /library
  - title: Docker volumes
    details: Back up and restore volumes on a local or remote daemon.
    link: /docker
  - title: Format and security
    details: The byte layout of a repository and the threat model it is built against.
    link: /format
---

ark keeps deduplicated, compressed and encrypted backups of directories and Docker volumes in a local or SFTP repository, as a Go library with a CLI on top. A Go program can plug in its own Source or Sink.

- [Getting started](getting-started.md): install ark and make a first backup.
- [How ark works](concepts.md): snapshots, deduplication, forget and prune.
- [Series](series.md) and [Labels](labels.md): how snapshots group, and how to tell jobs apart.
- [Retention policies](retention.md): what each forget rule keeps, on timelines.
- [Docker volumes](docker.md): back up and restore volumes.
- [Mount](mount.md): browse every snapshot read-only through FUSE.
- [Troubleshooting](troubleshooting.md): common errors and what to do.
- [CLI](cli/index.md): every command, the global flags, profiles, and exit codes.
- [Library](library.md): ark from Go, with your own backends, sources, and sinks.
- [Repository format](format.md): the byte layout of a repository.
- [Security](SECURITY.md): the threat model.
- [Versioning](versioning.md): what may change between releases.
