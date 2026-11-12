package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// docs/public/config.schema.json is the JSON Schema of the configuration file,
// published with the documentation for editors and other tools. This file
// generates it from the configuration types, the defaults and the notes
// below, and TestJSONSchema fails when the published file differs: a new
// field needs a note here, then
//
//	go test ./internal/config -run TestJSONSchema -update
//
// rewrites the file.
//
// The schema states what a document can state on its own: field names, types,
// enumerations, bounds and syntax. It leaves out every rule that involves two
// fields or the environment, so it never rejects a file that statica accepts;
// TestJSONSchemaAcceptsValidConfigurations keeps it so.
var updateJSONSchema = flag.Bool("update", false, "rewrite docs/public/config.schema.json")

var jsonSchemaFile = filepath.Join("..", "..", "docs", "public", "config.schema.json")

const (
	jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"
	jsonSchemaID      = "https://gtriggiano.github.io/statica/config.schema.json"

	// A string value can take all or part of its text from the environment.
	jsonSchemaEnvPattern      = `\$\{[A-Za-z_][A-Za-z0-9_]*\}`
	jsonSchemaDurationPattern = `^[+-]?(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h))+$`
	jsonSchemaSizePattern     = `^[0-9]+(KiB|MiB|GiB)$`
	jsonSchemaTokenPattern    = "^[A-Za-z0-9!#$%&'*+.^_`|~-]+$"
	jsonSchemaListenPattern   = `:[1-9][0-9]{0,4}$`
	jsonSchemaOriginPattern   = `^[Hh][Tt][Tt][Pp][Ss]?://`
	jsonSchemaPathPattern     = `^/`
	jsonSchemaPrefixPattern   = `^/(.*/)?$`
)

// jsonSchemaNote is what the types and defaults do not say about a field.
type jsonSchemaNote struct {
	doc       string
	required  bool // in its mapping
	noDefault bool // the default depends on another field
	enum      []string
	intEnum   []int
	min, max  int // of an integer; 0 leaves the bound out
	pattern   string
	minLength int
	// Of a list.
	minItems      int
	unique        bool
	itemEnum      []string
	itemPattern   string
	itemMinLength int
	// Of a mapping.
	oneProperty bool   // exactly one field
	keyPattern  string // of the keys of a map
}

var jsonSchemaNotes = map[string]jsonSchemaNote{
	"hosts": {doc: "Allowlist of request hostnames: exact DNS names or IP addresses, without scheme, port or wildcard. Without it, every valid hostname is accepted.", minItems: 1, itemMinLength: 1},

	"server":                         {doc: "The public listener."},
	"server.listen":                  {doc: "Address of the public listener, as host:port; an empty host listens on every interface.", pattern: jsonSchemaListenPattern},
	"server.tls":                     {doc: "Serves HTTPS on the public listener with one certificate chain. Without it, the listener serves plain HTTP."},
	"server.tls.cert_file":           {doc: "Path of the PEM certificate chain, leaf certificate first; a relative path is resolved against the directory of the configuration file.", required: true, minLength: 1},
	"server.tls.key_file":            {doc: "Path of the PEM private key that matches the leaf certificate.", required: true, minLength: 1},
	"server.tls.min_version":         {doc: "Minimum TLS version, as a quoted string.", enum: []string{"1.2", "1.3"}},
	"server.trusted_proxies":         {doc: "CIDR ranges of the proxies allowed to supply the client IP and the request ID. With an empty list, both headers are ignored.", itemPattern: `/[0-9]{1,3}$`},
	"server.client_ip_header":        {doc: "Header from which statica takes the client IP when the peer is a trusted proxy: X-Forwarded-For or X-Real-IP, in any letter case.", pattern: "^(" + jsonSchemaAnyCase("X-Forwarded-For") + "|" + jsonSchemaAnyCase("X-Real-IP") + ")$"},
	"server.request_id_header":       {doc: "Request header from which statica reuses a request ID when the peer is a trusted proxy.", pattern: jsonSchemaTokenPattern},
	"server.max_forwarded_hops":      {doc: "Maximum number of addresses statica reads from X-Forwarded-For.", min: 1, max: 256},
	"server.read_header_timeout":     {doc: "Time allowed to read the headers of a request. Positive."},
	"server.response_header_timeout": {doc: "Budget from the admission of a request to its response headers; when it expires first, the response is 504 Gateway Timeout. Positive."},
	"server.write_idle_timeout":      {doc: "Time within which each write of a response to the client must finish. Positive."},
	"server.idle_timeout":            {doc: "How long a keep-alive connection may stay idle between requests. Positive."},
	"server.request_timeout":         {doc: "Total time limit of a request, from its admission to the end of the response body; 0s sets no total limit."},
	"server.shutdown_timeout":        {doc: "Total budget of a graceful shutdown, from the first signal to the exit. Positive."},
	"server.max_header_bytes":        {doc: "Maximum size of the request line and headers, in bytes.", min: 1, max: 1 << 30},
	"server.max_connections":         {doc: "Maximum open connections on the public listener.", min: 1},
	"server.max_concurrent_requests": {doc: "Maximum requests in progress at once; a request over the limit gets 503 Service Unavailable.", min: 1},

	"admin":                                 {doc: "The admin listener, which serves /livez, /readyz and /metrics without authentication."},
	"admin.listen":                          {doc: "Address of the admin listener, as host:port. It must not share an address and port with server.listen.", pattern: jsonSchemaListenPattern},
	"admin.origin_health":                   {doc: "A background check of one object's metadata, which /readyz follows once enabled. Without it, readiness does not depend on the origin."},
	"admin.origin_health.enabled":           {doc: "Turns the check on."},
	"admin.origin_health.key":               {doc: "Object key to check, relative to s3.prefix. The default is the value of index.", noDefault: true, minLength: 1},
	"admin.origin_health.interval":          {doc: "Time from the end of one check to the start of the next. Positive."},
	"admin.origin_health.timeout":           {doc: "Deadline of one check. Positive and less than interval."},
	"admin.origin_health.failure_threshold": {doc: "Consecutive failed checks that turn a healthy origin unhealthy.", min: 1},
	"admin.origin_health.success_threshold": {doc: "Consecutive successful checks that turn an unhealthy origin healthy again.", min: 1},
	"admin.origin_health.max_staleness":     {doc: "Age of the last success beyond which /readyz reports origin_stale. Greater than interval plus timeout."},

	"s3":                            {doc: "The origin: one bucket and, optionally, a publication prefix in it. Credentials come from the standard AWS credential chain, never from this file.", required: true},
	"s3.bucket":                     {doc: "Bucket name.", required: true, pattern: `^[A-Za-z0-9][A-Za-z0-9._-]*$`},
	"s3.region":                     {doc: "Signing region. Without it, statica takes the region from the AWS configuration of the environment.", pattern: `^[A-Za-z0-9_-]+$`},
	"s3.prefix":                     {doc: "Publication prefix: the part of the bucket that holds the site. Empty selects the bucket root."},
	"s3.endpoint":                   {doc: "URL of an S3-compatible service: http or https, a host and an optional port. Without it, statica uses the AWS S3 endpoint of the region.", pattern: jsonSchemaOriginPattern},
	"s3.path_style":                 {doc: "Uses path-style requests (https://endpoint/bucket/key) instead of virtual-hosted-style requests."},
	"s3.allow_http":                 {doc: "Must be true for an http endpoint."},
	"s3.insecure_skip_verify":       {doc: "Skips verification of the origin's TLS certificate chain and hostname."},
	"s3.response_header_timeout":    {doc: "Time to wait for the response headers of one origin request; each retry waits again. Positive."},
	"s3.read_idle_timeout":          {doc: "Longest wait for data while statica reads an object body from the origin. Positive."},
	"s3.max_attempts":               {doc: "Attempts per origin request, the first included.", min: 1, max: 3},
	"s3.features":                   {doc: "Switches for optional origin capabilities: turn off a capability the provider does not support."},
	"s3.features.conditional_reads": {doc: "Allows If-None-Match and If-Match on origin reads."},
	"s3.features.range_reads":       {doc: "Allows Range on origin reads."},
	"s3.features.checksums":         {doc: "Asks the origin for object checksums and validates a checksum when the response carries one."},

	"cache":                            {doc: "The memory cache."},
	"cache.enabled":                    {doc: "false disables the memory cache: every request reads the origin, and no rule can enable it again."},
	"cache.max_bytes":                  {doc: "Memory charged to cache entries: bodies, metadata and a fixed overhead per entry. Positive."},
	"cache.max_entries":                {doc: "Maximum number of entries.", min: 1},
	"cache.max_object_bytes":           {doc: "Largest object body stored in memory; larger objects are streamed from the origin and not stored. Positive, at most max_bytes and at most max_fill_bytes."},
	"cache.max_fill_bytes":             {doc: "Memory for the bodies being read from the origin into the cache at the same time. Positive."},
	"cache.max_concurrent_fills":       {doc: "Maximum shared fills (origin reads into the cache) in progress at once.", min: 1},
	"cache.max_waiters_per_key":        {doc: "Maximum requests sharing one fill, the request that started it included; 1 disables sharing.", min: 1},
	"cache.fill_timeout":               {doc: "Lifetime of one shared fill or revalidation, from its start. Positive."},
	"cache.ttl":                        {doc: "How long a stored object stays fresh; cache.ttl_mode decides how it combines with the freshness lifetime that the origin assigns."},
	"cache.ttl_mode":                   {doc: "The role of cache.ttl when the origin assigns the object a freshness lifetime: ceiling caps it at the TTL, floor raises it to the TTL, fallback keeps it, fixed ignores it.", enum: []string{TTLModeCeiling, TTLModeFloor, TTLModeFallback, TTLModeFixed}},
	"cache.negative_ttl":               {doc: "How long statica remembers that an object does not exist; 0s stores no negative entries."},
	"cache.stale_if_error":             {doc: "How long after it expires an entry may still be served when refreshing it fails because the origin timed out, kept throttling or was unavailable."},
	"cache.request_directives":         {doc: "Which Cache-Control directives of a request the memory cache applies: honor applies no-store, no-cache and max-age, revalidate_only drops no-store, ignore drops all three.", enum: []string{RequestDirectivesHonor, RequestDirectivesRevalidateOnly, RequestDirectivesIgnore}},
	"cache.rules":                      {doc: "Overrides of the cache policy for some objects: the first rule whose match matches the object path applies, and the fields it omits keep the global value."},
	"cache.rules[].match":              {doc: "The object paths of the rule: exactly one of exact, prefix and regex.", required: true, oneProperty: true},
	"cache.rules[].match.exact":        {doc: "An absolute decoded object path, compared for equality; it cannot end in /.", noDefault: true, pattern: jsonSchemaPathPattern},
	"cache.rules[].match.prefix":       {doc: "An absolute path ending in /: it matches the paths under it and the path without the trailing slash.", noDefault: true, pattern: jsonSchemaPrefixPattern},
	"cache.rules[].match.regex":        {doc: "A regular expression in RE2 syntax, at most 1024 bytes, that the whole object path must match.", noDefault: true, minLength: 1},
	"cache.rules[].enabled":            {doc: "false bypasses the memory cache for the matching objects. Inherits cache.enabled."},
	"cache.rules[].ttl":                {doc: "Replaces cache.ttl for the matching objects."},
	"cache.rules[].ttl_mode":           {doc: "Replaces cache.ttl_mode for the matching objects.", enum: []string{TTLModeCeiling, TTLModeFloor, TTLModeFallback, TTLModeFixed}},
	"cache.rules[].negative_ttl":       {doc: "Replaces cache.negative_ttl for the matching objects."},
	"cache.rules[].stale_if_error":     {doc: "Replaces cache.stale_if_error for the matching objects."},
	"cache.rules[].request_directives": {doc: "Replaces cache.request_directives for the matching objects.", enum: []string{RequestDirectivesHonor, RequestDirectivesRevalidateOnly, RequestDirectivesIgnore}},

	"index":     {doc: "Name of the directory index object: a request path ending in / reads <path><index>.", pattern: `^[^/]+$`},
	"not_found": {doc: "Object key, relative to s3.prefix, of a custom 404 page. Without it, statica answers with a built-in plain-text 404 page.", minLength: 1},

	"errors":               {doc: "How statica answers some origin errors."},
	"errors.access_denied": {doc: "Response to a 403 Forbidden from the origin: not_found answers 404 Not Found, bad_gateway answers 502 Bad Gateway.", enum: []string{"not_found", "bad_gateway"}},

	"redirects":                           {doc: "Ordered redirect rules, evaluated before the origin is read: the first matching rule answers with its status and a Location header."},
	"redirects[].from":                    {doc: "Source template: an absolute path, then an optional ?query, with {name} placeholders.", required: true, pattern: jsonSchemaPathPattern},
	"redirects[].to":                      {doc: "Destination template: an absolute path or an https:// URL, which can use the placeholders bound in from.", required: true, minLength: 1},
	"redirects[].status":                  {doc: "Status of the redirect.", required: true, intEnum: []int{301, 302, 307, 308}},
	"redirects[].query":                   {doc: "What happens to the query of the request: drop removes it, preserve forwards it, unmatched forwards the fields that from does not match.", enum: []string{"drop", "preserve", "unmatched"}},
	"redirects[].captures":                {doc: "Constraints on placeholders, by placeholder name; each name must be bound in from.", noDefault: true, keyPattern: `^[A-Za-z_][A-Za-z0-9_]*$`},
	"redirects[].captures.<name>":         {doc: "The constraint for one placeholder."},
	"redirects[].captures.<name>.pattern": {doc: "A regular expression in RE2 syntax that the whole decoded value must match.", required: true, minLength: 1},

	"headers":            {doc: "Static response headers."},
	"headers.set":        {doc: "Headers added to every response statica writes on the public listener, errors and redirects included. Some header names are reserved.", keyPattern: jsonSchemaTokenPattern},
	"headers.set.<name>": {doc: "Value of the header, sent as written.", minLength: 1},

	"cache_control":                      {doc: "The Cache-Control header of the responses that serve an object."},
	"cache_control.default":              {doc: "Value for the requests no rule matches: auto or a Cache-Control value.", minLength: 1},
	"cache_control.rules":                {doc: "The first rule whose match matches the decoded request path sets the value."},
	"cache_control.rules[].match":        {doc: "The request paths of the rule: exactly one of exact, prefix and regex.", required: true, oneProperty: true},
	"cache_control.rules[].match.exact":  {doc: "An absolute decoded path, compared for equality with the request path.", noDefault: true, pattern: jsonSchemaPathPattern},
	"cache_control.rules[].match.prefix": {doc: "An absolute path ending in /: it matches the paths under it and the path without the trailing slash.", noDefault: true, pattern: jsonSchemaPrefixPattern},
	"cache_control.rules[].match.regex":  {doc: "A regular expression in RE2 syntax, at most 1024 bytes, that the whole request path must match.", noDefault: true, minLength: 1},
	"cache_control.rules[].value":        {doc: "auto or a Cache-Control value.", required: true, minLength: 1},

	"cors":                 {doc: "Cross-origin access to GET and HEAD. Without it, statica sends no CORS headers."},
	"cors.allowed_origins": {doc: "Exact origins, scheme://host with an optional port and nothing else.", required: true, minItems: 1, itemPattern: jsonSchemaOriginPattern + `[^/?#]+$`},

	"compression":           {doc: "Precompressed variants published next to an object; responses are never compressed on the fly."},
	"compression.mode":      {doc: "off serves the object itself; precompressed serves a published variant that the client accepts.", enum: []string{"off", "precompressed"}},
	"compression.encodings": {doc: "The codings statica looks for, in order of preference at equal quality. The default is [br, gzip] in precompressed mode; not allowed in off mode.", minItems: 1, unique: true, itemEnum: []string{"br", "gzip"}},

	"spa":                  {doc: "Serves an app shell for the client-side routes of a single-page application. Without it, there is no fallback."},
	"spa.fallback":         {doc: "Object key of the app shell, relative to s3.prefix.", required: true, minLength: 1},
	"spa.include_prefixes": {doc: "The fallback applies only under these prefixes: absolute paths ending in /.", minItems: 1, unique: true, itemPattern: jsonSchemaPrefixPattern},
	"spa.exclude_prefixes": {doc: "Paths under these prefixes never get the fallback: absolute paths ending in /.", unique: true, itemPattern: jsonSchemaPrefixPattern},

	"observability":                     {doc: "Logs, written as logfmt events on standard output."},
	"observability.log_level":           {doc: "Minimum level of the events written.", enum: []string{"debug", "info", "warn", "error"}},
	"observability.access_log":          {doc: "false stops the http_request events only."},
	"observability.log_client_ip":       {doc: "false leaves the client and peer addresses out of the http_request events."},
	"observability.redact_query_params": {doc: "Query parameters whose values are replaced with [REDACTED] in the logged path, compared ignoring ASCII case.", itemMinLength: 1},
}

// jsonSchemaOptionalBlocks is a configuration with every optional block and
// one item of every list, each with its required fields only: the other fields
// show the defaults that apply once the block is present.
const jsonSchemaOptionalBlocks = `
s3:
  bucket: example-websites
server:
  tls:
    cert_file: cert.pem
    key_file: key.pem
admin:
  origin_health: {}
cors:
  allowed_origins: ["https://www.example.com"]
spa:
  fallback: index.html
redirects:
  - from: /old
    to: /new
    status: 301
cache:
  rules:
    - match:
        prefix: /
cache_control:
  rules:
    - match:
        prefix: /
      value: auto
`

// jsonSchemaAnyCase is a pattern for text in any letter case.
func jsonSchemaAnyCase(text string) string {
	var b strings.Builder
	for _, r := range text {
		if lower, upper := strings.ToLower(string(r)), strings.ToUpper(string(r)); lower != upper {
			b.WriteString("[" + upper + lower + "]")
		} else {
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}

// jsonObject is a JSON object that keeps the order of its members.
type jsonObject []jsonMember
type jsonMember struct {
	key   string
	value any
}

func (o jsonObject) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := json.Marshal(m.key)
		if err != nil {
			return nil, err
		}
		value, err := jsonSchemaMarshal(m.value)
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// jsonSchemaMarshal keeps <, > and & as they are, for a file people read.
func jsonSchemaMarshal(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

type jsonSchemaBuilder struct {
	t        *testing.T
	defaults *Config // of a configuration without optional blocks
	optional *Config // of jsonSchemaOptionalBlocks
	used     map[string]bool
}

func buildJSONSchema(t *testing.T) []byte {
	t.Helper()
	optional, err := Parse([]byte(jsonSchemaOptionalBlocks), nil)
	if err != nil {
		t.Fatalf("jsonSchemaOptionalBlocks: %v", err)
	}
	b := &jsonSchemaBuilder{t: t, defaults: defaults(), optional: optional, used: map[string]bool{}}
	root := jsonObject{
		{"$schema", jsonSchemaDialect},
		{"$id", jsonSchemaID},
		{"title", "statica configuration"},
		{"description", "The configuration file of statica, a web server that serves one S3 bucket, or a prefix inside it, as one static website. " +
			"This schema checks field names, types, allowed values, integer bounds and the shape of some strings. " +
			"It does not check the rules that involve two fields (such as an admin listener that shares the address of the public one, or cache limits that must fit one another), the syntax of redirect templates and regular expressions, reserved header names, the syntax of hostnames, addresses, ports, object keys and Cache-Control values, or environment variables: " +
			"a string value can take text from the environment with ${NAME}, and such a value is checked only after substitution. " +
			"`statica config validate` is the authority on whether a file is valid."},
	}
	root = append(root, b.mapping(reflect.TypeOf(Config{}), "", true)...)
	root = append(root, jsonMember{"$defs", jsonObject{
		{"duration", jsonObject{
			{"description", "A duration with explicit units, such as 500ms, 30s, 5m or 1h30m. Write 0s for zero: a number without a unit is rejected."},
			{"type", "string"},
			{"anyOf", []any{jsonObject{{"pattern", jsonSchemaDurationPattern}}, jsonObject{{"$ref", "#/$defs/environment"}}}},
		}},
		{"byteSize", jsonObject{
			{"description", "A whole number of bytes with a binary unit: KiB, MiB or GiB, such as 512KiB or 128MiB."},
			{"type", "string"},
			{"anyOf", []any{jsonObject{{"pattern", jsonSchemaSizePattern}}, jsonObject{{"$ref", "#/$defs/environment"}}}},
		}},
		{"environment", jsonObject{
			{"description", "A string that takes all or part of its value from the environment, with ${NAME}. The value is checked after substitution."},
			{"type", "string"},
			{"pattern", jsonSchemaEnvPattern},
		}},
	}})
	for path := range jsonSchemaNotes {
		if !b.used[path] {
			t.Errorf("jsonSchemaNotes has %s, which is not a configuration field", path)
		}
	}
	var out bytes.Buffer
	raw, err := jsonSchemaMarshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func (b *jsonSchemaBuilder) note(path string) jsonSchemaNote {
	note, ok := jsonSchemaNotes[path]
	if !ok || note.doc == "" {
		b.t.Errorf("configuration field %s has no description in jsonSchemaNotes", path)
	}
	b.used[path] = true
	return note
}

// mapping returns the keywords of a struct: a closed set of fields.
func (b *jsonSchemaBuilder) mapping(t reflect.Type, prefix string, always bool) jsonObject {
	properties := jsonObject{}
	required := []string{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		path := childPath(prefix, fieldName(f))
		note := b.note(path)
		if note.required {
			required = append(required, fieldName(f))
		}
		properties = append(properties, jsonMember{fieldName(f), b.field(f, path, note, always)})
	}
	out := jsonObject{{"type", "object"}, {"additionalProperties", false}}
	if len(required) > 0 {
		out = append(out, jsonMember{"required", required})
	}
	return append(out, jsonMember{"properties", properties})
}

// field returns the schema of one field. always reports whether the field
// exists in every configuration: it is in no optional block and in no list.
func (b *jsonSchemaBuilder) field(f reflect.StructField, path string, note jsonSchemaNote, always bool) jsonObject {
	out := jsonObject{{"description", note.doc}}
	t := f.Type
	pointer := t.Kind() == reflect.Pointer
	if pointer {
		t = t.Elem()
	}
	switch {
	case t == durationType:
		out = append(out, jsonMember{"$ref", "#/$defs/duration"})
	case t == sizeType:
		out = append(out, jsonMember{"$ref", "#/$defs/byteSize"})
	case t.Kind() == reflect.Struct:
		out = append(out, b.mapping(t, path, always && !pointer)...)
		if note.oneProperty {
			out = append(out, jsonMember{"minProperties", 1}, jsonMember{"maxProperties", 1})
		}
		return out
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct:
		item := b.mapping(t.Elem(), path+"[]", false)
		out = append(out, jsonMember{"type", "array"}, jsonMember{"items", item})
		return out
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.String:
		out = append(out, jsonMember{"type", "array"})
		if note.minItems > 0 {
			out = append(out, jsonMember{"minItems", note.minItems})
		}
		if note.unique {
			out = append(out, jsonMember{"uniqueItems", true})
		}
		out = append(out, jsonMember{"items", jsonSchemaString(note.itemEnum, note.itemPattern, note.itemMinLength)})
	case t.Kind() == reflect.Map:
		entry := path + ".<name>"
		entryNote := b.note(entry)
		value := jsonObject{{"description", entryNote.doc}}
		if t.Elem().Kind() == reflect.Struct {
			value = append(value, b.mapping(t.Elem(), entry, false)...)
		} else {
			value = append(value, jsonSchemaString(entryNote.enum, entryNote.pattern, entryNote.minLength)...)
		}
		out = append(out, jsonMember{"type", "object"})
		if note.keyPattern != "" {
			out = append(out, jsonMember{"propertyNames", jsonObject{{"pattern", note.keyPattern}}})
		}
		out = append(out, jsonMember{"additionalProperties", value})
	case t.Kind() == reflect.String:
		out = append(out, jsonSchemaString(note.enum, note.pattern, note.minLength)...)
	case t.Kind() == reflect.Bool:
		out = append(out, jsonMember{"type", "boolean"})
	case t.Kind() == reflect.Int:
		out = append(out, jsonMember{"type", "integer"})
		if len(note.intEnum) > 0 {
			out = append(out, jsonMember{"enum", note.intEnum})
		}
		if note.min != 0 {
			out = append(out, jsonMember{"minimum", note.min})
		}
		if note.max != 0 {
			out = append(out, jsonMember{"maximum", note.max})
		}
	default:
		b.t.Fatalf("%s: no JSON Schema for the Go type %s", path, f.Type)
	}
	// A field has a default unless it is required, takes it from another
	// field, or is left unset: an override in a rule, or an omitted value.
	if note.required || note.noDefault || pointer {
		return out
	}
	source := b.optional
	if always {
		source = b.defaults
	}
	value := jsonSchemaValueAt(b.t, reflect.ValueOf(source).Elem(), path)
	if value.IsZero() && strings.Contains(f.Tag.Get("yaml"), ",omitempty") {
		return out
	}
	return append(out, jsonMember{"default", jsonSchemaLiteral(b.t, path, value)})
}

// jsonSchemaString returns the keywords of a string. A pattern or an
// enumeration applies to the text of the file only when no part of it comes
// from the environment.
func jsonSchemaString(enum []string, pattern string, minLength int) jsonObject {
	out := jsonObject{{"type", "string"}}
	if minLength > 0 {
		out = append(out, jsonMember{"minLength", minLength})
	}
	var literal jsonObject
	switch {
	case len(enum) > 0:
		literal = jsonObject{{"enum", enum}}
	case pattern != "":
		literal = jsonObject{{"pattern", pattern}}
	default:
		return out
	}
	return append(out, jsonMember{"anyOf", []any{literal, jsonObject{{"$ref", "#/$defs/environment"}}}})
}

// jsonSchemaValueAt follows a dotted path, taking the first item of a list.
func jsonSchemaValueAt(t *testing.T, v reflect.Value, path string) reflect.Value {
	t.Helper()
	for _, part := range strings.Split(path, ".") {
		list := strings.HasSuffix(part, "[]")
		part = strings.TrimSuffix(part, "[]")
		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				t.Fatalf("%s: the block is absent from jsonSchemaOptionalBlocks", path)
			}
			v = v.Elem()
		}
		found := false
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.PkgPath == "" && fieldName(f) == part {
				v, found = v.Field(i), true
				break
			}
		}
		if !found {
			t.Fatalf("%s: no field %s", path, part)
		}
		if list {
			if v.Len() == 0 {
				t.Fatalf("%s: the list is empty in jsonSchemaOptionalBlocks", path)
			}
			v = v.Index(0)
		}
	}
	return v
}

// jsonSchemaLiteral writes a default as the configuration file writes it.
func jsonSchemaLiteral(t *testing.T, path string, v reflect.Value) any {
	t.Helper()
	switch x := v.Interface().(type) {
	case time.Duration:
		for _, unit := range []struct {
			d time.Duration
			s string
		}{{time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}, {time.Millisecond, "ms"}} {
			if x != 0 && x%unit.d == 0 {
				return fmt.Sprintf("%d%s", x/unit.d, unit.s)
			}
		}
		if x == 0 {
			return "0s"
		}
		return x.String()
	case ByteSize:
		s, _ := x.MarshalYAML()
		return s
	case string, bool, int:
		return x
	case []string:
		return append([]string{}, x...)
	case map[string]string:
		return jsonObject{}
	}
	t.Fatalf("%s: no JSON literal for a default of type %s", path, v.Type())
	return nil
}

func TestJSONSchema(t *testing.T) {
	want := buildJSONSchema(t)
	if t.Failed() {
		return
	}
	if *updateJSONSchema {
		if err := os.WriteFile(jsonSchemaFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(jsonSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the configuration types, defaults and jsonSchemaNotes: regenerate it with\n\tgo test ./internal/config -run TestJSONSchema -update", filepath.ToSlash(jsonSchemaFile))
	}
}

// The published file, not the generated one, is what editors read.
func loadJSONSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(jsonSchemaFile)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// jsonSchemaDocument reads a YAML document as the values JSON Schema sees.
func jsonSchemaDocument(data []byte) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 {
		return nil, fmt.Errorf("expected one YAML document")
	}
	var convert func(n *yaml.Node) (any, error)
	convert = func(n *yaml.Node) (any, error) {
		switch n.Kind {
		case yaml.MappingNode:
			out := map[string]any{}
			for i := 0; i < len(n.Content); i += 2 {
				value, err := convert(n.Content[i+1])
				if err != nil {
					return nil, err
				}
				out[n.Content[i].Value] = value
			}
			return out, nil
		case yaml.SequenceNode:
			out := []any{}
			for _, item := range n.Content {
				value, err := convert(item)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			}
			return out, nil
		case yaml.ScalarNode:
			var value any
			if err := n.Decode(&value); err != nil {
				return nil, err
			}
			return value, nil
		}
		return nil, fmt.Errorf("line %d: unsupported YAML node", n.Line)
	}
	return convert(doc.Content[0])
}

// jsonSchemaProblems validates a value with the keywords the schema uses, and
// fails on any other keyword, so that the schema cannot grow a rule that no
// test exercises.
func jsonSchemaProblems(t *testing.T, root, schema map[string]any, value any, path string) []string {
	t.Helper()
	var problems []string
	report := func(format string, args ...any) {
		problems = append(problems, path+": "+fmt.Sprintf(format, args...))
	}
	keys := make([]string, 0, len(schema))
	for key := range schema {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		rule := schema[key]
		switch key {
		case "$schema", "$id", "$defs", "title", "description", "default":
		case "$ref":
			name := strings.TrimPrefix(rule.(string), "#/$defs/")
			problems = append(problems, jsonSchemaProblems(t, root, root["$defs"].(map[string]any)[name].(map[string]any), value, path)...)
		case "type":
			ok := false
			switch rule {
			case "object":
				_, ok = value.(map[string]any)
			case "array":
				_, ok = value.([]any)
			case "string":
				_, ok = value.(string)
			case "boolean":
				_, ok = value.(bool)
			case "integer":
				_, ok = value.(int)
			default:
				t.Fatalf("the test validator does not know the type %v", rule)
			}
			if !ok {
				report("expected %v", rule)
			}
		case "enum":
			found := false
			for _, allowed := range rule.([]any) {
				if number, isNumber := allowed.(float64); isNumber {
					allowed = int(number)
				}
				found = found || allowed == value
			}
			if !found {
				report("not one of %v", rule)
			}
		case "anyOf":
			matched := false
			for _, branch := range rule.([]any) {
				matched = matched || len(jsonSchemaProblems(t, root, branch.(map[string]any), value, path)) == 0
			}
			if !matched {
				report("matches no alternative")
			}
		case "pattern":
			if s, ok := value.(string); ok && !regexp.MustCompile(rule.(string)).MatchString(s) {
				report("does not match %v", rule)
			}
		case "minLength":
			if s, ok := value.(string); ok && len([]rune(s)) < int(rule.(float64)) {
				report("shorter than %v", rule)
			}
		case "minimum":
			if n, ok := value.(int); ok && float64(n) < rule.(float64) {
				report("less than %v", rule)
			}
		case "maximum":
			if n, ok := value.(int); ok && float64(n) > rule.(float64) {
				report("greater than %v", rule)
			}
		case "minItems":
			if list, ok := value.([]any); ok && len(list) < int(rule.(float64)) {
				report("fewer than %v items", rule)
			}
		case "uniqueItems":
			if list, ok := value.([]any); ok {
				seen := map[any]bool{}
				for _, item := range list {
					if seen[item] {
						report("duplicate item %v", item)
					}
					seen[item] = true
				}
			}
		case "items":
			if list, ok := value.([]any); ok {
				for i, item := range list {
					problems = append(problems, jsonSchemaProblems(t, root, rule.(map[string]any), item, fmt.Sprintf("%s[%d]", path, i))...)
				}
			}
		case "minProperties":
			if m, ok := value.(map[string]any); ok && len(m) < int(rule.(float64)) {
				report("fewer than %v fields", rule)
			}
		case "maxProperties":
			if m, ok := value.(map[string]any); ok && len(m) > int(rule.(float64)) {
				report("more than %v fields", rule)
			}
		case "required":
			if m, ok := value.(map[string]any); ok {
				for _, name := range rule.([]any) {
					if _, present := m[name.(string)]; !present {
						report("%v is required", name)
					}
				}
			}
		case "propertyNames":
			if m, ok := value.(map[string]any); ok {
				for name := range m {
					problems = append(problems, jsonSchemaProblems(t, root, rule.(map[string]any), name, childPath(path, name))...)
				}
			}
		case "properties":
			if m, ok := value.(map[string]any); ok {
				for name, sub := range rule.(map[string]any) {
					if item, present := m[name]; present {
						problems = append(problems, jsonSchemaProblems(t, root, sub.(map[string]any), item, childPath(path, name))...)
					}
				}
			}
		case "additionalProperties":
			m, ok := value.(map[string]any)
			if !ok {
				break
			}
			known, _ := schema["properties"].(map[string]any)
			for name, item := range m {
				if _, declared := known[name]; declared {
					continue
				}
				if sub, isSchema := rule.(map[string]any); isSchema {
					problems = append(problems, jsonSchemaProblems(t, root, sub, item, childPath(path, name))...)
				} else {
					report("unknown field %s", name)
				}
			}
		default:
			t.Fatalf("the test validator does not know the keyword %q", key)
		}
	}
	return problems
}

func jsonSchemaCheck(t *testing.T, schema map[string]any, data []byte) []string {
	t.Helper()
	value, err := jsonSchemaDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	return jsonSchemaProblems(t, schema, schema, value, "configuration")
}

// The schema never rejects a file that statica accepts: every example, every
// configuration snippet of the documentation, and values that only
// substitution makes valid.
func TestJSONSchemaAcceptsValidConfigurations(t *testing.T) {
	schema := loadJSONSchema(t)
	repo := filepath.Join("..", "..")
	lookup := func(string) (string, bool) { return "example-websites", true }
	check := func(name string, data []byte) {
		t.Helper()
		if _, err := Parse(data, lookup); err != nil {
			return // Reported by the test that owns the file.
		}
		for _, problem := range jsonSchemaCheck(t, schema, data) {
			t.Errorf("%s: statica accepts it, the schema does not: %s", name, problem)
		}
	}

	examples, err := filepath.Glob(filepath.Join(repo, "docs", "examples", "*.yaml"))
	if err != nil || len(examples) == 0 {
		t.Fatalf("no examples found: %v", err)
	}
	for _, path := range examples {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		check(filepath.Base(path), data)
	}

	err = filepath.WalkDir(filepath.Join(repo, "docs"), func(path string, entry fs.DirEntry, err error) error {
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
		snippets, _ := extractConfigSnippets(string(data))
		for _, snippet := range snippets {
			if complete, err := completeConfigSnippet(snippet.content); err == nil {
				check(fmt.Sprintf("%s:%d", filepath.Base(path), snippet.line), complete)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, text := range map[string]string{
		"defaults only":        "s3:\n  bucket: example-websites\n",
		"every optional block": jsonSchemaOptionalBlocks,
		"values from the environment": `
s3:
  bucket: ${BUCKET}
  region: ${REGION}
  prefix: releases/${RELEASE}
  endpoint: ${ENDPOINT}
server:
  listen: ${LISTEN}
  read_header_timeout: ${TIMEOUT}
cache:
  max_bytes: ${CACHE_SIZE}
  ttl_mode: ${TTL_MODE}
observability:
  log_level: ${LOG_LEVEL}
`,
		"spellings statica accepts": `
s3:
  bucket: example-websites
  endpoint: HTTPS://s3.example.com
server:
  client_ip_header: x-real-ip
  request_timeout: 0s
  idle_timeout: 1h30m
  write_idle_timeout: 1.5s
cache:
  stale_if_error: +10m
headers:
  set:
    X-Frame-Options: DENY
cors:
  allowed_origins: ["http://[2001:db8::10]:8080"]
`,
	} {
		data := []byte(text)
		environment := map[string]string{"BUCKET": "example-websites", "REGION": "eu-west-1", "RELEASE": "v1", "ENDPOINT": "https://s3.example.com", "LISTEN": ":8080", "TIMEOUT": "5s", "CACHE_SIZE": "64MiB", "TTL_MODE": "fixed", "LOG_LEVEL": "warn"}
		if _, err := Parse(data, func(k string) (string, bool) { v, ok := environment[k]; return v, ok }); err != nil {
			t.Errorf("%s: statica rejects a case meant to be valid: %v", name, err)
			continue
		}
		for _, problem := range jsonSchemaCheck(t, schema, data) {
			t.Errorf("%s: statica accepts it, the schema does not: %s", name, problem)
		}
	}
}

// Each kind of rule in the schema rejects something, and statica rejects it too.
func TestJSONSchemaRejectsInvalidConfigurations(t *testing.T) {
	schema := loadJSONSchema(t)
	for name, text := range map[string]string{
		"missing s3":            "index: index.html\n",
		"missing bucket":        "s3:\n  region: eu-west-1\n",
		"unknown field":         "s3:\n  bucket: example-websites\n  regoin: eu-west-1\n",
		"unknown block":         "s3:\n  bucket: example-websites\nlogging: {}\n",
		"number for a duration": "s3:\n  bucket: example-websites\nserver:\n  idle_timeout: 60\n",
		"duration without unit": "s3:\n  bucket: example-websites\nserver:\n  idle_timeout: \"60\"\n",
		"decimal size unit":     "s3:\n  bucket: example-websites\ncache:\n  max_bytes: 128MB\n",
		"string for an integer": "s3:\n  bucket: example-websites\nserver:\n  max_connections: \"1024\"\n",
		"string for a boolean":  "s3:\n  bucket: example-websites\ncache:\n  enabled: \"true\"\n",
		"integer below bound":   "s3:\n  bucket: example-websites\nserver:\n  max_connections: 0\n",
		"integer above bound":   "s3:\n  bucket: example-websites\n  max_attempts: 4\n",
		"unknown enum value":    "s3:\n  bucket: example-websites\ncache:\n  ttl_mode: max\n",
		"unquoted TLS version":  "s3:\n  bucket: example-websites\nserver:\n  tls:\n    cert_file: c.pem\n    key_file: k.pem\n    min_version: 1.3\n",
		"TLS without key":       "s3:\n  bucket: example-websites\nserver:\n  tls:\n    cert_file: c.pem\n",
		"redirect status":       "s3:\n  bucket: example-websites\nredirects:\n  - from: /a\n    to: /b\n    status: 303\n",
		"redirect without to":   "s3:\n  bucket: example-websites\nredirects:\n  - from: /a\n    status: 301\n",
		"two matchers":          "s3:\n  bucket: example-websites\ncache:\n  rules:\n    - match:\n        exact: /a\n        prefix: /b/\n",
		"no matcher":            "s3:\n  bucket: example-websites\ncache:\n  rules:\n    - match: {}\n",
		"prefix without slash":  "s3:\n  bucket: example-websites\ncache_control:\n  rules:\n    - match:\n        prefix: /assets\n      value: auto\n",
		"empty hosts":           "s3:\n  bucket: example-websites\nhosts: []\n",
		"empty CORS origins":    "s3:\n  bucket: example-websites\ncors:\n  allowed_origins: []\n",
		"CORS origin with path": "s3:\n  bucket: example-websites\ncors:\n  allowed_origins: [\"https://www.example.com/\"]\n",
		"duplicate encoding":    "s3:\n  bucket: example-websites\ncompression:\n  mode: precompressed\n  encodings: [br, br]\n",
		"unknown encoding":      "s3:\n  bucket: example-websites\ncompression:\n  mode: precompressed\n  encodings: [zstd]\n",
		"empty header value":    "s3:\n  bucket: example-websites\nheaders:\n  set:\n    X-Frame-Options: \"\"\n",
		"header name":           "s3:\n  bucket: example-websites\nheaders:\n  set:\n    \"X Frame\": DENY\n",
		"listener without port": "s3:\n  bucket: example-websites\nserver:\n  listen: localhost\n",
		"bucket path":           "s3:\n  bucket: example-websites/site\n",
		"index with a slash":    "s3:\n  bucket: example-websites\nindex: docs/index.html\n",
		"list for a mapping":    "s3:\n  bucket: example-websites\nheaders:\n  set: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			data := []byte(text)
			if _, err := Parse(data, nil); err == nil {
				t.Fatal("statica accepts a case meant to be invalid")
			}
			if len(jsonSchemaCheck(t, schema, data)) == 0 {
				t.Error("the schema accepts it")
			}
		})
	}
}
