# 0023: A VitePress docs site with Node only in docs/

Status: accepted
Date: 2026-11-16

## Context

The documentation is a set of Markdown files in docs/ (format, security, releasing, series,
labels, mount) and the ADRs in docs/adr, read on GitHub. There is no getting-started page, no
navigation between the pages and no search. A static site generator adds those, but every one of
them brings a toolchain, and 0016 removed Node from the build and the release so that `task build`
and the release need Go only. The pages must also keep reading well on GitHub, and the ADR doc test
expects every ADR to start with its `# NNNN: title` line, so no generator may require frontmatter
on them.

## Decision

docs/ is a VitePress site. Its package.json, lockfile and config live in docs/ and nowhere else; a
GitHub Pages workflow of its own runs `npm ci` and `npm run docs:build` there. The pages stay plain
Markdown: raw HTML in them is rendered as text, links are relative `.md` links, dead links fail the
build, and the ADR list in the sidebar is read from the directory, so a new ADR needs no config
edit.

- Not Astro Starlight, because it wants frontmatter with a title on every page and its content in
  its own source tree, which would move the pages away from where GitHub and the doc tests read
  them.
- Not Hugo, because although it is a single Go binary, it renders nothing without a theme pulled
  in as a module or submodule or layouts of its own, and its templates are a second language to
  maintain for a few pages.

## Consequences

`task build`, `task test` and the release workflow stay free of Node; only the Pages workflow and a
contributor previewing the site run npm. Renovate now also proposes updates for the docs
dependencies. A page with a broken relative link no longer builds. A placeholder like `<name>`
outside backticks shows as text on the site, as it does on GitHub. Enabling GitHub Pages for the
repository is a manual step in its settings.
