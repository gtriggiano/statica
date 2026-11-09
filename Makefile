GO ?= go
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt
NPM ?= npm
BUILD_DIR ?= bin
GOVULNCHECK_VERSION := v1.9.0
ACTIONLINT_VERSION := v1.7.12
# Keep equal to the version input of zizmor-action in .github/workflows/ci.yml.
ZIZMOR_VERSION := 1.32.0
UVX ?= uvx
# Keep equal to the version inputs of goreleaser-action and download-syft in
# .github/workflows/ci.yml and release.yml.
GORELEASER_VERSION := v2.19.0
SYFT_VERSION := v1.55.0
TOOLS_DIR := $(CURDIR)/.cache/tools
GORELEASER := $(TOOLS_DIR)/goreleaser-$(GORELEASER_VERSION)/goreleaser
SYFT_DIR := $(TOOLS_DIR)/syft-$(SYFT_VERSION)
SNAPSHOT_FLAGS ?=
FUZZ_TIME ?= 10s

.PHONY: help build fmt fmt-check vet test fuzz mod-check check ci vuln workflows-check workflows-audit release-check release-snapshot docs-install docs-audit docs-dev docs-build docs-check docs-preview

help:
	@echo "make check           Check Go formatting, modules, vet, tests and build"
	@echo "make ci              Run Go checks, vulnerability scan, workflow lint, docs build and checks"
	@echo "make build           Build statica in bin/"
	@echo "make fmt             Format Go sources"
	@echo "make fuzz            Fuzz request-target and key safety for $(FUZZ_TIME)"
	@echo "make vuln            Scan Go code using pinned govulncheck"
	@echo "make workflows-check Validate GitHub Actions workflows"
	@echo "make workflows-audit Audit workflows and Dependabot configuration using pinned zizmor (needs uv)"
	@echo "make release-check   Validate the GoReleaser configuration"
	@echo "make release-snapshot Build archives, checksums, SBOMs and images in dist/ without publishing (needs Docker)"
	@echo "make docs-install    Install locked documentation dependencies"
	@echo "make docs-audit      Check documentation dependency advisories"
	@echo "make docs-dev        Serve documentation locally"
	@echo "make docs-build      Build the static documentation site and check its links"
	@echo "make docs-check      Render every built page in Chrome and check its diagrams"
	@echo "make docs-preview    Preview the built documentation site"

build:
	$(GO) build -trimpath -o $(BUILD_DIR)/statica ./cmd/statica

fmt:
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "make fmt: not inside a Git work tree; run gofmt -w on the Go files instead" >&2; exit 1; }
	git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 "$(GOFMT)" -w

fmt-check:
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "make fmt-check: not inside a Git work tree; run gofmt -l on the Go files instead" >&2; exit 1; }
	@files=$$(git ls-files -z --cached --others --exclude-standard -- '*.go' | xargs -0 "$(GOFMT)" -l) || exit $$?; if [ -n "$$files" ]; then echo "Go files need formatting:"; echo "$$files"; exit 1; fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -race -count=1 ./...

fuzz:
	$(GO) test ./internal/site -run='^$$' -fuzz=FuzzParseTarget -fuzztime=$(FUZZ_TIME) -parallel=2

mod-check:
	$(GO) mod tidy -diff

check: fmt-check mod-check vet test build

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

workflows-check:
	$(GO) run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -shellcheck= .github/workflows/*.yml

workflows-audit:
	@command -v $(UVX) >/dev/null 2>&1 || { echo "make workflows-audit: $(UVX) not found; install uv from https://docs.astral.sh/uv/" >&2; exit 1; }
	$(UVX) zizmor@$(ZIZMOR_VERSION) --offline .

# GoReleaser and syft are built once per version into .cache/tools/; GoReleaser
# runs syft from PATH to write the SBOMs of the archives.
$(GORELEASER):
	GOBIN=$(dir $@) $(GO) install github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION)

$(SYFT_DIR)/syft:
	GOBIN=$(SYFT_DIR) $(GO) install github.com/anchore/syft/cmd/syft@$(SYFT_VERSION)

release-check: $(GORELEASER)
	$(GORELEASER) check

release-snapshot: $(GORELEASER) $(SYFT_DIR)/syft
	PATH="$(SYFT_DIR):$$PATH" $(GORELEASER) release --snapshot --clean $(SNAPSHOT_FLAGS)

ci: check fuzz vuln workflows-check
	$(MAKE) docs-install
	$(MAKE) docs-audit
	$(MAKE) docs-build
	$(MAKE) docs-check

docs-install:
	$(NPM) --prefix docs ci

docs-audit:
	$(NPM) --prefix docs audit --audit-level=moderate

docs-dev:
	$(NPM) --prefix docs run docs:dev

docs-build:
	$(NPM) --prefix docs run docs:build

docs-check:
	$(NPM) --prefix docs run docs:check

docs-preview:
	$(NPM) --prefix docs run docs:preview
