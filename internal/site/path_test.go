package site

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/gtriggiano/statica/internal/redirect"
)

// request deliberately preserves malformed wire targets that net/http would
// reject before a handler, so the package's own boundary is tested as well.
func request(raw string) *http.Request {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		u = &url.URL{Path: raw}
	}
	return &http.Request{RequestURI: raw, URL: u, Host: u.Host}
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("error = %v, want status %d", err, status)
	}
}

func TestParseTargetAndMapping(t *testing.T) {
	m, err := NewMapper("docs/", "index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ raw, path, key string }{
		{"/", "/", "docs/index.html"},
		{"/guide/", "/guide/", "docs/guide/index.html"},
		{"/logo.svg", "/logo.svg", "docs/logo.svg"},
		{"/a%20b.txt", "/a b.txt", "docs/a b.txt"},
		{"/a+b.txt", "/a+b.txt", "docs/a+b.txt"},
		{"/caf%C3%A9.html", "/café.html", "docs/café.html"},
		{"/café.html", "/café.html", "docs/café.html"},
		{"/cafe%CC%81.html", "/café.html", "docs/café.html"},
		{"/100%25.txt", "/100%.txt", "docs/100%.txt"},
		{"/x%3Fy%23z", "/x?y#z", "docs/x?y#z"},
		{"/x%3fy?versionId=secret&response-content-type=text/plain", "/x?y", "docs/x?y"},
		{"/.well-known/", "/.well-known/", "docs/.well-known/index.html"},
		{"/.well-known/acme-challenge/token", "/.well-known/acme-challenge/token", "docs/.well-known/acme-challenge/token"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			target, err := ParseTarget(request(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			rawPath, rawQuery, hasQuery := strings.Cut(tc.raw, "?")
			if target != (Target{EscapedPath: rawPath, Path: tc.path, RawQuery: rawQuery, ForceQuery: hasQuery && rawQuery == ""}) {
				t.Fatalf("target = %#v", target)
			}
			object, err := m.Map(target.Path)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := tc.path
			if strings.HasSuffix(wantPath, "/") {
				wantPath += "index.html"
			}
			if object != (Object{Path: wantPath, Key: tc.key}) {
				t.Fatalf("object = %#v, want path %q, key %q", object, wantPath, tc.key)
			}
		})
	}
}

func TestRejectUnsafeTargets(t *testing.T) {
	for _, raw := range []string{
		"", "*", "example.com:443", "ftp://example.com/file", "//other-host/file",
		"/../secret", "/%2e%2e/secret", "/%252e%252e/secret", "/./a", "/a/..", "/a//b", "//", "/a//",
		"/a%2fb", "/a%2Fb", "/a%5cb", "/a%5Cb", "/a\\b", "/%2f", "/%252f", "/%25ab",
		"/%", "/%2", "/%GG", "/a#fragment", "/a?q=#fragment", "/%00", "/%0d%0a", "/%7f", "/%C2%85",
		"/\x01", "/\x7f", "/\xff", "/%ff", "/%c0%af", "/%ed%a0%80",
		"/a?key=%", "/a?key=%ff", "/a?%00=value", "/a?key=%0d%0a", "/a?key=\xff", "/a?key=\x01",
		"/.secret/../leak", "/.secret?key=%00",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseTarget(request(raw))
			wantStatus(t, err, http.StatusBadRequest)
		})
	}
	for _, raw := range []string{"/.env", "/%2eenv", "/x/.git/config", "/.well-known/.secret", "/x/.well-known/token", "/.well-knownish/token", "/..."} {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseTarget(request(raw))
			wantStatus(t, err, http.StatusNotFound)
		})
	}
}

func TestQueryValidationMatchesRedirect(t *testing.T) {
	for _, raw := range []string{"", "q=a+b", "q=a%20b", "q=%3f%23%2f", "a=one;two&&=empty&flag", "q=100%2525", "q=café", "q=a b", "q=%", "q=%ff", "q=%C2%85", "q=\xff", "q=%00"} {
		_, err := ParseTarget(request("/a?" + raw))
		_, _, redirectErr := (*redirect.Set)(nil).Resolve("/a", raw, false)
		if (err == nil) != (redirectErr == nil) {
			t.Errorf("query %q: site error %v, redirect error %v", raw, err, redirectErr)
		}
	}
}

func TestRequestRepresentationsAndAmbiguity(t *testing.T) {
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/a%20b?q=a+b", nil),
		{URL: &url.URL{Path: "/a b", RawQuery: "q=a+b"}},
		{URL: &url.URL{Path: "/a?b#c", RawPath: "/a%3Fb%23c"}},
		{URL: &url.URL{Path: "/café"}},
		{URL: &url.URL{Path: "/a", ForceQuery: true}},
		request("/a?"),
	} {
		if _, err := ParseTarget(r); err != nil {
			t.Errorf("valid request %#v: %v", r, err)
		}
	}
	for _, r := range []*http.Request{
		nil, {}, {RequestURI: "/a"},
		{RequestURI: "/a", URL: &url.URL{Path: "/b"}},
		{RequestURI: "/a?q=1", URL: &url.URL{Path: "/a", RawQuery: "q=2"}},
		{RequestURI: "/a?", URL: &url.URL{Path: "/a"}},
		{RequestURI: "/a", URL: &url.URL{Path: "/a", ForceQuery: true}},
		{RequestURI: "/%61", URL: &url.URL{Path: "/a", RawPath: "/%62"}},
		{RequestURI: "/%61", URL: &url.URL{Path: "/a", RawPath: "/a"}},
		{URL: &url.URL{Path: "/a", RawPath: "/%"}},
		{RequestURI: "/a", URL: &url.URL{Path: "/a", Host: "example.com"}},
		{URL: &url.URL{Opaque: "/a"}},
		{URL: &url.URL{Path: "/a", User: url.User("secret")}},
		{URL: &url.URL{Path: "/a", Fragment: "secret"}},
		{URL: &url.URL{Path: "/a", RawFragment: "secret"}},
	} {
		_, err := ParseTarget(r)
		wantStatus(t, err, http.StatusBadRequest)
	}
}

func TestTargetByteLimit(t *testing.T) {
	for _, raw := range []string{"/" + strings.Repeat("a", MaxTargetBytes-1), "/?" + strings.Repeat("q", MaxTargetBytes-2), "/" + strings.Repeat("a", MaxTargetBytes-2) + "?"} {
		if _, err := ParseTarget(request(raw)); err != nil {
			t.Errorf("exact cap: %v", err)
		}
		_, err := ParseTarget(request(raw + "x"))
		wantStatus(t, err, http.StatusRequestURITooLong)
	}
	// The budget counts UTF-8 wire bytes, not rune count.
	_, err := ParseTarget(request("/" + strings.Repeat("é", MaxTargetBytes/2)))
	wantStatus(t, err, http.StatusRequestURITooLong)
}

func TestMapperConfigurationAndConfinement(t *testing.T) {
	for _, tc := range []struct{ prefix, index string }{
		{"/docs/", "index.html"}, {"docs", "index.html"}, {"docs//", "index.html"}, {"../docs/", "index.html"},
		{"docs/./", "index.html"}, {"docs\\/", "index.html"}, {"docs\x00/", "index.html"}, {"\xff/", "index.html"},
		{"", ""}, {"", "."}, {"", ".."}, {"", "a/b"}, {"", "/index.html"}, {"", "a\\b"}, {"", "\xff"},
		{"", strings.Repeat("a", MaxKeyBytes+1)}, {strings.Repeat("a", MaxKeyBytes) + "/", "i"},
	} {
		if _, err := NewMapper(tc.prefix, tc.index); err == nil {
			t.Errorf("accepted prefix/index %q/%q", tc.prefix, tc.index)
		}
	}
	for _, prefix := range []string{"", "docs/", "%2e%2e/space and café/", ".private/"} {
		m, err := NewMapper(prefix, "index.html")
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/", "/guide/", "/a?b#c", "/100%", "/café"} {
			obj, err := m.Map(path)
			if err != nil {
				t.Fatal(err)
			}
			if obj.Key != prefix+obj.Path[1:] {
				t.Fatalf("key escaped prefix: %#v", obj)
			}
		}
	}
	m, _ := NewMapper("docs/", "index.html")
	for _, path := range []string{"", "relative", "//authority/a", "/a//b", "/a/../b", "/a/./b", "/a\\b", "/%2e%2e", "/\x00", "/\xff"} {
		_, err := m.Map(path)
		wantStatus(t, err, http.StatusBadRequest)
	}
	_, err := m.Map("/.git/config")
	wantStatus(t, err, http.StatusNotFound)
	if _, err := (*Mapper)(nil).Map("/"); err == nil {
		t.Fatal("nil mapper accepted")
	}
	if _, err := (&Mapper{}).Map("/"); err == nil {
		t.Fatal("zero mapper accepted")
	}
}

func TestObjectKeyByteLimit(t *testing.T) {
	m, _ := NewMapper("docs/", "index.html")
	for _, path := range []string{"/" + strings.Repeat("a", MaxKeyBytes-len("docs/")), "/" + strings.Repeat("é", 509) + "a"} {
		obj, err := m.Map(path)
		if err != nil || len(obj.Key) != MaxKeyBytes {
			t.Fatalf("exact cap: key bytes %d, error %v", len(obj.Key), err)
		}
		_, err = m.Map(path + "x")
		wantStatus(t, err, http.StatusRequestURITooLong)
	}
	_, err := m.Map("/" + strings.Repeat("a", MaxKeyBytes-len("docs/")-len("/index.html")+1) + "/")
	wantStatus(t, err, http.StatusRequestURITooLong)
}

func TestErrorsDoNotEchoValues(t *testing.T) {
	const secret = "PRIVATE_SENTINEL"
	for _, raw := range []string{"/../" + secret, "/.hidden/" + secret, "/?key=%00" + secret, "/" + strings.Repeat(secret, MaxTargetBytes)} {
		_, err := ParseTarget(request(raw))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe error %v", err)
		}
	}
}

func FuzzParseTarget(f *testing.F) {
	for _, raw := range []string{"/", "/a+b?x=a+b", "/café/", "/a%20b", "/a%3Fb%23c?x=%26", "/100%25", "/.well-known/token", "/../secret", "/%252e%252e", "/a%2fb", "/a//b", "/.env", "/?x=%ff", "/?", "http://example.com/a?q=1", "https://example.com?", "http://example.com"} {
		f.Add(raw)
	}
	m, _ := NewMapper("publication/fixed/", "index.html")
	f.Fuzz(func(t *testing.T, raw string) {
		target, err := ParseTarget(request(raw))
		if err != nil {
			var rejection *Error
			if !errors.As(err, &rejection) || rejection.Status != 400 && rejection.Status != 404 && rejection.Status != 414 {
				t.Fatalf("unclassified rejection: %v", err)
			}
			return
		}
		if len(raw) > MaxTargetBytes || !strings.HasPrefix(target.Path, "/") || !utf8.ValidString(target.Path) {
			t.Fatal("accepted target outside wire/path bounds")
		}
		decoded, err := url.PathUnescape(target.EscapedPath)
		if err != nil || decoded != target.Path || strings.Count(target.EscapedPath, "/") != strings.Count(target.Path, "/") {
			t.Fatal("path did not decode exactly once")
		}
		decodedQuery, err := url.QueryUnescape(target.RawQuery)
		if err != nil || !utf8.ValidString(decodedQuery) {
			t.Fatal("accepted invalid query encoding")
		}
		for _, r := range decodedQuery {
			if unicode.IsControl(r) {
				t.Fatal("accepted query control")
			}
		}
		segments := strings.Split(target.Path[1:], "/")
		for i, segment := range segments {
			if segment == "." || segment == ".." || strings.Contains(segment, "\\") || strings.HasPrefix(segment, ".") && !(i == 0 && segment == ".well-known") {
				t.Fatal("accepted traversal, hidden or backslash segment")
			}
			if segment == "" && i != len(segments)-1 {
				t.Fatal("accepted empty interior segment")
			}
			for _, r := range segment {
				if unicode.IsControl(r) {
					t.Fatal("accepted control")
				}
			}
			for j := 0; j+2 < len(segment); j++ {
				if segment[j] == '%' && strings.ContainsRune("0123456789abcdefABCDEF", rune(segment[j+1])) && strings.ContainsRune("0123456789abcdefABCDEF", rune(segment[j+2])) {
					t.Fatal("accepted second decoding opportunity")
				}
			}
		}
		obj, err := m.Map(target.Path)
		if err != nil {
			wantStatus(t, err, http.StatusRequestURITooLong)
			return
		}
		wantPath := target.Path
		if strings.HasSuffix(wantPath, "/") {
			wantPath += "index.html"
		}
		if obj.Path != wantPath || obj.Key != "publication/fixed/"+wantPath[1:] || len(obj.Key) > MaxKeyBytes {
			t.Fatal("object escaped fixed publication prefix or changed path")
		}
		// Changing only query data cannot alter the object identity.
		other := request(target.EscapedPath + "?other=value")
		otherTarget, err := ParseTarget(other)
		if err != nil {
			// Adding a query can exceed the wire budget, which is independent of keys.
			wantStatus(t, err, http.StatusRequestURITooLong)
			return
		}
		otherObj, err := m.Map(otherTarget.Path)
		if err != nil || obj != otherObj {
			t.Fatal("query changed object identity")
		}
	})
}

func TestAbsoluteFormTargets(t *testing.T) {
	for _, tc := range []struct {
		raw, path, query string
		force            bool
	}{
		{"http://example.com/a%20b?q=a+b", "/a b", "q=a+b", false},
		{"https://EXAMPLE.com:443/guide/", "/guide/", "", false},
		{"http://[2001:db8::1]:8080/café", "/café", "", false},
		{"http://example.com", "/", "", false},
		{"http://example.com?", "/", "", true},
		{"http://example.com?x=y", "/", "x=y", false},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			client, err := http.NewRequest(http.MethodGet, tc.raw, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []*http.Request{request(tc.raw), httptest.NewRequest(http.MethodGet, tc.raw, nil), client} {
				target, err := ParseTarget(r)
				if err != nil {
					t.Fatal(err)
				}
				if target.Path != tc.path || target.RawQuery != tc.query || target.ForceQuery != tc.force {
					t.Fatalf("target = %#v", target)
				}
			}
		})
	}
	for _, raw := range []string{"http://user:password@example.com/a", "http:///a", "http:opaque", "ftp://example.com/a", "https://example.com/a#frag"} {
		_, err := ParseTarget(request(raw))
		wantStatus(t, err, http.StatusBadRequest)
	}
	for _, mutate := range []func(*http.Request){
		func(r *http.Request) { r.Host = "other.example" },
		func(r *http.Request) { r.Host = "" },
		func(r *http.Request) { r.URL.Host = "other.example" },
		func(r *http.Request) { r.URL.Scheme = "https" },
		func(r *http.Request) { r.URL.Path = "/different" },
		func(r *http.Request) { r.URL.RawQuery = "different=1" },
	} {
		r := request("http://example.com/a?x=1")
		mutate(r)
		_, err := ParseTarget(r)
		wantStatus(t, err, http.StatusBadRequest)
	}
	// Absolute-form's authority bytes count toward the complete wire budget.
	raw := "http://example.com/" + strings.Repeat("a", MaxTargetBytes-len("http://example.com/"))
	if _, err := ParseTarget(request(raw)); err != nil {
		t.Fatal(err)
	}
	_, err := ParseTarget(request(raw + "x"))
	wantStatus(t, err, http.StatusRequestURITooLong)
}
