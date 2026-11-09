import type { MarkdownRenderer } from 'vitepress'

// Turns ```mermaid fences into the <Mermaid> component (theme/Mermaid.vue),
// which renders the diagram in the browser only. Every diagram must declare
// accTitle and accDescr: the description is rendered at build time as the
// figure's caption, hidden on screen and read by screen readers in place of
// the diagram, with or without JavaScript. A diagram missing either fails the
// build.

const title = /^[ \t]*accTitle[ \t]*:[ \t]*(.+?)[ \t]*$/m
const lineDescription = /^[ \t]*accDescr[ \t]*:[ \t]*(.+?)[ \t]*$/m
const blockDescription = /^[ \t]*accDescr[ \t]*\{([\s\S]*?)\}/m

// encodeURIComponent leaves no quote, angle bracket or mustache, so the value
// is inert inside a Vue template attribute.
const attr = (value: string) => encodeURIComponent(value)

export function mermaidFence(md: MarkdownRenderer): void {
  const fallback = md.renderer.rules.fence
  if (!fallback) throw new Error('mermaid-fence: markdown-it fence renderer not found')

  md.renderer.rules.fence = (tokens, idx, options, env, self) => {
    const token = tokens[idx]
    if (token.info.trim() !== 'mermaid') return fallback(tokens, idx, options, env, self)

    const source = token.content
    const where = `${env?.relativePath ?? 'unknown page'}:${(token.map?.[0] ?? 0) + 1}`
    const t = source.match(title)?.[1]
    const d = (source.match(lineDescription)?.[1] ?? source.match(blockDescription)?.[1])
      ?.replace(/\s+/g, ' ')
      .trim()
    if (!t || !d) {
      throw new Error(`${where}: a mermaid diagram needs both accTitle and accDescr (the text summary that screen readers read)`)
    }
    return `<Mermaid code="${attr(source)}" title="${attr(t)}" description="${attr(d)}" />\n`
  }
}
