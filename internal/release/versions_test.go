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

// README.md is read as it is on GitHub, so it names the released version
// literally. The release PR updates it: release-please's generic updater
// replaces, on each line between these markers, only the first SemVer string,
// and only in the files listed in extra-files.
//
// The pages in docs/ write the placeholder instead, which the documentation
// build replaces with the release it documents (docs/.vitepress/config.mts):
// a page can name the version more than once on a line, as in a download URL,
// and no release PR has to touch it.
const (
	blockStart  = "<!-- x-release-please-start-version -->"
	blockEnd    = "<!-- x-release-please-end -->"
	placeholder = "{{VERSION}}"
)

var (
	repoRoot = filepath.Join("..", "..")

	// Like release-please's own pattern, without word boundaries: it also
	// finds the 1.27.3 in go1.27.3.
	semver = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?`)

	// References to a released version: the image tag, a download URL and an
	// asset name.
	references = []*regexp.Regexp{
		regexp.MustCompile(`ghcr\.io/gtriggiano/statica:(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)`),
		regexp.MustCompile(`releases/download/v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)/`),
		regexp.MustCompile(`statica_(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)_`),
	}

	// Any tag of the image: a floating tag such as latest, or a minor version,
	// would not name the release a page documents.
	imageTag = regexp.MustCompile(`ghcr\.io/gtriggiano/statica:(\{\{VERSION\}\}|[0-9A-Za-z._$\{\}-]+)`)

	// A shell variable that stands for the version: readers get commands they
	// can paste as they are.
	versionVariable = regexp.MustCompile(`\bVERSION=|\$\{?VERSION\b`)
)

// checkReadmeReferences returns the problems of a file that names the version
// literally: a version reference outside a marked block, a version in a block
// different from the released one, a block line with more than one version,
// unbalanced markers, an image tag that is not a full version and a VERSION
// shell variable. marked reports whether the file has at least one block.
func checkReadmeReferences(name, content, version string) (problems []string, marked bool) {
	inBlock := false
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
		if versionVariable.MatchString(line) {
			problems = append(problems, at+": a VERSION shell variable: write the version itself, in a block")
		}
		for _, m := range imageTag.FindAllStringSubmatch(line, -1) {
			if tag := m[1]; !semver.MatchString(tag) {
				problems = append(problems, fmt.Sprintf("%s: image tag %q: write the version, in a block", at, tag))
			}
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
	return problems, marked
}

// checkPageReferences returns the problems of a page in docs/: a literal
// version in an image tag, a download URL or an asset name, an image tag that
// is not the placeholder, a VERSION shell variable and a release-please
// marker, which nothing updates there.
func checkPageReferences(name, content string) (problems []string) {
	for i, line := range strings.Split(content, "\n") {
		at := fmt.Sprintf("%s:%d", name, i+1)
		if strings.Contains(line, "x-release-please") {
			problems = append(problems, at+": a release-please marker: no release PR updates the pages in docs/, write "+placeholder)
		}
		if versionVariable.MatchString(line) {
			problems = append(problems, at+": a VERSION shell variable: write "+placeholder)
		}
		for _, m := range imageTag.FindAllStringSubmatch(line, -1) {
			if tag := m[1]; tag != placeholder && !semver.MatchString(tag) {
				problems = append(problems, fmt.Sprintf("%s: image tag %q: write %s", at, tag, placeholder))
			}
		}
		for _, re := range references {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				problems = append(problems, fmt.Sprintf("%s: %q names a version: write %s", at, m[0], placeholder))
			}
		}
	}
	return problems
}

func TestCheckReadmeReferences(t *testing.T) {
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
		{name: "references in a block", content: block("docker pull ghcr.io/gtriggiano/statica:0.2.0", "tar -xzf statica_0.2.0_linux_amd64.tar.gz"), wantMarked: true},
		{name: "VERSION variable", content: block("VERSION=0.2.0") + "docker pull ghcr.io/gtriggiano/statica:$VERSION\n", wantProblem: "VERSION shell variable", wantMarked: true},
		{name: "floating image tag", content: "docker pull ghcr.io/gtriggiano/statica:latest\n", wantProblem: `image tag "latest"`},
		{name: "minor image tag", content: "image: ghcr.io/gtriggiano/statica:0.2\n", wantProblem: `image tag "0.2"`},
		{name: "image name without a tag", content: "The image `ghcr.io/gtriggiano/statica` is public, pinned as `ghcr.io/gtriggiano/statica@sha256:…`.\n"},
		{name: "image reference outside a block", content: "docker pull ghcr.io/gtriggiano/statica:0.2.0\n", wantProblem: "outside a block"},
		{name: "asset name outside a block", content: "tar -xzf statica_0.2.0_linux_amd64.tar.gz\n", wantProblem: "outside a block"},
		{name: "version changed by hand", content: block("IMAGE=ghcr.io/gtriggiano/statica:0.1.9"), wantProblem: "version 0.1.9 in a block", wantMarked: true},
		{name: "two versions on a line", content: block("curl -LO .../releases/download/v0.2.0/statica_0.2.0_linux_amd64.tar.gz"), wantProblem: "2 versions on one line", wantMarked: true},
		{name: "another version in a block", content: block("docker pull ghcr.io/gtriggiano/statica:0.2.0", "go install golang.org/dl/go1.27.3"), wantProblem: "version 1.27.3 in a block", wantMarked: true},
		{name: "unclosed block", content: blockStart + "\ndocker pull ghcr.io/gtriggiano/statica:0.2.0\n", wantProblem: "block not closed", wantMarked: true},
		{name: "inline marker", content: "docker pull ghcr.io/gtriggiano/statica:0.2.0 <!-- x-release-please-version -->\n", wantProblem: "lines of their own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems, marked := checkReadmeReferences("README.md", tc.content, version)
			if marked != tc.wantMarked {
				t.Errorf("marked = %v, want %v", marked, tc.wantMarked)
			}
			checkProblems(t, problems, tc.wantProblem)
		})
	}
}

func TestCheckPageReferences(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		wantProblem   string // empty: no problem
	}{
		{name: "no references", content: "Install statica.\n"},
		{name: "placeholder before punctuation", content: "Use `ghcr.io/gtriggiano/statica:{{VERSION}}`, or ghcr.io/gtriggiano/statica:{{VERSION}}.\n"},
		{name: "placeholder", content: "docker pull ghcr.io/gtriggiano/statica:{{VERSION}}\ncurl -LO .../releases/download/v{{VERSION}}/statica_{{VERSION}}_linux_amd64.tar.gz\n"},
		{name: "image name without a tag", content: "The image `ghcr.io/gtriggiano/statica` is public, pinned as `ghcr.io/gtriggiano/statica@sha256:…`.\n"},
		{name: "another version", content: "You need Go 1.27.3 or later, and a port such as 127.0.0.1:9090.\n"},
		{name: "literal image tag", content: "docker pull ghcr.io/gtriggiano/statica:0.2.0\n", wantProblem: "names a version"},
		{name: "literal download URL", content: "curl -LO .../releases/download/v0.2.0/checksums.txt\n", wantProblem: "names a version"},
		{name: "literal asset name", content: "tar -xzf statica_0.2.0_linux_amd64.tar.gz\n", wantProblem: "names a version"},
		{name: "floating image tag", content: "docker pull ghcr.io/gtriggiano/statica:latest\n", wantProblem: `image tag "latest"`},
		{name: "minor image tag", content: "image: ghcr.io/gtriggiano/statica:0.2\n", wantProblem: `image tag "0.2"`},
		{name: "VERSION set", content: "VERSION=0.2.0\n", wantProblem: "VERSION shell variable"},
		{name: "VERSION used", content: "docker pull \"ghcr.io/gtriggiano/statica:${VERSION}\"\n", wantProblem: "VERSION shell variable"},
		{name: "release-please marker", content: blockStart + "\n", wantProblem: "release-please marker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkProblems(t, checkPageReferences("page.md", tc.content), tc.wantProblem)
		})
	}
}

func checkProblems(t *testing.T, problems []string, want string) {
	t.Helper()
	if want == "" {
		if len(problems) > 0 {
			t.Errorf("unexpected problems: %q", problems)
		}
		return
	}
	if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
		t.Errorf("problems %q, want one containing %q", problems, want)
	}
}

// TestRepositoryVersionReferences checks README.md and every page in docs/.
func TestRepositoryVersionReferences(t *testing.T) {
	version := manifestVersion(t)
	extraFiles := configuredExtraFiles(t)

	const readme = "README.md"
	data, err := os.ReadFile(filepath.Join(repoRoot, readme))
	if err != nil {
		t.Fatal(err)
	}
	problems, marked := checkReadmeReferences(readme, string(data), version)
	for _, p := range problems {
		t.Error(p)
	}
	if marked && !slices.Contains(extraFiles, readme) {
		t.Errorf("%s has version blocks but is not in extra-files in release-please-config.json: the release PR would not update it", readme)
	}
	for _, name := range extraFiles {
		if strings.HasPrefix(name, "docs/") {
			t.Errorf("extra-files in release-please-config.json lists %s: the pages in docs/ write %s, which the documentation build replaces", name, placeholder)
		}
		if _, err := os.Stat(filepath.Join(repoRoot, name)); err != nil {
			t.Errorf("extra-files in release-please-config.json lists %s: %v", name, err)
		}
	}

	err = filepath.WalkDir(filepath.Join(repoRoot, "docs"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".vitepress") {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		// The build does not replace the placeholder in the files that pages
		// include with <<<.
		example := filepath.Base(filepath.Dir(path)) == "examples"
		if !example && !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, p := range checkPageReferences(filepath.ToSlash(rel), string(data)) {
			t.Error(p)
		}
		if example && strings.Contains(string(data), placeholder) {
			t.Errorf("%s: %s in an included file: the build would not replace it", filepath.ToSlash(rel), placeholder)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
