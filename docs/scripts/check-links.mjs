// Checks internal links and anchors in the built site. VitePress fails the build
// on a link to a missing page, but not on a #fragment that matches nothing.
// This script reads every HTML page in .vitepress/dist and checks every internal
// <a href> (navigation and sidebar included): the target file must exist and a
// #fragment must be an element id in the target page. Links to other origins
// (http(s) to other hosts, mailto:) and bare "#" are ignored.
// It also checks the addresses of the published site that are written out in
// full, in the Markdown version of the pages and in the skills for users
// (skills/*/SKILL.md): each must be a file of the build, so that renaming a
// page cannot break the skill.
// Run it after `vitepress build`, with the same DOCS_BASE. Node built-ins only.

import { existsSync, readdirSync, readFileSync } from 'node:fs'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

// Same default as .vitepress/config.mts.
const base = process.env.DOCS_BASE ?? '/statica/'
const dist = fileURLToPath(new URL('../.vitepress/dist/', import.meta.url))
// Placeholder origin used to resolve hrefs; any other origin is external.
const origin = 'http://docs.invalid'
// The root of the published site, which describes the latest release.
const site = 'https://gtriggiano.github.io/statica/'
const skills = fileURLToPath(new URL('../../skills/', import.meta.url))
// An archived version (see .vitepress/config.mts) does not publish the skills.
const archived = base !== (process.env.DOCS_SITE_ROOT ?? base)

if (!base.startsWith('/') || !base.endsWith('/')) {
  console.error(`check-links: DOCS_BASE must start and end with / (got ${base})`)
  process.exit(1)
}
if (!existsSync(join(dist, 'index.html'))) {
  console.error(`check-links: no build in ${dist}; run vitepress build first`)
  process.exit(1)
}

const named = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: '\u00a0' }
const decodeEntities = (value) =>
  value.replace(/&(#[xX][0-9a-fA-F]+|#[0-9]+|[a-zA-Z]+);/g, (entity, name) => {
    if (name[0] !== '#') return named[name] ?? entity
    const code = name[1] === 'x' || name[1] === 'X' ? parseInt(name.slice(2), 16) : parseInt(name.slice(1), 10)
    try {
      return String.fromCodePoint(code)
    } catch {
      return entity
    }
  })

const decodePercent = (value) => {
  try {
    return decodeURIComponent(value)
  } catch {
    return value
  }
}

// Start tags with their attribute text; quoted values may contain ">".
const tagRE = /<([a-zA-Z][\w:-]*)((?:"[^"]*"|'[^']*'|[^'">])*)>/g
const attrRE = /([^\s"'>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+)))?/g

function scan(html) {
  const ids = new Set()
  const hrefs = []
  // Comments, scripts and styles hold no markup to check.
  const markup = html.replace(/<!--[\s\S]*?-->|<(script|style)\b[\s\S]*?<\/\2\s*>/gi, ' ')
  for (const [, tag, attributes] of markup.matchAll(tagRE)) {
    for (const [, name, ...values] of attributes.matchAll(attrRE)) {
      const value = values.find((v) => v !== undefined)
      if (value === undefined) continue
      const lower = name.toLowerCase()
      if (lower === 'id') ids.add(decodeEntities(value))
      else if (lower === 'href' && tag.toLowerCase() === 'a') hrefs.push(decodeEntities(value))
    }
  }
  return { ids, hrefs }
}

function walk(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    const path = join(dir, entry.name)
    return entry.isDirectory() ? walk(path) : [path]
  })
}

// Every file of the build by its path under the base, with "/" separators.
// Matching against this set keeps targets case-sensitive on any file system.
const files = new Set(walk(dist).map((file) => relative(dist, file).split(sep).join('/')))
const pages = new Map()
for (const file of [...files].filter((f) => f.endsWith('.html')).sort()) {
  pages.set(file, scan(readFileSync(join(dist, file), 'utf8')))
}

// Returns what is wrong with an internal link, or null.
function problem(url) {
  const path = decodePercent(url.pathname)
  if (!path.startsWith(base)) return `points outside the site base ${base}`
  let target = path.slice(base.length)
  if (target === '' || target.endsWith('/')) target += 'index.html'
  const targetPage = pages.get(target)
  if (!files.has(target)) return `no file ${target} in the build`
  const fragment = url.hash.slice(1)
  if (!fragment || !targetPage) return null
  if (targetPage.ids.has(fragment) || targetPage.ids.has(decodePercent(fragment))) return null
  return `no element with id "${decodePercent(fragment)}" in ${target}`
}

let checked = 0
const failures = []
for (const [page, { hrefs }] of pages) {
  for (const href of hrefs) {
    if (href === '' || href === '#') continue
    let url
    try {
      url = new URL(href, origin + base + page)
    } catch {
      failures.push(`${page}: ${href} -> malformed URL`)
      continue
    }
    if (url.origin !== origin) continue
    checked++
    const reason = problem(url)
    if (reason) failures.push(`${page}: ${href} -> ${reason}`)
  }
}

// Addresses of the site written in full. One that continues with a placeholder
// (v<major>.<minor>/) or names a version folder is not a file of this build.
const siteRE = new RegExp(site.replace(/[.]/g, '\\.') + "[^\\s\"'`()<>\\[\\]]*(<)?", 'g')
const sources = [...files].filter((f) => f.endsWith('.md')).map((f) => [f, join(dist, f)])
if (existsSync(skills)) {
  for (const name of readdirSync(skills)) sources.push([`skills/${name}/SKILL.md`, join(skills, name, 'SKILL.md')])
}
for (const [name, file] of sources) {
  if (!existsSync(file)) continue
  for (const [text, placeholder] of readFileSync(file, 'utf8').matchAll(siteRE)) {
    let target = decodePercent(text.slice(site.length).replace(/[#?].*$/, '').replace(/[.,;:]+$/, ''))
    if (placeholder || /^(v[0-9]+\.[0-9]+|next)\//.test(target)) continue
    if (archived && target.startsWith('.well-known/')) continue
    if (target === '' || target.endsWith('/')) target += 'index.html'
    checked++
    if (!files.has(target)) failures.push(`${name}: ${text} -> no file ${target} in the build`)
  }
}

if (failures.length) {
  console.error(`check-links: ${failures.length} broken internal link(s) or anchor(s):`)
  for (const failure of failures) console.error(`  ${failure}`)
  process.exit(1)
}
console.log(`check-links: ${checked} internal links and site addresses in ${pages.size} pages, all targets and anchors found`)
