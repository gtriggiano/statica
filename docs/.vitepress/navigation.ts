import type { DefaultTheme } from 'vitepress'

// Top navigation and per-section sidebars for the three sections: Guide
// (/guide/), Architecture (/architecture/) and Reference (/reference/).
// List only pages that exist: each documentation change appends the pages it
// adds (a nav entry when a section gets its first page, sidebar items as pages
// land), so the site never links to a missing page.

export const nav: DefaultTheme.NavItem[] = [
  { text: 'Guide', link: '/guide/what-is-statica', activeMatch: '^/guide/' },
  { text: 'Architecture', link: '/architecture/', activeMatch: '^/architecture/' },
  { text: 'Reference', link: '/reference/configuration', activeMatch: '^/reference/' },
]

export const sidebar: DefaultTheme.SidebarMulti = {
  '/guide/': [
    {
      text: 'Introduction',
      items: [
        { text: 'What is statica?', link: '/guide/what-is-statica' },
        { text: 'Quickstart', link: '/guide/quickstart' },
        { text: 'Installation', link: '/guide/installation' },
        { text: 'Configuration basics', link: '/guide/configuration' },
      ],
    },
    {
      text: 'Serving your site',
      items: [
        { text: 'Publish a site to S3', link: '/guide/publishing' },
        { text: 'S3 and compatible providers', link: '/guide/s3-providers' },
        { text: 'Single-page applications', link: '/guide/single-page-apps' },
        { text: 'Redirects', link: '/guide/redirects' },
        { text: 'Precompressed files', link: '/guide/precompressed-files' },
        { text: 'Browser and CDN caching', link: '/guide/browser-caching' },
        { text: 'Response headers and CORS', link: '/guide/headers-and-cors' },
      ],
    },
    {
      text: 'Running in production',
      items: [
        { text: 'Serve HTTPS', link: '/guide/tls' },
        { text: 'Behind a proxy or ingress', link: '/guide/behind-a-proxy' },
        { text: 'Deploy and shut down', link: '/guide/deployment' },
        { text: 'Tune the memory cache', link: '/guide/cache-tuning' },
      ],
    },
    {
      text: 'Monitoring',
      items: [
        { text: 'Logs', link: '/guide/logs' },
        { text: 'Metrics', link: '/guide/metrics' },
        { text: 'Health and readiness', link: '/guide/health-checks' },
        { text: 'Troubleshooting', link: '/guide/troubleshooting' },
      ],
    },
    {
      text: 'Project',
      items: [{ text: 'Contributing', link: '/guide/contributing' }],
    },
  ],
  '/architecture/': [
    {
      text: 'How it works',
      items: [
        { text: 'Overview', link: '/architecture/' },
        { text: 'Request flow and URL mapping', link: '/architecture/request-flow' },
      ],
    },
    {
      text: 'Caching',
      items: [
        { text: 'Memory cache', link: '/architecture/memory-cache' },
        { text: 'Cache-Control and Age', link: '/architecture/cache-control' },
      ],
    },
    {
      text: 'HTTP behavior',
      items: [
        { text: 'SPA fallback', link: '/architecture/spa-fallback' },
        { text: 'Precompressed variants', link: '/architecture/precompressed-variants' },
        { text: 'Ranges and conditional requests', link: '/architecture/ranges-and-conditionals' },
        { text: 'Access denied', link: '/architecture/access-denied' },
        { text: 'Timeouts and long downloads', link: '/architecture/timeouts' },
      ],
    },
    {
      text: 'Operations and security',
      items: [
        { text: 'Startup, readiness and shutdown', link: '/architecture/lifecycle' },
        { text: 'Security model', link: '/architecture/security' },
      ],
    },
  ],
  '/reference/': [
    {
      text: 'Configuration and CLI',
      items: [
        { text: 'Configuration', link: '/reference/configuration' },
        { text: 'Configuration examples', link: '/reference/examples' },
        { text: 'CLI', link: '/reference/cli' },
      ],
    },
    {
      text: 'Runtime',
      items: [
        { text: 'HTTP responses', link: '/reference/http-responses' },
        { text: 'Logs', link: '/reference/logs' },
        { text: 'Metrics', link: '/reference/metrics' },
        { text: 'Admin endpoints', link: '/reference/admin-endpoints' },
      ],
    },
    {
      text: 'Releases',
      items: [
        { text: 'Container image', link: '/reference/container-image' },
        { text: 'Versioning', link: '/reference/versioning' },
        { text: 'Changelog', link: '/reference/changelog' },
      ],
    },
  ],
}
