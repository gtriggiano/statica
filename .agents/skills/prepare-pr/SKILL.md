---
name: prepare-pr
description: Verify a finished statica change and write its pull request title and description in the Conventional Commits form that the PR title check and release-please require. Use before opening or updating a pull request, and when asked for a commit message.
---

# Prepare a pull request

Pull requests are squash-merged. The title becomes the commit message on `main` and the changelog entry, and its type decides the next version. The description becomes the commit body. The rules are in `CONTRIBUTING.md` § Commit messages and pull request titles.

## 1. Verify the change

- Run the checks for what you changed, from the table "Checks to run before you finish" in `AGENTS.md`, and keep their result for the description.
- A behavior change includes its tests and its documentation in the same diff.
- `git status` shows only the files of the change: no credentials, no scratch notes or plans, no generated output.

## 2. Choose the type

| Type | When | Release |
| --- | --- | --- |
| `feat` | A capability users can see: a configuration key, a command, a metric, a header | minor |
| `fix` | A correction of wrong behavior | patch |
| `perf` | A performance improvement with no change in behavior | patch |
| `deps` | A dependency update that changes the binary or the image: Go modules, the Go version, the base image | patch |
| `revert` | The revert of an earlier change | patch |
| `docs` | Documentation only | none |
| `refactor`, `test`, `build`, `ci`, `chore` | Internal work that changes nothing statica distributes | none |

A change that mixes code and documentation takes the type of the code. The PR title check rejects any other type.

## 3. Write the title

`<type>(<optional scope>)<optional !>: <description>`

- The description is the changelog entry: imperative, starting with a lowercase letter, without a final period, about what users get rather than how the code changed.
- Scopes are optional: `cache`, `config`, `cli`, `http`, `s3`, `admin`, `metrics`, `logs`, `tls`, `image`, `docs`.
- A change that breaks a public interface (configuration file, CLI, documented HTTP behavior, admin endpoints, metric names and labels, log events and fields, container image, release asset names) adds `!`: `feat(config)!: rename cache.ttl to cache.fresh_for`.
- A revert is titled `revert: <original title>`, not GitHub's `Revert "…"`.

## 4. Write the description

Follow `.github/pull_request_template.md` and delete its comment. Say what changes and why, and list the checks you ran.

release-please reads the description too. Anywhere in it:

- a line that starts with `BREAKING CHANGE:`, or contains `BREAKING-CHANGE:`, marks the change as breaking;
- a paragraph that starts with a type and a colon, such as `fix: `, adds a changelog entry with its own version bump;
- a final paragraph `Release-As: X.Y.Z` forces a version.

Start no line that way unless you mean it. For a breaking change, end the description with one paragraph that starts with `BREAKING CHANGE: ` and gives the migration steps, with nothing after it.

## 5. Leave release files alone

Never edit `CHANGELOG.md` or `.release-please-manifest.json` and never create a tag: release-please and the Release workflow own them.
