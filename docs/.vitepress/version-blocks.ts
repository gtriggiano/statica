// Every reference to a released version in the documentation (image tag,
// download URLs, asset names) sits in a block between these markers, which
// release-please updates to the new version in the release PR; a Go test,
// internal/release/versions_test.go, keeps it so. A build that documents a
// release (DOCS_VERSION) shows that version in the blocks: a release candidate
// is built from main, whose blocks still name the last release. As
// release-please does, it replaces the first version on each line of a block.
const blockStart = '<!-- x-release-please-start-version -->'
const blockEnd = '<!-- x-release-please-end -->'
const semver = /\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?/

export function setBlockVersions(source: string, version: string): string {
  let inBlock = false
  return source
    .split('\n')
    .map((line) => {
      const marker = line.trim()
      if (marker === blockStart) inBlock = true
      else if (marker === blockEnd) inBlock = false
      else if (inBlock) return line.replace(semver, version)
      return line
    })
    .join('\n')
}
