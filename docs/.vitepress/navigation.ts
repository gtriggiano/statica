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
      text: 'Get started',
      items: [
        { text: 'What is statica?', link: '/guide/what-is-statica' },
        { text: 'Quickstart', link: '/guide/quickstart' },
        { text: 'Installation', link: '/guide/installation' },
        { text: 'Configuration basics', link: '/guide/configuration' },
        { text: 'Configuration examples', link: '/reference/examples' },
        { text: 'Configure statica with an AI agent', link: '/guide/ai-agents' },
      ],
    },
    {
      text: 'Deploy',
      items: [
        { text: 'Deploy on Kubernetes', link: '/guide/kubernetes' },
        { text: 'Deploy, roll out and shut down', link: '/guide/deployment' },
        { text: 'Connect to S3 and compatible providers', link: '/guide/s3-providers' },
        { text: 'Run behind a proxy, CDN or ingress', link: '/guide/behind-a-proxy' },
        { text: 'Serve HTTPS directly', link: '/guide/tls' },
        { text: 'Health checks and readiness', link: '/guide/health-checks' },
      ],
    },
    {
      text: 'Build your site',
      items: [
        { text: 'Publish a site to S3', link: '/guide/publishing' },
        { text: 'Serve a single-page application', link: '/guide/single-page-apps' },
        { text: 'Configure redirects', link: '/guide/redirects' },
        { text: 'Serve precompressed files', link: '/guide/precompressed-files' },
        { text: 'Control browser and CDN caching', link: '/guide/browser-caching' },
        { text: 'Response headers and CORS', link: '/guide/headers-and-cors' },
      ],
    },
    {
      text: 'Operate',
      items: [
        { text: 'Work with the logs', link: '/guide/logs' },
        { text: 'Monitor with Prometheus', link: '/guide/metrics' },
        { text: 'Tune the memory cache', link: '/guide/cache-tuning' },
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
