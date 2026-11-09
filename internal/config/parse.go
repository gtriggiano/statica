package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const MaxConfigBytes = 1 << 20

type location struct{ line, column int }
type Error struct {
	Path         string
	Line, Column int
	Problem      string
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s (line %d, column %d): %s", e.Path, e.Line, e.Column, e.Problem)
	}
	return e.Path + ": " + e.Problem
}
func (c *Config) err(path, problem string) error {
	l := c.nodes[path]
	if l.line == 0 {
		p := path
		for l.line == 0 && strings.Contains(p, ".") {
			p = p[:strings.LastIndex(p, ".")]
			l = c.nodes[p]
		}
	}
	return &Error{path, l.line, l.column, problem}
}

func Load(path string, lookup LookupEnv) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("configuration file: cannot open")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("configuration file: cannot read")
	}
	c, err := Parse(b, lookup)
	if err != nil {
		return nil, err
	}
	if c.Server.TLS != nil {
		dir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return nil, fmt.Errorf("server.tls: cannot resolve file directory")
		}
		for _, p := range []*string{&c.Server.TLS.CertFile, &c.Server.TLS.KeyFile} {
			if !filepath.IsAbs(*p) {
				*p = filepath.Join(dir, *p)
			}
		}
	}
	return c, nil
}

func Parse(data []byte, lookup LookupEnv) (*Config, error) {
	if len(data) > MaxConfigBytes {
		return nil, fmt.Errorf("configuration: exceeds 1 MiB limit")
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	c := defaults()
	d := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := d.Decode(&doc); err != nil {
		return nil, syntaxError(err)
	}
	var extra yaml.Node
	if err := d.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, syntaxError(err)
		}
		return nil, fmt.Errorf("configuration: exactly one YAML document required")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("configuration: expected a YAML mapping")
	}
	if err := c.decode(doc.Content[0], reflect.ValueOf(c).Elem(), "", lookup); err != nil {
		return nil, err
	}
	c.applyOptionalDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

var lineRE = regexp.MustCompile(`line ([0-9]+)`)

func syntaxError(err error) error {
	if m := lineRE.FindStringSubmatch(err.Error()); len(m) > 1 {
		n, _ := strconv.Atoi(m[1])
		return &Error{Path: "configuration", Line: n, Column: 1, Problem: "invalid YAML syntax"}
	}
	return fmt.Errorf("configuration: invalid YAML syntax")
}

var durationType = reflect.TypeOf(time.Duration(0))
var sizeType = reflect.TypeOf(ByteSize(0))
var integerRE = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
var sizeRE = regexp.MustCompile(`^([0-9]+)(KiB|MiB|GiB)$`)
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func fieldName(f reflect.StructField) string { return strings.Split(f.Tag.Get("yaml"), ",")[0] }
func childPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func (c *Config) decode(n *yaml.Node, v reflect.Value, path string, lookup LookupEnv) error {
	c.nodes[path] = location{n.Line, n.Column}
	if n.Kind == yaml.AliasNode {
		return c.err(path, "YAML aliases are not allowed")
	}
	switch n.Tag {
	case "!!map", "!!seq", "!!str", "!!int", "!!bool", "!!null", "!!float", "!!timestamp":
	default:
		return c.err(path, "custom YAML tags are not allowed")
	}
	if n.Tag == "!!null" {
		return c.err(path, "null is not allowed; omit the field to use its default")
	}
	if v.Kind() == reflect.Pointer {
		v.Set(reflect.New(v.Type().Elem()))
		return c.decode(n, v.Elem(), path, lookup)
	}
	if v.Type() == durationType || v.Type() == sizeType {
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			return c.err(path, "expected a string with explicit units")
		}
		s, err := c.expand(n.Value, path, lookup)
		if err != nil {
			return err
		}
		if v.Type() == durationType {
			d, err := time.ParseDuration(s)
			if err != nil || s == "0" || s == "+0" || s == "-0" {
				return c.err(path, "invalid duration; explicit units required")
			}
			v.SetInt(int64(d))
			return nil
		}
		m := sizeRE.FindStringSubmatch(s)
		if m == nil {
			return c.err(path, "expected an integral size with KiB, MiB or GiB units")
		}
		x, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return c.err(path, "size exceeds supported range")
		}
		mul := int64(1 << 10)
		if m[2] == "MiB" {
			mul = 1 << 20
		}
		if m[2] == "GiB" {
			mul = 1 << 30
		}
		if x > math.MaxInt64/mul {
			return c.err(path, "size exceeds supported range")
		}
		v.SetInt(x * mul)
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		if n.Kind != yaml.MappingNode || n.Tag != "!!map" {
			return c.err(path, "expected a mapping")
		}
		fields := map[string]int{}
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if f.PkgPath == "" {
				fields[fieldName(f)] = i
			}
		}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if err := c.key(k, path); err != nil {
				return err
			}
			p := childPath(path, k.Value)
			c.nodes[p] = location{k.Line, k.Column}
			if seen[k.Value] {
				return c.err(p, "duplicate field")
			}
			seen[k.Value] = true
			fi, ok := fields[k.Value]
			if !ok {
				if !envNameRE.MatchString(k.Value) {
					p = childPath(path, "<unknown>")
					c.nodes[p] = location{k.Line, k.Column}
				}
				return c.err(p, "unknown field")
			}
			if err := c.decode(val, v.Field(fi), p, lookup); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode || n.Tag != "!!seq" {
			return c.err(path, "expected a list")
		}
		v.Set(reflect.MakeSlice(v.Type(), len(n.Content), len(n.Content)))
		for i, item := range n.Content {
			if err := c.decode(item, v.Index(i), fmt.Sprintf("%s[%d]", path, i), lookup); err != nil {
				return err
			}
		}
	case reflect.Map:
		if n.Kind != yaml.MappingNode || n.Tag != "!!map" {
			return c.err(path, "expected a mapping")
		}
		v.Set(reflect.MakeMap(v.Type()))
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if err := c.key(k, path); err != nil {
				return err
			}
			if seen[k.Value] {
				return c.err(path, "duplicate key")
			}
			seen[k.Value] = true
			x := reflect.New(v.Type().Elem()).Elem()
			if err := c.decode(val, x, childPath(path, k.Value), lookup); err != nil {
				return err
			}
			v.SetMapIndex(reflect.ValueOf(k.Value).Convert(v.Type().Key()), x)
		}
	case reflect.String:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			return c.err(path, "expected a string")
		}
		s, err := c.expand(n.Value, path, lookup)
		if err != nil {
			return err
		}
		v.SetString(s)
	case reflect.Bool:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" || (n.Value != "true" && n.Value != "false") {
			return c.err(path, "expected YAML true or false")
		}
		v.SetBool(n.Value == "true")
	case reflect.Int, reflect.Int64:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!int" || !integerRE.MatchString(n.Value) {
			return c.err(path, "expected a decimal YAML integer")
		}
		x, err := strconv.ParseInt(n.Value, 10, v.Type().Bits())
		if err != nil {
			return c.err(path, "integer exceeds supported range")
		}
		v.SetInt(x)
	default:
		return c.err(path, "unsupported configuration type")
	}
	return nil
}

func (c *Config) key(k *yaml.Node, path string) error {
	if k.Kind != yaml.ScalarNode || k.Tag != "!!str" || k.Value == "<<" {
		return c.err(path, "mapping keys must be ordinary strings; merges are not allowed")
	}
	return nil
}

func (c *Config) expand(s, path string, lookup LookupEnv) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			out.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			out.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			out.WriteByte('$')
			i++
			continue
		}
		end := strings.IndexByte(s[i+2:], '}')
		if end < 0 {
			return "", c.err(path, "invalid environment substitution")
		}
		end += i + 2
		name := s[i+2 : end]
		if !envNameRE.MatchString(name) {
			return "", c.err(path, "invalid environment variable name")
		}
		value, ok := lookup(name)
		if !ok {
			return "", c.err(path, "missing environment variable "+name)
		}
		c.tainted[path] = true
		out.WriteString(value)
		i = end + 1
	}
	return out.String(), nil
}

func (c *Config) present(path string) bool { _, ok := c.nodes[path]; return ok }
func (c *Config) applyOptionalDefaults() {
	if t := c.Server.TLS; t != nil && !c.present("server.tls.min_version") {
		t.MinVersion = "1.2"
	}
	if h := c.Admin.OriginHealth; h != nil {
		if !c.present("admin.origin_health.key") {
			h.Key = c.Index
			if c.tainted["index"] {
				c.tainted["admin.origin_health.key"] = true
			}
		}
		if !c.present("admin.origin_health.interval") {
			h.Interval = 30 * time.Second
		}
		if !c.present("admin.origin_health.timeout") {
			h.Timeout = 3 * time.Second
		}
		if !c.present("admin.origin_health.failure_threshold") {
			h.FailureThreshold = 3
		}
		if !c.present("admin.origin_health.success_threshold") {
			h.SuccessThreshold = 1
		}
		if !c.present("admin.origin_health.max_staleness") {
			h.MaxStaleness = 90 * time.Second
		}
	}
	if c.Compression.Mode == "precompressed" && !c.present("compression.encodings") {
		c.Compression.Encodings = []string{"br", "gzip"}
		if c.tainted["compression.mode"] {
			c.tainted["compression.encodings"] = true
		}
	}
	for i := range c.Redirects {
		if !c.present(fmt.Sprintf("redirects[%d].query", i)) {
			c.Redirects[i].Query = "drop"
		}
	}
}

// ValidateTLS is the only loading operation which reads certificate material.
func (c *Config) ValidateTLS() error {
	if c.Server.TLS == nil {
		return nil
	}
	pair, err := tls.LoadX509KeyPair(c.Server.TLS.CertFile, c.Server.TLS.KeyFile)
	if err != nil {
		return c.err("server.tls", "cannot load a matching certificate and private key")
	}
	for _, der := range pair.Certificate {
		if _, err := x509.ParseCertificate(der); err != nil {
			return c.err("server.tls", "certificate chain contains invalid certificate material")
		}
	}
	return nil
}

// MarshalRedacted returns diagnostic YAML; it is not a round-trip format.
func (c *Config) MarshalRedacted() ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(c); err != nil {
		return nil, fmt.Errorf("configuration: cannot encode diagnostic settings")
	}
	var walk func(*yaml.Node, string)
	walk = func(n *yaml.Node, path string) {
		if c.tainted[path] {
			n.Kind = yaml.ScalarNode
			n.Tag = "!!str"
			n.Value = "[REDACTED]"
			n.Content = nil
			return
		}
		if n.Kind == yaml.MappingNode {
			for i := 0; i < len(n.Content); i += 2 {
				walk(n.Content[i+1], childPath(path, n.Content[i].Value))
			}
		}
		if n.Kind == yaml.SequenceNode {
			for i, x := range n.Content {
				walk(x, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(&n, "")
	return yaml.Marshal(&n)
}

func (b ByteSize) MarshalYAML() (any, error) {
	for _, unit := range []struct {
		n int64
		s string
	}{{1 << 30, "GiB"}, {1 << 20, "MiB"}, {1 << 10, "KiB"}} {
		if int64(b)%unit.n == 0 {
			return fmt.Sprintf("%d%s", int64(b)/unit.n, unit.s), nil
		}
	}
	return fmt.Sprintf("%d bytes", b), nil
}
