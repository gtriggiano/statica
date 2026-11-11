# Releasing statica

This is the runbook for maintainers. Contributors only need the [commit message rules](CONTRIBUTING.md#commit-messages-and-pull-request-titles).

## How a release works

- Every pull request is squash-merged with a Conventional Commits title, so each commit on `main` says what kind of change it is.
- [release-please](https://github.com/googleapis/release-please) keeps a **Release PR** open, titled `chore(main): release X.Y.Z`. It holds the next version, the new section of `CHANGELOG.md` and the version references in the documentation, and it updates itself on every push to `main`. When nothing visible to users has changed since the last release, there is no Release PR.
- **Merging the Release PR is the release.** The Release workflow then:
  1. creates the tag `vX.Y.Z` and a draft GitHub Release with the changelog section;
  2. builds with [GoReleaser](https://goreleaser.com/) the archives for Linux and macOS on amd64 and arm64, the checksums, the SBOMs and the image `ghcr.io/gtriggiano/statica:X.Y.Z` for linux/amd64 and linux/arm64, from the same binaries; in parallel it builds the documentation of this version;
  3. attests the provenance of the archives and of the image;
  4. runs the image and the archives on amd64 and arm64 runners;
  5. moves the image tags `X.Y`, `X` and `latest` to the verified digest, adds an Install section to the notes and publishes the release, which becomes immutable;
  6. publishes the documentation site.

Everything up to step 5 happens on a draft: a failed job can run again. After step 5 the tag and the assets never change; a defect is fixed with a new version.

The first version is `1.0.0`. A breaking change bumps the major version, and everything else follows the table in CONTRIBUTING.md.

## One-time repository setup

These settings are in place; check them when something behaves unexpectedly.

- **General**: squash merge only, with the default message "Pull request title and description"; release immutability on.
- **Actions**: actions pinned to full commit SHAs; workflow permissions read-only; GitHub Actions may not create or approve pull requests.
- **GitHub App `statica-release`**, installed on this repository only, with Contents, Issues and Pull requests read and write. It creates the Release PR, the tags and the releases: events created with `GITHUB_TOKEN` would not start the CI on the Release PR.
- **Environment `release`**, limited to the branch `main`, with the secret `RELEASE_APP_PRIVATE_KEY` (the App's private key) and the variable `RELEASE_APP_CLIENT_ID`.
- **GitHub Pages** with GitHub Actions as source. The environment `github-pages` allows the branch `main` and the tags `v*` (the tags only for recovery runs).
- **Ruleset `main`**: pull request required, squash only, linear history, no force push or deletion, branches up to date before merging, and these required checks: `Go checks`, `Go tests (linux/arm64)`, `Go tests (macOS)`, `Go vulnerabilities`, `Release dry run`, `Build documentation`, `PR title`. `Workflow audit` is not required: a new advisory about an action can turn it red without any change in the pull request. Fix what it reports before merging anyway.
- **Ruleset `release tags`** on `v*`: creation, update and deletion restricted, with the App as the only bypass actor. Nobody creates a release tag by hand.
- **Package `statica` on ghcr.io**: public, linked to this repository.

## Release a new version

1. Merge pull requests as usual. The Release PR updates itself.
2. When you want to release, open the Release PR and check:
   - the **version** in the title;
   - the new section of **`CHANGELOG.md`**; to fix an entry, see [Fix a version or a changelog entry](#fix-a-version-or-a-changelog-entry);
   - the **version references** updated in `README.md` and `docs/`, and the **checks** green.
3. Optionally, write an introduction at the top of the Release PR description just before merging: the release notes are taken from there. To have it in `CHANGELOG.md` too, push a commit to the Release PR branch. Do both last: release-please rewrites the Release PR on every push to `main`.
4. **Squash and merge.** Follow the Release workflow in the Actions tab.
5. When it ends, the release is marked Latest, the site shows the new version, and `docker pull ghcr.io/gtriggiano/statica:latest` gets the new image.

`deps:` updates from Dependabot keep a patch Release PR open most of the time. There is no obligation to release right away: release at once for a security fix, otherwise let the Release PR collect changes.

## Release a candidate

A release candidate lets people try a version before it is released, for example before a major version or before a minor version with large changes. It is optional.

1. Make sure the Release PR is up to date: release-please has run on the latest commit of `main`.
2. Actions → **Release** → **Run workflow** → branch `main` → select **candidate**.
3. The workflow creates the tag `vX.Y.Z-rc.N` on the current `main`, with `N` one more than the last candidate of that version, and runs the same pipeline. The result is a pre-release (never Latest), the image `X.Y.Z-rc.N` (the floating tags do not move) and the documentation under `/statica/next/`.
4. If the candidate has problems, fix them with normal pull requests and run step 2 again. When it is good, release as above: the final version comes from the current `main`.

Candidates do not go through release-please, so on `main` `CHANGELOG.md` and the version references in the documentation keep showing the last release. The documentation built for the candidate, under `/statica/next/`, shows the candidate's version in every image tag and installation command. A candidate cut before the first release, such as `1.0.0-rc.1`, is published, but the **Documentation site** job fails: the site root is built from the latest release, and there is none yet. Its documentation is never published: once the first release exists, the candidate is older than it, and `/statica/next/` only holds a candidate newer than the latest release.

## Fix a version or a changelog entry

- **A merged pull request has the wrong title** (a missing `!`, a `feat` that was a `fix`): edit the description of that merged pull request and add, at the end:

  ```text
  BEGIN_COMMIT_OVERRIDE
  fix(cache): count revalidated entries once
  END_COMMIT_OVERRIDE
  ```

  release-please uses the text between the markers instead of the commit message. Then run the Release workflow on `main`, or wait for the next merge: the Release PR updates itself.
- **Force a version**: merge a pull request whose description ends with `Release-As: X.Y.Z`, alone in the last paragraph.

## When something goes wrong

| Situation | What to do |
| --- | --- |
| The checks fail on the Release PR | Fix it on `main` with a normal pull request; the Release PR updates itself. |
| `build` or `Documentation archive` fails before the image is pushed | Re-run the failed jobs: the tag and the draft exist, GoReleaser reuses the draft and replaces the assets already uploaded. |
| `build` fails after pushing `X.Y.Z` | Re-run the job: the version tag is pushed again (the digest can change) and attested again. This is the only case in which `X.Y.Z` moves; it is harmless because the version is not announced and `latest` and `X.Y` still point to the previous release. |
| `build` stops because the tag does not point to the commit of the run | Run the Release workflow on the tag (see [Recovery run](#recovery-run-on-a-tag)). |
| `verify` fails: an artifact is broken | Do not publish and do not delete the tag: release-please finds the last release by its tag, and without it the next changelog would list every commit (the tag ruleset prevents it anyway). Leave the draft unpublished, delete the version `X.Y.Z` of the package on ghcr.io (no floating tag points to it), fix it with a `fix:` pull request and release the next patch. In that pull request, mark the `X.Y.Z` section of `CHANGELOG.md` as not published. |
| `verify` fails on a release candidate | Leave the draft, fix it on `main`, run the next candidate. |
| `publish` stops because the image is not public | Make the package public (Package settings → Change visibility → Public) and re-run the job. |
| `publish` fails halfway | Re-run it: every step can run twice (the floating tags are rewritten, the Install section is added only once). |
| `Documentation site` fails | Run the Documentation workflow on `main` with **deploy** selected. The release stays valid. |
| A defect is found after publishing | Never rewrite a tag or an asset: with immutable releases a tag name cannot be used again. Release a patch and, if useful, add a warning to the notes of the broken release (the text stays editable). |

## Recovery run on a tag

Actions → **Release** → **Run workflow** → choose the tag `vX.Y.Z` or `vX.Y.Z-rc.N` instead of a branch. The workflow skips release-please and runs the pipeline for that tag, with the tag's commit as the commit of the run. It works only while the release is a draft: the build job refuses a published release. If the documentation deploy is rejected in a recovery run, publish the site with the Documentation workflow instead.

## The documentation site

- The site root, `https://gtriggiano.github.io/statica/`, describes the latest release. Each released minor version has a folder, `/statica/vX.Y/`, built from its latest patch; `/statica/next/` holds the latest release candidate while it is newer than the latest release. A version selector lists them.
- Every release attaches the build of its own documentation, `statica_X.Y.Z_docs.tar.gz`. The deploy builds the root from the latest release tag and composes the folders from those archives, so a published version never changes and old tags are never rebuilt.
- Each version of the site names its own version in every image tag and download command. A release builds its documentation from its tag, whose version blocks the Release PR has updated; the build fails if `DOCS_VERSION` and `.release-please-manifest.json` disagree. A release candidate is built from `main`, and the build writes the candidate's version into the version blocks.
- The site is never published from `main`: pages there can describe unreleased behavior. A documentation-only fix goes online with the next release.
- To publish the site again, for example after a GitHub Pages incident, run the Documentation workflow on `main` with **deploy** selected.

## Periodic maintenance

- **Dependabot pull requests**: weekly, and monthly for the base image; versions newer than 7 days are not proposed, except for security updates. Review and merge them. `deps:` ones make the Release PR propose a patch release; `docs(deps-dev):` and `ci:` ones do not.
- **Go version**: after each Go security release, open a `deps: bump Go to 1.27.N` pull request that updates `go.mod`. `README.md`, `CONTRIBUTING.md` and the quickstart also name the Go version, outside the version references that release-please updates.
- **Security workflow**: if `govulncheck` reports a reachable vulnerability in the latest release, fix it (usually with a `deps:` update) and release a patch. The image scan results are in the Security tab.
- **Pinned tools**: the versions of `govulncheck`, `actionlint`, `zizmor`, GoReleaser and `syft` are Makefile variables that Dependabot does not update; the last three are also inputs of the workflows. Bump them together, by hand.
- **Scorecard**: check the score and its alerts in the Security tab.

## Verify a release

```sh
# The release, with the attestation GitHub creates for immutable releases
gh release verify vX.Y.Z --repo gtriggiano/statica
gh release verify-asset vX.Y.Z statica_X.Y.Z_linux_amd64.tar.gz --repo gtriggiano/statica

# The provenance of an archive and of the image, built by the Release workflow
gh attestation verify statica_X.Y.Z_linux_amd64.tar.gz --repo gtriggiano/statica \
  --signer-workflow gtriggiano/statica/.github/workflows/release.yml
gh attestation verify oci://ghcr.io/gtriggiano/statica:X.Y.Z --repo gtriggiano/statica \
  --signer-workflow gtriggiano/statica/.github/workflows/release.yml
```

`--signer-workflow` limits the check to the Release workflow: without it, an attestation made by any workflow of the repository would pass.
