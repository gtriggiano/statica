// Package redirect compiles ordered, immutable redirect templates. Neither
// configuration nor request errors include URL or captured values.
package redirect

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxTargetBytes  = 8 * 1024
	MaxCaptureBytes = 256
)

type Rule struct {
	From     string             `yaml:"from"`
	To       string             `yaml:"to"`
	Status   int                `yaml:"status"`
	Query    string             `yaml:"query"`
	Captures map[string]Capture `yaml:"captures"`
}

type Capture struct {
	Pattern string `yaml:"pattern"`
}
type Result struct {
	Location string
	Status   int
}

// RequestError identifies invalid client input, including an expansion that
// cannot safely be placed in a destination. Status is 400 or 414.
type RequestError struct {
	Status int
	Reason string
}

func (e *RequestError) Error() string { return "redirect request: " + e.Reason }

// Warning refers only to rule indices, never template or expanded values.
type Warning struct {
	Rule       int
	ShadowedBy int
}

type Set struct {
	rules    []compiledRule
	warnings []Warning
}

func (s *Set) Warnings() []Warning { return append([]Warning(nil), s.warnings...) }

type component struct{ literal, name string }
type queryPart struct {
	key   string
	value component
}
type template struct {
	path        []component
	query       []queryPart
	hasQuery    bool
	origin      string
	fragment    string
	hasFragment bool
}
type compiledRule struct {
	from, to    template
	status      int
	policy      string
	patterns    map[string]*regexp.Regexp
	patternText map[string]string
	bound       map[string]bool
}
type queryPair struct{ key, value, raw string }

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func configError(i int, field, reason string) error {
	return fmt.Errorf("redirects[%d].%s: %s", i, field, reason)
}

// Compile validates rules and takes its own copy of all mutable input data.
func Compile(rules []Rule) (*Set, error) {
	s := &Set{}
	seen := make(map[string]bool)
	for i, rule := range rules {
		if rule.Status != 301 && rule.Status != 302 && rule.Status != 307 && rule.Status != 308 {
			return nil, configError(i, "status", "must be 301, 302, 307 or 308")
		}
		policy := rule.Query
		if policy == "" {
			policy = "drop"
		}
		if policy != "drop" && policy != "preserve" && policy != "unmatched" {
			return nil, configError(i, "query", "unsupported forwarding policy")
		}
		from, err := parseTemplate(rule.From, false)
		if err != nil {
			return nil, configError(i, "from", err.Error())
		}
		if hiddenSource(from) {
			return nil, configError(i, "from", "hidden path segment never reaches redirects")
		}
		to, err := parseTemplate(rule.To, true)
		if err != nil {
			return nil, configError(i, "to", err.Error())
		}
		if policy != "drop" && to.hasQuery {
			return nil, configError(i, "query", "forwarding requires a destination without a query")
		}
		cr := compiledRule{from: from, to: to, status: rule.Status, policy: policy,
			patterns: make(map[string]*regexp.Regexp), patternText: make(map[string]string), bound: make(map[string]bool)}
		for _, c := range components(from) {
			if c.name == "" {
				continue
			}
			if cr.bound[c.name] {
				return nil, configError(i, "from", "duplicate capture binding")
			}
			cr.bound[c.name] = true
		}
		for _, c := range components(to) {
			if c.name != "" && !cr.bound[c.name] {
				return nil, configError(i, "to", "unbound capture reference")
			}
		}
		// Sort map keys so even errors are deterministic.
		names := make([]string, 0, len(rule.Captures))
		for name := range rule.Captures {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !cr.bound[name] {
				return nil, configError(i, "captures", "constraint has no source binding")
			}
			pattern := rule.Captures[name].Pattern
			if pattern == "" {
				return nil, configError(i, "captures.pattern", "pattern must not be empty")
			}
			re, err := regexp.Compile(`\A(?:` + pattern + `)\z`)
			if err != nil {
				return nil, configError(i, "captures.pattern", "invalid full-string RE2 pattern")
			}
			cr.patterns[name], cr.patternText[name] = re, pattern
		}
		// Even without captures, percent encoding may enlarge a configured URL.
		// Reject fixed template overhead that cannot fit the target budget; the
		// actual expanded size is checked again for every request.
		minimum := make(map[string]string, len(cr.bound))
		for name := range cr.bound {
			minimum[name] = "x"
		}
		if _, err := (compiledRule{to: from, policy: "drop"}).build(minimum, nil, "", false); err != nil {
			return nil, configError(i, "from", "encoded source exceeds byte limit")
		}
		if _, err := (compiledRule{to: to, policy: "drop"}).build(minimum, nil, "", false); err != nil {
			return nil, configError(i, "to", "encoded destination exceeds byte limit")
		}
		key := sourceKey(cr)
		if seen[key] {
			return nil, configError(i, "from", "duplicate equivalent source")
		}
		seen[key] = true
		for j, earlier := range s.rules {
			if shadows(earlier, cr) {
				s.warnings = append(s.warnings, Warning{Rule: i, ShadowedBy: j})
				break
			}
		}
		s.rules = append(s.rules, cr)
	}
	if err := s.checkCycles(); err != nil {
		return nil, err
	}
	return s, nil
}

// hiddenSource reports a literal source segment starting with a dot, other
// than a leading .well-known: requests for such paths are answered with 404
// before redirects are evaluated, so the rule could never match.
func hiddenSource(t template) bool {
	for i, c := range t.path {
		if strings.HasPrefix(c.literal, ".") && !(i == 0 && c.literal == ".well-known") {
			return true
		}
	}
	return false
}

func components(t template) []component {
	cs := append([]component(nil), t.path...)
	for _, q := range t.query {
		cs = append(cs, q.value)
	}
	return cs
}

func parseComponent(raw string, query bool) (component, error) {
	if strings.ContainsAny(raw, "{}") {
		if len(raw) < 3 || raw[0] != '{' || raw[len(raw)-1] != '}' || !namePattern.MatchString(raw[1:len(raw)-1]) {
			return component{}, fmt.Errorf("placeholder must occupy a whole component")
		}
		return component{name: raw[1 : len(raw)-1]}, nil
	}
	var value string
	var err error
	if query {
		value, err = url.QueryUnescape(raw)
	} else {
		value, err = url.PathUnescape(raw)
	}
	if err != nil || !validText(value) {
		return component{}, fmt.Errorf("invalid component encoding")
	}
	if !query && !safeSegment(value) {
		return component{}, fmt.Errorf("unsafe path segment")
	}
	return component{literal: value}, nil
}

func parseTemplate(raw string, destination bool) (template, error) {
	var t template
	if raw == "" || len(raw) > MaxTargetBytes || !validText(raw) {
		return t, fmt.Errorf("empty, oversized or invalid template")
	}
	if destination {
		if p := strings.IndexByte(raw, '#'); p >= 0 {
			fragment := raw[p+1:]
			if strings.ContainsAny(fragment, "{}") {
				return t, fmt.Errorf("fragment captures are unsupported")
			}
			decoded, err := url.PathUnescape(fragment)
			if err != nil || !validText(decoded) {
				return t, fmt.Errorf("invalid fragment encoding")
			}
			t.fragment, t.hasFragment, raw = decoded, true, raw[:p]
		}
		if len(raw) >= len("https://") && strings.EqualFold(raw[:len("https://")], "https://") {
			tail := raw[len("https://"):]
			p := strings.IndexAny(tail, "/?")
			authority := tail
			if p >= 0 {
				authority, raw = tail[:p], tail[p:]
			} else {
				raw = "/"
			}
			if strings.ContainsAny(authority, "{}\\%") {
				return t, fmt.Errorf("origin must be literal and safe")
			}
			u, err := url.Parse("https://" + authority)
			if err != nil || u.Host == "" || u.User != nil || !validAuthority(u) {
				return t, fmt.Errorf("invalid HTTPS origin")
			}
			t.origin = "https://" + authority
			if strings.HasPrefix(raw, "?") {
				raw = "/" + raw
			}
		}
	}
	if !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") || strings.Contains(raw, "#") {
		return t, fmt.Errorf("requires an absolute local path or literal HTTPS origin")
	}
	path, query, hasQuery := strings.Cut(raw, "?")
	t.hasQuery = hasQuery
	for _, rawSegment := range strings.Split(path[1:], "/") {
		c, err := parseComponent(rawSegment, false)
		if err != nil {
			return t, err
		}
		t.path = append(t.path, c)
	}
	for i, c := range t.path {
		if c.name == "" && c.literal == "" && i != len(t.path)-1 {
			return t, fmt.Errorf("empty interior path segment")
		}
	}
	if hasQuery && query != "" {
		keys := make(map[string]bool)
		for _, pair := range strings.Split(query, "&") {
			if pair == "" {
				return t, fmt.Errorf("empty query field")
			}
			rawKey, rawValue, _ := strings.Cut(pair, "=")
			if strings.ContainsAny(rawKey, "{}") {
				return t, fmt.Errorf("query names must be literal")
			}
			key, err := url.QueryUnescape(rawKey)
			if err != nil || key == "" || !validText(key) {
				return t, fmt.Errorf("invalid query name")
			}
			if !destination && keys[key] {
				return t, fmt.Errorf("duplicate required query name")
			}
			keys[key] = true
			value, err := parseComponent(rawValue, true)
			if err != nil {
				return t, err
			}
			t.query = append(t.query, queryPart{key: key, value: value})
		}
	}
	return t, nil
}

func validAuthority(u *url.URL) bool {
	host := u.Hostname()
	if host == "" || !validText(host) {
		return false
	}
	if strings.Contains(u.Host, ":") {
		// url.Parse validates bracket/port syntax but accepts an empty port.
		if strings.HasSuffix(u.Host, ":") {
			return false
		}
		// url.Parse leaves only digits in a port. The canonical form has no
		// leading zero, as for every other port in the configuration.
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || len(port) > 5 || port[0] == '0' || n > 65535 {
				return false
			}
		}
	}
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6() && ip.Zone() == ""
	}
	if strings.ContainsAny(host, ":[]") {
		return false
	}
	if len(host) > 253 {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func validText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func hex(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
func safeSegment(value string) bool {
	if !validText(value) || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	for i := 0; i+2 < len(value); i++ {
		if value[i] == '%' && hex(value[i+1]) && hex(value[i+2]) {
			return false
		}
	}
	return true
}

func decodePath(escaped string) ([]string, error) {
	if !strings.HasPrefix(escaped, "/") || strings.HasPrefix(escaped, "//") || strings.ContainsAny(escaped, "?#") {
		return nil, badRequest("invalid path")
	}
	parts := strings.Split(escaped[1:], "/")
	for i, raw := range parts {
		decoded, err := url.PathUnescape(raw)
		if err != nil || !safeSegment(decoded) || decoded == "" && i != len(parts)-1 {
			return nil, badRequest("invalid path encoding or segment")
		}
		parts[i] = decoded
	}
	return parts, nil
}

func parseQuery(raw string) ([]queryPair, error) {
	if !validText(raw) {
		return nil, badRequest("invalid query encoding")
	}
	var pairs []queryPair
	if raw == "" {
		return pairs, nil
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		key, value, _ := strings.Cut(pair, "=")
		key, e1 := url.QueryUnescape(key)
		value, e2 := url.QueryUnescape(value)
		if e1 != nil || e2 != nil || !validText(key) || !validText(value) {
			return nil, badRequest("invalid query encoding")
		}
		pairs = append(pairs, queryPair{key: key, value: value, raw: safeRawQuery(pair)})
	}
	return pairs, nil
}

// safeRawQuery retains legal raw spelling (including + and percent escape case)
// while encoding bytes that could change the structure of a Location header.
func safeRawQuery(raw string) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-._~!$&'()*+,;=:@/?%", rune(c)) {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		}
	}
	return b.String()
}

func badRequest(reason string) error { return &RequestError{Status: 400, Reason: reason} }
func tooLarge() error                { return &RequestError{Status: 414, Reason: "target exceeds byte limit"} }

// Resolve validates the once-escaped incoming path and raw query, then applies
// the first matching rule. A false match result leaves ordinary resolution to
// the caller. Errors must be handled before any origin or cache lookup.
// forceQuery distinguishes a bare trailing '?' from an absent query; preserve
// retains that marker before any destination fragment.
func (s *Set) Resolve(escapedPath, rawQuery string, forceQuery bool) (Result, bool, error) {
	n := len(escapedPath)
	if rawQuery != "" || forceQuery {
		n += 1 + len(rawQuery)
	}
	if n > MaxTargetBytes {
		return Result{}, false, tooLarge()
	}
	path, err := decodePath(escapedPath)
	if err != nil {
		return Result{}, false, err
	}
	pairs, err := parseQuery(rawQuery)
	if err != nil {
		return Result{}, false, err
	}
	if s == nil {
		return Result{}, false, nil
	}
	for _, rule := range s.rules {
		values, match, err := rule.match(path, pairs)
		if err != nil {
			return Result{}, false, err
		}
		if !match {
			continue
		}
		location, err := rule.build(values, pairs, rawQuery, forceQuery)
		if err != nil {
			return Result{}, false, err
		}
		return Result{Location: location, Status: rule.status}, true, nil
	}
	return Result{}, false, nil
}

func (r compiledRule) match(path []string, query []queryPair) (map[string]string, bool, error) {
	if len(path) != len(r.from.path) {
		return nil, false, nil
	}
	values := make(map[string]string)
	for i, c := range r.from.path {
		if !r.matchValue(c, path[i], values) {
			return nil, false, nil
		}
	}
	// Ambiguity is an input error for a path-matching rule, even if another
	// required query field is missing or fails its constraint.
	fields := make(map[string][]string)
	for _, pair := range query {
		fields[pair.key] = append(fields[pair.key], pair.value)
	}
	for _, q := range r.from.query {
		if len(fields[q.key]) > 1 {
			return nil, false, badRequest("ambiguous required query field")
		}
	}
	for _, q := range r.from.query {
		field := fields[q.key]
		if len(field) != 1 || !r.matchValue(q.value, field[0], values) {
			return nil, false, nil
		}
	}
	return values, true, nil
}
func (r compiledRule) matchValue(c component, value string, values map[string]string) bool {
	if c.name == "" {
		return c.literal == value
	}
	if value == "" || len(value) > MaxCaptureBytes {
		return false
	}
	if re := r.patterns[c.name]; re != nil && !re.MatchString(value) {
		return false
	}
	values[c.name] = value
	return true
}

func (r compiledRule) build(values map[string]string, pairs []queryPair, rawQuery string, forceQuery bool) (string, error) {
	var path strings.Builder
	for _, c := range r.to.path {
		value := c.literal
		if c.name != "" {
			value = values[c.name]
			if !safeSegment(value) {
				return "", badRequest("capture cannot inhabit destination path")
			}
		}
		path.WriteByte('/')
		path.WriteString(url.PathEscape(value))
	}
	// URL.Path is decoded; RawPath preserves our component-by-component escaping.
	decoded, _ := url.PathUnescape(path.String())
	u := &url.URL{Path: decoded, RawPath: path.String(), Fragment: r.to.fragment}
	if r.to.origin != "" {
		origin, _ := url.Parse(r.to.origin)
		u.Scheme, u.Host = origin.Scheme, origin.Host
	}
	if r.policy == "drop" {
		var query []string
		for _, q := range r.to.query {
			value := q.value.literal
			if q.value.name != "" {
				value = values[q.value.name]
			}
			query = append(query, url.QueryEscape(q.key)+"="+url.QueryEscape(value))
		}
		u.RawQuery, u.ForceQuery = strings.Join(query, "&"), r.to.hasQuery && len(query) == 0
	} else if r.policy == "preserve" {
		u.RawQuery = safeRawQuery(rawQuery)
		u.ForceQuery = forceQuery
	} else {
		consumed := make(map[string]bool)
		for _, q := range r.from.query {
			consumed[q.key] = true
		}
		var query []string
		for _, pair := range pairs {
			if !consumed[pair.key] {
				query = append(query, pair.raw)
			}
		}
		u.RawQuery = strings.Join(query, "&")
	}
	location := u.String()
	if r.to.hasFragment && r.to.fragment == "" {
		location += "#"
	}
	if len(location) > MaxTargetBytes {
		return "", tooLarge()
	}
	return location, nil
}

func sourceKey(r compiledRule) string {
	partKey := func(c component) string {
		if c.name != "" {
			return "capture:" + r.patternText[c.name]
		}
		return "literal:" + c.literal
	}
	var path, query []string
	for _, c := range r.from.path {
		path = append(path, partKey(c))
	}
	for _, q := range r.from.query {
		b, _ := json.Marshal([]string{q.key, partKey(q.value)})
		query = append(query, string(b))
	}
	sort.Strings(query)
	b, _ := json.Marshal([][]string{path, query})
	return string(b)
}

func shadows(a, b compiledRule) bool {
	if len(a.from.path) != len(b.from.path) {
		return false
	}
	covers := func(x, y component) bool {
		if x.name == "" {
			return y.name == "" && x.literal == y.literal
		}
		if y.name == "" {
			return y.literal != "" && len(y.literal) <= MaxCaptureBytes && (a.patterns[x.name] == nil || a.patterns[x.name].MatchString(y.literal))
		}
		return a.patternText[x.name] == "" || a.patternText[x.name] == b.patternText[y.name]
	}
	for i, c := range a.from.path {
		if !covers(c, b.from.path[i]) {
			return false
		}
	}
	for _, x := range a.from.query {
		found := false
		for _, y := range b.from.query {
			if x.key == y.key && covers(x.value, y.value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *Set) checkCycles() error {
	// Follow concrete Locations through literal rules. Each rule's own source
	// is a witness; incoming extra query fields cannot prevent literal matching.
	for start, rule := range s.rules {
		if len(rule.bound) != 0 {
			continue
		}
		path := make([]string, len(rule.from.path))
		for i, c := range rule.from.path {
			path[i] = c.literal
		}
		var query []queryPair
		for _, q := range rule.from.query {
			query = append(query, queryPair{key: q.key, value: q.value.literal, raw: url.QueryEscape(q.key) + "=" + url.QueryEscape(q.value.literal)})
		}
		visited := make(map[int]bool)
		current := start
		for {
			if visited[current] {
				return configError(current, "to", "literal redirect cycle")
			}
			visited[current] = true
			r := s.rules[current]
			if len(r.bound) != 0 || r.to.origin != "" {
				break
			}
			var raw []string
			for _, q := range query {
				raw = append(raw, q.raw)
			}
			location, err := r.build(nil, query, strings.Join(raw, "&"), false)
			if err != nil {
				return configError(current, "to", "expanded destination exceeds byte limit")
			}
			u, _ := url.Parse(location)
			path, _ = decodePath(u.EscapedPath())
			query, _ = parseQuery(u.RawQuery)
			next := -1
			for i, candidate := range s.rules {
				_, match, err := candidate.match(path, query)
				if err != nil {
					break
				}
				if match {
					if len(candidate.bound) == 0 {
						next = i
					}
					break
				}
			}
			if next < 0 {
				break
			}
			current = next
		}
	}
	return nil
}
