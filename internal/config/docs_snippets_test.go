package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// configSnippetInfo is the fence info string of a statica configuration
// snippet in the documentation.
const configSnippetInfo = "yaml [config.yaml]"

type configSnippet struct {
	line    int // line of the opening fence, from 1
	content string
}

// extractConfigSnippets returns the fenced code blocks of a Markdown document
// whose info string is exactly configSnippetInfo. A fence is three or more
// backticks or tildes, possibly indented (inside a list item), and is closed by
// a line of the same character at least as long; an unclosed fence runs to the
// end of the document. Content lines lose up to as much indentation as the
// opening fence has. Anything inside a fence, other fences included, is content.
//
// misplaced lists the lines, outside any fence, that mention [config.yaml]
// without opening a snippet: a fence with highlights or other modifiers, a
// fence inside a block quote, or prose. Such a block would escape validation.
func extractConfigSnippets(markdown string) (snippets []configSnippet, misplaced []int) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		indent, marker, length, info, ok := openingFence(lines[i])
		if (!ok || info != configSnippetInfo) && strings.Contains(lines[i], "[config.yaml]") {
			misplaced = append(misplaced, i+1)
		}
		if !ok {
			continue
		}
		start := i
		var body []string
		for i++; i < len(lines) && !closingFence(lines[i], marker, length); i++ {
			line := lines[i]
			for n := 0; n < indent && strings.HasPrefix(line, " "); n++ {
				line = line[1:]
			}
			body = append(body, line)
		}
		if info == configSnippetInfo {
			snippets = append(snippets, configSnippet{line: start + 1, content: strings.Join(body, "\n") + "\n"})
		}
	}
	return snippets, misplaced
}

func openingFence(line string) (indent int, marker byte, length int, info string, ok bool) {
	text := strings.TrimLeft(line, " ")
	if text == "" || (text[0] != '`' && text[0] != '~') {
		return 0, 0, 0, "", false
	}
	marker = text[0]
	for length < len(text) && text[length] == marker {
		length++
	}
	info = strings.TrimSpace(text[length:])
	if length < 3 || (marker == '`' && strings.Contains(info, "`")) {
		return 0, 0, 0, "", false
	}
	return len(line) - len(text), marker, length, info, true
}

func closingFence(line string, marker byte, length int) bool {
	text := strings.TrimLeft(line, " ")
	n := 0
	for n < len(text) && text[n] == marker {
		n++
	}
	return n >= length && strings.TrimSpace(text[n:]) == ""
}

// completeConfigSnippet turns a snippet into a whole configuration: it adds
// an s3.bucket (merged into an existing s3 mapping) when it is missing.
// Everything else is left for Parse to judge.
func completeConfigSnippet(snippet string) ([]byte, error) {
	decoder := yaml.NewDecoder(strings.NewReader(snippet))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("snippet: invalid YAML: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("snippet: exactly one YAML document required")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("snippet: expected a YAML mapping")
	}
	root := doc.Content[0]
	str := func(value string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value} }
	s3 := mappingValue(root, "s3")
	if s3 == nil {
		s3 = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, str("s3"), s3)
	}
	if s3.Kind == yaml.MappingNode && mappingValue(s3, "bucket") == nil {
		s3.Content = append([]*yaml.Node{str("bucket"), str("example-websites")}, s3.Content...)
	}
	return yaml.Marshal(&doc)
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// parseConfigSnippet completes a snippet and parses it; every ${VAR} expands
// to example-websites. It returns the completed YAML for diagnostics.
func parseConfigSnippet(snippet string) (*Config, []byte, error) {
	data, err := completeConfigSnippet(snippet)
	if err != nil {
		return nil, nil, err
	}
	c, err := Parse(data, func(string) (string, bool) { return "example-websites", true })
	return c, data, err
}

func TestDocsConfigSnippets(t *testing.T) {
	repo := filepath.Join("..", "..")
	count := 0
	err := filepath.WalkDir(filepath.Join(repo, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch entry.Name() {
			case "node_modules", ".vitepress", "public":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		snippets, misplaced := extractConfigSnippets(string(data))
		for _, line := range misplaced {
			t.Errorf("%s:%d: a line mentioning [config.yaml] must open a configuration snippet with exactly ```yaml [config.yaml] (not in a blockquote, no line highlights or other modifiers)", filepath.ToSlash(name), line)
		}
		for _, snippet := range snippets {
			count++
			_, completed, err := parseConfigSnippet(snippet.content)
			if err == nil {
				continue
			}
			where := fmt.Sprintf("%s:%d", filepath.ToSlash(name), snippet.line)
			if completed == nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			// Parse reports lines of the completed configuration.
			var numbered strings.Builder
			for i, line := range strings.Split(strings.TrimSuffix(string(completed), "\n"), "\n") {
				fmt.Fprintf(&numbered, "%4d  %s\n", i+1, line)
			}
			t.Errorf("%s: %v\ncompleted configuration:\n%s", where, err, numbered.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The reference pages contain snippets: finding none means the walk or
	// the extraction is broken, not that there is nothing to check.
	if count == 0 {
		t.Fatal("no configuration snippets found under docs/")
	}
	t.Logf("checked %d configuration snippet(s)", count)
}

func TestExtractConfigSnippets(t *testing.T) {
	// ^ stands for a backtick, which a Go raw string cannot hold.
	markdown := strings.ReplaceAll(`# Sample

^^^yaml [config.yaml]
cache:
  ttl: 1m
^^^

~~~ yaml [config.yaml]
index: home.html
^^^
~~~

^^^^md
^^^yaml [config.yaml]
inside: a longer fence
^^^
^^^^

^^^yaml
plain: yaml
^^^

^^^yaml [prometheus.yml]
scrape_configs: []
^^^

1. A step:

   ^^^yaml [config.yaml]
   s3:
     region: eu-west-1
   ^^^

^^^^yaml [config.yaml]
spa:
  fallback: index.html
^^^
^^^^^
`, "^", "`")
	want := []configSnippet{
		{3, "cache:\n  ttl: 1m\n"},
		{8, "index: home.html\n```\n"},
		{29, "s3:\n  region: eu-west-1\n"},
		{34, "spa:\n  fallback: index.html\n```\n"},
	}
	got, misplaced := extractConfigSnippets(markdown)
	if !reflect.DeepEqual(got, want) || misplaced != nil {
		t.Fatalf("got %#v, misplaced %v\nwant %#v, none misplaced", got, misplaced, want)
	}
}

func TestExtractConfigSnippetsNearMisses(t *testing.T) {
	// ^ stands for a backtick. Each near miss would escape validation; the
	// one inside an example fence is content, and the last block is a snippet.
	markdown := strings.ReplaceAll(`^^^yaml{2} [config.yaml]
index: a.html
^^^

^^^yaml:line-numbers [config.yaml]
index: b.html
^^^

^^^yaml [config.yaml]{1}
index: c.html
^^^

> ^^^yaml [config.yaml]
> index: d.html
> ^^^

Use a ^yaml [config.yaml]^ fence.

^^^^md
^^^yaml{2} [config.yaml]
index: e.html
^^^
^^^^

^^^yaml [config.yaml]
index: f.html
^^^
`, "^", "`")
	snippets, misplaced := extractConfigSnippets(markdown)
	if want := []configSnippet{{25, "index: f.html\n"}}; !reflect.DeepEqual(snippets, want) {
		t.Fatalf("snippets %#v, want %#v", snippets, want)
	}
	if want := []int{1, 5, 9, 13, 17}; !reflect.DeepEqual(misplaced, want) {
		t.Fatalf("misplaced lines %v, want %v", misplaced, want)
	}
}

func TestParseConfigSnippet(t *testing.T) {
	valid := []struct{ snippet, bucket, region string }{
		{"cache:\n  ttl: 1m\n", "example-websites", ""},
		{"s3:\n  region: eu-west-1\n", "example-websites", "eu-west-1"},
		{"s3: {bucket: example-docs}\n", "example-docs", ""},
		{"s3:\n  bucket: ${SITE_BUCKET}\n", "example-websites", ""},
	}
	for _, tt := range valid {
		c, _, err := parseConfigSnippet(tt.snippet)
		if err != nil {
			t.Fatalf("%q: %v", tt.snippet, err)
		}
		if c.S3.Bucket != tt.bucket || c.S3.Region != tt.region {
			t.Fatalf("%q: bucket %q, region %q", tt.snippet, c.S3.Bucket, c.S3.Region)
		}
	}
	if c, _, _ := parseConfigSnippet(valid[0].snippet); c.Cache.TTL != time.Minute {
		t.Fatal("snippet values were not kept")
	}
	for _, snippet := range []string{"", "- a\n", "cache: [\n", "index: a\n---\nindex: b\n", "cache:\n  ttl: 1\n", "version: 2\n", "index: &i a\nnot_found: *i\n"} {
		if _, _, err := parseConfigSnippet(snippet); err == nil {
			t.Fatalf("%q: invalid snippet accepted", snippet)
		}
	}
}
