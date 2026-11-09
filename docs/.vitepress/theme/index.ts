import DefaultTheme from 'vitepress/theme'
import type { Theme } from 'vitepress'
import { h } from 'vue'
import Mermaid from './Mermaid.vue'
import VersionBanner from './VersionBanner.vue'
import VersionSelector from './VersionSelector.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  // The version selector and banner (versions.ts) go in the default layout's
  // slots. The home page has no doc-before slot, so its banner goes in the hero.
  Layout: () =>
    h(DefaultTheme.Layout, null, {
      'nav-bar-content-after': () => h(VersionSelector),
      'nav-screen-content-after': () => h(VersionSelector, { screen: true }),
      'doc-before': () => h(VersionBanner),
      'home-hero-info-before': () => h(VersionBanner),
    }),
  enhanceApp({ app }) {
    app.component('Mermaid', Mermaid)
  },
} satisfies Theme
