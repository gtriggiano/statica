package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The configuration reference documents every field under a heading that is
// exactly its path in backticks, followed by a "**Type:**" line, a
// "**Default:**" line and, when validation narrows the type, an "**Allowed:**"
// line. A list item adds "[]" to the path and a map entry adds ".<name>":
// cache.rules[].match.prefix, headers.set.<name>.
//
// The default is a YAML literal in backticks or one of four words, so that
// "no default" never has two meanings:
//
//	required                  omitting the field is an error at its path
//	unset — <what happens>    the field stays unset, and no value that can
//	                          be written loads the same way
//	inherits `<field>`        unset here; the named field of the same type applies
//	derived from `<field>`    copied from the named field (derivedDefaults)
//
// An allowed line that starts with one of these forms is checked against
// Parse; any text may follow a form after "; ":
//
//	`a`, `b` or `c`           exactly these values load (for a list, as items)
//	`1` to `3`                the bounds load, their neighbors do not
//	positive                  zero is rejected
//	zero or positive          zero loads
//	nonempty                  "" or [] is rejected
//
// A list of values is complete: it equals the enumeration of the field in the
// JSON Schema (jsonSchemaNotes), which TestJSONSchema keeps equal to the
// published file. A string or list that rejects its empty value has an allowed
// line. The text after "; ", and an allowed line in free text, are not checked,
// except for the rules between two fields in crossFieldRules.
//
// Each top-level section with fields below it has a table of them, whose cells
// repeat these lines.
var referencePage = filepath.Join("..", "..", "docs", "reference", "configuration.md")

var (
	fieldHeadingRE   = regexp.MustCompile("^#{2,6} `([a-z0-9_]+(?:\\.[a-z0-9_]+|\\[\\]|\\.<name>)*)`$")
	typeLineRE       = regexp.MustCompile(`^\*\*Type:\*\* (.+)\\$`)
	defaultLiteralRE = regexp.MustCompile("^\\*\\*Default:\\*\\* `([^`]+)`$")
	defaultWordRE    = regexp.MustCompile("^\\*\\*Default:\\*\\* (required|unset|inherits|derived from)(?: `([^`]+)`)?(?: — (\\S.*))?$")
	allowedLineRE    = regexp.MustCompile(`^\*\*Allowed:\*\* (\S.*)$`)
	allowedEnumRE    = regexp.MustCompile("^`[^`]+`(?:(?:, | or )`[^`]+`)+$")
	allowedRangeRE   = regexp.MustCompile("^`(-?[0-9]+)` to `(-?[0-9]+)`$")
	codeRE           = regexp.MustCompile("`([^`]+)`")
	tableHeader      = "| Field | Type | Default | Allowed |"
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

// codeEnums are the fields whose values the code names with constants: their
// allowed line lists exactly these.
var codeEnums = func() map[string][]string {
	ttlModes := []string{TTLModeCeiling, TTLModeFloor, TTLModeFallback, TTLModeFixed}
	directives := []string{RequestDirectivesHonor, RequestDirectivesRevalidateOnly, RequestDirectivesIgnore}
	return map[string][]string{
		"cache.ttl_mode":                   ttlModes,
		"cache.rules[].ttl_mode":           ttlModes,
		"cache.request_directives":         directives,
		"cache.rules[].request_directives": directives,
	}
}()

// schemaEnum is the enumeration of the field in the JSON Schema, as the page
// writes its values, and false for a field without one. A list has the
// enumeration of its items.
func schemaEnum(path string) ([]string, bool) {
	note := jsonSchemaNotes[path]
	var values []string
	for _, n := range note.intEnum {
		values = append(values, strconv.Itoa(n))
	}
	values = append(values, note.enum...)
	values = append(values, note.itemEnum...)
	return values, len(values) > 0
}

// patternEnums are the fields whose listed values are spelled in any letter
// case, which the JSON Schema expresses with a pattern instead of an
// enumeration.
var patternEnums = map[string][]string{
	"server.client_ip_header": {"X-Forwarded-For", "X-Real-IP"},
}

// crossFieldRules are the rules between two fields that the page states after
// "; " on the allowed line of field. The line names every field of related,
// by its path or by its last segment, and Parse accepts the values of valid
// and rejects those of invalid, which differ at the boundary of the rule.
var crossFieldRules = []struct {
	field          string
	related        []string
	valid, invalid map[string]string
}{
	{
		field:   "admin.origin_health.timeout",
		related: []string{"admin.origin_health.interval"},
		valid:   map[string]string{"admin.origin_health.interval": "10s", "admin.origin_health.timeout": "9999ms", "admin.origin_health.max_staleness": "1h"},
		invalid: map[string]string{"admin.origin_health.interval": "10s", "admin.origin_health.timeout": "10s", "admin.origin_health.max_staleness": "1h"},
	},
	{
		field:   "admin.origin_health.max_staleness",
		related: []string{"admin.origin_health.interval", "admin.origin_health.timeout"},
		valid:   map[string]string{"admin.origin_health.interval": "10s", "admin.origin_health.timeout": "2s", "admin.origin_health.max_staleness": "12001ms"},
		invalid: map[string]string{"admin.origin_health.interval": "10s", "admin.origin_health.timeout": "2s", "admin.origin_health.max_staleness": "12s"},
	},
	{
		field:   "cache.max_object_bytes",
		related: []string{"cache.max_bytes"},
		valid:   map[string]string{"cache.max_bytes": "8MiB", "cache.max_fill_bytes": "16MiB", "cache.max_object_bytes": "8MiB"},
		invalid: map[string]string{"cache.max_bytes": "8MiB", "cache.max_fill_bytes": "16MiB", "cache.max_object_bytes": "8193KiB"},
	},
	{
		field:   "cache.max_object_bytes",
		related: []string{"cache.max_fill_bytes"},
		valid:   map[string]string{"cache.max_bytes": "16MiB", "cache.max_fill_bytes": "8MiB", "cache.max_object_bytes": "8MiB"},
		invalid: map[string]string{"cache.max_bytes": "16MiB", "cache.max_fill_bytes": "8MiB", "cache.max_object_bytes": "8193KiB"},
	},
}

type schemaField struct {
	path string
	typ  reflect.Type
}

type referenceEntry struct {
	line    int
	typ     string // the documented type
	literal string // the documented default, empty when it is a word
	word    string // required, unset, inherits or derived from
	ref     string // the field that "inherits" and "derived from" name
	summary string // the default as the section's table repeats it
	allowed string // the text of the **Allowed:** line, or empty
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
			if entry.ref != "" {
				if named, ok := documented[entry.ref]; !ok || named.typ != entry.typ {
					t.Errorf("%s:%d: %s: the default names `%s`, which is not a field of type %s", referencePage, entry.line, field.path, entry.ref, entry.typ)
				}
			}
			checkNoDefault(t, field, entry)
		} else {
			checkDefault(t, field.path, entry)
		}
		checkAllowed(t, field, entry)
	}
	checkCrossFieldRules(t, documented)
	checkSummaryTables(t, string(data), documented)
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
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
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
		// The first paragraph of the section: **Type:**, **Default:** and an
		// optional **Allowed:**, joined by hard line breaks.
		j := i + 1
		for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
			j++
		}
		var body []string
		for ; j < len(lines) && strings.TrimSpace(lines[j]) != ""; j++ {
			body = append(body, lines[j])
		}
		for k := 0; k < len(body)-1; k++ {
			if !strings.HasSuffix(body[k], `\`) {
				t.Errorf(`%s:%d: %s: every line of the first paragraph but the last must end with \`, referencePage, line, path)
			}
			body[k] = strings.TrimSuffix(body[k], `\`)
		}
		var typ []string
		if len(body) == 2 || len(body) == 3 {
			typ = typeLineRE.FindStringSubmatch(body[0] + `\`)
		}
		if typ == nil {
			t.Errorf("%s:%d: %s: the section must start with a **Type:** line, a **Default:** line and an optional **Allowed:** line", referencePage, line, path)
			continue
		}
		entry := referenceEntry{line: line, typ: typ[1]}
		if m := defaultLiteralRE.FindStringSubmatch(body[1]); m != nil {
			entry.literal, entry.summary = m[1], "`"+m[1]+"`"
		} else if m := defaultWordRE.FindStringSubmatch(body[1]); m != nil && (m[2] != "") == (m[1] == "inherits" || m[1] == "derived from") && (m[1] != "unset" || m[3] != "") {
			entry.word, entry.ref, entry.summary = m[1], m[2], m[1]
			if m[2] != "" {
				entry.summary += " `" + m[2] + "`"
			}
		} else {
			t.Errorf("%s:%d: %s: the line after **Type:** must be **Default:** followed by `<yaml literal>`, required, unset — <what happens>, inherits `<field>` or derived from `<field>`", referencePage, line, path)
			continue
		}
		if len(body) == 3 {
			m := allowedLineRE.FindStringSubmatch(body[2])
			if m == nil {
				t.Errorf("%s:%d: %s: the line after **Default:** must be **Allowed:** <constraint>", referencePage, line, path)
				continue
			}
			entry.allowed = m[1]
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

// checkNoDefault checks a field whose default is a word. A field derived from
// another field must follow it. A field is documented as required exactly
// when omitting it makes the configuration invalid at its path. Any other
// field must stay unset when omitted, and no explicit value may load the same
// way: "unset" and "inherits" never stand for false, zero or an empty string
// that can be written.
func checkNoDefault(t *testing.T, field schemaField, entry referenceEntry) {
	t.Helper()
	path := field.path
	d, derived := derivedDefaults[path]
	if derived != (entry.word == "derived from") || derived && d.from != entry.ref {
		t.Errorf("%s:%d: %s: a default is documented as derived from a field exactly when derivedDefaults names that field", referencePage, entry.line, path)
		return
	}
	if derived {
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
	if documented := entry.word == "required"; documented != required {
		if documented {
			t.Errorf("%s:%d: %s is documented as required, but omitting it does not fail at its path (error: %v)", referencePage, entry.line, path, err)
		} else {
			t.Errorf("%s:%d: omitting %s fails at its path, so document it as **Default:** required: %v", referencePage, entry.line, path, err)
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

// loads reports whether the base configuration loads with the field set to
// the YAML literal.
func loads(t *testing.T, path, literal string) bool {
	t.Helper()
	value, err := yamlValue(literal)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	_, err = Parse(referenceVariant(t, path, value), noEnv)
	return err == nil
}

// checkAllowed checks the **Allowed:** line of a field against Parse. Numbers
// always have one: every integer, duration and byte size is constrained.
func checkAllowed(t *testing.T, field schemaField, entry referenceEntry) {
	t.Helper()
	path, typ := field.path, typeName(field.typ)
	at := fmt.Sprintf("%s:%d: %s", referencePage, entry.line, path)
	form, _, _ := strings.Cut(entry.allowed, "; ")
	zero := zeroLiteral(field.typ)
	if strings.HasPrefix(typ, "list of ") {
		zero = "[]"
	}
	item := func(literal string) string {
		if typ == "list of strings" {
			return "[" + literal + "]"
		}
		return literal
	}
	if values, ok := codeEnums[path]; ok {
		var listed []string
		for _, m := range codeRE.FindAllStringSubmatch(form, -1) {
			listed = append(listed, m[1])
		}
		if !allowedEnumRE.MatchString(form) || !slices.Equal(listed, values) {
			t.Errorf("%s: the allowed line must list the values of the code, in order: %s", at, strings.Join(values, ", "))
		}
	}
	// A list of values on the page and the enumeration of the schema are the
	// same list.
	enum, hasEnum := schemaEnum(path)
	if values, ok := patternEnums[path]; ok {
		enum, hasEnum = values, true
	}
	if listed := allowedEnumRE.MatchString(form); listed || hasEnum {
		var values []string
		if listed {
			for _, m := range codeRE.FindAllStringSubmatch(form, -1) {
				values = append(values, strings.Trim(m[1], `"`))
			}
		}
		if !slices.Equal(values, enum) {
			t.Errorf("%s: the allowed line lists the values [%s], but the JSON Schema (jsonSchemaNotes) has [%s]: list the same values, in the same order", at, strings.Join(values, ", "), strings.Join(enum, ", "))
		}
	}
	switch {
	case allowedEnumRE.MatchString(form):
		for _, m := range codeRE.FindAllStringSubmatch(form, -1) {
			if !loads(t, path, item(m[1])) {
				t.Errorf("%s: the allowed value `%s` is rejected", at, m[1])
			}
		}
		other := "statica-other-value"
		if typ == "integer" {
			other = "1"
		}
		if loads(t, path, item(other)) {
			t.Errorf("%s: the allowed line lists every value, but `%s` loads too", at, other)
		}
	case allowedRangeRE.MatchString(form):
		m := allowedRangeRE.FindStringSubmatch(form)
		low, _ := strconv.Atoi(m[1])
		high, _ := strconv.Atoi(m[2])
		for _, n := range []int{low, high} {
			if !loads(t, path, strconv.Itoa(n)) {
				t.Errorf("%s: the allowed bound %d is rejected", at, n)
			}
		}
		for _, n := range []int{low - 1, high + 1} {
			if loads(t, path, strconv.Itoa(n)) {
				t.Errorf("%s: %d is outside the allowed range, but it loads", at, n)
			}
		}
	case form == "positive" || form == "nonempty":
		if zero == "" {
			t.Errorf("%s: %q does not apply to a %s", at, form, typ)
		} else if loads(t, path, zero) {
			t.Errorf("%s: documented as %s, but `%s` loads", at, form, zero)
		}
	case form == "zero or positive":
		if zero == "" || !loads(t, path, zero) {
			t.Errorf("%s: documented as zero or positive, but `%s` is rejected", at, zero)
		}
	default:
		if typ == "integer" || typ == "duration" || typ == "byte size" {
			t.Errorf("%s: a %s needs an **Allowed:** line that starts with positive, zero or positive, a range (`1` to `3`) or a list of values", at, typ)
		}
		// Validation narrows a string or a list that rejects its empty value.
		if entry.allowed == "" && (typ == "string" || typ == "list of strings") && !loads(t, path, zero) {
			t.Errorf("%s: `%s` is rejected, so the field needs an **Allowed:** line", at, zero)
		}
	}
}

// checkCrossFieldRules checks each rule of crossFieldRules against Parse and
// against the allowed line of its field.
func checkCrossFieldRules(t *testing.T, documented map[string]referenceEntry) {
	t.Helper()
	with := func(values map[string]string) error {
		data := []byte(referenceBase)
		for path, literal := range values {
			value, err := yamlValue(literal)
			if err != nil {
				t.Fatal(err)
			}
			data = variantOf(t, data, path, value)
		}
		_, err := Parse(data, noEnv)
		return err
	}
	for _, rule := range crossFieldRules {
		entry, ok := documented[rule.field]
		if !ok {
			continue
		}
		at := fmt.Sprintf("%s:%d: %s", referencePage, entry.line, rule.field)
		if err := with(rule.valid); err != nil {
			t.Errorf("%s: %v must load: %v", at, rule.valid, err)
		}
		var invalid *Error
		if err := with(rule.invalid); !errors.As(err, &invalid) || invalid.Path != rule.field {
			t.Errorf("%s: %v must be rejected at the field (error: %v)", at, rule.invalid, err)
		}
		_, text, _ := strings.Cut(entry.allowed, "; ")
		for _, related := range rule.related {
			short := related[strings.LastIndex(related, ".")+1:]
			if !strings.Contains(text, "`"+related+"`") && !strings.Contains(text, "`"+short+"`") {
				t.Errorf("%s: the rule involves %s: name it after \"; \" on the allowed line", at, related)
			}
		}
	}
}

// checkSummaryTables checks that each top-level field section with fields
// below it has one table that lists them in order, and that each row repeats
// the type, the default and the allowed line of its field.
func checkSummaryTables(t *testing.T, page string, documented map[string]referenceEntry) {
	t.Helper()
	var section string     // the top-level field of the current section
	var want, got []string // the rows of its table
	finish := func() {
		if section == "" {
			return
		}
		if len(want) > 0 && len(got) == 0 {
			t.Errorf("%s: section %s needs a table that starts with %s", referencePage, section, tableHeader)
		}
		for i := 0; i < len(want) || i < len(got); i++ {
			switch {
			case i >= len(got):
				t.Errorf("%s: the table of %s lacks the row %s", referencePage, section, want[i])
			case i >= len(want):
				t.Errorf("%s: the table of %s has a row without a field below it: %s", referencePage, section, got[i])
			case got[i] != want[i]:
				t.Errorf("%s: the table of %s has the row\n\t%s\nwant\n\t%s", referencePage, section, got[i], want[i])
			}
		}
	}
	lines := strings.Split(page, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if _, marker, length, _, ok := openingFence(line); ok {
			for i++; i < len(lines) && !closingFence(lines[i], marker, length); i++ {
			}
			continue
		}
		if strings.HasPrefix(line, "## ") {
			finish()
			section, want, got = "", nil, nil
		}
		if line == tableHeader && section != "" {
			for i += 2; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
				got = append(got, lines[i])
			}
			i--
			continue
		}
		m := fieldHeadingRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		path := m[1]
		if strings.HasPrefix(line, "## ") {
			section = path
			continue
		}
		entry, ok := documented[path]
		if !ok || section == "" {
			continue
		}
		anchor := strings.Trim(anchorRE.ReplaceAllString(path, "-"), "-")
		want = append(want, fmt.Sprintf("| [`%s`](#%s) | %s | %s | %s |", path, anchor, entry.typ, entry.summary, entry.allowed))
	}
	finish()
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
	return variantOf(t, []byte(referenceBase), path, value)
}

// variantOf is referenceVariant for another configuration.
func variantOf(t *testing.T, base []byte, path string, value *yaml.Node) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(base, &doc); err != nil {
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
