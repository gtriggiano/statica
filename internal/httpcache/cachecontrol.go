// Package httpcache implements the RFC 9111 mechanics shared by configuration
// validation, the response layer and the memory cache: strict Cache-Control
// and entity-tag grammar, the origin-metadata malformed predicate, age and
// freshness-lifetime calculation, request directives and 304 metadata merging.
// It performs no I/O and keeps no state, so every caller classifies the same
// metadata the same way.
//
// Every delta-seconds value, and every age or lifetime derived from one, lies
// in [0, MaxDelta] seconds: RFC 9111 §1.2.2 requires a recipient to treat a
// larger value, or an overflowing calculation, as 2^31 seconds.
package httpcache

import (
	"strings"
	"time"
	"unicode/utf8"
)

// MaxDelta is the RFC 9111 §1.2.2 delta-seconds saturation value, in seconds.
const MaxDelta = 1 << 31

// maxAge is MaxDelta as a Duration. Three of them still fit in an int64.
const maxAge = MaxDelta * time.Second

// Delta is a delta-seconds directive argument. Present distinguishes an
// absent directive from a zero one; Seconds is in [0, MaxDelta].
type Delta struct {
	Seconds int64
	Present bool
}

// Duration converts d; an absent d is zero.
func (d Delta) Duration() time.Duration { return time.Duration(d.Seconds) * time.Second }

// DeltaSeconds parses delta-seconds: one or more ASCII digits, without sign or
// spaces. A value beyond MaxDelta, however long, saturates to MaxDelta.
func DeltaSeconds(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		if n < MaxDelta {
			n = n*10 + int64(s[i]-'0')
		}
	}
	return min(n, MaxDelta), true
}

// Directives are the Cache-Control directives statica interprets. A boolean
// directive is set whenever it is present and any argument is ignored: a
// qualified no-cache="..." or private="..." is treated as its unqualified,
// stricter form because statica stores whole responses and cannot omit the
// named fields. A max-stale without argument accepts any staleness and is
// recorded as MaxDelta.
type Directives struct {
	NoStore, NoCache, Private, MustRevalidate, ProxyRevalidate, Public, Immutable bool
	MaxAge, SMaxAge, MinFresh, MaxStale, StaleIfError, StaleWhileRevalidate       Delta
}

// Parse strictly parses a Cache-Control field value (RFC 9111 §5.2): comma
// separated directives, each a token name with an optional "=" and a token or
// quoted-string argument, with optional spaces and tabs around members and
// around "=". It rejects an empty value, empty members including a trailing
// comma, a name repeated under ASCII case folding, invalid UTF-8, controls
// other than tab, and a recognized numeric directive without delta-seconds
// (quoted delta-seconds are accepted; max-stale's argument is optional).
// Unknown extensions are accepted under the same grammar. On failure the
// Directives are zero.
func Parse(s string) (Directives, bool) {
	var d Directives
	if s == "" || !fieldValue(s) {
		return Directives{}, false
	}
	var seen []string
	i := 0
	for {
		i = skipOWS(s, i)
		start := i
		for i < len(s) && tchar(s[i]) {
			i++
		}
		if i == start {
			return Directives{}, false
		}
		name := strings.ToLower(s[start:i]) // tokens are ASCII
		for _, n := range seen {
			if n == name {
				return Directives{}, false
			}
		}
		seen = append(seen, name)
		i = skipOWS(s, i)
		hasValue := i < len(s) && s[i] == '='
		value := ""
		if hasValue {
			i = skipOWS(s, i+1)
			var ok bool
			if value, i, ok = argument(s, i); !ok {
				return Directives{}, false
			}
		}
		if !d.set(name, hasValue, value) {
			return Directives{}, false
		}
		i = skipOWS(s, i)
		if i == len(s) {
			return d, true
		}
		if s[i] != ',' {
			return Directives{}, false
		}
		i++
	}
}

// Valid reports whether s is a well-formed Cache-Control field value.
func Valid(s string) bool {
	_, ok := Parse(s)
	return ok
}

// argument parses a token or quoted-string at s[i:] and returns its decoded
// value and the index after it. fieldValue already excluded controls, so any
// byte is valid qdtext or quoted-pair content.
func argument(s string, i int) (string, int, bool) {
	if i < len(s) && s[i] == '"' {
		var b strings.Builder
		for i++; i < len(s); i++ {
			switch s[i] {
			case '"':
				return b.String(), i + 1, true
			case '\\':
				if i++; i == len(s) {
					return "", i, false
				}
			}
			b.WriteByte(s[i])
		}
		return "", i, false
	}
	start := i
	for i < len(s) && tchar(s[i]) {
		i++
	}
	return s[start:i], i, i > start
}

// set records one directive and reports whether its argument is acceptable.
func (d *Directives) set(name string, hasValue bool, value string) bool {
	var delta *Delta
	switch name {
	case "no-store":
		d.NoStore = true
	case "no-cache":
		d.NoCache = true
	case "private":
		d.Private = true
	case "must-revalidate":
		d.MustRevalidate = true
	case "proxy-revalidate":
		d.ProxyRevalidate = true
	case "public":
		d.Public = true
	case "immutable":
		d.Immutable = true
	case "max-age":
		delta = &d.MaxAge
	case "s-maxage":
		delta = &d.SMaxAge
	case "min-fresh":
		delta = &d.MinFresh
	case "stale-if-error":
		delta = &d.StaleIfError
	case "stale-while-revalidate":
		delta = &d.StaleWhileRevalidate
	case "max-stale":
		delta = &d.MaxStale
		if !hasValue {
			*delta = Delta{MaxDelta, true}
			return true
		}
	}
	if delta == nil {
		return true
	}
	seconds, ok := DeltaSeconds(value)
	if !hasValue || !ok {
		return false
	}
	*delta = Delta{seconds, true}
	return true
}

// fieldValue accepts valid UTF-8 without controls other than tab.
func fieldValue(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 && s[i] != '\t' || s[i] == 0x7f {
			return false
		}
	}
	return utf8.ValidString(s)
}

func skipOWS(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

func tchar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// ValidETag reports whether tag is an RFC 9110 §8.8.3 entity-tag: an optional
// W/ weakness indicator and a double-quoted run of etagc (%x21, %x23-7E and
// obs-text).
func ValidETag(tag string) bool {
	tag = strings.TrimPrefix(tag, "W/")
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return false
	}
	for i := 1; i < len(tag)-1; i++ {
		if c := tag[i]; c != 0x21 && (c < 0x23 || c == 0x7f) {
			return false
		}
	}
	return true
}

// StrongETag reports whether tag is a valid entity-tag without W/.
func StrongETag(tag string) bool { return ValidETag(tag) && !strings.HasPrefix(tag, "W/") }
