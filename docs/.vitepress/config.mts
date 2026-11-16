import { readFileSync, readdirSync } from 'node:fs'
import { defineConfig } from 'vitepress'

const adrDir = new URL('../adr/', import.meta.url)

const adrs = readdirSync(adrDir)
  .filter((name) => /^\d{4}-.+\.md$/.test(name))
  .sort()
  .map((name) => ({
    text: readFileSync(new URL(name, adrDir), 'utf8').split(/\r?\n/, 1)[0].replace(/^\uFEFF?# /, ''),
    link: `/adr/${name.replace(/\.md$/, '')}`,
  }))

export default defineConfig({
  title: 'ark',
  description: 'Deduplicated backups as a Go library, with a CLI on top',
  base: '/ark/',
  cleanUrls: true,
  lastUpdated: true,
  rewrites: { 'adr/README.md': 'adr/index.md' },
  markdown: {
    // The pages are plain Markdown that also reads on GitHub; placeholders
    // such as <name> are text, not Vue tags.
    config: (md) => {
      md.set({ html: false })
    },
  },
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/getting-started' },
      { text: 'Reference', link: '/FORMAT' },
      { text: 'Decisions', link: '/adr/' },
    ],
    sidebar: [
      {
        text: 'Guide',
        items: [
          { text: 'Getting started', link: '/getting-started' },
          { text: 'Series', link: '/SERIES' },
          { text: 'Labels', link: '/LABELS' },
          { text: 'Mount', link: '/MOUNT' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Repository format', link: '/FORMAT' },
          { text: 'Security', link: '/SECURITY' },
          { text: 'Releasing', link: '/RELEASING' },
        ],
      },
      {
        text: 'Decisions',
        collapsed: true,
        items: [
          { text: 'Index', link: '/adr/' },
          ...adrs,
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: 'https://github.com/alexjoedt/ark' }],
    search: { provider: 'local' },
  },
})
