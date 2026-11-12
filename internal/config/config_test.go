package config

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

const minimal = "s3:\n  bucket: example-websites\n"

func noEnv(string) (string, bool) { return "", false }
func parseOK(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDefaultsAndExplicitValues(t *testing.T) {
	c := parseOK(t, minimal)
	if c.Server.Listen != "127.0.0.1:8080" || c.Admin.Listen != "127.0.0.1:9090" || c.Index != "index.html" || c.S3.Region != "" || c.S3.Prefix != "" {
		t.Fatalf("wrong core defaults: %#v", c)
	}
	if !c.Cache.Enabled || c.Cache.MaxBytes != 128<<20 || c.Cache.MaxEntries != 10000 || c.Cache.MaxObjectBytes != 2<<20 || c.Cache.MaxFillBytes != 32<<20 || c.Cache.MaxConcurrentFills != 16 || c.Cache.MaxWaitersPerKey != 64 || c.Cache.FillTimeout != 30*time.Second || c.Cache.TTL != 5*time.Minute || c.Cache.TTLMode != "ceiling" || c.Cache.NegativeTTL != 5*time.Second || c.Cache.StaleIfError != 0 || c.Cache.RequestDirectives != "honor" {
		t.Fatalf("wrong cache defaults: %#v", c.Cache)
	}
	if c.Server.RequestTimeout != 0 || c.Server.ResponseHeaderTimeout != 30*time.Second || c.Server.WriteIdleTimeout != time.Minute || c.S3.ResponseHeaderTimeout != 10*time.Second || c.S3.ReadIdleTimeout != 30*time.Second || c.S3.MaxAttempts != 2 || !c.S3.Features.ConditionalReads || !c.S3.Features.RangeReads || c.S3.Features.Checksums {
		t.Fatal("wrong deadline/provider defaults")
	}
	if c.Compression.Mode != "off" || c.SPA != nil || c.CORS != nil || c.Admin.OriginHealth != nil || c.Server.TLS != nil || c.CacheControl.Default != "auto" || c.Errors.AccessDenied != "not_found" || len(c.Observability.RedactQueryParams) != 9 {
		t.Fatal("wrong opt-in defaults")
	}
}

func TestExplicitZerosAndFalseRemain(t *testing.T) {
	c := parseOK(t, minimal+"cache:\n  enabled: false\n  ttl: 0s\n  negative_ttl: 0s\nobservability:\n  access_log: false\n  log_client_ip: false\n  redact_query_params: []\n")
	if c.Cache.Enabled || c.Cache.TTL != 0 || c.Cache.NegativeTTL != 0 || c.Observability.AccessLog || c.Observability.LogClientIP || len(c.Observability.RedactQueryParams) != 0 {
		t.Fatal("explicit false/zero/empty list was defaulted")
	}
}

func TestRepositoryExamples(t *testing.T) {
	for _, name := range []string{"minimal", "advanced", "local-s3", "container"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "..", "docs", "examples", name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			c, err := Parse(data, func(k string) (string, bool) { return "example-websites", k == "SITE_BUCKET" })
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.MarshalRedacted(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrefixNormalizationAndConfinement(t *testing.T) {
	for _, input := range []string{"docs", "/docs", "docs/", "/docs/", "///docs///"} {
		t.Run(input, func(t *testing.T) {
			data := "s3:\n  bucket: example-websites\n  prefix: " + strconvQuote(input) + "\n"
			c := parseOK(t, data)
			if c.S3.Prefix != "docs/" {
				t.Fatalf("prefix %q", c.S3.Prefix)
			}
		})
	}
	for _, input := range []string{"", "/", "////"} {
		c := parseOK(t, "s3:\n  bucket: example-websites\n  prefix: "+strconvQuote(input)+"\n")
		if c.S3.Prefix != "" {
			t.Fatalf("root prefix %q", c.S3.Prefix)
		}
	}
	c := parseOK(t, "s3:\n  bucket: example-websites\n  prefix: '/ café /files%2Fraw/'\n")
	if c.S3.Prefix != " café /files%2Fraw/" {
		t.Fatal("prefix bytes were cleaned or decoded")
	}
	for _, input := range []string{"docs//current", "a/./b", "a/../b", "a\\b", "a\x00b"} {
		t.Run(input, func(t *testing.T) {
			_, err := Parse([]byte("s3:\n  bucket: example-websites\n  prefix: "+strconvQuote(input)+"\n"), noEnv)
			if err == nil {
				t.Fatal("unsafe prefix accepted")
			}
		})
	}
}

func strconvQuote(s string) string { return strconv.Quote(s) }

func TestStrictYAML(t *testing.T) {
	cases := []struct{ name, input, path string }{
		{"empty", "", ""}, {"sequence", "- one\n", ""}, {"multidoc", minimal + "---\nindex: index.html\n", ""}, {"trailingemptydoc", minimal + "---\n", ""}, {"duplicate", "index: a.html\nindex: b.html\ns3: {bucket: x}\n", "index"}, {"nestedduplicate", minimal + "cache: {ttl: 1m, ttl: 2m}\n", "cache.ttl"}, {"unknown", minimal + "cache: {unrecognized: true}\n", "cache.unrecognized"}, {"nonstringkey", minimal + "headers: {set: {true: yes}}\n", "headers.set"}, {"alias", minimal + "index: &index index.html\nnot_found: *index\n", "not_found"}, {"merge", "s3: {bucket: x}\ncache: {<<: {ttl: 1m}}\n", "cache"}, {"customtag", minimal + "index: !secret index.html\n", "index"}, {"nestedcustomtag", minimal + "cache: !custom {ttl: 1m}\n", "cache"}, {"null", minimal + "index:\n", "index"}, {"nullblock", minimal + "cache: null\n", "cache"}, {"nullslice", minimal + "hosts: null\n", "hosts"}, {"emptystring", minimal + "index: ''\n", "index"}, {"quotedbool", minimal + "cache: {enabled: 'false'}\n", "cache.enabled"}, {"yaml11bool", minimal + "cache: {enabled: yes}\n", "cache.enabled"}, {"numberstring", minimal + "cache: {max_entries: '1'}\n", "cache.max_entries"}, {"hexinteger", minimal + "cache: {max_entries: 0x1}\n", "cache.max_entries"}, {"floatinteger", minimal + "cache: {max_entries: 1.0}\n", "cache.max_entries"}, {"integerstring", minimal + "index: 12\n", "index"}, {"timestampscalar", minimal + "index: 2026-11-06\n", "index"}, {"integerduration", minimal + "cache: {ttl: 0}\n", "cache.ttl"}, {"unitlesszeroduration", minimal + "cache: {ttl: '0'}\n", "cache.ttl"}, {"decimalbytes", minimal + "cache: {max_bytes: 128MB}\n", "cache.max_bytes"}, {"fractionalbytes", minimal + "cache: {max_bytes: 1.5MiB}\n", "cache.max_bytes"}, {"negativebytes", minimal + "cache: {max_bytes: -1MiB}\n", "cache.max_bytes"}, {"integerbytes", minimal + "cache: {max_bytes: 1024}\n", "cache.max_bytes"}, {"overflowbytes", minimal + "cache: {max_bytes: 999999999999999999GiB}\n", "cache.max_bytes"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.input), noEnv)
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			if tt.path != "" {
				var ce *Error
				if !errors.As(err, &ce) || ce.Path != tt.path || ce.Line == 0 || ce.Column == 0 {
					t.Fatalf("missing field location: %v", err)
				}
			}
		})
	}
	parseOK(t, "s3: {bucket: !!str example-websites}\ncache: {enabled: !!bool false, max_entries: !!int 10}\n")
	_, err := Parse([]byte(minimal+strings.Repeat("#", MaxConfigBytes)), noEnv)
	if err == nil {
		t.Fatal("oversized configuration accepted")
	}
}

func TestValidationFailures(t *testing.T) {
	cases := []struct{ name, input, path string }{
		{"removedversion", "s3: {bucket: x}\nversion: 1\n", "version"}, {"bucket", "s3: {bucket: 'arn:aws:s3:::x'}", "s3.bucket"}, {"templatedbucket", "s3: {bucket: '{host}'}", "s3.bucket"}, {"region", "s3: {bucket: x, region: ''}", "s3.region"},
		{"emptyhosts", minimal + "hosts: []", "hosts"}, {"duplicatehost", minimal + "hosts: [Example.COM., example.com]", "hosts[1]"}, {"schemehost", minimal + "hosts: ['https://example.com']", "hosts[0]"}, {"porthost", minimal + "hosts: ['example.com:443']", "hosts[0]"}, {"wildcardhost", minimal + "hosts: ['*.example.com']", "hosts[0]"}, {"badhost", minimal + "hosts: ['a..example.com']", "hosts[0]"},
		{"listen", minimal + "server: {listen: localhost}", "server.listen"}, {"adminlisten", minimal + "admin: {listen: ':abc'}", "admin.listen"}, {"sameport", minimal + "admin: {listen: '127.0.0.1:8080'}", "admin.listen"}, {"tlsmissingkey", minimal + "server: {tls: {cert_file: cert.pem}}", "server.tls.key_file"}, {"tlsversion", minimal + "server: {tls: {cert_file: cert, key_file: key, min_version: '1.1'}}", "server.tls.min_version"},
		{"trustall", minimal + "server: {trusted_proxies: [0.0.0.0/0]}", "server.trusted_proxies[0]"}, {"trustall6", minimal + "server: {trusted_proxies: ['::/0']}", "server.trusted_proxies[0]"}, {"proxyip", minimal + "server: {trusted_proxies: [10.0.0.1]}", "server.trusted_proxies[0]"}, {"ipheader", minimal + "server: {client_ip_header: Forwarded}", "server.client_ip_header"}, {"requestid", minimal + "server: {request_id_header: Cookie}", "server.request_id_header"}, {"requestidip", minimal + "server: {request_id_header: X-Forwarded-For}", "server.request_id_header"}, {"requestidnewline", minimal + "server: {request_id_header: 'bad header'}", "server.request_id_header"}, {"hopszero", minimal + "server: {max_forwarded_hops: 0}", "server.max_forwarded_hops"},
		{"requestnegative", minimal + "server: {request_timeout: -1s}", "server.request_timeout"}, {"preparationzero", minimal + "server: {response_header_timeout: 0s}", "server.response_header_timeout"}, {"readzero", minimal + "server: {read_header_timeout: 0s}", "server.read_header_timeout"}, {"fillzero", minimal + "cache: {fill_timeout: 0s}", "cache.fill_timeout"}, {"capacityzero", minimal + "cache: {max_entries: 0}", "cache.max_entries"}, {"oversizeobject", minimal + "cache: {max_bytes: 1MiB}", "cache.max_object_bytes"}, {"oversizefill", minimal + "cache: {max_fill_bytes: 1MiB}", "cache.max_object_bytes"}, {"negativettl", minimal + "cache: {ttl: -1s}", "cache.ttl"},
		{"missingmatch", minimal + "cache: {rules: [{ttl: 1m}]}", "cache.rules[0].match"}, {"bothmatch", minimal + "cache: {rules: [{match: {exact: /a, prefix: /a/}}]}", "cache.rules[0].match"}, {"prefixboundary", minimal + "cache: {rules: [{match: {prefix: /assets}}]}", "cache.rules[0].match.prefix"}, {"rulecapacity", minimal + "cache: {rules: [{match: {exact: /a}, max_bytes: 1GiB}]}", "cache.rules[0].max_bytes"},
		{"ttlmode", minimal + "cache: {ttl_mode: origin}", "cache.ttl_mode"}, {"ttlmodeempty", minimal + "cache: {ttl_mode: ''}", "cache.ttl_mode"}, {"ttlmodecase", minimal + "cache: {ttl_mode: Fixed}", "cache.ttl_mode"}, {"rulettlmode", minimal + "cache: {rules: [{match: {prefix: /a/}, ttl_mode: ignore}]}", "cache.rules[0].ttl_mode"}, {"rulettlmodeempty", minimal + "cache: {rules: [{match: {prefix: /a/}, ttl_mode: ''}]}", "cache.rules[0].ttl_mode"},
		{"requestdirectives", minimal + "cache: {request_directives: bypass}", "cache.request_directives"}, {"requestdirectivesempty", minimal + "cache: {request_directives: ''}", "cache.request_directives"}, {"requestdirectivesbool", minimal + "cache: {request_directives: false}", "cache.request_directives"}, {"rulerequestdirectives", minimal + "cache: {rules: [{match: {exact: /a}, request_directives: honour}]}", "cache.rules[0].request_directives"}, {"rulerequestdirectivesempty", minimal + "cache: {rules: [{match: {exact: /a}, request_directives: ''}]}", "cache.rules[0].request_directives"},
		{"healthtimeout", minimal + "admin: {origin_health: {enabled: true, timeout: 30s}}", "admin.origin_health.timeout"}, {"healthstale", minimal + "admin: {origin_health: {enabled: true, max_staleness: 33s}}", "admin.origin_health.max_staleness"}, {"healthkey", minimal + "admin: {origin_health: {key: ../private}}", "admin.origin_health.key"},
		{"badindex", minimal + "index: a/b.html", "index"}, {"badnotfound", minimal + "not_found: /404.html", "not_found"}, {"denial", minimal + "errors: {access_denied: ignore}", "errors.access_denied"}, {"compression", minimal + "compression: {mode: dynamic}", "compression.mode"}, {"offencodings", minimal + "compression: {encodings: []}", "compression.encodings"}, {"emptyencodings", minimal + "compression: {mode: precompressed, encodings: []}", "compression.encodings"}, {"duplicateencodings", minimal + "compression: {mode: precompressed, encodings: [br, br]}", "compression.encodings[1]"}, {"spashell", minimal + "spa: {}", "spa.fallback"}, {"spaincludes", minimal + "spa: {fallback: index.html, include_prefixes: []}", "spa.include_prefixes"}, {"spaexcludes", minimal + "spa: {fallback: index.html, exclude_prefixes: [/assets]}", "spa.exclude_prefixes[0]"},
		{"redactionempty", minimal + "observability: {redact_query_params: ['']}", "observability.redact_query_params[0]"}, {"redactionduplicate", minimal + "observability: {redact_query_params: [Token, token]}", "observability.redact_query_params[1]"}, {"loglevel", minimal + "observability: {log_level: trace}", "observability.log_level"}, {"corswildcard", minimal + "cors: {allowed_origins: ['*']}", "cors.allowed_origins[0]"}, {"corspath", minimal + "cors: {allowed_origins: ['https://example.com/path']}", "cors.allowed_origins[0]"}, {"corsdup", minimal + "cors: {allowed_origins: ['https://Example.com', 'https://example.com:443']}", "cors.allowed_origins[1]"}, {"cachecontrol", minimal + "cache_control: {default: 'max-age='}", "cache_control.default"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.input), noEnv)
			if err == nil {
				t.Fatal("invalid config accepted")
			}
			var ce *Error
			if !errors.As(err, &ce) || ce.Path != tt.path {
				t.Fatalf("want %s, got %v", tt.path, err)
			}
		})
	}
}

func TestFixedEndpoint(t *testing.T) {
	for _, endpoint := range []string{"https://s3.example.com", "https://s3.example.com/", "https://[::1]:9000", "http://127.0.0.1:9000"} {
		parseOK(t, "s3:\n  bucket: example-websites\n  endpoint: "+strconvQuote(endpoint)+"\n  allow_http: true\n")
	}
	for _, endpoint := range []string{"http://s3.example.com", "//s3.example.com", "https://user:secret@s3.example.com", "https://s3.example.com/path", "https://s3.example.com/?token=secret", "https://s3.example.com/#secret", "ftp://s3.example.com", "https://s3.example.com:0", "https://s3.example.com:", "https://s3.example.com/%2f"} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := Parse([]byte("s3:\n  bucket: example-websites\n  endpoint: "+strconvQuote(endpoint)+"\n"), noEnv)
			if err == nil {
				t.Fatal("invalid origin accepted")
			}
			if strings.Contains(err.Error(), endpoint) {
				t.Fatal("error leaked endpoint")
			}
		})
	}
	_, err := Parse([]byte("s3: {bucket: x, endpoint: 'http://127.0.0.1:9000', allow_http: true, insecure_skip_verify: true}"), noEnv)
	if err == nil {
		t.Fatal("HTTP skip verify accepted")
	}
}

func TestHeadersCannotReplaceOwnedFields(t *testing.T) {
	for _, name := range []string{"Connection", "Transfer-Encoding", "Set-Cookie", "Location", "X-Request-ID", "Content-Length", "Content-Type", "ETag", "Last-Modified", "Vary", "Content-Encoding", "Content-Range", "Cache-Control", "Access-Control-Allow-Origin", "X-Content-Type-Options"} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(minimal+"headers:\n  set:\n    "+name+": unsafe\n"), noEnv)
			if err == nil {
				t.Fatal("owned header accepted")
			}
		})
	}
	for _, s := range []string{"headers: {set: {X-Custom: a, x-custom: b}}", "headers: {set: {X-Custom: \"safe\\r\\nInjected: bad\"}}"} {
		if _, err := Parse([]byte(minimal+s), noEnv); err == nil {
			t.Fatal("ambiguous or injected header accepted")
		}
	}
	c := parseOK(t, minimal+"headers: {set: {referrer-policy: no-referrer}}\n")
	if c.Headers.Set["Referrer-Policy"] != "no-referrer" {
		t.Fatal("header not normalized")
	}
}

func TestRulePrecedenceAndDisable(t *testing.T) {
	c := parseOK(t, minimal+`cache:
  rules:
    - match: {prefix: /assets/}
      ttl: 1h
    - match: {exact: /assets/app.js}
      ttl: 2h
    - match: {exact: /health.txt}
      enabled: false
cache_control:
  rules:
    - match: {exact: /assets/app.js}
      value: public, max-age=60
    - match: {prefix: /assets/}
      value: no-cache
`)
	if c.Cache.PolicyFor("/assets/app.js").TTL != time.Hour || c.Cache.PolicyFor("/assets-old/app.js").TTL != 5*time.Minute || c.Cache.PolicyFor("/health.txt").Enabled {
		t.Fatal("wrong memory policy")
	}
	if c.CacheControl.ValueFor("/assets/app.js") != "public, max-age=60" || c.CacheControl.ValueFor("/assets/other.js") != "no-cache" || c.CacheControl.ValueFor("/other") != "auto" {
		t.Fatal("wrong HTTP policy")
	}
	c.Cache.Enabled = false
	b := true
	c.Cache.Rules[0].Enabled = &b
	if c.Cache.PolicyFor("/assets/app.js").Enabled {
		t.Fatal("rule re-enabled globally disabled cache")
	}
}

func TestCacheModeValues(t *testing.T) {
	for _, mode := range []string{"ceiling", "floor", "fallback", "fixed"} {
		c := parseOK(t, minimal+"cache:\n  ttl_mode: "+mode+"\n  rules:\n    - match: {prefix: /a/}\n      ttl_mode: "+mode+"\n")
		if c.Cache.TTLMode != mode || c.Cache.PolicyFor("/a/b").TTLMode != mode {
			t.Errorf("ttl_mode %s loads as %q and %q", mode, c.Cache.TTLMode, c.Cache.PolicyFor("/a/b").TTLMode)
		}
	}
	for _, directives := range []string{"honor", "revalidate_only", "ignore"} {
		c := parseOK(t, minimal+"cache:\n  request_directives: "+directives+"\n  rules:\n    - match: {prefix: /a/}\n      request_directives: "+directives+"\n")
		if c.Cache.RequestDirectives != directives || c.Cache.PolicyFor("/a/b").RequestDirectives != directives {
			t.Errorf("request_directives %s loads as %q and %q", directives, c.Cache.RequestDirectives, c.Cache.PolicyFor("/a/b").RequestDirectives)
		}
	}
}

func TestRuleModesOverrideGlobalModes(t *testing.T) {
	c := parseOK(t, minimal+`cache:
  ttl_mode: fixed
  request_directives: revalidate_only
  rules:
    - match: {prefix: /draft/}
      ttl: 1m
      request_directives: ignore
    - match: {exact: /index.html}
      ttl_mode: ceiling
      request_directives: honor
    - match: {prefix: /draft/}
      ttl_mode: ceiling
`)
	for _, tc := range []struct{ path, mode, directives string }{
		{"/other.js", "fixed", "revalidate_only"},
		{"/draft/app.js", "fixed", "ignore"}, // the first matching rule wins, and an omitted mode keeps the global one
		{"/index.html", "ceiling", "honor"},
	} {
		if p := c.Cache.PolicyFor(tc.path); p.TTLMode != tc.mode || p.RequestDirectives != tc.directives {
			t.Errorf("%s: ttl_mode %q and request_directives %q, want %q and %q", tc.path, p.TTLMode, p.RequestDirectives, tc.mode, tc.directives)
		}
	}
	if p := c.Cache.PolicyFor("/draft/app.js"); p.TTL != time.Minute {
		t.Errorf("rule ttl %v, want 1m", p.TTL)
	}
}

func TestEnvironmentExpansionAndRedaction(t *testing.T) {
	env := map[string]string{"BUCKET": "private-bucket-marker", "PREFIX": "/secret-prefix-marker/", "HOST": "PRIVATE-HOST-MARKER.EXAMPLE.", "INDEX": "secret-index-marker.html", "POLICY": "secret-policy-marker", "CERT": "secret-cert-marker.pem", "KEY": "secret-key-marker.pem", "MODE": "precompressed", "DURATION": "17s"}
	input := `hosts: ['${HOST}']
s3:
  bucket: '${BUCKET}'
  prefix: '${PREFIX}'
index: '${INDEX}'
server:
  tls: {cert_file: '${CERT}', key_file: '${KEY}'}
admin:
  origin_health: {enabled: true}
headers:
  set:
    referrer-policy: '${POLICY}'
    X-Literal: '$${BUCKET} $$HOME $HOME'
compression: {mode: '${MODE}'}
cache: {ttl: '${DURATION}'}
`
	c, err := Parse([]byte(input), func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if c.S3.Prefix != "secret-prefix-marker/" || c.Hosts[0] != "private-host-marker.example" || c.Admin.OriginHealth.Key != env["INDEX"] || c.Cache.TTL != 17*time.Second {
		t.Fatal("expansion or normalization failed")
	}
	if c.Headers.Set["X-Literal"] != "${BUCKET} $HOME $HOME" {
		t.Fatal("literal dollars expanded")
	}
	out, err := c.MarshalRedacted()
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range env {
		if strings.Contains(string(out), value) {
			t.Fatalf("secret leaked: %s", out)
		}
	}
	if strings.Contains(string(out), "private-host-marker") || strings.Contains(string(out), "secret-prefix-marker") {
		t.Fatal("normalized secret leaked")
	}
	var printed map[string]any
	if err := yaml.Unmarshal(out, &printed); err != nil {
		t.Fatal(err)
	}
	if printed["index"] != "[REDACTED]" || printed["admin"].(map[string]any)["origin_health"].(map[string]any)["key"] != "[REDACTED]" || printed["compression"].(map[string]any)["encodings"] != "[REDACTED]" {
		t.Fatalf("derived values were not redacted: %s", out)
	}
	// An environment result is data, never a second substitution program.
	c, err = Parse([]byte(minimal+"headers: {set: {X-Test: '${FIRST}'}}"), func(k string) (string, bool) {
		if k == "FIRST" {
			return "${SECOND}", true
		}
		t.Fatalf("recursive lookup: %s", k)
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Headers.Set["X-Test"] != "${SECOND}" {
		t.Fatal("recursive expansion")
	}
}

func TestEnvironmentAndValidationErrorSecrecy(t *testing.T) {
	for _, input := range []string{minimal + "index: '${MISSING}'", minimal + "index: '${SECRET}'", minimal + "cache: {ttl: '${SECRET}'}", minimal + "headers: {set: {X-Test: '${SECRET}'}}", minimal + "redirects: [{from: '/a', to: '${SECRET}', status: 308}]"} {
		_, err := Parse([]byte(input), func(k string) (string, bool) {
			if k == "MISSING" {
				return "", false
			}
			return "sensitive-value-marker\r\n", true
		})
		if err == nil {
			t.Fatal("invalid expanded value accepted")
		}
		if strings.Contains(err.Error(), "sensitive-value-marker") || strings.Contains(err.Error(), "${SECRET}") {
			t.Fatalf("secret error: %s", err)
		}
	}
	_, err := Parse([]byte(minimal+"index: '${MISSING}'"), noEnv)
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatal("missing variable diagnostic")
	}
	for _, expr := range []string{"${NAME:-secret}", "${NAME", "${}"} {
		_, err := Parse([]byte(minimal+"index: "+strconvQuote(expr)), noEnv)
		if err == nil {
			t.Fatal("malformed expression accepted")
		}
	}
	calls := 0
	_, err = Parse([]byte(minimal+"headers: {set: {'${KEY}': literal}}"), func(string) (string, bool) { calls++; return "X-Test", true })
	if calls != 0 || err == nil {
		t.Fatal("mapping key was substituted")
	}
}

func TestHostNormalization(t *testing.T) {
	c := parseOK(t, minimal+"hosts: [Example.COM., 'bücher.example', 127.0.0.1, '2001:0db8::1']\n")
	want := []string{"example.com", "xn--bcher-kva.example", "127.0.0.1", "2001:db8::1"}
	for i, h := range want {
		if c.Hosts[i] != h {
			t.Fatalf("host[%d]=%q", i, c.Hosts[i])
		}
	}
}

func TestOfflineLoadAndTLSResolution(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.yaml")
	text := minimal + "server: {tls: {cert_file: cert.pem, key_file: key.pem}}\n"
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file, noEnv)
	if err != nil {
		t.Fatal("load read absent TLS files", err)
	}
	if c.Server.TLS.CertFile != filepath.Join(dir, "cert.pem") || c.Server.TLS.KeyFile != filepath.Join(dir, "key.pem") {
		t.Fatal("TLS paths not relative to config file")
	}
	if _, err := c.MarshalRedacted(); err != nil {
		t.Fatal(err)
	}
	err = c.ValidateTLS()
	if err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("unsafe TLS error: %v", err)
	}
	writeTLSFixture(t, dir)
	if err := c.ValidateTLS(); err != nil {
		t.Fatal(err)
	}
	otherDir := t.TempDir()
	writeTLSFixture(t, otherDir)
	otherKey, err := os.ReadFile(filepath.Join(otherDir, "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), otherKey, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.ValidateTLS(); err == nil {
		t.Fatal("mismatched TLS key accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("PRIVATE-KEY-CONTENT-MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	err = c.ValidateTLS()
	if err == nil || strings.Contains(err.Error(), "PRIVATE-KEY-CONTENT-MARKER") {
		t.Fatal("unsafe invalid key diagnostic")
	}
	c = parseOK(t, text)
	if c.Server.TLS.CertFile != "cert.pem" {
		t.Fatal("Parse resolved relative path")
	}
}

func writeTLSFixture(t *testing.T, dir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeyByteLimit(t *testing.T) {
	c := parseOK(t, "s3: {bucket: x, prefix: "+strconvQuote(strings.Repeat("a", 1013))+"}\n")
	if len(c.S3.Prefix+c.Index) != 1024 {
		t.Fatal("boundary fixture")
	}
	_, err := Parse([]byte("s3: {bucket: x, prefix: "+strconvQuote(strings.Repeat("a", 1014))+"}\n"), noEnv)
	if err == nil {
		t.Fatal("oversized key accepted")
	}
	_, err = Parse([]byte("s3: {bucket: x, prefix: "+strconvQuote(strings.Repeat("é", 510))+"}\n"), noEnv)
	if err == nil {
		t.Fatal("UTF-8 key bound counted runes instead of bytes")
	}
}

func TestDiagnosticDefaultsAreHumanReadable(t *testing.T) {
	c := parseOK(t, minimal)
	out, err := c.MarshalRedacted()
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"request_timeout: 0s", "fill_timeout: 30s", "max_bytes: 128MiB", "ttl: 5m0s"} {
		if !strings.Contains(string(out), part) {
			t.Fatalf("missing %q in %s", part, out)
		}
	}
}

func TestNoOriginCredentialOrDNSIO(t *testing.T) {
	var requests atomic.Int32
	oldTransport := http.DefaultTransport
	http.DefaultTransport = recordingTransport{&requests}
	defer func() { http.DefaultTransport = oldTransport }()
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", "http://127.0.0.1:9")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "absent-aws-config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "absent-aws-credentials"))
	oldResolver := net.DefaultResolver
	var dns atomic.Int32
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		dns.Add(1)
		return nil, errors.New("unexpected DNS lookup")
	}}
	defer func() { net.DefaultResolver = oldResolver }()
	for _, endpoint := range []string{"http://127.0.0.1:9", "https://configuration-must-not-resolve.invalid"} {
		c, err := Parse([]byte("s3: {bucket: example-websites, allow_http: true, endpoint: "+strconvQuote(endpoint)+"}\n"), noEnv)
		if err != nil {
			t.Fatal(err)
		}
		if c.S3.Region != "" {
			t.Fatal("offline parse discovered an ambient region")
		}
		if _, err := c.MarshalRedacted(); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 0 || dns.Load() != 0 {
		t.Fatal("offline configuration performed network IO")
	}
}

type recordingTransport struct{ requests *atomic.Int32 }

func (r recordingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.requests.Add(1)
	return nil, errors.New("unexpected network request")
}

func TestRedirectRequiredFieldsAndLocations(t *testing.T) {
	for _, tc := range []struct{ rule, path string }{
		{"{from: /a, to: /b}", "redirects[0].status"},
		{"{from: /a, to: /b, status: 308, query: ''}", "redirects[0].query"},
		{"{from: /a, to: /a, status: 308}", "redirects[0].to"},
		{"{from: /a, to: 'https://user:secret@example.com/', status: 308}", "redirects[0].to"},
	} {
		_, err := Parse([]byte(minimal+"redirects: ["+tc.rule+"]\n"), noEnv)
		var ce *Error
		if !errors.As(err, &ce) || ce.Path != tc.path || ce.Line == 0 || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe or missing location: %v", err)
		}
	}
	c := parseOK(t, minimal+"redirects: [{from: /a, to: /b, status: 308}]\n")
	if c.Redirects[0].Query != "drop" || c.CompiledRedirects() == nil {
		t.Fatal("omitted redirect policy was not compiled with drop")
	}
}

func TestSignedUnitlessDurationZeroRejected(t *testing.T) {
	for _, zero := range []string{"0", "+0", "-0"} {
		if _, err := Parse([]byte(minimal+"server: {request_timeout: '"+zero+"'}"), noEnv); err == nil {
			t.Fatalf("accepted unitless %s", zero)
		}
	}
}

func TestSyntaxErrorsDoNotRevealValues(t *testing.T) {
	_, err := Parse([]byte(minimal+"headers: {set: {X-Test: sensitive-syntax-value-marker\n"), noEnv)
	if err == nil || strings.Contains(err.Error(), "sensitive-syntax-value-marker") {
		t.Fatalf("unsafe parser diagnostic: %v", err)
	}
}

func TestTLSRejectsMalformedChainCertificate(t *testing.T) {
	dir := t.TempDir()
	writeTLSFixture(t, dir)
	file := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(file, []byte(minimal+"server: {tls: {cert_file: cert.pem, key_file: key.pem}}"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "cert.pem"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("sensitive-invalid-chain-marker")})); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	c, err := Load(file, noEnv)
	if err != nil {
		t.Fatal("Load read TLS material", err)
	}
	err = c.ValidateTLS()
	if err == nil || strings.Contains(err.Error(), "sensitive-invalid-chain-marker") {
		t.Fatalf("malformed chain accepted or leaked: %v", err)
	}
}

func TestCacheControlDeltaSeconds(t *testing.T) {
	for _, name := range []string{"max-age", "s-maxage", "min-fresh", "max-stale", "stale-if-error", "stale-while-revalidate"} {
		for _, value := range []string{"-1", "banana", "1.5", "\"\"", "+1"} {
			_, err := Parse([]byte(minimal+"cache_control: {default: "+strconvQuote(name+"="+value)+"}"), noEnv)
			if err == nil {
				t.Fatalf("invalid delta accepted: %s=%s", name, value)
			}
		}
		for _, value := range []string{"0", "60", "\"60\""} {
			parseOK(t, minimal+"cache_control: {default: "+strconvQuote(name+"="+value)+"}")
		}
	}
	for _, policy := range []string{"max-stale", "public, max-age=60, extension=arbitrary-token", "private=\"Set-Cookie, X-Private\", max-age=0", "extension=\"value, with \\\"quoted\\\" words\""} {
		parseOK(t, minimal+"cache_control: {default: "+strconvQuote(policy)+"}")
	}
	if _, err := Parse([]byte(minimal+"cache_control: {default: 'max-age=60, Max-Age=120'}"), noEnv); err == nil {
		t.Fatal("duplicate directive accepted")
	}
}

func TestCanonicalRulesUseDecodedPathData(t *testing.T) {
	for _, path := range []string{"/100%.txt", "/search?term", "/anchor#fragment", "/café file.txt"} {
		c := parseOK(t, minimal+"cache: {rules: [{match: {exact: "+strconvQuote(path)+"}, ttl: 1h}]}\n")
		if c.Cache.PolicyFor(path).TTL != time.Hour {
			t.Fatal("decoded object path failed to match")
		}
	}
	if _, err := Parse([]byte(minimal+"server: {request_id_header: Proxy-Connection}"), noEnv); err == nil {
		t.Fatal("hop-by-hop request ID header accepted")
	}
	for _, origin := range []string{"https://[example.com]", "https://[127.0.0.1]", "https://example.com#"} {
		if _, err := Parse([]byte("s3: {bucket: x, endpoint: "+strconvQuote(origin)+"}"), noEnv); err == nil {
			t.Fatal("malformed authority or empty fragment accepted")
		}
	}
}

func TestValidationMatchesStartupRules(t *testing.T) {
	for _, tt := range []struct{ name, input, path string }{
		{"regiondot", "s3: {bucket: x, region: eu.west}", "s3.region"},
		{"regioncolon", "s3: {bucket: x, region: 'eu:west'}", "s3.region"},
		{"regionunicode", "s3: {bucket: x, region: 'eü-west-1'}", "s3.region"},
		{"directorybucket", "s3: {bucket: example--usw2-az1--x-s3}", "s3.bucket"},
		{"mrap", "s3: {bucket: example.mrap}", "s3.bucket"},
		{"objectlambda", "s3: {bucket: example--ol-s3}", "s3.bucket"},
		{"accesspointalias", "s3: {bucket: example-ab12cd34ef56-s3alias}", "s3.bucket"},
		{"tablebucket", "s3: {bucket: example--table-s3}", "s3.bucket"},
		{"localhostvsloopback", minimal + "server: {listen: 'localhost:8080'}\nadmin: {listen: '127.0.0.1:8080'}", "admin.listen"},
		{"loopbackrange", minimal + "server: {listen: '127.0.0.1:8080'}\nadmin: {listen: '127.0.0.2:8080'}", "admin.listen"},
		{"loopback6", minimal + "server: {listen: '[::1]:8080'}\nadmin: {listen: 'localhost:8080'}", "admin.listen"},
		{"wildcardpublic", minimal + "server: {listen: ':8080'}\nadmin: {listen: '192.0.2.1:8080'}", "admin.listen"},
		{"wildcardadmin", minimal + "server: {listen: '192.0.2.1:8080'}\nadmin: {listen: '0.0.0.0:8080'}", "admin.listen"},
		{"unspecified6", minimal + "server: {listen: '[::]:8080'}\nadmin: {listen: '127.0.0.1:8080'}", "admin.listen"},
		{"samehostcase", minimal + "server: {listen: 'Host.Example.com:8080'}\nadmin: {listen: 'host.example.com:8080'}", "admin.listen"},
		{"mappedipv4", minimal + "server: {listen: '[::ffff:192.0.2.1]:8080'}\nadmin: {listen: '192.0.2.1:8080'}", "admin.listen"},
		{"sameipspelling", minimal + "server: {listen: '[2001:db8::1]:8080'}\nadmin: {listen: '[2001:0db8::1]:8080'}", "admin.listen"},
		{"cacheexactslash", minimal + "cache: {rules: [{match: {exact: /docs/}, ttl: 1m}]}", "cache.rules[0].match.exact"},
		{"cacheexactroot", minimal + "cache: {rules: [{match: {exact: /}, ttl: 1m}]}", "cache.rules[0].match.exact"},
		{"hiddenredirect", minimal + "redirects: [{from: /.git/config, to: /, status: 308}]", "redirects[0].from"},
		{"hiddenredirectlater", minimal + "redirects: [{from: '/docs/.env', to: /, status: 308}]", "redirects[0].from"},
		{"hiddenwellknownlater", minimal + "redirects: [{from: '/docs/.well-known/x', to: /, status: 308}]", "redirects[0].from"},
		{"numerichost", minimal + "hosts: ['192.0.2.010']", "hosts[0]"},
		{"numericshorthost", minimal + "hosts: ['1.2.3']", "hosts[0]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.input), noEnv)
			var ce *Error
			if !errors.As(err, &ce) || ce.Path != tt.path {
				t.Fatalf("want %s, got %v", tt.path, err)
			}
		})
	}
	for _, input := range []string{
		"s3: {bucket: x, region: us-east-1}",
		"s3: {bucket: x, region: Custom_Region-2}",
		"s3: {bucket: example-s3alias-site}",
		minimal + "server: {listen: '127.0.0.1:8080'}\nadmin: {listen: '127.0.0.1:9090'}",
		minimal + "server: {listen: ':8080'}\nadmin: {listen: '127.0.0.1:9090'}",
		minimal + "server: {listen: '192.0.2.1:8080'}\nadmin: {listen: '127.0.0.1:8080'}",
		minimal + "server: {listen: '192.0.2.1:8080'}\nadmin: {listen: '192.0.2.2:8080'}",
		minimal + "cache: {rules: [{match: {exact: /docs/index.html}, ttl: 1m}, {match: {prefix: /}, ttl: 2m}]}",
		minimal + "cache_control: {rules: [{match: {exact: /docs/}, value: no-cache}, {match: {exact: /}, value: no-cache}]}",
		minimal + "redirects: [{from: /.well-known/old, to: /.well-known/new, status: 308}, {from: '/a.b/c', to: /, status: 308}]",
		minimal + "hosts: ['192.0.2.10', '192.0.2.11.', '1example.com', '123.example']",
	} {
		parseOK(t, input)
	}
}

func TestCanonicalPorts(t *testing.T) {
	for _, tt := range []struct{ name, input, path string }{
		{"listensign", minimal + "server: {listen: '127.0.0.1:+8080'}", "server.listen"},
		{"listenzero", minimal + "server: {listen: '127.0.0.1:08080'}", "server.listen"},
		{"listenzeros", minimal + "server: {listen: ':0008080'}", "server.listen"},
		{"adminsign", minimal + "admin: {listen: '127.0.0.1:+9090'}", "admin.listen"},
		{"adminzero", minimal + "admin: {listen: '127.0.0.1:09090'}", "admin.listen"},
		{"adminzeros", minimal + "admin: {listen: '127.0.0.1:0009090'}", "admin.listen"},
		{"corssign", minimal + "cors: {allowed_origins: ['https://example.com:+443']}", "cors.allowed_origins[0]"},
		{"corszero", minimal + "cors: {allowed_origins: ['https://example.com:0443']}", "cors.allowed_origins[0]"},
		{"corszeros", minimal + "cors: {allowed_origins: ['https://example.com:000443']}", "cors.allowed_origins[0]"},
		{"corsonlyzeros", minimal + "cors: {allowed_origins: ['https://example.com:00000']}", "cors.allowed_origins[0]"},
		{"endpointsign", "s3: {bucket: example-websites, endpoint: 'https://s3.example.com:+9000'}", "s3.endpoint"},
		{"endpointzero", "s3: {bucket: example-websites, endpoint: 'https://s3.example.com:09000'}", "s3.endpoint"},
		{"endpointzeros", "s3: {bucket: example-websites, endpoint: 'https://s3.example.com:000009000'}", "s3.endpoint"},
		{"endpointlarge", "s3: {bucket: example-websites, endpoint: 'https://s3.example.com:65536'}", "s3.endpoint"},
		{"corsdefaultdup", minimal + "cors: {allowed_origins: ['https://example.com', 'https://example.com:443']}", "cors.allowed_origins[1]"},
		{"corshttpdup", minimal + "cors: {allowed_origins: ['http://example.com:80', 'http://example.com']}", "cors.allowed_origins[1]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.input), noEnv)
			var ce *Error
			if !errors.As(err, &ce) || ce.Path != tt.path {
				t.Fatalf("want %s, got %v", tt.path, err)
			}
			for _, leak := range []string{"+", "0443", "08080", "09090", "09000"} {
				if strings.Contains(ce.Problem, leak) {
					t.Fatal("error repeated the configured port")
				}
			}
		})
	}
	for _, input := range []string{
		minimal + "server: {listen: '127.0.0.1:1'}\nadmin: {listen: '127.0.0.1:65535'}",
		minimal + "server: {listen: ':8080'}\nadmin: {listen: '[::1]:10000'}",
		minimal + "cors: {allowed_origins: ['https://example.com:443', 'http://example.com:80', 'https://example.com:8443', 'http://[2001:db8::1]:8080']}",
		"s3: {bucket: example-websites, endpoint: 'https://s3.example.com:443'}",
		"s3: {bucket: example-websites, endpoint: 'https://[2001:db8::1]:9000/'}",
	} {
		parseOK(t, input)
	}
}

func TestHeaderSetDiagnostics(t *testing.T) {
	for _, tt := range []struct{ name, input, path string }{
		{"reserved", "headers: {set: {X-Custom: a, Set-Cookie: b}}", "headers.set.Set-Cookie"},
		{"duplicate", "headers:\n  set:\n    X-Custom: a\n    x-custom: b\n", "headers.set.x-custom"},
		{"empty", "headers: {set: {X-Empty: ''}}", "headers.set.X-Empty"},
		{"newline", "headers: {set: {X-Bad: \"a\\nb\"}}", "headers.set.X-Bad"},
		{"delete", "headers: {set: {X-Bad: \"a\\x7Fb\"}}", "headers.set.X-Bad"},
		{"c1", "headers: {set: {X-Bad: \"a\\u0085b\"}}", "headers.set.X-Bad"},
		{"c1csi", "headers: {set: {X-Bad: \"a\\u009Bb\"}}", "headers.set.X-Bad"},
		{"invalidname", "headers: {set: {'bad name': a}}", "headers.set"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(minimal+tt.input), noEnv)
			var ce *Error
			if !errors.As(err, &ce) || ce.Path != tt.path || ce.Line == 0 {
				t.Fatalf("want %s, got %v", tt.path, err)
			}
		})
	}
	c := parseOK(t, minimal+"headers: {set: {X-Tab: \"a\\tb\", X-Unicode: 'café'}}\n")
	if c.Headers.Set["X-Tab"] != "a\tb" || c.Headers.Set["X-Unicode"] != "café" {
		t.Fatal("valid header values changed or rejected")
	}
}
