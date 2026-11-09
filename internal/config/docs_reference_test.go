package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The configuration reference documents every field under a heading that is
// exactly its path in backticks, followed by a "**Type:**" line and a
// "**Default:**" line. A list item adds "[]" to the path and a map entry adds
// ".<name>": cache.rules[].match.prefix, headers.set.<name>.
var referencePage = filepath.Join("..", "..", "docs", "reference", "configuration.md")

var (
	fieldHeadingRE   = regexp.MustCompile("^#{2,6} `([a-z0-9_]+(?:\\.[a-z0-9_]+|\\[\\]|\\.<name>)*)`$")
	typeLineRE       = regexp.MustCompile(`^\*\*Type:\*\* (.+)\\$`)
	defaultLiteralRE = regexp.MustCompile("^\\*\\*Default:\\*\\* `([^`]+)`")
	defaultNoneRE    = regexp.MustCompile(`^\*\*Default:\*\* none — (\S.*)$`)
	anchorRE         = regexp.MustCompile(`[^a-z0-9]+`)
	indexRE          = regexp.MustCompile(`\[[0-9]+\]`)
)

// referenceBase reaches every optional block, so that each documented default
// can be checked where it applies. Parse does not read the TLS files.
const referenceBase = `server:
  tls:
    cert_file: cert.pem
    key_file: key.pem
admin:
  origin_health:
    enabled: true
s3:
  bucket: example-websites
cache:
  rules:
    - match:
        prefix: /assets/
redirects:
  - from: /old/{slug}
    to: /new/{slug}
    status: 301
    captures:
      slug:
        pattern: "[a-z]+"
headers:
  set:
    Referrer-Policy: no-referrer
cache_control:
  rules:
    - match:
        prefix: /assets/
      value: public, max-age=31536000, immutable
cors:
  allowed_origins: [https://app.example.com]
compression:
  mode: precompressed
spa:
  fallback: index.html
`

// derivedDefaults are fields documented as "Default: none" whose omitted value
// is copied from another field: with that field set to value, the documented
// field must load as value too.
var derivedDefaults = map[string]struct{ from, value string }{
	"admin.origin_health.key": {from: "index", value: "home.html"},
}

type schemaField struct {
	path string
	typ  reflect.Type
}

type referenceEntry struct {
	line    int
	typ     string // the documented type
	literal string // the documented default, empty for "none"
	none    string // the explanation that follows "none —"
}

func TestDocsConfigReference(t *testing.T) {
	data, err := os.ReadFile(referencePage)
	if err != nil {
		t.Fatal(err)
	}
	documented := parseReferencePage(t, string(data))

	var schema []schemaField
	collectSchemaFields(reflect.TypeOf(Config{}), "", &schema)
	known := map[string]bool{}
	for _, field := range schema {
		known[field.path] = true
		if _, ok := documented[field.path]; !ok {
			t.Errorf("%s: field %s has no heading", referencePage, field.path)
		}
	}
	for path, entry := range documented {
		if !known[path] {
			t.Errorf("%s:%d: %s is not a configuration field", referencePage, entry.line, path)
		}
	}

	for _, field := range schema {
		entry, ok := documented[field.path]
		if !ok {
			continue
		}
		if want := typeName(field.typ); entry.typ != want {
			t.Errorf("%s:%d: %s: documented type %q, want %q", referencePage, entry.line, field.path, entry.typ, want)
		}
		if entry.literal == "" {
			checkNoDefault(t, field, entry)
		} else {
			checkDefault(t, field.path, entry)
		}
	}
}

// collectSchemaFields appends every exported field reachable from t,
// containers included, in schema order.
func collectSchemaFields(t reflect.Type, prefix string, fields *[]schemaField) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			path := childPath(prefix, fieldName(f))
			*fields = append(*fields, schemaField{path, f.Type})
			collectSchemaFields(f.Type, path, fields)
		}
	case reflect.Slice:
		collectSchemaFields(t.Elem(), prefix+"[]", fields)
	case reflect.Map:
		path := prefix + ".<name>"
		*fields = append(*fields, schemaField{path, t.Elem()})
		collectSchemaFields(t.Elem(), path, fields)
	}
}

// typeName is the page's name for a Go field type. A type without a name
// here is reported as its Go type, so a new kind of field needs a deliberate
// name.
func typeName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == durationType:
		return "duration"
	case t == sizeType:
		return "byte size"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int:
		return "integer"
	case reflect.Bool:
		return "boolean"
	case reflect.Struct:
		return "mapping"
	case reflect.Slice:
		if e := typeName(t.Elem()); e == "string" || e == "mapping" {
			return "list of " + e + "s"
		}
	case reflect.Map:
		if e := typeName(t.Elem()); t.Key().Kind() == reflect.String && (e == "string" || e == "mapping") {
			return "mapping of " + e + "s"
		}
	}
	return t.String()
}

// zeroLiteral is the YAML spelling of the zero value of a plain scalar field,
// or "" for other fields.
func zeroLiteral(t reflect.Type) string {
	switch {
	case t == durationType:
		return "0s"
	case t == sizeType:
		return "0KiB"
	}
	switch t.Kind() {
	case reflect.String:
		return `""`
	case reflect.Int:
		return "0"
	case reflect.Bool:
		return "false"
	}
	return ""
}

// parseReferencePage returns the field headings of the page, outside code
// fences, with their documented types and defaults.
func parseReferencePage(t *testing.T, page string) map[string]referenceEntry {
	t.Helper()
	lines := strings.Split(page, "\n")
	documented := map[string]referenceEntry{}
	anchors := map[string]int{} // heading anchor → line
	for i := 0; i < len(lines); i++ {
		if _, marker, length, _, ok := openingFence(lines[i]); ok {
			for i++; i < len(lines) && !closingFence(lines[i], marker, length); i++ {
			}
			continue
		}
		if !strings.HasPrefix(lines[i], "#") {
			continue
		}
		// Like the site's anchors: a field's link must not move to another
		// heading with the same text, such as "Errors" and `errors`.
		anchor := strings.Trim(anchorRE.ReplaceAllString(strings.ToLower(strings.TrimLeft(lines[i], "# ")), "-"), "-")
		if previous, ok := anchors[anchor]; ok {
			t.Errorf("%s:%d: this heading has the same anchor #%s as line %d", referencePage, i+1, anchor, previous)
		}
		anchors[anchor] = i + 1
		m := fieldHeadingRE.FindStringSubmatch(lines[i])
		if m == nil {
			if strings.Contains(lines[i], "`") {
				t.Errorf("%s:%d: a heading with code must be exactly one field path in backticks", referencePage, i+1)
			}
			continue
		}
		path, line := m[1], i+1
		if previous, ok := documented[path]; ok {
			t.Errorf("%s:%d: %s is already documented at line %d", referencePage, line, path, previous.line)
			continue
		}
		// The first two lines of the section: **Type:**, ending with a hard
		// line break, then **Default:**.
		var body []string
		for j := i + 1; j < len(lines) && len(body) < 2; j++ {
			if strings.HasPrefix(lines[j], "#") {
				break
			}
			if strings.TrimSpace(lines[j]) != "" || len(body) == 1 {
				body = append(body, lines[j])
			}
		}
		var typ []string
		if len(body) == 2 {
			typ = typeLineRE.FindStringSubmatch(body[0])
		}
		if typ == nil {
			t.Errorf(`%s:%d: %s: the section must start with a **Type:** line ending with \`, referencePage, line, path)
			continue
		}
		entry := referenceEntry{line: line, typ: typ[1]}
		if m := defaultLiteralRE.FindStringSubmatch(body[1]); m != nil {
			entry.literal = m[1]
		} else if m := defaultNoneRE.FindStringSubmatch(body[1]); m != nil {
			entry.none = m[1]
		} else {
			t.Errorf("%s:%d: %s: the line after **Type:** must be **Default:** `<yaml literal>` or **Default:** none — <explanation>", referencePage, line, path)
			continue
		}
		documented[path] = entry
	}
	return documented
}

// checkDefault parses the base configuration without the field and with the
// field set to the documented literal; both must give the same value.
func checkDefault(t *testing.T, path string, entry referenceEntry) {
	t.Helper()
	literal, err := yamlValue(entry.literal)
	if err != nil {
		t.Errorf("%s:%d: %s: default `%s` is not a YAML value", referencePage, entry.line, path, entry.literal)
		return
	}
	omitted, err := Parse(referenceVariant(t, path, nil), noEnv)
	if err != nil {
		t.Errorf("%s: the base configuration without the field is invalid: %v", path, err)
		return
	}
	explicit, err := Parse(referenceVariant(t, path, literal), noEnv)
	if err != nil {
		t.Errorf("%s:%d: %s: documented default `%s` is rejected: %v", referencePage, entry.line, path, entry.literal, err)
		return
	}
	got, gotOK := valueAt(omitted, path)
	want, wantOK := valueAt(explicit, path)
	if gotOK != wantOK || gotOK && !reflect.DeepEqual(got.Interface(), want.Interface()) {
		t.Errorf("%s:%d: %s: documented default `%s` loads as %s, but omitting the field gives %s", referencePage, entry.line, path, entry.literal, describe(want, wantOK), describe(got, gotOK))
	}
}

// checkNoDefault checks a field documented as "Default: none". A field whose
// default is copied from another field must follow it. A field is documented
// as required exactly when omitting it makes the configuration invalid at its
// path. Any other field must stay unset when omitted, and no explicit value
// may load the same way: "none" never stands for false, zero or an empty
// string that can be written.
func checkNoDefault(t *testing.T, field schemaField, entry referenceEntry) {
	t.Helper()
	path := field.path
	if d, ok := derivedDefaults[path]; ok {
		source, err := yamlValue(d.value)
		if err != nil {
			t.Fatal(err)
		}
		c, err := Parse(referenceVariant(t, d.from, source), noEnv)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if v, ok := valueAt(c, path); !ok || v.Interface() != any(d.value) {
			t.Errorf("%s:%d: %s: with %s set to %q, omitting it gives %s, not %q", referencePage, entry.line, path, d.from, d.value, describe(v, ok), d.value)
		}
		return
	}
	_, err := Parse(referenceVariant(t, path, nil), noEnv)
	required := false
	var invalid *Error
	if errors.As(err, &invalid) {
		// Redirect diagnostics name a capture's field without its name.
		at := indexRE.ReplaceAllString(invalid.Path, "[]")
		required = at == path || strings.HasPrefix(at, path+".") || at == strings.ReplaceAll(path, ".<name>", "")
	}
	if documented := strings.HasPrefix(entry.none, "required"); documented != required {
		if documented {
			t.Errorf("%s:%d: %s is documented as required, but omitting it does not fail at its path (error: %v)", referencePage, entry.line, path, err)
		} else {
			t.Errorf("%s:%d: omitting %s fails at its path, so document it as **Default:** none — required: %v", referencePage, entry.line, path, err)
		}
		return
	}
	if required {
		return
	}
	// Decoding and defaulting only: the field's siblings may be required.
	var doc yaml.Node
	if err := yaml.Unmarshal(referenceVariant(t, path, nil), &doc); err != nil {
		t.Fatal(err)
	}
	c := defaults()
	if err := c.decode(doc.Content[0], reflect.ValueOf(c).Elem(), "", noEnv); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	c.applyOptionalDefaults()
	omitted, omittedOK := valueAt(c, path)
	if omittedOK && !omitted.IsZero() {
		t.Errorf("%s:%d: %s is documented without a default, but omitting it gives %s; document that default", referencePage, entry.line, path, describe(omitted, omittedOK))
		return
	}
	zero := zeroLiteral(field.typ)
	if zero == "" || !omittedOK {
		return
	}
	value, err := yamlValue(zero)
	if err != nil {
		t.Fatal(err)
	}
	if explicit, err := Parse(referenceVariant(t, path, value), noEnv); err == nil {
		if v, ok := valueAt(explicit, path); ok && v.IsZero() {
			t.Errorf("%s:%d: %s is documented without a default, but `%s` loads like the omitted field; document **Default:** `%s`", referencePage, entry.line, path, zero, zero)
		}
	}
}

func yamlValue(literal string) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(literal), &doc); err != nil || len(doc.Content) != 1 {
		return nil, fmt.Errorf("not a YAML value: %s", literal)
	}
	return doc.Content[0], nil
}

// referenceVariant returns referenceBase with the field at path removed (nil
// value) or set to value. "[]" selects the first list item and "<name>" the
// first map entry.
func referenceVariant(t *testing.T, path string, value *yaml.Node) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(referenceBase), &doc); err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(strings.ReplaceAll(path, "[]", ".[]"), ".")
	node := doc.Content[0]
	for i, segment := range segments {
		last := i == len(segments)-1
		switch {
		case segment == "[]":
			if node.Kind != yaml.SequenceNode || len(node.Content) == 0 {
				t.Fatalf("%s: referenceBase needs an item at %s", path, segment)
			}
			node = node.Content[0]
		case segment == "<name>" && !last:
			if node.Kind != yaml.MappingNode || len(node.Content) == 0 {
				t.Fatalf("%s: referenceBase needs an entry at %s", path, segment)
			}
			node = node.Content[1]
		default:
			if node.Kind != yaml.MappingNode {
				t.Fatalf("%s: %s is not inside a mapping in referenceBase", path, segment)
			}
			k := -1
			for j := 0; j < len(node.Content); j += 2 {
				if node.Content[j].Value == segment || segment == "<name>" {
					k = j
					break
				}
			}
			switch {
			case last && value == nil:
				if k >= 0 {
					node.Content = append(node.Content[:k], node.Content[k+2:]...)
				}
			case last:
				if k >= 0 {
					node.Content[k+1] = value
				} else {
					name := segment
					if name == "<name>" {
						name = "Example-Name"
					}
					node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, value)
				}
			case k >= 0:
				node = node.Content[k+1]
			case value == nil:
				return mustMarshal(t, &doc) // nothing to remove
			default:
				child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: segment}, child)
				node = child
			}
		}
	}
	return mustMarshal(t, &doc)
}

func mustMarshal(t *testing.T, doc *yaml.Node) []byte {
	t.Helper()
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// valueAt returns the loaded value at path, and false when a parent is
// absent (nil pointer, empty list or map).
func valueAt(c *Config, path string) (reflect.Value, bool) {
	v := reflect.ValueOf(c).Elem()
	for _, segment := range strings.Split(strings.ReplaceAll(path, "[]", ".[]"), ".") {
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}
			v = v.Elem()
		}
		switch {
		case segment == "[]":
			if v.Len() == 0 {
				return reflect.Value{}, false
			}
			v = v.Index(0)
		case segment == "<name>":
			if v.Len() == 0 {
				return reflect.Value{}, false
			}
			v = v.MapIndex(v.MapKeys()[0])
		default:
			found := false
			for i := 0; i < v.NumField(); i++ {
				if f := v.Type().Field(i); f.IsExported() && fieldName(f) == segment {
					v, found = v.Field(i), true
					break
				}
			}
			if !found {
				return reflect.Value{}, false
			}
		}
	}
	return v, true
}

func describe(v reflect.Value, ok bool) string {
	if !ok {
		return "an absent value"
	}
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if v.Kind() == reflect.String {
		return fmt.Sprintf("%q", v.String())
	}
	return fmt.Sprintf("%v", v.Interface())
}
