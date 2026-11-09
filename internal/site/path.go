// Package site validates public paths and confines object keys to one publication
// prefix. It performs no filesystem cleaning, storage access or URL re-decoding.
package site

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxTargetBytes = 8 * 1024
	MaxKeyBytes    = 1024
)

// Error describes a local rejection. Reason is fixed text and never includes
// request values, so it can safely cross the public error/logging boundary.
// Resolution, when set, is how the rejected object was reached: a 406
// carries it.
type Error struct {
	Status     int
	Reason     string
	Resolution Resolution
}

func (e *Error) Error() string { return "site request: " + e.Reason }

// Target retains the original escaped path and query alongside the once-decoded
// absolute public path. A query never participates in object-key construction.
type Target struct {
	EscapedPath string
	Path        string
	RawQuery    string
	ForceQuery  bool
}

// ParseTarget accepts origin-form and HTTP(S) absolute-form request targets.
// When RequestURI is set, it is authoritative, but the parsed URL must agree
// with it. URL-only requests are also supported. Absolute-form authority must
// agree exactly with Request.Host; the HTTP admission boundary validates that
// authority's syntax and optional allowlist. It never selects an object prefix.
func ParseTarget(r *http.Request) (Target, error) {
	if r == nil || r.URL == nil {
		return Target{}, reject(http.StatusBadRequest, "invalid request target")
	}
	u := r.URL
	if u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawFragment != "" {
		return Target{}, reject(http.StatusBadRequest, "invalid request target")
	}
	// EscapedPath silently ignores an invalid RawPath. Do not permit that alternate
	// interpretation, even when the caller supplied RequestURI separately.
	if u.RawPath != "" {
		p, err := url.PathUnescape(u.RawPath)
		if err != nil || p != u.Path {
			return Target{}, reject(http.StatusBadRequest, "inconsistent request target")
		}
	}
	raw := r.RequestURI
	if raw == "" {
		raw = u.EscapedPath()
		if u.RawPath != "" {
			raw = u.RawPath
		}
		if u.Scheme != "" || u.Host != "" {
			raw = u.Scheme + "://" + u.Host + raw
		}
		if u.RawQuery != "" || u.ForceQuery {
			raw += "?" + u.RawQuery
		}
	}
	if len(raw) > MaxTargetBytes {
		return Target{}, reject(http.StatusRequestURITooLong, "target exceeds byte limit")
	}
	if strings.Contains(raw, "#") || !validText(raw) {
		return Target{}, reject(http.StatusBadRequest, "invalid request target")
	}
	pathQuery := raw
	absolute := !strings.HasPrefix(raw, "/")
	if absolute {
		parsed, err := url.ParseRequestURI(raw)
		if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
			return Target{}, reject(http.StatusBadRequest, "invalid absolute-form target")
		}
		if parsed.Scheme != u.Scheme || parsed.Host != u.Host || parsed.Host != r.Host {
			return Target{}, reject(http.StatusBadRequest, "inconsistent request authority")
		}
		// The authority is literal (userinfo was rejected). Slice the original
		// target to preserve escape spelling and raw Unicode, not URL.String().
		_, afterScheme, ok := strings.Cut(raw, "://")
		if !ok || !strings.HasPrefix(afterScheme, parsed.Host) {
			return Target{}, reject(http.StatusBadRequest, "invalid absolute-form target")
		}
		pathQuery = afterScheme[len(parsed.Host):]
	} else if u.Scheme != "" || u.Host != "" {
		return Target{}, reject(http.StatusBadRequest, "inconsistent request target")
	}
	escaped, query, hasQuery := strings.Cut(pathQuery, "?")
	if absolute && escaped == "" {
		escaped = "/"
	}
	path, err := decodePath(escaped)
	if err != nil {
		return Target{}, err
	}
	urlPath := u.Path
	if absolute && urlPath == "" {
		urlPath = "/"
	}
	forceQuery := hasQuery && query == ""
	if path != urlPath || query != u.RawQuery || forceQuery != u.ForceQuery || u.RawPath != "" && u.RawPath != escaped {
		return Target{}, reject(http.StatusBadRequest, "inconsistent request target")
	}
	// Match redirect query validation, including form-style '+' decoding, without
	// interpreting query parameters or using them in object selection.
	decodedQuery, err := url.QueryUnescape(query)
	if err != nil || !validText(query) || !validText(decodedQuery) {
		return Target{}, reject(http.StatusBadRequest, "invalid query encoding")
	}
	if hiddenPath(path) {
		return Target{}, reject(http.StatusNotFound, "hidden path")
	}
	return Target{EscapedPath: escaped, Path: path, RawQuery: query, ForceQuery: forceQuery}, nil
}

func decodePath(escaped string) (string, error) {
	if !strings.HasPrefix(escaped, "/") || strings.ContainsAny(escaped, "?#") {
		return "", reject(http.StatusBadRequest, "invalid path")
	}
	// Decode segments independently: a separator produced by unescaping must
	// never become a new path boundary.
	parts := strings.Split(escaped[1:], "/")
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil || !safeSegment(decoded) || decoded == "" && i != len(parts)-1 {
			return "", reject(http.StatusBadRequest, "invalid path encoding or segment")
		}
		parts[i] = decoded
	}
	return "/" + strings.Join(parts, "/"), nil
}

func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func hex(b byte) bool { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }

func safeSegment(s string) bool {
	if !validText(s) || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
		return false
	}
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && hex(s[i+1]) && hex(s[i+2]) {
			return false
		}
	}
	return true
}

func hiddenPath(path string) bool {
	for i, part := range strings.Split(path[1:], "/") {
		if strings.HasPrefix(part, ".") && !(i == 0 && part == ".well-known") {
			return true
		}
	}
	return false
}

func reject(status int, reason string) error { return &Error{Status: status, Reason: reason} }

// Mapper is immutable after construction and safe for concurrent use.
type Mapper struct{ prefix, index string }

// Object describes a resolved absolute object path for internal cache policies
// and its corresponding S3 key. Neither field contains a request query.
type Object struct {
	Path string
	Key  string
}

// NewMapper takes the already-normalized configured prefix (empty, or a relative
// directory ending in exactly one slash) and a nonempty index filename. Configured
// strings are literal object names, not URL encodings.
func NewMapper(prefix, index string) (*Mapper, error) {
	if prefix != "" && (!strings.HasSuffix(prefix, "/") || !relativeKey(strings.TrimSuffix(prefix, "/"))) {
		return nil, errors.New("site mapper: invalid canonical prefix")
	}
	if !relativeKey(index) || strings.Contains(index, "/") {
		return nil, errors.New("site mapper: invalid index filename")
	}
	if len(prefix)+len(index) > MaxKeyBytes {
		return nil, errors.New("site mapper: configured key exceeds byte limit")
	}
	return &Mapper{prefix: prefix, index: index}, nil
}

func relativeKey(s string) bool {
	if s == "" || !validText(s) || strings.Contains(s, "\\") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Map maps an absolute, already once-decoded public path to one object. A
// trailing slash selects the index directly; an exact non-directory path is
// retained. Any absence-dependent fallback belongs to the Resolver.
func (m *Mapper) Map(path string) (Object, error) {
	if m == nil || m.index == "" {
		return Object{}, errors.New("site mapper: uninitialized mapper")
	}
	if !strings.HasPrefix(path, "/") {
		return Object{}, reject(http.StatusBadRequest, "invalid path")
	}
	parts := strings.Split(path[1:], "/")
	for i, part := range parts {
		if !safeSegment(part) || part == "" && i != len(parts)-1 {
			return Object{}, reject(http.StatusBadRequest, "invalid path encoding or segment")
		}
	}
	if hiddenPath(path) {
		return Object{}, reject(http.StatusNotFound, "hidden path")
	}
	if strings.HasSuffix(path, "/") {
		path += m.index
	}
	// Both boundaries were validated. Concatenation preserves literal Unicode,
	// percent, question and fragment data and cannot clean away the fixed prefix.
	key := m.prefix + path[1:]
	if len(key) > MaxKeyBytes {
		return Object{}, reject(http.StatusRequestURITooLong, "object key exceeds byte limit")
	}
	return Object{Path: path, Key: key}, nil
}
