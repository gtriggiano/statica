package release

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The release PR updates every version reference in the documentation to the
// new version. release-please's generic updater replaces, on each line between
// these markers, only the first SemVer string, and only in the files listed in
// extra-files. This test fails, before a release, on a reference that the
// release PR would leave behind or corrupt.
const (
	blockStart = "<!-- x-release-please-start-version -->"
	blockEnd   = "<!-- x-release-please-end -->"
)

var (
	repoRoot = filepath.Join("..", "..")

	// Like release-please's own pattern, without word boundaries: it also
	// finds the 1.27.3 in go1.27.3.
	semver = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?`)

	// References to a released version: the image tag, the VERSION variable of
	// the download commands, a download URL and an asset name.
	references = []*regexp.Regexp{
		regexp.MustCompile(`ghcr\.io/gtriggiano/statica:(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`),
		regexp.MustCompile(`\bVERSION=(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`),
		regexp.MustCompile(`releases/download/v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)/`),
		regexp.MustCompile(`statica_(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)_`),
	}

	// Any tag of the image. Besides a literal version in a block, the only
	// accepted tag is the VERSION variable: a floating tag such as latest, or a
	// minor version, would not name the release a page documents.
	imageTag    = regexp.MustCompile(`ghcr\.io/gtriggiano/statica:([^\s"'\x60)\]>]+)`)
	versionUse  = regexp.MustCompile(`\$\{?VERSION\b`)
	versionDecl = regexp.MustCompile(`^\s*VERSION=`)
)

// checkVersionReferences returns the problems of one Markdown file: a version
// reference outside a marked block, a version in a block different from the
// released one, a block line with more than one version, unbalanced markers,
// an image tag that is neither the version nor ${VERSION}, and a use of
// VERSION in a file that does not set it in a block. marked reports whether
// the file has at least one block.
func checkVersionReferences(name, content, version string) (problems []string, marked bool) {
	inBlock := false
	usesVersion, setsVersion := false, false
	for i, line := range strings.Split(content, "\n") {
		at := fmt.Sprintf("%s:%d", name, i+1)
		switch strings.TrimSpace(line) {
		case blockStart:
			if inBlock {
				problems = append(problems, at+": block opened inside another block")
			}
			inBlock, marked = true, true
			continue
		case blockEnd:
			if !inBlock {
				problems = append(problems, at+": block closed without being opened")
			}
			inBlock = false
			continue
		}
		if strings.Contains(line, "x-release-please") {
			problems = append(problems, at+": use the block markers on lines of their own")
			continue
		}
		for _, m := range imageTag.FindAllStringSubmatch(line, -1) {
			if tag := m[1]; !semver.MatchString(tag) && tag != "${VERSION}" && tag != "$VERSION" {
				problems = append(problems, fmt.Sprintf("%s: image tag %q: write the version in a block, or ${VERSION}", at, tag))
			}
		}
		if inBlock && versionDecl.MatchString(line) {
			setsVersion = true
		} else if versionUse.MatchString(line) {
			usesVersion = true
		}
		if inBlock {
			found := semver.FindAllString(line, -1)
			if len(found) > 1 {
				problems = append(problems, fmt.Sprintf("%s: %d versions on one line in a block (%s): release-please replaces only the first", at, len(found), strings.Join(found, ", ")))
			}
			for _, v := range found {
				if v != version {
					problems = append(problems, fmt.Sprintf("%s: version %s in a block, want %s (the version in .release-please-manifest.json)", at, v, version))
				}
			}
			continue
		}
		for _, re := range references {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				problems = append(problems, fmt.Sprintf("%s: %q is outside a block: release-please would not update it", at, m[0]))
			}
		}
	}
	if inBlock {
		problems = append(problems, name+": block not closed")
	}
	if usesVersion && !setsVersion {
		problems = append(problems, name+": uses VERSION but does not set it with VERSION=<version> in a block")
	}
	return problems, marked
}

func TestCheckVersionReferences(t *testing.T) {
	const version = "0.2.0"
	block := func(lines ...string) string {
		return blockStart + "\n" + strings.Join(lines, "\n") + "\n" + blockEnd + "\n"
	}
	for _, tc := range []struct {
		name, content string
		wantProblem   string // empty: no problem
		wantMarked    bool
	}{
		{name: "no references", content: "Install statica.\n"},
		{name: "references in a block", content: block("VERSION=0.2.0", "docker pull ghcr.io/gtriggiano/statica:0.2.0"), wantMarked: true},
		{name: "reference without a literal version", content: block("VERSION=0.2.0") + "curl -LO .../releases/download/v${VERSION}/statica_${VERSION}_linux_amd64.tar.gz\n", wantMarked: true},
		{name: "image tag from VERSION", content: block("VERSION=0.2.0") + "docker pull \"ghcr.io/gtriggiano/statica:${VERSION}\"\n", wantMarked: true},
		{name: "VERSION used but not set", content: "docker pull ghcr.io/gtriggiano/statica:$VERSION\n", wantProblem: "does not set it"},
		{name: "VERSION set outside a block", content: "VERSION=0.2.0\ndocker pull ghcr.io/gtriggiano/statica:$VERSION\n", wantProblem: "outside a block"},
		{name: "floating image tag", content: "docker pull ghcr.io/gtriggiano/statica:latest\n", wantProblem: `image tag "latest"`},
		{name: "minor image tag", content: "image: ghcr.io/gtriggiano/statica:0.2\n", wantProblem: `image tag "0.2"`},
		{name: "image name without a tag", content: "The image `ghcr.io/gtriggiano/statica` is public, pinned as `ghcr.io/gtriggiano/statica@sha256:…`.\n"},
		{name: "image reference outside a block", content: "docker pull ghcr.io/gtriggiano/statica:0.2.0\n", wantProblem: "outside a block"},
		{name: "asset name outside a block", content: "tar -xzf statica_0.2.0_linux_amd64.tar.gz\n", wantProblem: "outside a block"},
		{name: "version changed by hand", content: block("VERSION=0.1.9"), wantProblem: "version 0.1.9 in a block", wantMarked: true},
		{name: "two versions on a line", content: block("VERSION=0.2.0 # needs Go 1.27.3"), wantProblem: "2 versions on one line", wantMarked: true},
		{name: "another version in a block", content: block("VERSION=0.2.0", "go install golang.org/dl/go1.27.3"), wantProblem: "version 1.27.3 in a block", wantMarked: true},
		{name: "unclosed block", content: blockStart + "\nVERSION=0.2.0\n", wantProblem: "block not closed", wantMarked: true},
		{name: "inline marker", content: "VERSION=0.2.0 <!-- x-release-please-version -->\n", wantProblem: "lines of their own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems, marked := checkVersionReferences("page.md", tc.content, version)
			if marked != tc.wantMarked {
				t.Errorf("marked = %v, want %v", marked, tc.wantMarked)
			}
			if tc.wantProblem == "" {
				if len(problems) > 0 {
					t.Errorf("unexpected problems: %q", problems)
				}
				return
			}
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.wantProblem) }) {
				t.Errorf("problems %q, want one containing %q", problems, tc.wantProblem)
			}
		})
	}
}

// TestRepositoryVersionReferences checks README.md and every page in docs/.
func TestRepositoryVersionReferences(t *testing.T) {
	version := manifestVersion(t)
	extraFiles := configuredExtraFiles(t)

	files := []string{"README.md"}
	err := filepath.WalkDir(filepath.Join(repoRoot, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".vitepress") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".md") {
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(repoRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		problems, marked := checkVersionReferences(name, string(data), version)
		for _, p := range problems {
			t.Error(p)
		}
		if marked && !slices.Contains(extraFiles, name) {
			t.Errorf("%s has version blocks but is not in extra-files in release-please-config.json: the release PR would not update it", name)
		}
	}
	for _, name := range extraFiles {
		if _, err := os.Stat(filepath.Join(repoRoot, name)); err != nil {
			t.Errorf("extra-files in release-please-config.json lists %s: %v", name, err)
		}
	}
}

func manifestVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, ".release-please-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]string
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	version := manifest["."]
	if !semver.MatchString(version) {
		t.Fatalf(".release-please-manifest.json: version of %q is %q, want a SemVer version", ".", version)
	}
	return version
}

func configuredExtraFiles(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "release-please-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Packages map[string]struct {
			ExtraFiles []json.RawMessage `json:"extra-files"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, raw := range config.Packages["."].ExtraFiles {
		// An entry is a path, or an object with a type and a path.
		var path string
		if err := json.Unmarshal(raw, &path); err != nil {
			var entry struct {
				Type string `json:"type"`
				Path string `json:"path"`
			}
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatalf("release-please-config.json: extra-files entry %s: %v", raw, err)
			}
			if entry.Type != "generic" {
				t.Errorf("release-please-config.json: extra-files entry %s: want type generic", raw)
			}
			path = entry.Path
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		t.Fatal("release-please-config.json: no extra-files for package .")
	}
	return paths
}
