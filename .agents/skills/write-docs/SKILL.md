---
name: write-docs
description: Write or edit a page of the statica documentation site in docs/, following its Diátaxis layout, its style rules, and the Go tests and build checks that read the pages. Use for any change under docs/, including the documentation that accompanies a code change.
---

# Write documentation

The rules are in `CONTRIBUTING.md` § Documentation. Read it before writing a new page; this skill is the procedure and the checklist.

## Steps

1. **Find the home of each fact.** The site follows Diátaxis:
   - `docs/reference/`: every configuration field, CLI command, HTTP response, log event, metric and admin endpoint, with defaults and allowed values;
   - `docs/architecture/`: how a mechanism works and the rules it follows;
   - `docs/guide/`: the quickstart and task-oriented how-to guides.

   Search for the fact before writing it, for example `git grep -n 'cache.max_bytes' -- docs`. Update its home and link to it; never state it a second time.
2. **Check the behavior in the code.** The code is the source of truth: document what it does today, never planned behavior.
3. **Write in the house style:**
   - one line per paragraph and per list item, never broken mid-sentence;
   - US English, second person, present tense, imperative steps, sentence-case headings, statica always lowercase and in bold at its first mention in the prose of each page;
   - no superlatives, no "simply", "just" or "easy", no performance claims, no claim that an S3 provider is qualified;
   - public interfaces only: configuration keys, CLI commands and flags, log events and fields, metric names and labels, admin endpoints, HTTP headers and statuses. No Go package, type or test names;
   - a configuration key with its full dotted path in code at its first mention on a page (`cache.max_bytes`), and values in code (`30s`, `128MiB`, `true`);
   - relative links to `.md` files, with a heading anchor when one exists: `[shared fills](../architecture/memory-cache.md#shared-fills)`;
   - commands in `sh` blocks without a prompt, their output in `text` blocks;
   - `::: tip` for advice, `::: warning` for surprising behavior, `::: danger` for a security impact;
   - example data only: `example.com`, `example-*` buckets, the documentation IP ranges, RFC 1918 ranges for proxies.
4. **Handle the blocks that tests read:**
   - **Configuration snippet:** a fence titled exactly `yaml [config.yaml]`. A test validates it, completing a partial fragment with an `s3.bucket`. Give any other YAML another title, such as `yaml [prometheus.yml]`, and never write that title in prose.
   - **Complete example:** a file in `docs/examples/`, included with `<<< @/examples/<name>.yaml{yaml}`. `TestRepositoryExamples` in `internal/config/config_test.go` loads every file there as a complete configuration.
   - **Version reference** (image tag, download URL, asset name): write the placeholder `{{VERSION}}`, as in `ghcr.io/gtriggiano/statica:{{VERSION}}`; the build replaces it with the release that the site documents. Never the version itself, a `VERSION` shell variable, or an image tag that is `latest` or a minor version. Keep version references out of files included with `<<<`, which are not replaced.
   - **Mermaid diagram:** introduced by a sentence, with `accTitle` and an `accDescr` that summarizes the diagram on its own, and no `style`, `classDef` or colors. Aim for top-down flowcharts of about 10 nodes and sequence diagrams of at most 4 participants.
5. **New page:** add it to the sidebar in `docs/.vitepress/navigation.ts`. Every `.md` file under `docs/` is published, so put nothing else there. Open the page with a paragraph that says what it covers: it becomes the description of the page in `llms.txt`, the index that AI agents read.

## Check

```sh
make docs-install   # once per clone, and after a change to docs/package-lock.json
make docs-build     # fails on a dead link or anchor
make docs-check     # renders every page and diagram in headless Chrome
```

Run `make check` as well when you touched a configuration snippet, `docs/examples/`, `docs/reference/configuration.md`, `docs/reference/metrics.md` or a version reference. `make docs-check` needs Google Chrome or Chromium, found in the standard install locations or through `CHROME_PATH` or `CHROME_BIN`; if none is available, say that the check did not run.
