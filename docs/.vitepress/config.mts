import { readFileSync } from 'node:fs'
import { defineConfig, type HeadConfig } from 'vitepress'
import { mermaidFence } from './mermaid-fence'
import { nav, sidebar } from './navigation'
import { setBlockVersions } from './version-blocks'

const repository = 'https://github.com/gtriggiano/statica'

// A custom domain can build with DOCS_BASE=/; project Pages uses /<repo>/.
const base = process.env.DOCS_BASE ?? '/statica/'
// The root of the whole site, where versions.json lives. A build whose base is
// below the root is an archived version, for example /statica/v0.1/ or
// /statica/next/ (see docs/scripts/assemble-site.mjs).
const siteRoot = process.env.DOCS_SITE_ROOT ?? base
// The statica release this build documents, set by the workflows from the tag;
// empty for local and pull request builds.
const version = process.env.DOCS_VERSION ?? ''

for (const [name, value] of [['DOCS_BASE', base], ['DOCS_SITE_ROOT', siteRoot]]) {
  if (!value.startsWith('/') || !value.endsWith('/')) throw new Error(`${name} must start and end with /`)
}
if (!base.startsWith(siteRoot)) throw new Error('DOCS_BASE must be DOCS_SITE_ROOT or a folder below it')
if (version && !/^\d+\.\d+\.\d+(-rc\.\d+)?$/.test(version)) {
  throw new Error(`DOCS_VERSION must be X.Y.Z or X.Y.Z-rc.N (got ${version})`)
}
// The commit of a release documents that release: its version blocks, and the
// manifest, already name it. A release candidate is built from main, whose
// blocks name the last release; setBlockVersions shows the candidate instead.
const manifest = JSON.parse(readFileSync(new URL('../../.release-please-manifest.json', import.meta.url), 'utf8'))
if (version && !version.includes('-') && version !== manifest['.']) {
  throw new Error(
    `DOCS_VERSION is ${version}, but this commit documents ${manifest['.']} (.release-please-manifest.json): build the documentation of ${version} from its tag`,
  )
}
// Search engines index only the site root, and old pages are not edited.
const archived = base !== siteRoot

export default defineConfig({
  title: "statica",
  description:
    "A Go web server for static websites stored in S3, with a bounded in-memory cache.",
  lang: "en-US",
  base,
  lastUpdated: true,
  cleanUrls: false,
  ignoreDeadLinks: false,
  // head entries are not rewritten with the base, so prefix it here.
  head: [
    [
      "link",
      { rel: "icon", type: "image/svg+xml", href: `${base}favicon.svg` },
    ],
    ["link", { rel: "apple-touch-icon", href: `${base}apple-touch-icon.png` }],
    ["meta", { property: "og:type", content: "website" }],
    [
      "meta",
      {
        property: "og:title",
        content: "statica — Static websites, served from S3",
      },
    ],
    [
      "meta",
      {
        property: "og:description",
        content:
          "Static websites, served from S3. One instance serves one bucket or prefix. Configured by a single YAML file.",
      },
    ],
    ["meta", { property: "og:image", content: `${base}og-image.png` }],
    ["meta", { name: "twitter:card", content: "summary_large_image" }],
    ...(archived
      ? [["meta", { name: "robots", content: "noindex" }] as HeadConfig]
      : []),
  ],
  markdown: {
    config: (md) => {
      mermaidFence(md);
      if (version) {
        // The source reaches the parser with its includes already expanded.
        const parse = md.parse.bind(md);
        md.parse = (source, env) =>
          parse(setBlockVersions(source, version), env);
      }
    },
  },
  vite: {
    // Read by the version selector and banner (theme/versions.ts).
    define: {
      __DOCS_SITE_ROOT__: JSON.stringify(siteRoot),
      __DOCS_VERSION__: JSON.stringify(version),
    },
    // Mermaid's diagram engines (elk, cytoscape, katex) are split into chunks
    // over 500 kB. They are loaded on demand, only by pages with diagrams.
    build: { chunkSizeWarningLimit: 2048 },
    // Pre-bundle mermaid for the dev server, so the first diagram does not
    // trigger a dependency re-optimization and a full page reload.
    optimizeDeps: { include: ["mermaid"] },
  },
  themeConfig: {
    logo: {
      light: "/logo-light.svg",
      dark: "/logo-dark.svg",
      alt: "statica",
    },
    search: { provider: "local" },
    outline: [2, 3],
    nav,
    sidebar,
    socialLinks: [{ icon: "github", link: repository }],
    editLink: archived
      ? undefined
      : {
          pattern: `${repository}/edit/main/docs/:path`,
          text: "Edit this page on GitHub",
        },
    footer: {
      message: "Released under the MIT License.",
      copyright: "Copyright © 2026 Giacomo Triggiano",
    },
  },
});
