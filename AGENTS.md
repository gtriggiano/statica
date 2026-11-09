# AGENTS.md

Instructions for AI coding agents working on statica. [CONTRIBUTING.md](CONTRIBUTING.md) is the authoritative guide for every contributor; this file condenses the parts an agent needs on every task and links to the rest. If the two disagree, CONTRIBUTING.md is right: fix this file in the same change.

statica is a web server that serves one S3 bucket, or a prefix inside it, as one static website, through a bounded in-memory cache. It is a single Go binary configured by one YAML file, with a separate admin listener for health checks and Prometheus metrics. The user documentation is a VitePress site in `docs/`.

## Commands

Go is the version in `go.mod`; an installed Go 1.21 or later downloads it unless `GOTOOLCHAIN=local` is set. The race detector in `make check` needs cgo and a C compiler on Linux and Windows. Run every command from the repository root.

| Command | What it does |
| --- | --- |
| `make check` | gofmt check, `go mod tidy -diff`, `go vet`, tests with the race detector, build. The gate for every Go change. |
| `make fmt` | Formats the Go sources. |
| `go test -race -count=1 ./internal/config -run TestStrictYAML` | One package or one test while iterating. Finish with `make check`. |
| `make fuzz` | Bounded fuzzing of request-target parsing and key confinement. |
| `make docs-install` | Installs the locked documentation dependencies (`npm ci`), once. |
| `make docs-build` | Builds the site and fails on a dead link or anchor. |
| `make docs-check` | Renders every built page in headless Chrome and checks its diagrams. Needs Chrome or Chromium. |
| `make workflows-check` | Lints `.github/workflows/*.yml` with actionlint. |
| `make ci` | Most of what CI runs: `make check`, `make fuzz`, `make vuln`, `make workflows-check`, then the documentation install, audit, build and check. |

`make fmt` and `make fmt-check` list files with `git ls-files`: they fail outside a Git work tree.

The tests need no AWS credentials, Docker or S3 endpoint. They bind loopback ports and create temporary certificates. The first build downloads Go modules, and `make vuln`, `make workflows-check`, `make workflows-audit`, `make docs-install` and the first `make release-check` download tools: they need network access. In a sandbox that cannot write the default caches, keep them in the ignored `.cache/` directory:

```sh
export GOMODCACHE="$PWD/.cache/go-mod" GOCACHE="$PWD/.cache/go-build" npm_config_cache="$PWD/.cache/npm"
```

### Checks to run before you finish

| You changed | Run |
| --- | --- |
| Go code | `make check` |
| A configuration field, a metric, `docs/examples/`, a `yaml [config.yaml]` snippet or a version reference | `make check`, then `make docs-build` and `make docs-check` |
| Other documentation | `make docs-build`, then `make docs-check` |
| `AGENTS.md`, `CLAUDE.md`, `.agents/` or `.claude/` | `make check` |
| `.github/workflows/` or `.github/dependabot.yml` | `make workflows-check`, and `make workflows-audit` when `uvx` is installed |
| `.goreleaser.yaml` or `Dockerfile` | `make release-check`, and `make release-snapshot` when Docker is available |

Report a check you could not run, and why, instead of skipping it silently.

## Repository map

```text
cmd/statica/            Entry point
internal/cli/           Commands: serve, config validate|print|check-origin, version
internal/config/        Load and validate the YAML: types.go (fields, defaults), parse.go, validate.go
internal/app/           Build every component from a validated config, bind listeners, drain on shutdown
internal/httpserver/    Public listener, admission, host allowlist, client IP and request ID, CORS, response writer
internal/redirect/      Compiled redirect rules
internal/site/          Path validation, key confinement to the publication prefix, key resolution, precompressed variants
internal/content/       Object responses: conditional requests, ranges, headers, streaming
internal/httpcache/     RFC 9111 mechanics: Cache-Control grammar, freshness, age
internal/cache/         Bounded memory cache: store, lookup, shared fills, readers
internal/storage/       Read-only origin interface; storage/s3/ is the AWS SDK v2 adapter
internal/telemetry/     Per-request record and logfmt events
internal/metrics/       Prometheus metrics on a private registry
internal/admin/         Admin listener (/livez, /readyz, /metrics) and the origin health worker
internal/release/       Tests only: version references in the docs
internal/agentdocs/     Tests only: consistency of this file, CLAUDE.md and the skills
docs/                   Site: guide/ (how-to), architecture/ (mechanisms), reference/ (every field, metric, event)
```

Read `docs/architecture/index.md` before a change that crosses packages: it explains the request path and the role of each component.

## Design rules

These hold across the code base. Do not weaken one without the maintainer's agreement.

- **The origin is read-only.** statica sends only `GetObject` and `HeadObject`. It never lists, writes or deletes objects.
- **Keys stay inside the publication prefix.** `internal/site` validates paths and confines keys. It does no filesystem cleaning and no URL re-decoding.
- **The `Host` never selects content.** It is used for admission and telemetry only: never in an S3 request, an object key or a cache entry.
- **Configuration is immutable.** Everything is built and validated before any socket opens; there is no reload.
- **Every resource has a configured ceiling, and statica refuses work instead of queueing it.** A new resource that clients can consume needs a limit.
- **An origin failure never falls back** to an index, single-page-app or 404 object, and an origin 403 is a denial, never a missing object.
- **No secrets or request data in errors.** Configuration errors never repeat configured values; values from `${ENV}` substitution are redacted. Redirect errors include no URL or captured values.
- **`internal/` is not a public API.** The public interfaces are the configuration file, the CLI, documented HTTP behavior, the admin endpoints, metric names and labels, log events and fields, the container image and the release asset names. Changing one of them is a breaking change (see [Pull requests](#pull-requests)).

## Code and tests

- Format with `make fmt`. There is no linter beyond `go vet`.
- Tests use the standard `testing` package only, with no assertion library. Reuse the existing fakes before writing new ones: the fake origin and fake clock in `internal/cache/harness_test.go`, the harness in `internal/app/harness_test.go`, and the helpers in the other tests of the package you change.
- Test the observable contract (HTTP semantics, key confinement, failure modes, resource limits) rather than internals. Every behavior change comes with a test, and the suite passes with the race detector.
- Use example data only: `example.com` hostnames, `example-*` buckets, the documentation IP ranges `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` and `2001:db8::/32`. Never real credentials, hostnames or bucket names.
- Ask before adding a Go module or an npm package. `docs/package.json` and `docs/package-lock.json` change together.

## Documentation

Every behavior change updates the documentation in the same pull request. The rules are in [CONTRIBUTING.md § Documentation](CONTRIBUTING.md#documentation) and the procedure in the `write-docs` skill; the ones agents break most often:

- Document what the code does today, never planned behavior, and mention only public interfaces: no Go package, type or test names.
- Write each paragraph and each list item on one line, in US English, second person and present tense. statica is always lowercase. No "simply", "just" or "easy", no superlatives, no performance claims.
- Keep each fact in its home (values in `docs/reference/`, mechanisms in `docs/architecture/`, tasks in `docs/guide/`) and link to it.
- Every `.md` file under `docs/` becomes a published page: put nothing else there, agent instructions included.
- Go tests in `make check` read the configuration and metrics references, the `yaml [config.yaml]` snippets, the release version blocks and `docs/examples/`, and fail on any drift from the code.

## Pull requests

Pull requests are squash-merged: the title becomes the commit on `main`, and release-please reads it to choose the next version and write the changelog. The full rules are in [CONTRIBUTING.md § Commit messages and pull request titles](CONTRIBUTING.md#commit-messages-and-pull-request-titles), and the procedure in the `prepare-pr` skill.

- Title: `<type>(<optional scope>)<optional !>: <description>`, with the description in the imperative, starting with a lowercase letter, without a final period.
- Types: `feat` (a capability users can see), `fix`, `perf`, `deps` (Go modules, Go version, base image), `revert`, `docs`, `refactor`, `test`, `build`, `ci`, `chore`. A check rejects any other.
- A breaking change takes `!` and ends the description with a paragraph that starts with `BREAKING CHANGE: ` and explains the migration. A configuration change renames or removes a key; it never gives an existing key a new meaning.
- In the description, start no line with `BREAKING CHANGE:`, `Release-As:` or a type followed by a colon, and write `BREAKING-CHANGE:` nowhere, unless you mean it: release-please reads such lines anywhere.
- Never edit `CHANGELOG.md` or `.release-please-manifest.json`, and never create tags: release-please and the Release workflow own them.

## CI, workflows and release tooling

- Actions are pinned to full commit SHAs, workflows start with no permissions and each job requests only what it needs, and checkouts do not persist credentials. Keep it that way in any workflow you touch.
- The govulncheck, actionlint, zizmor, GoReleaser and syft versions are Makefile variables. The last three are repeated as inputs in `.github/workflows/ci.yml` and `release.yml`: change them together.

## Boundaries

- Never read, print or commit `.env` files, `.aws/`, `config.local.yaml` or any credential.
- Never commit build output: `bin/`, `dist/`, `.cache/`, `docs/node_modules/`, `docs/.vitepress/dist/`.

## Code review

When you review a change, check that:

- a behavior change has its tests and its documentation in the same pull request;
- the design rules above still hold, and every new limit or resource is bounded and documented;
- a change to a public interface is marked as breaking, with a migration in the description;
- the documentation states each fact in its home, matches the code and describes no planned behavior;
- the pull request title has the type that matches what users get.

## Skills

Skills are step-by-step procedures for recurring tasks, in the [Agent Skills](https://agentskills.io/) format. Codex, Antigravity and other tools read them from `.agents/skills/`. Claude Code reads `.claude/skills/`, where each skill is a symbolic link to its directory in `.agents/skills/`. Use the skill that matches your task; if your tool does not load skills, read its `SKILL.md`.

| Skill | Use it to |
| --- | --- |
| `change-config-field` | Add, change, rename or remove a configuration field. |
| `change-metric` | Add, change, rename or remove a Prometheus metric. |
| `write-docs` | Write or edit a documentation page. |
| `prepare-pr` | Verify a change and write its pull request title and description. |

To change or add a skill, follow [CONTRIBUTING.md § AI coding agents](CONTRIBUTING.md#ai-coding-agents).
