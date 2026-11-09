package httpcache

import (
	"strings"
	"testing"
	"time"
)

func TestParseGrammar(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"no-store", true},
		{"public, max-age=60", true},
		{"public, max-age=60, extension=arbitrary-token", true},
		{`private="Set-Cookie, X-Private", max-age=0`, true},
		{`extension="value, with \"quoted\" words"`, true},
		{"extension=\"caf\u00e9\", ext2=\"\\\\\"", true},
		{"max-stale", true},
		{"MAX-AGE=60, No-Cache", true},
		{" \tmax-age=60 ,\tno-cache\t", true},
		{"max-age = 60", true},
		{`max-age="60"`, true},
		{"auto", true}, // a token extension; config accepts it before Parse
		{"s-maxage=99999999999999999999999999", true},
		{"", false},
		{" ", false},
		{",", false},
		{"max-age=60,", false},
		{"max-age=60, ", false},
		{",max-age=60", false},
		{"max-age=60,,no-cache", false},
		{"max-age=", false},
		{"max-age=60, Max-Age=120", false},
		{"no-cache, NO-CACHE", false},
		{`no-cache="a", no-cache`, false},
		{"max-age 60", false},
		{"no store", false},
		{"max-age=6 0", false},
		{"ext=a b", false},
		{`ext="a"b`, false},
		{`ext="unclosed`, false},
		{`ext="dangling\`, false},
		{"ext=@", false},
		{"@", false},
		{"max-age=60\r\n", false},
		{"no\x00store", false},
		{"max-age=60\x7f", false},
		{"max-age=60\v", false},
		{"ext=\"\xff\"", false},
		// Only spaces and tabs are whitespace: U+00A0 (no-break space) and
		// U+2028 (line separator) are written as escapes because they look
		// like ordinary spaces in source.
		{"\u00a0max-age=60", false},
		{"max-age=60\u00a0", false},
		{"max-age=60,\u2028no-cache", false},
	} {
		if _, ok := Parse(tc.value); ok != tc.valid || Valid(tc.value) != tc.valid {
			t.Errorf("Parse(%q) valid = %v, want %v", tc.value, ok, tc.valid)
		}
	}
	for _, name := range []string{"max-age", "s-maxage", "min-fresh", "max-stale", "stale-if-error", "stale-while-revalidate"} {
		for _, value := range []string{"-1", "banana", "1.5", `""`, "+1", "0x10", `"6 0"`} {
			if Valid(name + "=" + value) {
				t.Errorf("invalid delta accepted: %s=%s", name, value)
			}
		}
		for _, value := range []string{"0", "60", `"60"`, "99999999999999999999"} {
			if !Valid(name + "=" + value) {
				t.Errorf("valid delta rejected: %s=%s", name, value)
			}
		}
		if Valid(name) != (name == "max-stale") {
			t.Errorf("%s without argument: valid = %v", name, Valid(name))
		}
	}
}

func TestParseDirectives(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  Directives
	}{
		{`public, max-age=60, s-maxage="120", immutable`, Directives{Public: true, Immutable: true, MaxAge: Delta{60, true}, SMaxAge: Delta{120, true}}},
		{`No-Store, PRIVATE="Set-Cookie", no-cache="X-A, X-B", must-revalidate, proxy-revalidate`, Directives{NoStore: true, Private: true, NoCache: true, MustRevalidate: true, ProxyRevalidate: true}},
		{"min-fresh=1, max-stale, stale-if-error=2, stale-while-revalidate=3", Directives{MinFresh: Delta{1, true}, MaxStale: Delta{MaxDelta, true}, StaleIfError: Delta{2, true}, StaleWhileRevalidate: Delta{3, true}}},
		{"max-stale=7, max-age=0", Directives{MaxStale: Delta{7, true}, MaxAge: Delta{0, true}}},
		{"max-age=99999999999999999999, extension", Directives{MaxAge: Delta{MaxDelta, true}}},
		{"max-age=60, max-age=60", Directives{}},
		{"no-store, max-age=x", Directives{}},
	} {
		if got, _ := Parse(tc.value); got != tc.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.value, got, tc.want)
		}
	}
	if d := (Delta{MaxDelta, true}); d.Duration() != MaxDelta*time.Second || d.Duration() <= 0 {
		t.Fatalf("MaxDelta duration = %v", d.Duration())
	}
}

func TestDeltaSecondsSaturates(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
		ok    bool
	}{
		{"0", 0, true},
		{"60", 60, true},
		{"2147483647", MaxDelta - 1, true},
		{"2147483648", MaxDelta, true},
		{"2147483649", MaxDelta, true},
		{"18446744073709551616", MaxDelta, true}, // 2^64
		{strings.Repeat("9", 400), MaxDelta, true},
		{strings.Repeat("0", 400) + "60", 60, true},
		{"", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{" 1", 0, false},
		{"1 ", 0, false},
		{"1.0", 0, false},
		{"\u0661", 0, false}, // Arabic-Indic digit one
	} {
		if got, ok := DeltaSeconds(tc.value); got != tc.want || ok != tc.ok {
			t.Errorf("DeltaSeconds(%q) = %d, %v; want %d, %v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEntityTagGrammar(t *testing.T) {
	for _, tc := range []struct {
		tag           string
		valid, strong bool
	}{
		{`"abc"`, true, true},
		{`""`, true, true},
		{`"a,b"`, true, true},
		{`"!#~"`, true, true},
		{"\"\x80\xff\"", true, true},
		{`W/"abc"`, true, false},
		{`W/""`, true, false},
		{"", false, false},
		{"abc", false, false},
		{`"abc`, false, false},
		{`abc"`, false, false},
		{`"`, false, false},
		{`W/"`, false, false},
		{`w/"abc"`, false, false},
		{`W/abc`, false, false},
		{` "abc"`, false, false},
		{`"a"b"`, false, false},
		{`"a b"`, false, false},
		{"\"a\tb\"", false, false},
		{"\"a\x7fb\"", false, false},
	} {
		if ValidETag(tc.tag) != tc.valid || StrongETag(tc.tag) != tc.strong {
			t.Errorf("%q: valid %v strong %v, want %v %v", tc.tag, ValidETag(tc.tag), StrongETag(tc.tag), tc.valid, tc.strong)
		}
	}
}
