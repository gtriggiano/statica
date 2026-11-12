// Pages for AI agents, written into the build next to the HTML (llmstxt.org):
//
//   <page>.md   the Markdown twin of <page>.html, at the path of its source:
//               guide/quickstart.md, and index.md for a folder's index.html
//   llms.txt    the index of the twins, in the order of the sidebar
//
// Every HTML page links to its twin and to llms.txt from its <head>. Each build
// writes its own files with its own base, so every version folder of the
// published site (docs/scripts/assemble-site.mjs) has the llms.txt of its
// version. A page with `llms: optional` in its frontmatter is listed in the
// Optional section, which an agent can skip.
//
// The build of the site root also publishes the skills for users of statica,
// the folders of skills/ in the repository, for discovery by agents and by
// tools such as `npx skills add <site URL>`:
//
//   .well-known/agent-skills/index.json        the list, with a digest per skill
//   .well-known/agent-skills/<name>/SKILL.md   a copy of skills/<name>/SKILL.md
//
// The format is the Agent Skills discovery RFC, version 0.2.0
// (github.com/cloudflare/agent-skills-discovery-rfc). An archived version
// publishes none: the skill describes the latest release, at the site root.
//
// A twin is the page source with what only VitePress understands rewritten:
// frontmatter removed, included files expanded, the version placeholder
// replaced, containers turned into block quotes and code groups into labeled
// fences. The build fails on a construct that this file cannot rewrite.

import { createHash } from 'node:crypto'
import { mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname, extname, join, resolve } from 'node:path'
import type { DefaultTheme, HeadConfig, SiteConfig, TransformContext } from 'vitepress'
import { setReleaseVersion } from './release-version'

type Frontmatter = Record<string, any>

// Parsed by VitePress and collected while it renders the pages, so this file
// needs no YAML parser.
const frontmatters = new Map<string, Frontmatter>()
const titles = new Map<string, string>()

const published = (page: string) => page !== '404.md'

export function llmsHead(context: TransformContext): HeadConfig[] {
  const { page, siteConfig, pageData } = context
  if (!published(page)) return []
  frontmatters.set(page, pageData.frontmatter)
  titles.set(page, pageData.title)
  const base = siteConfig.site.base
  return [
    ['link', { rel: 'alternate', type: 'text/markdown', href: `${base}${page}` }],
    ['link', { rel: 'describedby', type: 'text/plain', href: `${base}llms.txt` }],
  ]
}

const fail = (page: string, message: string): never => {
  throw new Error(`llms: ${page}: ${message}`)
}

// Reads a file named by a page: @/ is the documentation root, any other path
// is relative to the page.
function readIncluded(srcDir: string, page: string, path: string): string {
  const file = path.startsWith('@/') ? join(srcDir, path.slice(2)) : resolve(srcDir, dirname(page), path)
  try {
    return readFileSync(file, 'utf8').trimEnd()
  } catch {
    return fail(page, `cannot read the included file ${path}`)
  }
}

const containerTitles: Record<string, string> = {
  tip: 'Tip',
  info: 'Info',
  warning: 'Warning',
  danger: 'Danger',
  details: 'Details',
}

// The body of a page as plain Markdown.
function plainMarkdown(srcDir: string, page: string, source: string): string {
  const out: string[] = []
  // Open containers, innermost last; a quoted one prefixes its lines with ">".
  const containers: { name: string; quoted: boolean }[] = []
  let fence = '' // the marker of the open code fence
  const emit = (line: string) => {
    const depth = containers.filter((c) => c.quoted).length
    out.push(depth ? `${'> '.repeat(depth)}${line}`.trimEnd() : line)
  }
  const inCodeGroup = () => containers.at(-1)?.name === 'code-group'
  // In a code group the label of a fence names its tab: write it above.
  const emitLabel = (label?: string) => {
    if (label && inCodeGroup()) emit(`**${label}:**`), emit('')
  }

  for (const line of source.split('\n')) {
    const fenceMatch = /^\s*(`{3,}|~{3,})(.*)$/.exec(line)
    if (fence) {
      if (fenceMatch && fenceMatch[1].startsWith(fence) && !fenceMatch[2].trim()) fence = ''
      emit(line)
      continue
    }
    if (fenceMatch) {
      fence = fenceMatch[1]
      const label = /^(\S*)\s+\[(.+)\]\s*$/.exec(fenceMatch[2])
      if (label && inCodeGroup()) {
        emitLabel(label[2])
        emit(`${fence}${label[1]}`)
      } else emit(line)
      continue
    }

    // <!--@include: path--> inserts a Markdown file.
    const include = /^\s*<!--\s*@include:\s*(\S+?)\s*-->\s*$/.exec(line)
    if (include) {
      if (/[#{]/.test(include[1])) fail(page, `unsupported include ${include[1]}: regions and line ranges are not expanded`)
      const text = readIncluded(srcDir, page, include[1])
      if (/^\s*(:::|<<<|<!--\s*@include)/m.test(text)) fail(page, `the included file ${include[1]} uses a container, a snippet or an include`)
      for (const included of text.split('\n')) emit(included)
      continue
    }

    // <<< path{lang} [label] inserts a file as a code block.
    if (line.startsWith('<<<')) {
      const snippet = /^<<<\s*([^\s{#]+)(?:\{([\w-]+)\})?(?:\s+\[(.+)\])?\s*$/.exec(line)
      if (!snippet) return fail(page, `unsupported snippet: ${line}`)
      const [, path, lang, label] = snippet
      const text = readIncluded(srcDir, page, path).replace(/\n$/, '')
      // A fence longer than any run of backticks in the file.
      const ticks = '`'.repeat(Math.max(3, ...[...text.matchAll(/`+/g)].map((run) => run[0].length + 1)))
      emitLabel(label)
      emit(ticks + (lang ?? extname(path).slice(1)))
      for (const included of text.split('\n')) emit(included)
      emit(ticks)
      continue
    }

    const container = /^\s*:::\s*([\w-]*)\s*(.*)$/.exec(line)
    if (container) {
      if (/^\s/.test(line)) fail(page, 'an indented container, such as one inside a list item, is not supported')
      const [, name, title] = container
      if (!name) {
        if (!containers.pop()) fail(page, 'a container is closed without being opened')
      } else if (name in containerTitles) {
        containers.push({ name, quoted: true })
        emit(`**${containerTitles[name]}${title ? `: ${title.trim()}` : ''}**`)
        emit('')
      } else if (name === 'code-group' || name === 'v-pre') {
        containers.push({ name, quoted: false })
      } else fail(page, `unsupported container ::: ${name}`)
      continue
    }

    emit(line)
  }
  if (fence) fail(page, 'a code fence is not closed')
  if (containers.length) fail(page, `the container ::: ${containers.at(-1)!.name} is not closed`)
  return out.join('\n').replace(/\n{3,}/g, '\n\n').trim() + '\n'
}

// A site link (/guide/quickstart, /architecture/) as the path of a twin.
function twinPath(link: string): string {
  const [path, hash = ''] = link.replace(/^\//, '').split('#')
  const page = path === '' || path.endsWith('/') ? `${path}index.md` : `${path.replace(/\.(md|html)$/, '')}.md`
  return hash ? `${page}#${hash}` : page
}

// The home page keeps its content in the frontmatter: write it as an intro.
function homeIntro(frontmatter: Frontmatter): string {
  const { hero = {}, features = [] } = frontmatter
  const lines = [`# ${hero.name ?? 'statica'}`, '']
  for (const text of [hero.text, hero.tagline]) {
    if (text) lines.push(String(text).replace(/\s+/g, ' ').trim(), '')
  }
  if (features.length) {
    lines.push('## Features', '')
    for (const feature of features) {
      const details = String(feature.details ?? '').replace(/\s+/g, ' ').trim()
      const target = /^[a-z][a-z0-9+.-]*:/i.test(feature.link ?? '') ? feature.link : `./${twinPath(feature.link ?? '')}`
      const link = feature.link ? ` ([${feature.linkText ?? 'more'}](${target}))` : ''
      lines.push(`- **${feature.title}.** ${details}${link}`)
    }
    lines.push('')
  }
  return lines.join('\n') + '\n'
}

// One line of plain text: the description in the frontmatter, or the first
// paragraph of the page.
function describe(frontmatter: Frontmatter, markdown: string): string {
  const paragraph =
    frontmatter.description ??
    markdown
      .split(/\n\s*\n/)
      .map((block) => block.trim())
      .find((block) => /^[A-Za-z*`[]/.test(block) && !block.startsWith('```')) ??
    ''
  const text = String(paragraph)
    .replace(/\[([^\]]+)\]\([^)]*\)/g, '$1')
    .replace(/[*`]/g, '')
    .replace(/\s+/g, ' ')
    .trim()
  // The first sentence is enough when the paragraph is long.
  const sentence = /^.{40,}?[.:](?=\s)/.exec(text)
  return (text.length > 200 && sentence ? sentence[0] : text).replace(/[:.]$/, '')
}

const skillsIndex = '.well-known/agent-skills/index.json'

// Copies each skill of skills/ into the build and writes their index. The URL
// of a skill is relative to the index, so the files work under any base.
export function writeAgentSkills(siteConfig: SiteConfig): void {
  const source = resolve(siteConfig.srcDir, '..', 'skills')
  const out = join(siteConfig.outDir, dirname(skillsIndex))
  const skills = readdirSync(source, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map(({ name }) => {
      const file = `skills/${name}/SKILL.md`
      const others = readdirSync(join(source, name)).filter((other) => other !== 'SKILL.md')
      // A skill with more files would have to be published as an archive.
      if (others.length) fail(file, `the skill has other files (${others.join(', ')}): only a single SKILL.md is published`)
      const content = readFileSync(join(source, name, 'SKILL.md'))
      const frontmatter = /^---\n([\s\S]*?)\n---\n/.exec(content.toString('utf8'))?.[1] ?? fail(file, 'no frontmatter')
      const field = (key: string) => new RegExp(`^${key}: (.+)$`, 'm').exec(frontmatter)?.[1].trim() ?? fail(file, `no ${key} on one line`)
      if (field('name') !== name) fail(file, 'the name differs from the directory')
      mkdirSync(join(out, name), { recursive: true })
      writeFileSync(join(out, name, 'SKILL.md'), content)
      return {
        name,
        type: 'skill-md',
        description: field('description'),
        url: `${name}/SKILL.md`,
        digest: `sha256:${createHash('sha256').update(content).digest('hex')}`,
      }
    })
  if (!skills.length) fail('skills/', 'no skill to publish')
  const index = { $schema: 'https://schemas.agentskills.io/discovery/0.2.0/schema.json', skills }
  writeFileSync(join(siteConfig.outDir, skillsIndex), JSON.stringify(index, null, 2) + '\n')
}

export function writeLlmsFiles(siteConfig: SiteConfig, releaseVersion: string, siteRoot: string): void {
  const { srcDir, outDir, pages, site } = siteConfig
  const base = site.base
  const descriptions = new Map<string, string>()

  for (const page of pages.filter(published)) {
    const frontmatter = frontmatters.get(page) ?? fail(page, 'the page was not rendered')
    const source = readFileSync(join(srcDir, page), 'utf8').replace(/^---\n[\s\S]*?\n---\n/, '')
    const intro = frontmatter.layout === 'home' ? homeIntro(frontmatter) : ''
    // As in the HTML, the version is not replaced in files included with <<<.
    const markdown = plainMarkdown(srcDir, page, setReleaseVersion(intro + source, releaseVersion))
    descriptions.set(page, setReleaseVersion(describe(frontmatter, markdown.replace(/^# .*\n/, '')), releaseVersion))
    mkdirSync(dirname(join(outDir, page)), { recursive: true })
    writeFileSync(join(outDir, page), markdown)
  }

  const listed = new Set<string>()
  const optional: string[] = []
  const entry = (name: string, page: string) => {
    if (!descriptions.has(page)) fail(page, `the sidebar lists ${name}, but the page does not exist`)
    // A page that the sidebar lists twice is listed once, in its first group.
    if (listed.has(page)) return []
    listed.add(page)
    const line = `- [${name}](${base}${page}): ${descriptions.get(page)}`
    return frontmatters.get(page)?.llms === 'optional' ? (optional.push(line), []) : [line]
  }

  // One section per sidebar group, named after its site section: "Guide: Introduction".
  const sections: string[] = []
  const sidebar = (site.themeConfig as DefaultTheme.Config).sidebar as DefaultTheme.SidebarMulti
  for (const [prefix, groups] of Object.entries(sidebar)) {
    const section = prefix.replace(/\//g, '')
    for (const group of groups as DefaultTheme.SidebarItem[]) {
      const lines = (group.items ?? []).flatMap((item) => (item.link ? entry(item.text ?? item.link, twinPath(item.link)) : []))
      if (lines.length) sections.push(`## ${section[0].toUpperCase()}${section.slice(1)}: ${group.text}`, '', ...lines, '')
    }
  }
  // Pages outside the sidebar, such as the home page, come first.
  const unlisted = [...descriptions.keys()].filter((page) => !listed.has(page)).sort((a, b) => a.length - b.length)
  const overview = unlisted.flatMap((page) => entry(page === 'index.md' ? 'Home' : titles.get(page) || page, page))

  const llms = [
    `# ${site.title}`,
    '',
    `> ${site.description} This is the documentation of statica ${releaseVersion}.`,
    '',
    'statica serves one S3 bucket, or a prefix inside it, as one static website. It is a single binary, also published as a container image, configured by one YAML file. Every link below is a page of the documentation as Markdown.',
    '',
    'To configure statica for someone, start from a complete example, look up each field in the configuration reference, then check the result with the CLI: `statica config validate` validates a configuration file offline, and `statica config check-origin` reads one object to check the bucket, the region and the credentials.',
    '',
    `The skill \`deploy-statica\` is that procedure for an agent, from the facts to gather to the checks to run: [index of the skills](${siteRoot}${skillsIndex}), in the Agent Skills discovery format.`,
    '',
    ...(overview.length ? ['## Overview', '', ...overview, ''] : []),
    ...sections,
    ...(optional.length ? ['## Optional', '', ...optional, ''] : []),
  ]
  writeFileSync(join(outDir, 'llms.txt'), llms.join('\n'))
}
