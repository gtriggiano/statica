package agentdocs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

var repoRoot = filepath.Join("..", "..")

const (
	sharedSkills = ".agents/skills"
	claudeSkills = ".claude/skills"
	// userSkills holds the skills for users of statica, which the
	// documentation build publishes (docs/.vitepress/llms.ts).
	userSkills = "skills"
)

var (
	// skillNameRE is the name rule of the Agent Skills specification
	// (https://agentskills.io/specification), with its 64-character limit
	// checked apart.
	skillNameRE  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	skillRowRE   = regexp.MustCompile("(?m)^\\| `([a-z0-9-]+)` \\|")
	linkRE       = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	codeSpanRE   = regexp.MustCompile("`([^`\n]+)`")
	headingRE    = regexp.MustCompile(`^#{1,6}\s+(.*?)(?:\s+#+)?\s*$`)
	makeLineRE   = regexp.MustCompile(`^\s*make ([a-z][a-z-]*)`)
	makeTargetRE = regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+):`)
	testNameRE   = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)
	testFuncRE   = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)
	// repoPathRE matches a path under a tracked top-level directory, and
	// goFileRE a Go file name without a directory, wherever they start a token
	// that is not a glob such as *_test.go.
	repoPathRE = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./*-])((?:cmd|internal|docs|\.github|\.agents|\.claude)/[A-Za-z0-9_./-]*)`)
	goFileRE   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./*-])([A-Za-z0-9_]+\.go)\b`)
)

// ignoredPaths are named in the instructions as build output that a clone does
// not have until a build creates it.
var ignoredPaths = []string{"docs/node_modules/", "docs/.vitepress/dist/"}

type skill struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// goSources lists the Go files of the module: the base names of all of them,
// and the test functions they declare.
type goSources struct {
	files map[string]bool
	tests map[string]bool
}

func TestClaudeMDImportsAgentsMD(t *testing.T) {
	for line := range strings.Lines(read(t, "CLAUDE.md")) {
		if strings.TrimSpace(line) == "@AGENTS.md" {
			return
		}
	}
	t.Error("CLAUDE.md: no line @AGENTS.md; Claude Code would not load the shared instructions")
}

func TestSkills(t *testing.T) {
	names := skillDirs(t, sharedSkills)
	if len(names) == 0 {
		t.Fatalf("%s: no skills", sharedSkills)
	}
	_, index, _ := strings.Cut(read(t, "AGENTS.md"), "\n## Skills\n")
	var listed []string
	for _, m := range skillRowRE.FindAllStringSubmatch(index, -1) {
		listed = append(listed, m[1])
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			sharedPath := path.Join(sharedSkills, name, "SKILL.md")
			shared := readSkill(t, sharedPath)
			if len(name) > 64 || !skillNameRE.MatchString(name) {
				t.Errorf("%s: the directory name is not a valid skill name", sharedPath)
			}
			if shared.Name != name {
				t.Errorf("%s: name %q differs from its directory", sharedPath, shared.Name)
			}
			if n := utf8.RuneCountInString(shared.Description); n == 0 || n > 1024 {
				t.Errorf("%s: description has %d characters, want 1 to 1024", sharedPath, n)
			}
			link := path.Join(claudeSkills, name)
			want := "../../" + path.Join(sharedSkills, name)
			if target, err := linkTarget(link); err != nil {
				t.Errorf("%s: %v; create it with: ln -s %s %s", link, err, want, link)
			} else if target != want {
				t.Errorf("%s: links to %s, want %s", link, target, want)
			}
			if !slices.Contains(listed, name) {
				t.Errorf("AGENTS.md: no row for %s in the Skills table", name)
			}
		})
	}
	for _, name := range listed {
		if !slices.Contains(names, name) {
			t.Errorf("AGENTS.md: the Skills table lists %s, which is not in %s", name, sharedSkills)
		}
	}
	for _, e := range dirEntries(t, claudeSkills) {
		if !e.IsDir() && !slices.Contains(names, e.Name()) {
			t.Errorf("%s: links to a skill that is not in %s", path.Join(claudeSkills, e.Name()), sharedSkills)
		}
	}
}

// TestUserSkills checks the skills that the documentation site publishes for
// users of statica: the rules of the Agent Skills specification, and those of
// the build that publishes them. A user skill is read away from the
// repository, so it links to the published documentation only.
func TestUserSkills(t *testing.T) {
	names := skillDirs(t, userSkills)
	if len(names) == 0 {
		t.Fatalf("%s: no skills", userSkills)
	}
	contributor := skillDirs(t, sharedSkills)
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			file := path.Join(userSkills, name, "SKILL.md")
			s := readSkill(t, file)
			if len(name) > 64 || !skillNameRE.MatchString(name) {
				t.Errorf("%s: the directory name is not a valid skill name", file)
			}
			if s.Name != name {
				t.Errorf("%s: name %q differs from its directory", file, s.Name)
			}
			if n := utf8.RuneCountInString(s.Description); n == 0 || n > 1024 {
				t.Errorf("%s: description has %d characters, want 1 to 1024", file, n)
			}
			if slices.Contains(contributor, name) {
				t.Errorf("%s: a skill of %s has the same name", file, sharedSkills)
			}
			text := read(t, file)
			if !strings.Contains(text, "\ndescription: "+s.Description+"\n") {
				t.Errorf("%s: write the description on one line, unquoted: the documentation build copies it into the index of the skills", file)
			}
			if n := strings.Count(text, "\n"); n > 500 {
				t.Errorf("%s: %d lines, want at most 500", file, n)
			}
			for _, e := range dirEntries(t, path.Join(userSkills, name)) {
				if e.Name() != "SKILL.md" {
					t.Errorf("%s: %s: the documentation build publishes a single SKILL.md per skill", file, e.Name())
				}
			}
			for _, m := range linkRE.FindAllStringSubmatch(codeSpanRE.ReplaceAllString(text, ""), -1) {
				if !strings.HasPrefix(m[1], "https://") {
					t.Errorf("%s: link to %s: a user skill is installed away from the repository, link to the published documentation", file, m[1])
				}
			}
			for _, m := range repoPathRE.FindAllStringSubmatch(text, -1) {
				t.Errorf("%s: names the repository path %s: a user skill is installed away from the repository", file, m[1])
			}
		})
	}
}

// linkTarget returns the target of the symbolic link at file. A clone without
// symbolic links, such as one on Windows without core.symlinks, has a plain
// file that holds the target instead: linkTarget reads it, so that the content
// of the repository is checked on every platform.
func linkTarget(file string) (string, error) {
	p := filepath.Join(repoRoot, filepath.FromSlash(file))
	info, err := os.Lstat(p)
	switch {
	case err != nil:
		return "", err
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(p)
		return filepath.ToSlash(target), err
	case info.Mode().IsRegular():
		data, err := os.ReadFile(p)
		return string(data), err
	default:
		return "", errors.New("not a symbolic link")
	}
}

// TestReferences fails when an instruction file names something that does not
// exist: a relative link or its anchor, a path under a tracked top-level
// directory, a Go file, a Make target, or a Go test.
func TestReferences(t *testing.T) {
	// The links in .claude/skills lead to the shared skills; a directory there
	// is a skill for Claude Code only.
	files := []string{"AGENTS.md", "CLAUDE.md"}
	for _, dir := range []string{sharedSkills, claudeSkills} {
		for _, name := range skillDirs(t, dir) {
			files = append(files, path.Join(dir, name, "SKILL.md"))
		}
	}
	targets := map[string]bool{}
	for _, m := range makeTargetRE.FindAllStringSubmatch(read(t, "Makefile"), -1) {
		targets[m[1]] = true
	}
	src := readGoSources(t)
	for _, file := range files {
		text := read(t, file)
		// A link inside a code span is an example of Markdown, not a link.
		for _, m := range linkRE.FindAllStringSubmatch(codeSpanRE.ReplaceAllString(text, ""), -1) {
			checkLink(t, file, m[1])
		}
		for _, m := range repoPathRE.FindAllStringSubmatch(text, -1) {
			p := strings.TrimRight(m[1], ".,")
			if ignored(p) {
				continue
			}
			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(p))); err != nil {
				t.Errorf("%s: names %s, which does not exist", file, p)
			}
		}
		for _, m := range goFileRE.FindAllStringSubmatch(text, -1) {
			if !src.files[m[1]] {
				t.Errorf("%s: names %s, which is not a Go file of the module", file, m[1])
			}
		}
		for _, target := range makeTargets(text) {
			if !targets[target] {
				t.Errorf("%s: names make %s, which is not a Makefile target", file, target)
			}
		}
		for _, name := range testNameRE.FindAllString(text, -1) {
			if !src.tests[name] {
				t.Errorf("%s: names %s, which is not a test function", file, name)
			}
		}
	}
}

func TestSlug(t *testing.T) {
	for heading, want := range map[string]string{
		"Commit messages and pull request titles": "commit-messages-and-pull-request-titles",
		"AI coding agents":                        "ai-coding-agents",
		"`cache.max_entries`":                     "cachemax_entries",
		"CI, workflows and release tooling":       "ci-workflows-and-release-tooling",
	} {
		if got := slug(heading); got != want {
			t.Errorf("slug(%q) = %q, want %q", heading, got, want)
		}
	}
}

func TestMarkdownScanning(t *testing.T) {
	text := "# Title ##\nmake sure of it, `make check`\n```sh\n# not a heading\n  make docs-build FOO=1\n```\n## Next\n"
	if got, want := headings(text), []string{"title", "next"}; !slices.Equal(got, want) {
		t.Errorf("headings = %q, want %q", got, want)
	}
	if got, want := makeTargets(text), []string{"check", "docs-build"}; !slices.Equal(got, want) {
		t.Errorf("makeTargets = %q, want %q", got, want)
	}
	var files []string
	for _, m := range goFileRE.FindAllStringSubmatch("`types.go`, a `*_test.go` file, internal/config/parse.go and go.mod", -1) {
		files = append(files, m[1])
	}
	if want := []string{"types.go"}; !slices.Equal(files, want) {
		t.Errorf("Go files = %q, want %q", files, want)
	}
}

func checkLink(t *testing.T, file, link string) {
	t.Helper()
	if strings.Contains(link, "://") || strings.HasPrefix(link, "mailto:") {
		return
	}
	target, anchor, _ := strings.Cut(link, "#")
	if target == "" {
		target = file
	} else {
		target = path.Join(path.Dir(file), target)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(target)))
	if err != nil {
		t.Errorf("%s: link %s: %v", file, link, err)
		return
	}
	if anchor != "" && !slices.Contains(headings(string(data)), anchor) {
		t.Errorf("%s: link %s: no heading in %s has the anchor #%s", file, link, target, anchor)
	}
}

// headings returns the anchors of the ATX headings outside code fences.
func headings(text string) []string {
	var anchors []string
	prose, _ := splitFences(text)
	for _, line := range prose {
		if m := headingRE.FindStringSubmatch(line); m != nil {
			anchors = append(anchors, slug(m[1]))
		}
	}
	return anchors
}

// makeTargets returns the targets of the make commands in code spans, and in
// command lines inside code fences.
func makeTargets(text string) []string {
	var targets []string
	for _, m := range codeSpanRE.FindAllStringSubmatch(text, -1) {
		if args, ok := strings.CutPrefix(m[1], "make "); ok {
			if f := strings.Fields(args); len(f) > 0 {
				targets = append(targets, f[0])
			}
		}
	}
	_, code := splitFences(text)
	for _, line := range code {
		if m := makeLineRE.FindStringSubmatch(line); m != nil {
			targets = append(targets, m[1])
		}
	}
	return targets
}

// splitFences separates the lines outside code fences from the lines inside
// them; the fence lines belong to neither.
func splitFences(text string) (prose, code []string) {
	inFence := false
	for line := range strings.Lines(text) {
		line = strings.TrimSuffix(line, "\n")
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			code = append(code, line)
		} else {
			prose = append(prose, line)
		}
	}
	return prose, code
}

// slug is the anchor GitHub gives a heading, without the suffix it adds to
// repeated headings.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func ignored(p string) bool {
	for _, prefix := range ignoredPaths {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

func readGoSources(t *testing.T) goSources {
	t.Helper()
	src := goSources{files: map[string]bool{}, tests: map[string]bool{}}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			src.files[d.Name()] = true
			if !strings.HasSuffix(p, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(p)
			for _, m := range testFuncRE.FindAllStringSubmatch(string(data), -1) {
				src.tests[m[1]] = true
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return src
}

// skillDirs returns the names of the directories in dir, without following
// symbolic links.
func skillDirs(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	for _, e := range dirEntries(t, dir) {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func dirEntries(t *testing.T, dir string) []fs.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(repoRoot, filepath.FromSlash(dir)))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return entries
}

func readSkill(t *testing.T, file string) skill {
	t.Helper()
	text := read(t, file)
	rest, ok := strings.CutPrefix(text, "---\n")
	front, _, closed := strings.Cut(rest, "\n---\n")
	if !ok || !closed {
		t.Fatalf("%s: does not start with YAML frontmatter between --- lines", file)
	}
	var s skill
	if err := yaml.Unmarshal([]byte(front), &s); err != nil {
		t.Fatalf("%s: frontmatter: %v", file, err)
	}
	return s
}

func read(t *testing.T, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
