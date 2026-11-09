// Assembles the published documentation site from the root build and the
// documentation archives attached to the releases. Node built-ins and the
// system tar only.
//
// Usage, in two steps (the workflow downloads the archives in between):
//
//   gh api --paginate --slurp repos/OWNER/REPO/releases > releases.json
//   node assemble-site.mjs plan --releases releases.json [--root-tag vX.Y.Z] > plan.json
//   (build the site root from plan.root.tag with DOCS_BASE=DOCS_SITE_ROOT, and
//    download each plan.archives[].asset of plan.archives[].tag into ARCHIVES)
//   node assemble-site.mjs assemble --plan plan.json --root DIST --archives ARCHIVES --out SITE
//
// plan reads the releases (a JSON array, or the array of pages of --slurp) and
// prints the plan: the tag the root is built from (--root-tag, or by default
// the latest release that is not a release candidate) and the archives to
// download. Drafts and tags other than vX.Y.Z and vX.Y.Z-rc.N do not count; a
// published release without its statica_X.Y.Z_docs.tar.gz is skipped with a
// warning. It fails when there is no release to build the root from.
//
// assemble copies the root build to SITE, extracts each archive into its
// folder and writes SITE/versions.json:
//
//   SITE/               the root build: the latest release, the indexed site
//   SITE/vX.Y/          one folder per released minor, from its highest patch
//                       (the minor of the root too)
//   SITE/next/          the latest release candidate, only when it is newer
//                       than the release at the root
//   SITE/versions.json  read by the version selector of every folder
//
// versions.json (docs/public/versions.json is the empty list of every build):
//
//   {
//     "latest": "0.2.1",                                 // version at the root, or null
//     "versions": [{ "version": "0.2.1", "path": "v0.2/" },  // one per minor folder,
//                  { "version": "0.1.3", "path": "v0.1/" }], // newest first
//     "next": { "version": "0.3.0-rc.1", "path": "next/" }   // or null
//   }
//
// Paths are relative to the site root and end with "/".

import { spawnSync } from 'node:child_process'
import { cpSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { parseArgs } from 'node:util'

function fail(message) {
  console.error(`assemble-site: ${message}`)
  process.exit(1)
}

const warn = (message) => console.error(`::warning::assemble-site: ${message}`)

// [major, minor, patch, rc]; a release sorts after its candidates.
function parseTag(tag) {
  const match = /^v(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?$/.exec(tag)
  if (!match) return null
  const [major, minor, patch, rc] = match.slice(1).map((n) => (n === undefined ? Infinity : Number(n)))
  return { tag, version: tag.slice(1), minor: `${major}.${minor}`, candidate: rc !== Infinity, key: [major, minor, patch, rc] }
}

const newer = (a, b) => {
  const i = a.key.findIndex((n, index) => n !== b.key[index])
  return i !== -1 && a.key[i] > b.key[i]
}
const newestFirst = (a, b) => (newer(a, b) ? -1 : newer(b, a) ? 1 : 0)
const assetOf = (release) => `statica_${release.version}_docs.tar.gz`

function plan(releasesFile, rootTag) {
  const releases = JSON.parse(readFileSync(releasesFile, 'utf8')).flat()
  const published = []
  for (const release of releases) {
    if (release.draft) continue
    const parsed = parseTag(release.tag_name)
    if (!parsed) {
      warn(`ignoring release ${release.tag_name}: not a vX.Y.Z or vX.Y.Z-rc.N tag`)
      continue
    }
    parsed.hasArchive = (release.assets ?? []).some((asset) => asset.name === assetOf(parsed))
    published.push(parsed)
  }
  published.sort(newestFirst)

  const stable = published.filter((release) => !release.candidate)
  const root = rootTag ? stable.find((release) => release.tag === rootTag) : stable[0]
  if (!root) {
    fail(
      rootTag
        ? `${rootTag} is not a published release vX.Y.Z`
        : 'no published release vX.Y.Z: the site root is built from the latest release, so there is nothing to deploy yet',
    )
  }

  const archives = []
  const minors = new Set()
  for (const release of published) {
    if (!release.hasArchive) {
      warn(`skipping ${release.tag}: the release has no ${assetOf(release)}`)
      continue
    }
    let path
    if (release.candidate) {
      // Only the newest candidate, and only when it is newer than the root.
      if (archives.some((archive) => archive.path === 'next/') || !newer(release, root)) continue
      path = 'next/'
    } else {
      if (minors.has(release.minor)) continue
      minors.add(release.minor)
      path = `v${release.minor}/`
    }
    archives.push({ tag: release.tag, version: release.version, asset: assetOf(release), path })
  }
  return { root: { tag: root.tag, version: root.version }, archives }
}

function assemble({ plan: planFile, root, archives, out }) {
  const { root: rootRelease, archives: list } = JSON.parse(readFileSync(planFile, 'utf8'))
  if (!existsSync(join(root, 'index.html'))) fail(`no build in ${root}`)
  if (existsSync(out) && readdirSync(out).length > 0) fail(`${out} is not empty`)
  cpSync(root, out, { recursive: true })
  for (const { asset, path } of list) {
    const archive = join(archives, asset)
    const folder = join(out, path)
    if (!existsSync(archive)) fail(`missing archive ${archive}`)
    if (existsSync(folder)) fail(`the root build already has ${path}`)
    mkdirSync(folder)
    const tar = spawnSync('tar', ['-xzf', archive, '-C', folder], { stdio: 'inherit' })
    if (tar.status !== 0) fail(`cannot extract ${archive}`)
    if (!existsSync(join(folder, 'index.html'))) fail(`${asset} has no index.html`)
    console.log(`assemble-site: ${path} from ${asset}`)
  }
  const folder = ({ version, path }) => ({ version, path })
  const versions = {
    latest: rootRelease.version,
    versions: list.filter((archive) => archive.path !== 'next/').map(folder),
    next: list.filter((archive) => archive.path === 'next/').map(folder)[0] ?? null,
  }
  writeFileSync(join(out, 'versions.json'), `${JSON.stringify(versions, null, 2)}\n`)
  console.log(`assemble-site: site root from ${rootRelease.tag}; ${list.length} version folder(s) in ${out}`)
}

const commands = {
  plan: { releases: { type: 'string' }, 'root-tag': { type: 'string', default: '' } },
  assemble: { plan: { type: 'string' }, root: { type: 'string' }, archives: { type: 'string' }, out: { type: 'string' } },
}
const [command, ...rest] = process.argv.slice(2)
if (!Object.hasOwn(commands, command)) fail('usage: assemble-site.mjs plan|assemble [options] (see the comment at the top of the script)')
const options = commands[command]
let values
try {
  ;({ values } = parseArgs({ args: rest, options }))
} catch (error) {
  fail(error.message)
}
for (const name of Object.keys(options)) if (values[name] === undefined) fail(`${command}: --${name} is required`)

if (command === 'plan') console.log(JSON.stringify(plan(values.releases, values['root-tag']), null, 2))
else assemble(values)
