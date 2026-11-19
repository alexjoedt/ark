---
layout: home

landing:
  eyebrow: [Go library, CLI, local or SFTP]
  title: Every chunk, stored once.
  tagline: ark keeps deduplicated, compressed and encrypted backups as a Go library, with a CLI on top.
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
  - title: CLI
    details: Every command, the global flags, profiles and exit codes.
    link: /cli/
  - title: Library
    details: Backends, sources and sinks, encryption, progress hooks, profiles and search for Go programs.
    link: /library
  - title: Series and labels
    details: How snapshots group into series, and how labels tell jobs of one source apart.
    link: /series
  - title: Mount
    details: Browse every snapshot read-only through FUSE.
    link: /mount
  - title: Format and security
    details: The byte layout of a repository and the threat model it is built against.
    link: /format
  - title: Decisions
    details: The architecture decision records behind the format, the API and the CLI.
    link: /adr/
---

ark keeps deduplicated, compressed and encrypted backups as a Go library, with a CLI on top.

- [Getting started](getting-started.md): install ark and make a first backup.
- [CLI](cli/index.md): every command, the global flags, profiles and exit codes.
- [Library](library.md): ark from Go: backends, sources and sinks, hooks, profiles and search.
- [Series](series.md): how snapshots group into series.
- [Labels](labels.md): how labels tell jobs of one source apart.
- [Mount](mount.md): browse every snapshot read-only through FUSE.
- [Repository format](format.md): the byte layout of a repository.
- [Security](security.md): the threat model.
- [Releasing](releasing.md): versioning and how releases are cut.
- [Decisions](adr/README.md): the architecture decision records.
