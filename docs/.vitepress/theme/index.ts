import { h } from 'vue'
import type { Theme } from 'vitepress'
import DefaultTheme from 'vitepress/theme-without-fonts'
import '@fontsource-variable/instrument-sans'
import '@fontsource-variable/geist-mono'
import './vault.css'
import Home from './Home.vue'

export default {
  extends: DefaultTheme,
  Layout: () => h(DefaultTheme.Layout, null, { 'home-hero-before': () => h(Home) }),
} satisfies Theme
