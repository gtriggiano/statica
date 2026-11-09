// Renders every built page in the system Chrome and checks its diagrams. The
// build cannot catch a broken diagram: Mermaid parses most diagram types only
// with a DOM, so diagrams are rendered in the browser alone.
// For each page except 404.html, in the light and dark color schemes and at a
// desktop and a mobile viewport, it waits for every diagram to settle, then
// fails on a diagram that did not render, a console error, an uncaught page
// error, an HTTP response >= 400, or a page wider than the viewport (a wide
// diagram must scroll inside its own frame, never widen the page).
// It serves .vitepress/dist itself under DOCS_BASE, so run it after
// `vitepress build` with the same DOCS_BASE and DOCS_SITE_ROOT. An archived
// build (DOCS_BASE below DOCS_SITE_ROOT) asks for versions.json at the site
// root: the server answers it with the build's own empty list. Chrome comes
// from CHROME_PATH, CHROME_BIN or a standard install location.

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { extname, join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'
import { chromium } from 'playwright-core'

// Same defaults as .vitepress/config.mts.
const base = process.env.DOCS_BASE ?? '/statica/'
const siteRoot = process.env.DOCS_SITE_ROOT ?? base
const dist = fileURLToPath(new URL('../.vitepress/dist/', import.meta.url))
const timeout = 30_000

if (!base.startsWith('/') || !base.endsWith('/')) fail(`DOCS_BASE must start and end with / (got ${base})`)
if (!siteRoot.startsWith('/') || !siteRoot.endsWith('/')) fail(`DOCS_SITE_ROOT must start and end with / (got ${siteRoot})`)

const chromePaths = [
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  // Windows: machine-wide and per-user installs.
  ...['PROGRAMFILES', 'PROGRAMFILES(X86)', 'LOCALAPPDATA'].flatMap((variable) => {
    const dir = process.env[variable]
    return dir ? [join(dir, 'Google', 'Chrome', 'Application', 'chrome.exe'), join(dir, 'Chromium', 'Application', 'chrome.exe')] : []
  }),
]

const variants = ['light', 'dark'].flatMap((colorScheme) =>
  [
    { device: 'desktop', viewport: { width: 1280, height: 900 } },
    { device: 'mobile', viewport: { width: 390, height: 844 } },
  ].map(({ device, viewport }) => ({ name: `${colorScheme} ${device}`, colorScheme, viewport })),
)

const types = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.woff2': 'font/woff2',
  '.txt': 'text/plain; charset=utf-8',
}

function fail(message) {
  console.error(`check-diagrams: ${message}`)
  process.exit(1)
}

function findChrome() {
  for (const variable of ['CHROME_PATH', 'CHROME_BIN']) {
    const configured = process.env[variable]
    if (!configured) continue
    if (!existsSync(configured)) fail(`${variable} is set to ${configured}, which does not exist`)
    return configured
  }
  const found = chromePaths.find((path) => existsSync(path))
  if (!found) fail(`no Chrome found; install Google Chrome or set CHROME_PATH (looked in: ${chromePaths.join(', ')})`)
  return found
}

function walk(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name)
    return entry.isDirectory() ? walk(path) : [path]
  })
}

// Serves the build under the base: "dir/" means "dir/index.html"; any path
// that is not exactly a file of the build (case included) is a 404, except
// the site root's versions.json for an archived build.
function serve(files) {
  const server = createServer((request, response) => {
    let path
    try {
      path = decodeURIComponent(new URL(request.url, 'http://localhost').pathname)
    } catch {
      path = ''
    }
    let target = path.startsWith(base) ? path.slice(base.length) : null
    if (siteRoot !== base && path === `${siteRoot}versions.json`) target = 'versions.json'
    if (target === '' || target?.endsWith('/')) target += 'index.html'
    if (!files.has(target)) {
      response.writeHead(404, { 'content-type': 'text/plain; charset=utf-8' })
      response.end('Not found\n')
      return
    }
    response.writeHead(200, { 'content-type': types[extname(target)] ?? 'application/octet-stream' })
    response.end(readFileSync(join(dist, target)))
  })
  return new Promise((ready, failed) => {
    server.once('error', failed)
    server.listen(0, '127.0.0.1', () => ready(server))
  })
}

async function checkPage(context, url) {
  const problems = []
  const page = await context.newPage()
  page.on('console', (message) => {
    if (message.type() !== 'error') return
    // Drop stack frames: the message and its first lines identify the error.
    const text = message.text().split('\n').filter((line) => !/^\s+at /.test(line)).join('\n')
    problems.push(`console error: ${text}`)
  })
  page.on('pageerror', (error) => problems.push(`page error: ${error.message}`))
  page.on('response', (response) => {
    if (response.status() >= 400) problems.push(`HTTP ${response.status()} for ${response.url()}`)
  })
  let figures = []
  try {
    await page.goto(url, { waitUntil: 'load', timeout })
    // Server-rendered figures start "pending"; each settles once Mermaid ran.
    await page
      .waitForFunction(() => !document.querySelector('[data-mermaid-state="pending"]'), null, { timeout })
      .catch(() => problems.push(`diagrams still pending after ${timeout / 1000}s`))
    const state = await page.evaluate(() => ({
      figures: [...document.querySelectorAll('figure[data-mermaid-state]')].map((figure) => ({
        title: figure.getAttribute('aria-label') ?? '',
        state: figure.getAttribute('data-mermaid-state'),
        error: figure.querySelector('.mermaid-error')?.textContent?.trim() ?? '',
      })),
      scrollWidth: document.documentElement.scrollWidth,
      clientWidth: document.documentElement.clientWidth,
    }))
    figures = state.figures
    for (const figure of figures) {
      if (figure.state === 'error') problems.push(`diagram "${figure.title}" failed: ${figure.error}`)
      else if (figure.state !== 'rendered') problems.push(`diagram "${figure.title}" is ${figure.state}`)
    }
    if (state.scrollWidth > state.clientWidth) {
      problems.push(`page overflows horizontally: ${state.scrollWidth}px of content in ${state.clientWidth}px`)
    }
  } catch (error) {
    problems.push(`could not load the page: ${error instanceof Error ? error.message : String(error)}`)
  } finally {
    await page.close()
  }
  return { diagrams: figures.length, rendered: figures.filter((f) => f.state === 'rendered').length, problems }
}

if (!existsSync(join(dist, 'index.html'))) fail(`no build in ${dist}; run make docs-build first`)
const chrome = findChrome()
// Every file of the build by its path under the base, with "/" separators.
const files = new Set(walk(dist).map((file) => relative(dist, file).split(sep).join('/')))
const pages = [...files].filter((file) => file.endsWith('.html') && file !== '404.html').sort()

const server = await serve(files)
const root = `http://127.0.0.1:${server.address().port}${base}`
let browser
const results = new Map(pages.map((page) => [page, []]))
try {
  browser = await chromium.launch({ executablePath: chrome, headless: true })
  console.log(`check-diagrams: ${pages.length} page(s) x ${variants.map((v) => v.name).join(', ')}; Chrome: ${chrome}`)
  // One browser context per variant, run in parallel; pages run in order.
  await Promise.all(
    variants.map(async (variant, index) => {
      const context = await browser.newContext({ colorScheme: variant.colorScheme, viewport: variant.viewport })
      try {
        for (const page of pages) {
          results.get(page)[index] = { variant: variant.name, ...(await checkPage(context, root + page)) }
        }
      } finally {
        await context.close()
      }
    }),
  )
} finally {
  await browser?.close()
  server.close()
}

let failed = 0
for (const [page, runs] of results) {
  // The same problem usually shows in several variants: list it once.
  const problems = new Map()
  for (const run of runs) {
    for (const problem of run.problems) problems.set(problem, (problems.get(problem) ?? new Set()).add(run.variant))
  }
  const diagrams = Math.max(...runs.map((run) => run.diagrams))
  if (problems.size === 0) {
    const summary = diagrams === 0 ? 'no diagrams' : `${diagrams} diagram(s) rendered in each of ${runs.length} variants`
    console.log(`  ok    ${page}: ${summary}`)
    continue
  }
  failed++
  const rendered = runs.map((run) => `${run.variant} ${run.rendered}/${run.diagrams}`).join(', ')
  console.log(`  FAIL  ${page}: ${diagrams === 0 ? 'no diagrams' : `diagrams rendered: ${rendered}`}`)
  for (const [problem, where] of problems) {
    console.log(`          ${[...where].join(', ')}: ${problem.replaceAll('\n', '\n            ')}`)
  }
}

if (failed > 0) fail(`${failed} of ${pages.length} page(s) failed`)
console.log(`check-diagrams: all ${pages.length} page(s) passed`)
