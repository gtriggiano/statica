package site

import (
	"net/http"
	"testing"
)

func TestQualityParsingAndConservativeDuplicates(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int
	}{
		{"0", 0}, {"1", 1000}, {"0.", 0}, {"1.", 1000}, {"0.001", 1}, {"0.01", 10}, {"0.1", 100}, {"0.999", 999}, {"1.000", 1000},
		{"", 0}, {".5", 0}, {"00.5", 0}, {"0.0001", 0}, {"1.001", 0}, {"2", 0}, {"+0.5", 0}, {"0.1e0", 0}, {"0.1 ", 0},
	} {
		if got := quality(tc.value); got != tc.want {
			t.Errorf("quality(%q)=%d want %d", tc.value, got, tc.want)
		}
	}
	h := http.Header{"Accept-Encoding": {"BR;q=0.1, br;q=0.9", "gzip;q=0.123,identity;q=0,*;q=0"}}
	p := parseEncodings(h)
	for coding, want := range map[string]int{"br": 100, "gzip": 123, "identity": 0, "deflate": 0} {
		if got := p.q(coding); got != want {
			t.Errorf("q(%s)=%d want %d", coding, got, want)
		}
	}
	for _, value := range []string{"br;q=invalid", "br;q=0.5;extra=true", "br;foo=1", "br;q=0.1234", "br;q=\"1\""} {
		if q := parseEncodings(http.Header{"Accept-Encoding": {value}}).q("br"); q != 0 {
			t.Errorf("malformed %q accepted with q%d", value, q)
		}
	}
}

func TestEncodingHeaderPresence(t *testing.T) {
	for _, tc := range []struct {
		name         string
		header       http.Header
		identity, br int
		present      bool
	}{
		{"absent", nil, 1000, 1000, false},
		{"empty", http.Header{"Accept-Encoding": {""}}, 1000, 0, true},
		{"nil values", http.Header{"accept-encoding": nil}, 1000, 0, true},
		{"wildcard positive", http.Header{"Accept-Encoding": {"*;q=0.5"}}, 1000, 500, true},
		{"wildcard refusal", http.Header{"Accept-Encoding": {"*;q=0"}}, 0, 0, true},
		{"explicit overrides wildcard", http.Header{"Accept-Encoding": {"*;q=0,br;q=0.5,identity;q=0.3"}}, 300, 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseEncodings(tc.header)
			if got.present != tc.present || got.q("identity") != tc.identity || got.q("br") != tc.br {
				t.Fatalf("preferences = %+v identity=%d br=%d", got, got.q("identity"), got.q("br"))
			}
		})
	}
}

func TestExplicitHTMLAcceptance(t *testing.T) {
	for _, tc := range []struct {
		accept string
		want   bool
	}{
		{"text/html", true}, {"text/html;charset=utf-8", true}, {"text/html;charset=utf-8;q=0.8", true}, {`text/html;example="a,b";q=0.8`, true}, {"TEXT/HTML;q=0.25", true}, {"text/html;q=0.001,application/json", true},
		{"", false}, {"*/*", false}, {"text/*", false}, {"text/html;q=0,*/*", false}, {"text/html;q=oops", false}, {`text/html;q="0.8"`, false}, {"text/html;q=0,text/html;q=1", false},
	} {
		if got := acceptsHTML(http.Header{"Accept": {tc.accept}}); got != tc.want {
			t.Errorf("Accept %q = %v want %v", tc.accept, got, tc.want)
		}
	}
}
