<script setup lang="ts">
// Renders one Mermaid diagram in the browser. The server-rendered HTML holds
// the figure and its text summary; mermaid is imported only after mounting, so
// it never runs during VitePress's server-side rendering. The diagram follows
// the site's light/dark theme and is re-rendered when the theme changes.
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useData } from 'vitepress'
import { serialize } from './render-queue'

const props = defineProps<{ code: string; title: string; description: string }>()

const decode = (value: string) => {
  try {
    return decodeURIComponent(value)
  } catch {
    return value
  }
}
const source = computed(() => decode(props.code))
const label = computed(() => decode(props.title))
const summary = computed(() => decode(props.description))

const { isDark } = useData()
const svg = ref('')
const state = ref<'pending' | 'rendered' | 'error'>('pending')
const failure = ref('')
const viewport = ref<HTMLElement | null>(null)
const scrollable = ref(false)

const uid = Math.random().toString(36).slice(2, 10)
let generation = 0
let mounted = false

let canvas: CanvasRenderingContext2D | null = null
// Resolves a CSS color (custom properties included) to an opaque #rrggbb,
// blending any transparency over the page background, because Mermaid's color
// functions need plain colors.
function color(variable: string, background?: string): string {
  const raw = getComputedStyle(document.documentElement).getPropertyValue(variable).trim()
  canvas ??= document.createElement('canvas').getContext('2d')
  if (!canvas || !raw) return background ?? '#808080'
  canvas.fillStyle = '#808080'
  canvas.fillStyle = raw
  const normalized = String(canvas.fillStyle)
  if (normalized.startsWith('#')) return normalized
  const m = normalized.match(/rgba?\(\s*([\d.]+),\s*([\d.]+),\s*([\d.]+)(?:,\s*([\d.]+))?\s*\)/)
  if (!m) return background ?? '#808080'
  const alpha = m[4] === undefined ? 1 : Number(m[4])
  const base = background ? [1, 3, 5].map((i) => parseInt(background.slice(i, i + 2), 16)) : [255, 255, 255]
  return (
    '#' +
    [1, 2, 3]
      .map((i, k) => Math.round(Number(m[i]) * alpha + base[k] * (1 - alpha)))
      .map((v) => v.toString(16).padStart(2, '0'))
      .join('')
  )
}

function themeVariables(dark: boolean) {
  const bg = color('--vp-c-bg')
  const soft = color('--vp-c-bg-soft', bg)
  const alt = color('--vp-c-bg-alt', bg)
  const text = color('--vp-c-text-1', bg)
  const muted = color('--vp-c-text-2', bg)
  const brand = color('--vp-c-brand-1', bg)
  const brandSoft = color('--vp-c-brand-soft', bg)
  const border = color('--vp-c-border', bg)
  const divider = color('--vp-c-divider', bg)
  const noteBg = color('--vp-c-warning-soft', bg)
  const note = color('--vp-c-warning-1', bg)
  const font = getComputedStyle(document.body).fontFamily
  return {
    darkMode: dark,
    background: bg,
    fontFamily: font,
    fontSize: '15px',
    textColor: text,
    lineColor: muted,
    primaryColor: brandSoft,
    primaryTextColor: text,
    primaryBorderColor: brand,
    secondaryColor: soft,
    secondaryTextColor: text,
    secondaryBorderColor: border,
    tertiaryColor: alt,
    tertiaryTextColor: text,
    tertiaryBorderColor: divider,
    mainBkg: brandSoft,
    nodeBorder: brand,
    nodeTextColor: text,
    clusterBkg: alt,
    clusterBorder: border,
    titleColor: text,
    edgeLabelBackground: bg,
    noteBkgColor: noteBg,
    noteTextColor: text,
    noteBorderColor: note,
    actorBkg: brandSoft,
    actorBorder: brand,
    actorTextColor: text,
    actorLineColor: muted,
    signalColor: text,
    signalTextColor: text,
    labelBoxBkgColor: soft,
    labelBoxBorderColor: border,
    labelTextColor: text,
    loopTextColor: text,
    activationBkgColor: soft,
    activationBorderColor: brand,
    sequenceNumberColor: bg,
    labelBackgroundColor: bg,
    stateLabelColor: text,
    altBackground: alt,
    compositeBackground: alt,
    compositeTitleBackground: soft,
    sectionBkgColor: alt,
    altSectionBkgColor: bg,
    sectionBkgColor2: soft,
    taskBkgColor: brandSoft,
    taskBorderColor: brand,
    taskTextColor: text,
    taskTextLightColor: text,
    taskTextOutsideColor: text,
    taskTextDarkColor: text,
    activeTaskBkgColor: brandSoft,
    activeTaskBorderColor: brand,
    gridColor: divider,
    todayLineColor: note,
  }
}

function measure() {
  const el = viewport.value
  scrollable.value = !!el && el.scrollWidth > el.clientWidth + 1
}

// Gantt charts size themselves from the page width, not their container:
// give them the frame's width, with a floor that keeps labels readable.
function ganttWidth(): number {
  const el = viewport.value
  const inner = el ? el.clientWidth - 32 : 640
  return Math.max(Math.floor(inner), 560)
}

async function render() {
  if (!mounted) return
  const mine = ++generation
  await nextTick()
  // Let the theme class switch and web fonts settle before reading styles.
  await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()))
  await document.fonts?.ready
  if (mine !== generation) return
  const dark = isDark.value
  try {
    const out = await serialize(async () => {
      const { default: mermaid } = await import('mermaid')
      mermaid.initialize({
        startOnLoad: false,
        securityLevel: 'strict',
        theme: 'base',
        themeVariables: themeVariables(dark),
        flowchart: { useMaxWidth: false },
        sequence: { useMaxWidth: false },
        state: { useMaxWidth: false },
        gantt: { useMaxWidth: false, useWidth: ganttWidth() },
        class: { useMaxWidth: false },
        er: { useMaxWidth: false },
        timeline: { useMaxWidth: false },
      })
      const id = `mermaid-${uid}-${mine}`
      try {
        return (await mermaid.render(id, source.value)).svg
      } finally {
        // On a syntax error Mermaid leaves its scratch element in <body>.
        document.getElementById(`d${id}`)?.remove()
      }
    })
    if (mine !== generation) return
    svg.value = out
    state.value = 'rendered'
    failure.value = ''
  } catch (error) {
    if (mine !== generation) return
    svg.value = ''
    state.value = 'error'
    failure.value = error instanceof Error ? error.message : String(error)
    console.error(`[mermaid] "${label.value}" failed to render:`, error)
  }
  await nextTick()
  measure()
}

onMounted(() => {
  mounted = true
  window.addEventListener('resize', measure, { passive: true })
  render()
})
onBeforeUnmount(() => {
  mounted = false
  generation++
  window.removeEventListener('resize', measure)
})
watch([isDark, source], () => render())
</script>

<template>
  <figure class="mermaid-figure" :aria-label="label" :data-mermaid-state="state">
    <div
      ref="viewport"
      class="mermaid-viewport"
      :tabindex="scrollable ? 0 : undefined"
      :role="scrollable ? 'region' : undefined"
      :aria-label="scrollable ? `${label} (diagram, scrolls horizontally)` : undefined"
    >
      <div v-if="state === 'rendered'" class="mermaid-svg" aria-hidden="true" v-html="svg" />
      <div v-else-if="state === 'error'" class="mermaid-error" role="alert">
        Diagram failed to render: {{ failure }}
      </div>
      <div v-else class="mermaid-pending" aria-hidden="true">Loading diagram…</div>
    </div>
    <p v-if="scrollable" class="mermaid-hint" aria-hidden="true">Wide diagram: scroll sideways to see all of it.</p>
    <figcaption class="mermaid-summary">{{ summary }}</figcaption>
  </figure>
</template>
