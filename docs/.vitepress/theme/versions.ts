// The version selector and the banner of the documentation site. Each version
// is a separate VitePress build in its own folder of the site (the layout and
// versions.json are described in docs/scripts/assemble-site.mjs). A build knows
// its base (DOCS_BASE), the site root (DOCS_SITE_ROOT) and the release it
// documents (DOCS_VERSION, empty for local and pull request builds). The list
// of versions is read in the browser from ${DOCS_SITE_ROOT}versions.json, so an
// old version lists the versions released after it.
import { computed, onMounted, shallowRef } from 'vue'
import { useData } from 'vitepress'

declare const __DOCS_SITE_ROOT__: string
declare const __DOCS_VERSION__: string

export const siteRoot = __DOCS_SITE_ROOT__
const buildVersion = __DOCS_VERSION__

interface Folder {
  version: string
  path: string
}

interface Versions {
  latest: string | null
  versions: Folder[]
  next: Folder | null
}

// name is the version alone, shown on the selector; label adds "latest" or
// "candidate" in the list.
export interface VersionOption {
  name: string
  label: string
  href: string
}

export type Banner =
  | { kind: 'candidate'; version: string; latest: string | null }
  | { kind: 'older'; minor: string; latest: string }

const release = /^\d+\.\d+\.\d+(-rc\.\d+)?$/
const folderPath = /^[\w.-]+\/$/

const isFolder = (value: unknown): value is Folder => {
  const folder = value as Partial<Folder> | null
  return (
    typeof folder?.version === 'string' &&
    release.test(folder.version) &&
    typeof folder.path === 'string' &&
    folderPath.test(folder.path)
  )
}

// Anything unexpected in the file counts as "no other versions".
function parse(data: unknown): Versions {
  const value = data as Partial<Record<keyof Versions, unknown>> | null
  return {
    latest: typeof value?.latest === 'string' && release.test(value.latest) ? value.latest : null,
    versions: Array.isArray(value?.versions) ? value.versions.filter(isFolder) : [],
    next: isFolder(value?.next) ? value.next : null,
  }
}

// One request per page load, shared by every component, made only after
// mounting: never during server-side rendering.
const versions = shallowRef<Versions | null>(null)
let request: Promise<void> | undefined

function load() {
  request ??= fetch(`${siteRoot}versions.json`)
    .then((response) => (response.ok ? response.json() : null))
    .catch(() => null)
    .then((data) => {
      versions.value = parse(data)
    })
}

const minor = (version: string) => version.split('.').slice(0, 2).join('.')
const isCandidate = (version: string) => version.includes('-')

function olderMinor(version: string, than: string) {
  const [a, b] = [version, than].map((v) => v.split('.').map(Number))
  return a[0] < b[0] || (a[0] === b[0] && a[1] < b[1])
}

export function useVersions() {
  const { site } = useData()
  onMounted(load)

  // The versions to choose from, newest first, and this build's.
  // Until versions.json is loaded, and with the empty list of local and pull
  // request builds, the only option is this build.
  const options = computed(() => {
    const here = site.value.base
    const data = versions.value
    const list: VersionOption[] = []
    // The folder of the latest minor has the pages of the site root.
    let currentHref = here
    if (data?.next) {
      const name = data.next.version
      list.push({ name, label: `${name} (candidate)`, href: siteRoot + data.next.path })
    }
    if (data?.latest) {
      const name = minor(data.latest)
      list.push({ name, label: `${name} (latest)`, href: siteRoot })
    }
    for (const folder of data?.versions ?? []) {
      if (data?.latest && minor(folder.version) === minor(data.latest)) {
        if (here === siteRoot + folder.path) currentHref = siteRoot
        continue
      }
      const name = minor(folder.version)
      list.push({ name, label: name, href: siteRoot + folder.path })
    }
    let current = list.find((option) => option.href === currentHref)
    if (!current) {
      const name = !buildVersion ? 'Unreleased' : isCandidate(buildVersion) ? buildVersion : minor(buildVersion)
      current = { name, label: name, href: here }
      list.unshift(current)
    }
    return { list, current }
  })

  // Shown on an archived build of an older minor, and on the release candidate.
  const banner = computed<Banner | null>(() => {
    const data = versions.value
    if (!data || !buildVersion || site.value.base === siteRoot) return null
    if (isCandidate(buildVersion)) return { kind: 'candidate', version: buildVersion, latest: data.latest }
    if (data.latest && olderMinor(buildVersion, data.latest)) {
      return { kind: 'older', minor: minor(buildVersion), latest: minor(data.latest) }
    }
    return null
  })

  // Another version is another VitePress app, so this is a full page load. It
  // keeps the current page when the chosen version has it (one HEAD request),
  // and goes to that version's home page otherwise.
  async function open(href: string) {
    const base = site.value.base
    const page = location.pathname.startsWith(base) ? location.pathname.slice(base.length) : ''
    let target = href
    if (page) {
      try {
        if ((await fetch(href + page, { method: 'HEAD' })).ok) target = href + page + location.hash
      } catch {
        // Unreachable: go to the home page.
      }
    }
    location.assign(target)
  }

  return { options, banner, open }
}
