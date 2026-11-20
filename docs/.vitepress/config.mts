import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'ark',
  description: 'Deduplicated, compressed and encrypted backups of directories and Docker volumes in a local or SFTP repository, as a Go library with a CLI on top. A Go program can plug in its own Source or Sink.',
  base: '/ark/',
  cleanUrls: true,
  lastUpdated: true,
  srcExclude: ['adr/**', 'releasing.md'],
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
          { text: 'How ark works', link: '/concepts' },
          { text: 'Series', link: '/series' },
          { text: 'Labels', link: '/labels' },
          { text: 'Retention policies', link: '/retention' },
          { text: 'Docker volumes', link: '/docker' },
          { text: 'Mount', link: '/mount' },
          { text: 'Troubleshooting', link: '/troubleshooting' },
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
        items: [
          { text: 'Overview', link: '/library' },
          { text: 'Backends', link: '/library/backends' },
          { text: 'Sources and sinks', link: '/library/sources-and-sinks' },
          { text: 'Forget policies', link: '/library/forget' },
          { text: 'Profiles', link: '/library/profiles' },
          { text: 'Progress and logging', link: '/library/observing' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'Repository format', link: '/format' },
          { text: 'Security', link: '/SECURITY' },
          { text: 'Versioning', link: '/versioning' },
        ],
      },
    ],
    socialLinks: [{ icon: 'github', link: 'https://github.com/alexjoedt/ark' }],
    search: { provider: 'local' },
  },
})
