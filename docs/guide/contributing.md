# Contributing

statica is developed in the [GitHub repository](https://github.com/gtriggiano/statica). Bug reports, documentation fixes and code changes go there, as issues and pull requests. Read [CONTRIBUTING.md](https://github.com/gtriggiano/statica/blob/main/CONTRIBUTING.md) before you start.

- **Bug reports.** Include the output of `statica version`, the relevant part of the configuration and the log lines. Remove credentials, bucket names and hostnames you do not want to publish.
- **Documentation.** Documentation pages link to their source with "Edit this page on GitHub". A statement about behavior must match the code.
- **Code.** Include tests for the behavior you change and update the documentation in the same change.
- **Pull request titles.** Pull requests are squash-merged, and the title becomes the commit message that decides the next version and the changelog entry. It follows [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/), such as `fix(cache): count revalidated entries once`; CONTRIBUTING.md lists the allowed types and how to mark a breaking change.

## Build and check

You need Go 1.27.3 or later, Git and Make (`make fmt` and `make check` list the Go files with `git ls-files` and fail outside a Git work tree), and for the documentation Node.js 24 and Google Chrome or Chromium.

```sh
make check         # Go formatting, modules, vet, tests and build
make docs-install  # documentation dependencies, once
make docs-build    # build the site; fails on dead links and anchors
make docs-check    # render every page and diagram in Chrome
```

`make check` runs the tests with the race detector, which on Linux and Windows needs cgo and a C compiler. `make docs-check` looks for Google Chrome or Chromium in the standard install locations on Linux, macOS and Windows; set `CHROME_PATH` to use another browser binary.

## Documentation conventions

- Every Mermaid diagram declares `accTitle` and `accDescr`; the build fails without them. The description is not shown on the page: screen readers read it in place of the diagram.
- A statica configuration snippet is a `yaml` fence titled `config.yaml`. A Go test in `make check` validates every such snippet, so keep them valid.
