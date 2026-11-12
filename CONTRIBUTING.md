# Contributing

**statica** is developed at <https://github.com/gtriggiano/statica>. Report bugs and propose changes there, as issues and pull requests.

A bug report is most useful with the output of `statica version`, the relevant part of the configuration and the log lines; remove credentials, bucket names and hostnames you do not want to publish.

A change in behavior comes with tests that cover it and with the documentation updated in the same pull request.

## Requirements

- **Go 1.27.4**, as declared in `go.mod`. An installed Go 1.21 or later downloads this toolchain automatically when it has network access, unless `GOTOOLCHAIN=local` is set.
- **A C compiler on Linux and Windows.** `make check` runs the tests with the race detector, which needs cgo there. On macOS it does not.
- **Make and Git.** The Makefile drives every check. `make fmt` and `make check` list the Go sources with Git, so run them in a Git clone: outside a Git work tree, `make fmt` and `make fmt-check` (part of `make check`) fail with `not inside a Git work tree`.
- **Node.js 24 and npm** for the documentation (`docs/.node-version`).
- **uv**, optional, for `make workflows-audit`, which runs [zizmor](https://docs.zizmor.sh/) with `uvx`. CI runs it too, with zizmor's online audits.
- **Docker with buildx**, for `make release-snapshot` only.
- **Google Chrome or Chromium** for `make docs-check`. It uses `CHROME_PATH` or `CHROME_BIN` when set, and otherwise finds Google Chrome or Chromium in the standard install locations on Linux, macOS and Windows. For any other browser binary, set `CHROME_PATH` or `CHROME_BIN`.

The test suite needs no AWS credentials, Docker or S3 endpoint. The HTTP tests bind temporary loopback ports and create temporary certificates, so the environment must allow local listening sockets. The first build downloads the Go modules, and `make vuln`, `make workflows-check`, `make workflows-audit`, the first `make release-check` or `make release-snapshot` and the first `make docs-install` download their tools or packages: they need network access.

## Repository layout

```text
cmd/statica/         Command entry point
internal/            Implementation packages (not a public Go API); see the code map below
docs/                Documentation site: guide/, architecture/, reference/
docs/examples/       Complete configuration examples, validated by the tests
docs/scripts/        Link and diagram checks, and the assembly of the site with versions
docs/.vitepress/     Site configuration, navigation and theme
.github/             CI workflows, Dependabot configuration, issue and pull request templates
.goreleaser.yaml     Release packaging: binaries, archives, checksums, SBOMs, image
Dockerfile           Container image, built from the binaries GoReleaser compiles
AGENTS.md            Instructions for AI coding agents; CLAUDE.md imports it for Claude Code
.agents/skills/      Agent skills for contributors; .claude/skills/ links them for Claude Code
skills/              Agent skills for users of statica, published by the documentation site
```

### Code map

Each package under `internal/` has one role. Read [the architecture overview](docs/architecture/index.md) before a change that crosses packages: it follows a request through the components below.

| Package | Role | Explained in |
| --- | --- | --- |
| `cmd/statica/` | Entry point. | |
| `internal/cli/` | Commands: `serve`, `config validate`, `config print`, `config check-origin`, `version`. | [CLI reference](docs/reference/cli.md) |
| `internal/config/` | Loads and validates the YAML: `types.go` (fields, defaults), `parse.go`, `validate.go`. | [Configuration reference](docs/reference/configuration.md) |
| `internal/app/` | Builds every component from a validated configuration, binds the listeners, drains on shutdown. | [Startup, readiness and shutdown](docs/architecture/lifecycle.md) |
| `internal/httpserver/` | Public listener: admission, host allowlist, client IP and request ID, CORS, response writer. | [Request flow](docs/architecture/request-flow.md), [Security model](docs/architecture/security.md), [Timeouts](docs/architecture/timeouts.md) |
| `internal/redirect/` | Compiled redirect rules. | [Request flow](docs/architecture/request-flow.md) |
| `internal/site/` | Path validation, key confinement to the publication prefix, key resolution, precompressed variants. | [Request flow](docs/architecture/request-flow.md), [Precompressed variants](docs/architecture/precompressed-variants.md), [SPA fallback](docs/architecture/spa-fallback.md) |
| `internal/content/` | Object responses: conditional requests, ranges, headers, streaming. | [Ranges and conditional requests](docs/architecture/ranges-and-conditionals.md) |
| `internal/httpcache/` | RFC 9111 mechanics: `Cache-Control` grammar, freshness, age. | [Cache-Control and Age](docs/architecture/cache-control.md) |
| `internal/cache/` | Bounded memory cache: store, lookup, shared fills, readers. | [Memory cache](docs/architecture/memory-cache.md) |
| `internal/storage/` | Read-only origin interface; `internal/storage/s3/` is the AWS SDK v2 adapter. | [Access denied](docs/architecture/access-denied.md) |
| `internal/telemetry/` | Per-request record and logfmt events. | [Log reference](docs/reference/logs.md) |
| `internal/metrics/` | Prometheus metrics on a private registry. | [Metrics reference](docs/reference/metrics.md) |
| `internal/admin/` | Admin listener (`/livez`, `/readyz`, `/metrics`) and the origin health worker. | [Admin endpoints](docs/reference/admin-endpoints.md) |
| `internal/release/` | Tests only: version references in the documentation. | [Pages guarded by tests](#pages-guarded-by-tests) |
| `internal/agentdocs/` | Tests only: consistency of `AGENTS.md`, `CLAUDE.md` and the skills. | [AI coding agents](#ai-coding-agents) |

## Design rules

These hold across the code base. A change that weakens one needs the maintainer's agreement first: open an issue before the pull request.

- **The origin is read-only.** statica sends only `GetObject` and `HeadObject`. It never lists, writes or deletes objects.
- **Keys stay inside the publication prefix.** `internal/site` validates paths and confines keys. It does no filesystem cleaning and no URL re-decoding.
- **The `Host` never selects content.** It is used for admission and telemetry only: never in an S3 request, an object key or a cache entry.
- **Configuration is immutable.** Everything is built and validated before any socket opens; there is no reload.
- **Every resource has a configured ceiling, and statica refuses work instead of queueing it.** A new resource that clients can consume needs a limit.
- **An origin failure never falls back** to an index, single-page-app or 404 object, and an origin 403 is a denial, never a missing object.
- **No secrets or request data in errors.** Configuration errors never repeat configured values; values from `${ENV}` substitution are redacted. Redirect errors include no URL or captured values.
- **`internal/` is not a public API.** The public interfaces are the configuration file, the CLI, documented HTTP behavior, the admin endpoints, metric names and labels, log events and fields, the container image and the release asset names. Changing one of them is a breaking change (see [Commit messages and pull request titles](#commit-messages-and-pull-request-titles)).

## Make targets

| Target | What it does |
| --- | --- |
| `make build` | Builds `bin/statica`. |
| `make check` | Checks Go formatting and module tidiness, runs `go vet`, runs the tests with the race detector, and builds the binary. |
| `make fmt` | Formats the Go sources. |
| `make fuzz` | Fuzzes request-target parsing and object-key confinement for 10 seconds; set `FUZZ_TIME`, for example `make fuzz FUZZ_TIME=30s`. |
| `make vuln` | Scans the Go code with `govulncheck`, at the version pinned in the Makefile. |
| `make workflows-check` | Lints `.github/workflows/*.yml` with `actionlint`, at the version pinned in the Makefile, without ShellCheck. |
| `make workflows-audit` | Audits the workflows and the Dependabot configuration with `zizmor`, at the version pinned in the Makefile, without its online audits. Needs `uvx`. |
| `make release-check` | Validates `.goreleaser.yaml` with GoReleaser, at the version pinned in the Makefile. |
| `make release-snapshot` | Builds what a release would publish into the ignored `dist/` directory, without publishing it: the binaries and archives for Linux and macOS on amd64 and arm64, the checksums, the SBOMs, and one image per platform, loaded into Docker as `ghcr.io/gtriggiano/statica:<version>-amd64` and `-arm64`. Needs Docker; `make release-snapshot SNAPSHOT_FLAGS=--skip=docker` builds everything except the images. |
| `make docs-install` | Installs the locked documentation dependencies (`npm ci`). |
| `make docs-audit` | Runs `npm audit` on the documentation dependencies, failing at moderate severity or higher. |
| `make docs-dev` | Serves the documentation with live reload, on `http://127.0.0.1:5173/statica/` by default. |
| `make docs-build` | Builds the site into `docs/.vitepress/dist/` and checks its internal links and anchors. |
| `make docs-check` | Renders every page of the last build, except `404.html`, in headless Chrome and checks its diagrams. |
| `make docs-preview` | Serves the built site, on `http://127.0.0.1:4173/statica/` by default. |
| `make ci` | Runs `check`, `fuzz`, `vuln` and `workflows-check`, then `docs-install`, `docs-audit`, `docs-build` and `docs-check`. |

Run `make check` for every Go change. Run `make docs-build` then `make docs-check` for every documentation change, and `make check` as well when the change touches a configuration snippet, a file in `docs/examples/`, or the configuration or metrics reference: Go tests guard them (see [Pages guarded by tests](#pages-guarded-by-tests)). `make ci` runs the checks of the CI and Documentation workflows on your machine, except the Linux cross-compilation, the tests on the other platforms, the workflow audit, which `make workflows-audit` runs without the online audits, and the release dry run, which `make release-snapshot` runs.

The `govulncheck`, `actionlint`, `zizmor`, GoReleaser and `syft` versions are Makefile variables, which Dependabot does not update: review and bump them by hand. The last three are also inputs in `.github/workflows/ci.yml` and `.github/workflows/release.yml` (`version` of `zizmor-action` and `goreleaser-action`, `syft-version` of `download-syft`): change them together. `make release-check` and `make release-snapshot` build GoReleaser and `syft` once per version into the ignored `.cache/tools/`.

To keep the Go and npm caches inside the checkout, for example in a restricted environment, point them at the ignored `.cache/` directory:

```sh
export GOMODCACHE="$PWD/.cache/go-mod"
export GOCACHE="$PWD/.cache/go-build"
export npm_config_cache="$PWD/.cache/npm"
```

## Tests

- Cover each behavior change with a behavioral test of the contract: HTTP semantics, confinement to the publication prefix, failure modes and resource limits. A test of the observable result is worth more than a test of internals.
- Keep the suite passing with the race detector (`make check`).
- Fuzz seed cases run with the ordinary tests; `make fuzz` runs the bounded fuzzing.
- Never put production credentials, `.env` files, customer hostnames or private bucket names in fixtures. Use `example.com` hostnames, `example-*` buckets and documentation IP ranges.

## Documentation

The documentation is a [VitePress](https://vitepress.dev/) site in `docs/`.

### Content

- The code is the source of truth. Every statement about behavior, and every diagram, must match what the code does today. Do not document planned behavior.
- Write in US English, in the second person and the present tense, with sentence-case headings and the imperative for steps. The name is statica, always lowercase: set it in bold at its first mention in the prose of each page, and in code formatting only for the command or the image. No superlatives and no "simply", "just" or "easy". Make no performance claims and no claims that an S3 provider is qualified.
- Use only public interfaces in the user documentation: configuration keys, CLI commands and flags, log events and fields, metric names and labels, admin endpoints, HTTP headers and statuses. No Go package, type or test names.
- The site follows [Diátaxis](https://diataxis.fr/): `docs/guide/` holds the quickstart and the how-to guides, `docs/architecture/` explains the mechanisms, and `docs/reference/` lists every field, command, response, log event and metric. Keep each fact in one home (defaults and allowed values in Reference, rules and mechanisms in Architecture) and link to it from the guides.
- Add a new page to the sidebar in `docs/.vitepress/navigation.ts`.

### Formatting

- Write each paragraph and each list item of a Markdown file on one line, and let your editor wrap it for display: never break a line in the middle of a sentence. Use a hard line break, a `\` at the end of a line, only where the layout needs one, as between the `**Type:**` and `**Default:**` lines of the configuration reference.
- Write a configuration key with its full dotted path at its first mention on a page (`cache.max_bytes`), and values in code (`30s`, `128MiB`, `true`).
- Link between pages with relative links to `.md` files, to a heading anchor when one exists: `[shared fills](../architecture/memory-cache.md#shared-fills)`.
- Put commands in `sh` blocks without a prompt, and their output in `text` blocks.
- Use example data only: `example.com` hostnames, `example-*` buckets, the documentation IP ranges (192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24, 2001:db8::/32) and RFC 1918 ranges for proxies. Never credentials.
- Use `::: tip` for advice, `::: warning` for surprising behavior and `::: danger` for a security impact.

### Pages guarded by tests

`make check` runs Go tests that read the documentation:

- **Configuration reference.** Every configuration field has its own heading in `docs/reference/configuration.md`, the exact field path in backticks, followed by its `**Type:**` and `**Default:**` lines and, when validation narrows the type, an `**Allowed:**` line. The default is a YAML value in backticks or one of `required`, `unset — <what statica does then>`, ``inherits `<field>` `` and ``derived from `<field>` ``. The allowed line starts, where it applies, with a form that the test checks against the code: a list of values (`` `off` or `precompressed` ``), a range (`` `1` to `3` ``), `positive`, `zero or positive` or `nonempty`; other text follows after `; `. Each top-level section starts with a table of its fields that repeats these lines. The test fails on a missing or unknown field, on a documented type, default or allowed form that differs from the code, on a list of values that differs from the enumeration of the configuration schema, on a string or list that rejects its empty value and has no allowed line, and on a table that differs from the entries. Text after `; `, an allowed line in free text and the prose are not checked, except for the rules between two fields that the test names: verify them against the code.
- **Metrics reference.** Every exported `statica_` metric family has its section and its summary row in `docs/reference/metrics.md`, with its type, labels and buckets. The test fails in both directions.
- **Configuration snippets.** A statica configuration snippet in `docs/` is a fence whose info string is exactly `yaml [config.yaml]`. The test validates every such snippet; a partial fragment is completed with an `s3.bucket`. Write that title nowhere else in `docs/`, not even in prose, and give other YAML another title, such as `yaml [prometheus.yml]`.
- **Version references.** A page in `docs/` names the release it documents with the placeholder `{{VERSION}}`, in an image tag `ghcr.io/gtriggiano/statica:{{VERSION}}`, a download URL or an asset name. The build replaces it with the release that the site documents, so readers see commands they can paste as they are. Do not write the version itself, a `VERSION` shell variable, or an image tag that is `latest` or a minor version. A file included with `<<<` is not replaced: keep version references out of `docs/examples/`. `README.md` is read on GitHub as it is, so it names the version itself, in a block between the lines `<!-- x-release-please-start-version -->` and `<!-- x-release-please-end -->`, with one version per line and no other version in the block: in the Release PR, release-please replaces the first version of each line of a block. A test in `internal/release` fails on any of these.
- **Configuration schema.** `docs/public/config.schema.json`, the JSON Schema that the site publishes for editors and other tools, is generated from the configuration types, the defaults and the notes in `internal/config/json_schema_test.go`. `TestJSONSchema` fails when the file differs: add a note for a new field, then regenerate the file with `go test ./internal/config -run TestJSONSchema -update`. Two more tests check that the schema accepts every example and configuration snippet of the documentation, and that each of its rules rejects a file that statica rejects too. The schema states only what a file states on its own: leave out a rule that involves two fields or the environment.
- **Examples.** `TestRepositoryExamples` in `internal/config/config_test.go` loads and validates every file in `docs/examples/` as a complete configuration; the only environment variable an example can read is `SITE_BUCKET`. Pages include the files with `<<< @/examples/<name>.yaml{yaml}` instead of copying them.

### Diagrams

Diagrams are Mermaid, in ` ```mermaid ` fences.

- Every diagram declares `accTitle` and `accDescr`, or the build fails. The description is not shown on the page: screen readers read it in place of the diagram, so it must summarize the diagram on its own. Introduce each diagram with a sentence before it.
- Use no `style`, `classDef` or colors: the theme provides them. A frontmatter `config` block is allowed for layout settings only, such as the width of a sequence diagram.
- Aim for the 670 px content width: top-down flowcharts of about 10 nodes at most, and sequence diagrams of at most 4 participants with short messages.

### Build and checks

- `make docs-build` fails on a link to a missing page and on a `#fragment` that matches no element id in the target page. It also fails on an address of the published site, `https://gtriggiano.github.io/statica/...`, written in a page or in a skill for users, that is not a file of the build.
- `make docs-check` serves the build and renders every page except `404.html` in headless Chrome, in the light and dark themes, at desktop and mobile widths. It fails when a diagram does not render, on a console or page error, on an HTTP error, and when a page is wider than the viewport. A wide diagram scrolls inside its own frame.
- The build also writes the pages for AI agents ([llmstxt.org](https://llmstxt.org/)): a Markdown twin of every page at the path of its source, such as `guide/quickstart.md`, and `llms.txt`, an index of the twins in the order of the sidebar, with the first paragraph of each page as its description. Every HTML page links to both from its `<head>`. `docs/.vitepress/llms.ts` rewrites what only VitePress understands (frontmatter, `<<<` and `<!--@include-->` files, `{{VERSION}}`, containers and code groups) and fails the build on a construct it does not know, such as a new container type: teach it there. Give a page `llms: optional` in its frontmatter to list it in the section that an agent can skip, and a `description` when its first paragraph does not describe it.
- The site base is `/statica/`. Build for another base with `DOCS_BASE`, for example `DOCS_BASE=/ make docs-build`, and run `make docs-check` with the same value.
- The published site has a folder per released version (see [RELEASING.md](RELEASING.md#the-documentation-site)). Two more variables shape a build: `DOCS_SITE_ROOT`, the root of the whole site, which defaults to `DOCS_BASE`, and `DOCS_VERSION`, the release that the build documents, empty for local builds. With `DOCS_VERSION` set, the pages name that version instead of the one in `.release-please-manifest.json`: it must match `.release-please-manifest.json`, unless it is a release candidate, whose documentation is built from `main`. A build whose `DOCS_BASE` is below `DOCS_SITE_ROOT` is an archived version: it is not indexed by search engines, has no "Edit this page" link, and shows a banner when a newer version exists. Check that variant with, for example, `DOCS_SITE_ROOT=/statica/ DOCS_BASE=/statica/v0.1/ make docs-build`, then `make docs-check` with the same variables. The version selector reads `versions.json` from the site root; `docs/public/versions.json` is an empty placeholder that the deploy replaces.
- The documentation dependencies are pinned in `docs/package.json` and locked in `docs/package-lock.json`. Commit both together, and install with `make docs-install`. An npm override in `docs/package.json` selects Vite 6.4.6, outside the Vite range that VitePress 1.6.4 declares. Check `make docs-dev`, `make docs-build` and `make docs-preview` whenever you change the override or upgrade VitePress, and remove the override once VitePress no longer needs it.

## Commit messages and pull request titles

Pull requests are merged with squash merge only: each one becomes a single commit on `main`, whose message is the pull request title (GitHub appends the pull request number) and whose body is the pull request description. [release-please](https://github.com/googleapis/release-please) reads these commits to compute the next version and to write `CHANGELOG.md`, so the title follows [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):

```text
<type>(<optional scope>)<optional !>: <description>
```

The **PR title** check rejects any other title, any type not in this table and a description that starts with an uppercase letter or ends with a period, and the pull request cannot be merged until it passes.

| Type | Use it for | Version bump | Changelog section |
| --- | --- | --- | --- |
| `feat` | A capability users can see: a configuration key, a command, a metric, a header | minor | Features |
| `fix` | A correction of wrong behavior | patch | Bug Fixes |
| `perf` | A performance improvement with no change in behavior | patch | Performance Improvements |
| `deps` | A dependency update that changes the binary or the image: Go modules, the Go version, the base image | patch | Dependencies |
| `revert` | The revert of an earlier change | patch | Reverts |
| `docs` | Documentation only | none | hidden |
| `refactor`, `test`, `build`, `ci`, `chore` | Internal work that changes nothing statica distributes | none | hidden |

Only the visible types, and breaking changes of any type, make release-please propose a release: a series of `docs:` and `ci:` commits alone does not. Among the commits since the last release, the highest bump wins.

- **Breaking changes.** A change that breaks a public interface (the configuration file, the CLI, documented HTTP behavior, the admin endpoints, metric names and labels, log events and fields, the container image or the release asset names) takes a `!` after the type or scope, as in `feat(config)!: rename cache.ttl to cache.fresh_for`. A breaking change to the configuration renames or removes a key, and never gives an existing key a new meaning: statica then rejects an old file instead of reading it differently. Explain the migration in a footer: end the description with a paragraph that starts with `BREAKING CHANGE: ` followed by the instructions, which appear in the changelog, and write nothing after it.
- **Lines that release-please reads anywhere in the description.** A line that starts with `BREAKING CHANGE:`, or that contains `BREAKING-CHANGE:` anywhere, marks the change as breaking wherever it is, and a paragraph that starts with a type and a colon, such as `fix: `, becomes an extra changelog entry with its own bump. Start such lines only when you mean them.
- **Scopes** are optional and not checked. The suggested ones follow the public areas: `cache`, `config`, `cli`, `http`, `s3`, `admin`, `metrics`, `logs`, `tls`, `image`, `docs`.
- **The description** is the changelog entry: write it in the imperative, starting with a lowercase letter and without a final period.
- **Reverts.** GitHub's **Revert** button proposes the title `Revert "fix(cache): …"`, which the check rejects: rename it to `revert: fix(cache): …`.

The pull request template repeats these rules. Dependabot titles already follow them (see [Dependabot](#dependabot)).

## AI coding agents

`AGENTS.md` condenses this guide for AI coding agents: the commands, the checks to run for each kind of change, the design rules and the conventions. Codex, Antigravity, Cursor, GitHub Copilot and other tools read it directly; Claude Code reads it through `CLAUDE.md`, which imports it. Procedures for recurring tasks, such as adding a configuration field, are skills in `.agents/skills/`, in the [Agent Skills](https://agentskills.io/) format. Claude Code does not read that directory, so each skill has a symbolic link in `.claude/skills/` to its directory in `.agents/skills/`. `.claude/settings.json` lets Claude Code run the Make checks without asking and keeps its file tools away from the local credential files.

- **Keep them in step with this guide.** A pull request that changes a rule here also updates `AGENTS.md` and the skills that state it. This guide wins when they disagree.
- **Add a skill** in `.agents/skills/<name>/SKILL.md`, link it for Claude Code with `ln -s ../../.agents/skills/<name> .claude/skills/<name>`, and list it in the table at the end of `AGENTS.md`.
- **Symbolic links on Windows.** Git checks the links out as plain text files unless the clone supports symbolic links: turn on Developer Mode, or work as an administrator, and clone with `git clone -c core.symlinks=true`. Without them Claude Code does not find the skills; the other tools and the tests are not affected.
- **Tests guard them.** `make check` runs the tests in `internal/agentdocs`. They fail when a skill in `.agents/skills/` has no link in `.claude/skills/` or its link leads elsewhere, when `AGENTS.md` does not list a skill, and when `CLAUDE.md` does not import `AGENTS.md`. They also fail when one of these files names something that does not exist: a path under `cmd/`, `internal/`, `docs/`, `.github/`, `.agents/` or `.claude/`, a Go file, a Make target, a test, or a relative link or its anchor. Other names, such as Go identifiers, are not checked.
- **The skill for users is separate.** `skills/deploy-statica/SKILL.md` is for people who deploy statica, not for contributors: the documentation build publishes it at the site root under `.well-known/agent-skills/`, with an index that names its digest, and [Configure statica with an AI agent](docs/guide/ai-agents.md) tells users how to install it. It is read away from the repository, so it links to the published documentation by URL and names no file of the repository. Update it in the pull request that changes how statica is configured, deployed or verified. A skill there is one `SKILL.md`, with its description on one line; the tests in `internal/agentdocs` check both.
- **Personal instructions stay local.** Git ignores `CLAUDE.local.md`, `.claude/settings.local.json` and `AGENTS.override.md`.

## Releases

Maintainers release statica by merging the Release PR that release-please keeps open; the Release workflow then builds, verifies and publishes the archives, the container image and the documentation site. [RELEASING.md](RELEASING.md) describes the process. A contribution needs nothing beyond a correct pull request title: its changelog entry and its effect on the version come from it.

## Continuous integration

| Workflow | Runs on | Jobs |
| --- | --- | --- |
| PR title (`.github/workflows/pr-title.yml`) | Pull requests: opened, edited, reopened, and on every new commit | **PR title**: checks the title against Conventional Commits, the allowed types and the form of the description. It checks out nothing and reads only the title through the API, which the read-only token of a pull request from a fork allows. |
| CI (`.github/workflows/ci.yml`) | Pull requests, pushes to `main`, manual runs | **Go checks**: `make check`, `make fuzz`, a Linux `amd64` and `arm64` cross-compilation without cgo, `make workflows-check`. **Go tests (linux/arm64)** and **Go tests (macOS)**: `make check` on an arm64 Linux runner and on a macOS runner. **Release dry run**: `goreleaser check`, then a snapshot build of every archive, SBOM and per-platform image, and a run of the `linux/amd64` image with `version` and `config validate`. **Workflow audit**: `zizmor` on the workflows and the Dependabot configuration, with its online audits; it is not a required check, because a new advisory about an action can turn it red without any change in the pull request, but fix what it reports before merging. **Go vulnerabilities**: `make vuln`. |
| Documentation (`.github/workflows/docs.yml`) | Pull requests, pushes to `main`, manual runs, and the Release workflow | **Build documentation** (pull requests and pushes): `make docs-install`, `make docs-audit`, `make docs-build` and `make docs-check` with the runner's Chrome, the same for an archived version, and a test of `docs/scripts/assemble-site.mjs` on those builds. **Assemble documentation** and **Deploy documentation** (deploys only): the site root built from the latest release, the older versions from the documentation archives of the releases, and the publication to GitHub Pages. |
| Security (`.github/workflows/security.yml`) | Every week, manual runs | **Go vulnerabilities (main)**: `make vuln`. **Latest release**: finds the latest stable release, not counting release candidates. **Go vulnerabilities (latest release)**: `make vuln` on its tag. **Image scan (latest release)**: Grype on its `linux/amd64` image, with the results in the repository's Security tab. |
| Scorecard (`.github/workflows/scorecard.yml`) | Pushes to `main`, every week | **Scorecard analysis**: [OpenSSF Scorecard](https://github.com/ossf/scorecard), with the results published for the README badge and in the repository's Security tab. |
| Release (`.github/workflows/release.yml`) | Pushes to `main`, manual runs | **Release PR**: release-please, with the token of the release App. **Release candidate**: manual runs with **candidate** selected. **Prepare**, **Build**, **Documentation archive**, **Verify (linux/amd64)**, **Verify (linux/arm64)**, **Publish** and **Documentation site** run only for a new tag, or for a manual run on a tag. See [RELEASING.md](RELEASING.md). |

The Release workflow publishes the documentation site after each release; the site is never built from `main`. To publish it again, run the Documentation workflow on `main` with **deploy** selected. The site root is `/<repository name>/`, or the repository variable `DOCS_BASE` when it is set.

Actions are pinned to commit SHAs, checkouts do not persist credentials, and every workflow starts with no permissions; each job gets only the ones it needs, read-only unless stated here. In the Release workflow, the jobs that build, upload and publish a release can write the repository contents and the packages, and the build job requests an OpenID Connect token to sign the attestations; the Release PR and the release candidate use the token of the release App. The documentation deploy can write to Pages, and the image scan and Scorecard can upload results to the Security tab; Scorecard also requests an OpenID Connect token to publish its results.

## Dependabot

Dependabot checks the Go modules, the documentation's npm packages (version updates grouped in one pull request) and the GitHub Actions every week, and the base image of the `Dockerfile` every month. It proposes a new version only once it has been published for 7 days; security updates are not delayed. Its titles follow the [commit message format](#commit-messages-and-pull-request-titles): `deps:` for the Go modules and the base image, which make the Release PR propose a patch release, and `docs(deps-dev):` for the documentation packages and `ci:` for the actions, which do not. Review each update and run the checks it affects; commit npm updates together with `docs/package-lock.json`.
