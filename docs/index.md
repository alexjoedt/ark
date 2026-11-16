---
layout: home

hero:
  name: ark
  text: Deduplicated backups as a Go library, with a CLI on top
  tagline: Content split with FastCDC, stored once, compressed with zstd and encrypted, in a local directory or over SFTP.
  actions:
    - theme: brand
      text: Series
      link: /SERIES
    - theme: alt
      text: Repository format
      link: /FORMAT
    - theme: alt
      text: GitHub
      link: https://github.com/alexjoedt/ark

features:
  - title: Series and labels
    details: How snapshots group into series, and how labels tell jobs of one source apart.
    link: /SERIES
  - title: Mount
    details: Browse every snapshot read-only through FUSE.
    link: /MOUNT
  - title: Format and security
    details: The byte layout of a repository and the threat model it is built against.
    link: /FORMAT
  - title: Decisions
    details: The architecture decision records behind the format, the API and the CLI.
    link: /adr/
---
