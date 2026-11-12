// The pages name the statica release they document with this placeholder, in
// image tags, download URLs and asset names, and the build replaces it: a page
// can name the version more than once on a line, and no release PR has to edit
// the pages. A Go test, internal/release/versions_test.go, rejects a literal
// version in their place. Files included with <<< are not replaced.
export const placeholder = '{{VERSION}}'

export function setReleaseVersion(source: string, version: string): string {
  return source.replaceAll(placeholder, version)
}
