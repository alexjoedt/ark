import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'ark',
  description: 'Deduplicated backups as a Go library, with a CLI on top',
  base: '/ark/',
  cleanUrls: true,
  lastUpdated: true,
  srcExclude: ['adr/**'],
  head: [['link', { rel: 'icon', type: 'image/svg+xml', href: '/ark/ark.svg' }]],
  markdown: {
    theme: { light: 'github-light-default', dark: 'github-dark-default' },
    // The pages are plain Markdown that also reads on GitHub; placeholders
    // such as <name> are text, not Vue tags.
    config: (md) => {
      md.set({ html: false })
    },
  },
  themeConfig: {
    logo: '/ark.svg',
    nav: [
      { text: 'Guide', link: '/getting-started' },
      { text: 'CLI', link: '/cli/' },
      { text: 'Library', link: '/library' },
      { text: 'Reference', link: '/format' },
    ],
    sidebar: [
      {
        text: 'Guide',
        items: [
          { text: 'Getting started', link: '/getting-started' },
          { text: 'Series', link: '/series' },
          { text: 'Labels', link: '/labels' },
          { text: 'Retention policies', link: '/retention' },
          { text: 'Mount', link: '/mount' },
        ],
      },
      {
        text: 'CLI',
        items: [
          { text: 'Overview', link: '/cli/' },
          { text: 'Commands', link: '/cli/commands' },
          { text: 'Profiles', link: '/cli/profiles' },
        ],
      },
      {
        text: 'Library',
        items: [{ text: 'Go library', link: '/library' }],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Repository format', link: '/format' },
          { text: 'Security', link: '/security' },
          { text: 'Releasing', link: '/releasing' },
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: 'https://github.com/alexjoedt/ark' }],
    search: { provider: 'local' },
  },
})
