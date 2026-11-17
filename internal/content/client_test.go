package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/redirect"
	"github.com/gtriggiano/statica/internal/storage"
)

// clientConfig enables client_redirects with p, whose zero fields take the
// defaults of the block.
func clientConfig(t *testing.T, p config.ClientRedirects) *config.Config {
	t.Helper()
	c := testConfig(t)
	if p.Status == 0 {
		p.Status = 301
	}
	if p.Detect == nil {
		p.Detect = []string{redirect.DetectMetaRefresh, redirect.DetectScript}
	}
	if p.MaxObjectBytes == 0 {
		p.MaxObjectBytes = 8 << 10
	}
	if p.Query == "" {
		p.Query = "preserve"
	}
	c.ClientRedirects = &p
	return c
}

// redirectPage is a redirect page as site generators publish them.
func redirectPage(destination string) string {
	return `<!DOCTYPE html><html><head><meta charset="utf-8"><title>Redirecting</title>` +
		`<meta http-equiv="refresh" content="0; url=` + destination + `"></head>` +
		`<body><a href="` + destination + `">Moved</a></body></html>`
}

func htmlPage(body, etag string) memObject {
	return memObject{body, storage.Metadata{ETag: etag, ContentType: "text/html; charset=utf-8"}}
}

func clientOrigin(objects map[string]memObject) *memOrigin {
	return &memOrigin{objects: objects, denied: map[string]bool{}}
}

// lastObservation keeps the observation of the latest request.
type lastObservation struct {
	mu sync.Mutex
	o  Observation
}

func (l *lastObservation) option() HandlerOption {
	return WithObserver(func(o Observation) { l.mu.Lock(); l.o = o; l.mu.Unlock() })
}

func (l *lastObservation) get() Observation {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.o
}

func TestClientRedirectAnswersIndexPages(t *testing.T) {
	origin := clientOrigin(map[string]memObject{
		"docs/old/index.html": htmlPage(redirectPage("../new/"), `"old"`),
		"anchor/index.html":   htmlPage(redirectPage("/docs/#install"), `"anchor"`),
		"script/index.html":   htmlPage(`<script>window.location.replace("/docs/new/" + location.search)</script>`, `"script"`),
	})
	var observed lastObservation
	h := boundary(t, clientConfig(t, config.ClientRedirects{}), origin, observed.option())
	for _, tc := range []struct {
		name, method, target string
		headers              http.Header
		location             string
	}{
		{"index", http.MethodGet, "/docs/old/", nil, "/docs/new/"},
		{"query preserved", http.MethodGet, "/docs/old/?ref=feed", nil, "/docs/new/?ref=feed"},
		{"without trailing slash", http.MethodGet, "/docs/old?ref=feed", nil, "/docs/new/?ref=feed"},
		{"head", http.MethodHead, "/docs/old/", nil, "/docs/new/"},
		{"head without trailing slash", http.MethodHead, "/docs/old", nil, "/docs/new/"},
		{"if-none-match", http.MethodGet, "/docs/old/", http.Header{"If-None-Match": {`"old"`}}, "/docs/new/"},
		{"if-match failing", http.MethodGet, "/docs/old/", http.Header{"If-Match": {`"other"`}}, "/docs/new/"},
		{"range", http.MethodGet, "/docs/old/", http.Header{"Range": {"bytes=0-0"}}, "/docs/new/"},
		{"fragment", http.MethodGet, "/anchor/?ref=feed", nil, "/docs/?ref=feed#install"},
		{"script", http.MethodGet, "/script/", nil, "/docs/new/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(h, tc.method, tc.target, tc.headers)
			if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != tc.location {
				t.Fatalf("got %d Location %q, want 301 %q", w.Code, w.Header().Get("Location"), tc.location)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" || w.Header().Get("Content-Range") != "" || !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
				t.Fatalf("redirect headers %v", w.Header())
			}
		})
	}
	if o := observed.get(); o.Redirect != RedirectScript || o.Location != "/docs/new/" || o.OriginKey != "script/index.html" || o.Resolution != "index" || o.Status != 301 {
		t.Fatalf("script observation %+v", o)
	}
	request(h, http.MethodGet, "/docs/old?ref=feed", nil)
	if o := observed.get(); o.Redirect != RedirectMetaRefresh || o.Location != "/docs/new/?ref=feed" || o.OriginKey != "docs/old/index.html" || o.Resolution != "index" {
		t.Fatalf("meta refresh observation %+v", o)
	}
}

// Only an eligible index object is read for a redirect, once with its body,
// which also serves the page: any other object is served as without
// client_redirects.
func TestClientRedirectReadsOnlyEligiblePages(t *testing.T) {
	plain := "<!DOCTYPE html><p>A page</p>"
	origin := clientOrigin(map[string]memObject{
		"plain/index.html": htmlPage(plain, `"plain"`),
		"page.html":        htmlPage(redirectPage("/new/"), `"page"`),
		"large/index.html": htmlPage(redirectPage("/new/")+strings.Repeat(" ", 8<<10), `"large"`),
		"data/index.html":  {redirectPage("/new/"), storage.Metadata{ETag: `"data"`, ContentType: "application/octet-stream"}},
		"gzip/index.html":  {redirectPage("/new/"), storage.Metadata{ETag: `"gzip"`, ContentType: "text/html", ContentEncoding: "gzip"}},
		"empty/index.html": htmlPage("", `"empty"`),
	})
	var observed lastObservation
	h := boundary(t, clientConfig(t, config.ClientRedirects{}), origin, observed.option())
	for _, tc := range []struct {
		method, target string
		calls          []string
	}{
		{http.MethodGet, "/plain/", []string{"GET plain/index.html"}},
		{http.MethodHead, "/plain/", []string{"HEAD plain/index.html", "GET plain/index.html"}},
		{http.MethodGet, "/plain", []string{"GET plain", "GET plain/index.html"}},
		{http.MethodHead, "/plain", []string{"HEAD plain", "GET plain", "HEAD plain/index.html", "GET plain/index.html"}},
		{http.MethodGet, "/page.html", []string{"GET page.html"}},
		{http.MethodHead, "/page.html", []string{"HEAD page.html"}},
		{http.MethodHead, "/large/", []string{"HEAD large/index.html"}},
		{http.MethodHead, "/data/", []string{"HEAD data/index.html"}},
		{http.MethodHead, "/gzip/", []string{"HEAD gzip/index.html"}},
		{http.MethodHead, "/empty/", []string{"HEAD empty/index.html"}},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			origin.take()
			w := request(h, tc.method, tc.target, nil)
			if w.Code != http.StatusOK && w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "" && w.Code != http.StatusPermanentRedirect {
				t.Fatalf("got %d Location %q", w.Code, w.Header().Get("Location"))
			}
			if calls := origin.take(); strings.Join(calls, "|") != strings.Join(tc.calls, "|") {
				t.Fatalf("origin calls %q, want %q", calls, tc.calls)
			}
			if o := observed.get(); o.Redirect != "" || o.Location != "" {
				t.Fatalf("observation %+v", o)
			}
		})
	}
	// The page read for the inspection is the page served.
	w := request(h, http.MethodGet, "/plain/", nil)
	if w.Code != 200 || w.Body.String() != plain || w.Header().Get("Content-Length") != strconv.Itoa(len(plain)) || w.Header().Get("ETag") != `"plain"` {
		t.Fatalf("plain page: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	origin.take()
	for _, tc := range []struct {
		headers http.Header
		status  int
		body    string
	}{
		{http.Header{"Range": {"bytes=0-8"}}, http.StatusPartialContent, plain[:9]},
		{http.Header{"Range": {"bytes=0-8"}, "If-None-Match": {`"plain"`}}, http.StatusNotModified, ""},
		{http.Header{"Range": {"bytes=0-8"}, "If-Range": {`"other"`}}, http.StatusOK, plain},
	} {
		w = request(h, http.MethodGet, "/plain/", tc.headers)
		if w.Code != tc.status || w.Body.String() != tc.body {
			t.Fatalf("plain page %v: %d %q", tc.headers, w.Code, w.Body.String())
		}
		// The read for the inspection serves the response.
		if calls := origin.take(); strings.Join(calls, "|") != "HEAD plain/index.html|GET plain/index.html" {
			t.Fatalf("plain page %v: origin calls %q", tc.headers, calls)
		}
	}
}

// With precompressed variants, the canonical object's body decides, whichever
// variant the request accepts; a variant without its canonical object is
// never inspected.
func TestClientRedirectInspectsTheCanonicalObject(t *testing.T) {
	br := func(body, etag string) memObject {
		return memObject{body, storage.Metadata{ETag: etag, ContentType: "text/html", ContentEncoding: "br"}}
	}
	origin := clientOrigin(map[string]memObject{
		"old/index.html":      htmlPage(redirectPage("/new/"), `"old"`),
		"old/index.html.br":   br("brotli old", `"old-br"`),
		"plain/index.html":    htmlPage("<p>plain</p>", `"plain"`),
		"plain/index.html.br": br(redirectPage("/variant/"), `"plain-br"`),
		"only/index.html.br":  br(redirectPage("/variant/"), `"only-br"`),
	})
	c := clientConfig(t, config.ClientRedirects{})
	c.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br", "gzip"}}
	h := boundary(t, c, origin)
	accept := http.Header{"Accept-Encoding": {"br"}}
	for _, tc := range []struct {
		target, method string
		status         int
		location, body string
		calls          []string
	}{
		{"/old/", http.MethodGet, 301, "/new/", "", []string{"GET old/index.html", "GET old/index.html.br", "GET old/index.html"}},
		{"/old/", http.MethodHead, 301, "/new/", "", []string{"HEAD old/index.html", "HEAD old/index.html.br", "GET old/index.html"}},
		{"/plain/", http.MethodGet, 200, "", redirectPage("/variant/"), []string{"GET plain/index.html", "GET plain/index.html.br", "GET plain/index.html"}},
		{"/only/", http.MethodGet, 200, "", redirectPage("/variant/"), []string{"GET only/index.html", "GET only/index.html.br"}},
	} {
		origin.take()
		w := request(h, tc.method, tc.target, accept.Clone())
		if w.Code != tc.status || w.Header().Get("Location") != tc.location || w.Body.String() != tc.body && tc.method == http.MethodGet && tc.status == 200 {
			t.Fatalf("%s %s: %d Location %q body %q", tc.method, tc.target, w.Code, w.Header().Get("Location"), w.Body.String())
		}
		if calls := origin.take(); strings.Join(calls, "|") != strings.Join(tc.calls, "|") {
			t.Fatalf("%s %s: origin calls %q, want %q", tc.method, tc.target, calls, tc.calls)
		}
	}
}

func TestClientRedirectScope(t *testing.T) {
	website := redirectPage("/page/")
	origin := clientOrigin(map[string]memObject{
		"shell.html":         htmlPage(redirectPage("/elsewhere/"), `"shell"`),
		"404.html":           htmlPage(redirectPage("/elsewhere/"), `"404"`),
		"both/index.html":    {website, storage.Metadata{ETag: `"both"`, ContentType: "text/html", WebsiteRedirectLocation: "/website/"}},
		"refused/index.html": {website, storage.Metadata{ETag: `"refused"`, ContentType: "text/html", WebsiteRedirectLocation: "http://example.com/website/"}},
		"meta/index.html":    htmlPage(redirectPage("/meta/"), `"meta"`),
		"script/index.html":  htmlPage(`<script>location = "/scripted/"</script>`, `"script"`),
	})
	c := clientConfig(t, config.ClientRedirects{})
	c.SPA = &config.SPA{Fallback: "shell.html"}
	c.NotFound = "404.html"
	c.WebsiteRedirects = &config.WebsiteRedirects{Status: 302, Query: "drop"}
	var observed lastObservation
	h := boundary(t, c, origin, observed.option())
	if w := request(h, http.MethodGet, "/route/", http.Header{"Accept": {"text/html"}}); w.Code != 200 || w.Header().Get("Location") != "" {
		t.Fatalf("SPA shell redirected: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := request(h, http.MethodGet, "/missing/", nil); w.Code != 404 || w.Header().Get("Location") != "" {
		t.Fatalf("not_found object redirected: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := request(h, http.MethodGet, "/both/", nil); w.Code != 302 || w.Header().Get("Location") != "/website/" || observed.get().Redirect != RedirectWebsite {
		t.Fatalf("website redirect must win: %d %q %+v", w.Code, w.Header().Get("Location"), observed.get())
	}
	if w := request(h, http.MethodGet, "/refused/", nil); w.Code != 301 || w.Header().Get("Location") != "/page/" || observed.get().Redirect != RedirectMetaRefresh {
		t.Fatalf("a refused website redirect leaves the page: %d %q %+v", w.Code, w.Header().Get("Location"), observed.get())
	}
	scriptOnly := clientConfig(t, config.ClientRedirects{Detect: []string{redirect.DetectScript}})
	h = boundary(t, scriptOnly, origin)
	if w := request(h, http.MethodGet, "/meta/", nil); w.Code != 200 {
		t.Fatalf("meta refresh without its detector: %d", w.Code)
	}
	if w := request(h, http.MethodGet, "/script/", nil); w.Code != 301 || w.Header().Get("Location") != "/scripted/" {
		t.Fatalf("script detector: %d %q", w.Code, w.Header().Get("Location"))
	}
	h = boundary(t, testConfig(t), origin)
	if w := request(h, http.MethodGet, "/meta/", nil); w.Code != 200 || w.Header().Get("Location") != "" {
		t.Fatalf("redirect without client_redirects: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestClientRedirectDestinationPolicy(t *testing.T) {
	long := "/" + strings.Repeat("a", redirect.MaxTargetBytes-1)
	origin := clientOrigin(map[string]memObject{
		"same/index.html":    htmlPage(redirectPage("https://example.com/new/"), `"same"`),
		"other/index.html":   htmlPage(redirectPage("https://docs.example.com/new/"), `"other"`),
		"http/index.html":    htmlPage(redirectPage("http://example.com/new/"), `"http"`),
		"network/index.html": htmlPage(redirectPage("//docs.example.com/new/"), `"network"`),
		"space/index.html":   htmlPage(redirectPage("new page/"), `"space"`),
		"long/index.html":    htmlPage(redirectPage(long), `"long"`),
		"evil/index.html":    htmlPage(redirectPage("..//evil.example/x"), `"evil"`),
	})
	var observed lastObservation
	local := boundary(t, clientConfig(t, config.ClientRedirects{Status: 302, Query: "drop"}), origin, observed.option())
	external := boundary(t, clientConfig(t, config.ClientRedirects{Status: 307, MaxObjectBytes: 32 << 10, Query: "preserve", External: true}), origin, observed.option())
	for _, tc := range []struct {
		name     string
		h        http.Handler
		target   string
		status   int
		location string
	}{
		{"request host", local, "/same/?ref=feed", 302, "https://example.com/new/"},
		{"other host refused", local, "/other/", 200, ""},
		{"other host allowed", external, "/other/?ref=feed", 307, "https://docs.example.com/new/?ref=feed"},
		{"http refused", external, "/http/", 200, ""},
		{"network path refused", external, "/network/", 200, ""},
		{"space refused", external, "/space/", 200, ""},
		{"too long", external, "/long/?ref=feed", 414, ""},
		{"relative to a network path refused", external, "/evil/", 200, ""},
		{"refused falls back to the trailing slash", local, "/other", 308, "/other/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(tc.h, http.MethodGet, tc.target, nil)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("got %d Location %q, want %d %q", w.Code, w.Header().Get("Location"), tc.status, tc.location)
			}
			o := observed.get()
			sent := tc.status == 302 || tc.status == 307
			if o.Redirect != RedirectMetaRefresh || sent && (o.Location != tc.location || o.OriginKey == "") || !sent && o.Location != "" {
				t.Fatalf("observation %+v", o)
			}
			if tc.status == 200 && o.OriginKey == "" || tc.status >= 308 && o.OriginKey != "" {
				t.Fatalf("origin_key on a %d: %+v", tc.status, o)
			}
		})
	}
}

func TestClientRedirectSizeLimit(t *testing.T) {
	page := redirectPage("/new/")
	fit := page + strings.Repeat(" ", 1024-len(page))
	origin := clientOrigin(map[string]memObject{
		"fit/index.html":  htmlPage(fit, `"fit"`),
		"over/index.html": htmlPage(fit+" ", `"over"`),
	})
	h := boundary(t, clientConfig(t, config.ClientRedirects{MaxObjectBytes: 1 << 10}), origin)
	if w := request(h, http.MethodGet, "/fit/", nil); w.Code != 301 {
		t.Fatalf("a page of max_object_bytes: %d", w.Code)
	}
	if w := request(h, http.MethodGet, "/over/", nil); w.Code != 200 || w.Body.String() != fit+" " {
		t.Fatalf("a page over max_object_bytes: %d", w.Code)
	}
}

// The page read after a metadata-only selection must be the selected
// revision: another one restarts resolution once, and a second change ends in
// a 503.
func TestClientRedirectChangedPage(t *testing.T) {
	revision := func(n int) memObject {
		return htmlPage(redirectPage("/v"+strconv.Itoa(n)+"/"), `"v`+strconv.Itoa(n)+`"`)
	}
	t.Run("once", func(t *testing.T) {
		origin := clientOrigin(map[string]memObject{"old/index.html": revision(1)})
		var once sync.Once
		origin.setHook(func(_ context.Context, key string, opts storage.ReadOptions) error {
			once.Do(func() { origin.set("old/index.html", revision(2)) }) // after the first HEAD
			return nil
		})
		h := boundary(t, clientConfig(t, config.ClientRedirects{}), origin)
		if w := request(h, http.MethodHead, "/old/", nil); w.Code != 301 || w.Header().Get("Location") != "/v2/" {
			t.Fatalf("retry: %d %q", w.Code, w.Header().Get("Location"))
		}
		if calls := origin.take(); strings.Join(calls, "|") != "HEAD old/index.html|GET old/index.html|HEAD old/index.html|GET old/index.html" {
			t.Fatalf("calls %q", calls)
		}
	})
	t.Run("twice", func(t *testing.T) {
		origin := clientOrigin(map[string]memObject{"old/index.html": revision(0)})
		n := 0
		origin.setHook(func(_ context.Context, key string, opts storage.ReadOptions) error {
			n++
			origin.set("old/index.html", revision(n)) // every read sees a newer revision
			return nil
		})
		p := newPipeline(t, clientConfig(t, config.ClientRedirects{}), origin)
		w := p.do(httptest.NewRequest(http.MethodHead, "http://example.com/old/", nil))
		records := p.take()
		if w.Code != 503 || len(records) != 1 || records[0].ErrorClass != "origin_inconsistent" || records[0].Redirect != "" {
			t.Fatalf("status %d, records %+v", w.Code, records)
		}
	})
	t.Run("redirect source of an earlier revision", func(t *testing.T) {
		origin := clientOrigin(map[string]memObject{
			"old/index.html":    htmlPage(redirectPage("http://example.com/refused/"), `"v1"`),
			"old/index.html.br": {"brotli bytes", storage.Metadata{ETag: `"br"`, ContentType: "text/html", ContentEncoding: "br"}},
		})
		var once sync.Once
		origin.setHook(func(_ context.Context, key string, opts storage.ReadOptions) error {
			if opts.Range == "" {
				return nil
			}
			var changed bool
			once.Do(func() { origin.set("old/index.html", htmlPage("<p>plain page</p>", `"v2"`)); changed = true })
			if changed {
				return &storage.Error{Kind: storage.Precondition, Size: -1}
			}
			return nil
		})
		c := clientConfig(t, config.ClientRedirects{})
		c.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br"}}
		var observed lastObservation
		h := boundary(t, c, origin, observed.option())
		if w := request(h, http.MethodGet, "/old/", http.Header{"Range": {"bytes=0-6"}, "Accept-Encoding": {"br"}}); w.Code != 206 || w.Body.String() != "brotli " {
			t.Fatalf("retry: %d %q", w.Code, w.Body.String())
		}
		if o := observed.get(); o.Redirect != "" || o.OriginKey != "old/index.html.br" {
			t.Fatalf("observation %+v", o)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		origin := clientOrigin(map[string]memObject{"old/index.html": revision(1)})
		calls := 0
		origin.setHook(func(_ context.Context, key string, opts storage.ReadOptions) error {
			// The HEAD selects the page, which is gone when it is read.
			if calls++; calls == 2 {
				origin.mu.Lock()
				delete(origin.objects, key)
				origin.mu.Unlock()
				return &storage.Error{Kind: storage.NotFound, Size: -1}
			}
			return nil
		})
		h := boundary(t, clientConfig(t, config.ClientRedirects{}), origin)
		if w := request(h, http.MethodHead, "/old/", nil); w.Code != 404 {
			t.Fatalf("retry: %d", w.Code)
		}
		if calls := origin.take(); strings.Join(calls, "|") != "HEAD old/index.html|GET old/index.html|HEAD old/index.html|GET old/index.html" {
			t.Fatalf("calls %q", calls)
		}
	})
}

type failingRead struct{ err error }

func (f failingRead) Read([]byte) (int, error) { return 0, f.err }
func (failingRead) Close() error               { return nil }

// A page that cannot be read is an origin failure, never a page served
// without its inspection.
func TestClientRedirectReadFailure(t *testing.T) {
	unavailable := &storage.Error{Kind: storage.Unavailable, Size: -1}
	meta := storage.Metadata{Size: 64, ETag: `"p"`, ContentType: "text/html"}
	for _, tc := range []struct {
		name   string
		method string
		get    func(context.Context, string, storage.ReadOptions) (*storage.Object, error)
		status int
		class  string
	}{
		{"body of the selection", http.MethodGet, func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return &storage.Object{Metadata: meta, Body: failingRead{unavailable}}, nil
		}, 502, "origin_unavailable"},
		{"short body", http.MethodGet, func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return object("<p>", meta), nil
		}, 502, "origin_invalid_response"},
		{"read for a HEAD", http.MethodHead, func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return nil, unavailable
		}, 502, "origin_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := fakeReader{get: tc.get, head: func(context.Context, string) (storage.Metadata, error) { return meta, nil }}
			p := newPipeline(t, clientConfig(t, config.ClientRedirects{}), reader)
			w := p.do(httptest.NewRequest(tc.method, "http://example.com/old/", nil))
			records := p.take()
			if w.Code != tc.status || len(records) != 1 || records[0].ErrorClass != tc.class || records[0].OriginKey != "" {
				t.Fatalf("status %d, records %+v", w.Code, records)
			}
		})
	}
}

// A HEAD reads an eligible page through the memory cache, and a new
// destination, uploaded as a new page, reaches clients when the entry expires.
func TestClientRedirectThroughMemoryCache(t *testing.T) {
	origin := clientOrigin(map[string]memObject{"old/index.html": htmlPage(redirectPage("/a/"), `"a"`)})
	cfg := clientConfig(t, config.ClientRedirects{})
	clock := &testClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	memory, _ := newCache(t, cfg, origin, clock)
	h := boundary(t, cfg, origin, WithCache(memory))
	for i, tc := range []struct {
		method, location string
		calls            []string
	}{
		{http.MethodHead, "/a/", []string{"HEAD old/index.html", "GET old/index.html"}},
		{http.MethodHead, "/a/", nil},
		{http.MethodGet, "/a/", nil},
	} {
		if w := request(h, tc.method, "/old/", nil); w.Code != 301 || w.Header().Get("Location") != tc.location {
			t.Fatalf("%d: %d %q", i, w.Code, w.Header().Get("Location"))
		}
		if calls := origin.take(); strings.Join(calls, "|") != strings.Join(tc.calls, "|") {
			t.Fatalf("%d: origin calls %q, want %q", i, calls, tc.calls)
		}
	}
	origin.set("old/index.html", htmlPage(redirectPage("/b/"), `"b"`))
	if w := request(h, http.MethodGet, "/old/", nil); w.Header().Get("Location") != "/a/" {
		t.Fatalf("fresh entry: %d %q", w.Code, w.Header().Get("Location"))
	}
	clock.Advance(cfg.Cache.TTL + time.Second)
	if w := request(h, http.MethodGet, "/old/", nil); w.Code != 301 || w.Header().Get("Location") != "/b/" {
		t.Fatalf("after expiry: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := request(h, http.MethodHead, "/old", nil); w.Code != 301 || w.Header().Get("Location") != "/b/" {
		t.Fatalf("HEAD without trailing slash: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestClientRedirectAccessEvent(t *testing.T) {
	origin := clientOrigin(map[string]memObject{
		"old/index.html":     htmlPage(redirectPage("/new/?token=SECRET"), `"old"`),
		"refused/index.html": htmlPage(redirectPage("http://example.com/new/"), `"refused"`),
	})
	p := newPipeline(t, clientConfig(t, config.ClientRedirects{}), origin)
	p.do(httptest.NewRequest(http.MethodHead, "http://example.com/old", nil))
	p.do(httptest.NewRequest(http.MethodGet, "http://example.com/refused/", nil))
	events := logEvents(t, p.log.String())
	if len(events) != 2 {
		t.Fatalf("events %v", events)
	}
	if e := events[0]; e["status"] != "301" || e["resolution"] != "index" || e["origin_key"] != "old/index.html" || e["redirect"] != "meta_refresh" || e["location"] != "/new/?token=[REDACTED]" {
		t.Fatalf("redirect event %v", e)
	}
	if e := events[1]; e["status"] != "200" || e["origin_key"] != "refused/index.html" || e["redirect"] != "meta_refresh" || e["location"] != "" {
		t.Fatalf("refused redirect event %v", e)
	}
}

// A destination that leads back to the page would loop: the page is served
// instead, or the 308 that adds the slash.
func TestClientRedirectRefusesSelfRedirects(t *testing.T) {
	origin := clientOrigin(map[string]memObject{
		"loop/index.html":  htmlPage(redirectPage("./#top"), `"loop"`),
		"same/index.html":  htmlPage(redirectPage("/same/"), `"same"`),
		"query/index.html": htmlPage(redirectPage("?lang=en"), `"query"`),
	})
	var observed lastObservation
	h := boundary(t, clientConfig(t, config.ClientRedirects{}), origin, observed.option())
	for _, tc := range []struct {
		target, location string
		status           int
	}{
		{"/loop/", "", 200},
		{"/loop/?ref=feed", "", 200},
		{"/same/?ref=feed", "", 200},
		{"/same", "/same/", 308},
		{"/query/?lang=en", "", 200},
		{"/query/", "/query/?lang=en", 301},
	} {
		w := request(h, http.MethodGet, tc.target, nil)
		if w.Code != tc.status || w.Header().Get("Location") != tc.location {
			t.Fatalf("%s: %d Location %q, want %d %q", tc.target, w.Code, w.Header().Get("Location"), tc.status, tc.location)
		}
		if o := observed.get(); o.Redirect != RedirectMetaRefresh || o.Location != tc.location && tc.status != 308 {
			t.Fatalf("%s: observation %+v", tc.target, o)
		}
	}
}

func TestClientRedirectPageShapes(t *testing.T) {
	origin := clientOrigin(map[string]memObject{
		"zola/index.html":     htmlPage(`<noscript><meta http-equiv="refresh" content="0; url=../new/"></noscript><script>const target = "../new/"; const hash = window.location.hash || ""; window.location.replace(target + hash);</script>`, `"zola"`),
		"svg/index.html":      htmlPage(redirectPage("/new/")+`<svg><title>Moved</title></svg>`, `"svg"`),
		"legacy/index.html":   htmlPage(`<script type="text/jscript">"use strict"; window.location.replace("/new/")</script>`, `"legacy"`),
		"spa/index.html":      htmlPage(`<noscript><meta http-equiv="refresh" content="0; url=/no-javascript/"></noscript><script type="module" src="/app.js"></script>`, `"spa"`),
		"language/index.html": htmlPage(`<meta http-equiv="refresh" content="0; url=/en/"><script>location.replace(navigator.language.startsWith('it') ? '/it/' : '/en/')</script>`, `"language"`),
		"template/index.html": htmlPage(`<template><meta http-equiv="refresh" content="0; url=/new/"></template>`, `"template"`),
		"café/old/index.html": htmlPage(redirectPage("../new/"), `"cafe"`),
	})
	var observed lastObservation
	h := boundary(t, clientConfig(t, config.ClientRedirects{}), origin, observed.option())
	for _, tc := range []struct {
		target, location, redirect string
		status                     int
	}{
		{"/zola/", "", "", 200},
		{"/svg/", "", "", 200},
		{"/legacy/", "/new/", RedirectScript, 301},
		{"/spa/", "", "", 200},
		{"/language/", "", "", 200},
		{"/template/", "", "", 200},
		{"/caf%C3%A9/old/", "/caf%C3%A9/new/", RedirectMetaRefresh, 301},
	} {
		w := request(h, http.MethodGet, tc.target, nil)
		if w.Code != tc.status || w.Header().Get("Location") != tc.location || observed.get().Redirect != tc.redirect {
			t.Fatalf("%s: %d Location %q %+v", tc.target, w.Code, w.Header().Get("Location"), observed.get())
		}
	}
}

// A GET whose response comes from the page read through the memory cache
// reports that read; a HEAD keeps its metadata lookup.
func TestClientRedirectCacheOutcome(t *testing.T) {
	plain := "<p>plain page</p>"
	origin := clientOrigin(map[string]memObject{
		"plain/index.html": htmlPage(plain, `"plain"`),
		"old/index.html":   htmlPage(redirectPage("/new/"), `"old"`),
	})
	cfg := clientConfig(t, config.ClientRedirects{})
	clock := &testClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	memory, _ := newCache(t, cfg, origin, clock)
	var observed lastObservation
	h := boundary(t, cfg, origin, WithCache(memory), observed.option())
	for i, tc := range []struct {
		method, target string
		headers        http.Header
		status         int
		outcome        cache.Outcome
		memoryBytes    int64
	}{
		{http.MethodHead, "/plain/", nil, 200, cache.OutcomeMiss, 0},
		{http.MethodHead, "/plain/", nil, 200, cache.OutcomeMetadataHit, 0},
		{http.MethodGet, "/plain/", http.Header{"Range": {"bytes=0-2"}}, 206, cache.OutcomeHit, 3},
		{http.MethodGet, "/old/", http.Header{"Range": {"bytes=0-2"}}, 301, cache.OutcomeMiss, 0},
		{http.MethodGet, "/old/", http.Header{"Range": {"bytes=0-2"}}, 301, cache.OutcomeHit, 0},
		{http.MethodHead, "/old/", nil, 301, cache.OutcomeMetadataHit, 0},
	} {
		w := request(h, tc.method, tc.target, tc.headers)
		if o := observed.get(); w.Code != tc.status || o.CacheOutcome != tc.outcome || o.CacheBytes != tc.memoryBytes {
			t.Fatalf("%d %s %s: %d %+v", i, tc.method, tc.target, w.Code, o)
		}
	}
	if calls := origin.take(); strings.Join(calls, "|") != "HEAD plain/index.html|GET plain/index.html|HEAD old/index.html|GET old/index.html" {
		t.Fatalf("origin calls %q", calls)
	}
}

// Range requests on eligible pages: through the 308 that adds the slash, and
// with provider ranges off, the page read for the check answers the range.
func TestClientRedirectRanges(t *testing.T) {
	plain := "<p>plain page</p>"
	origin := clientOrigin(map[string]memObject{
		"plain/index.html": htmlPage(plain, `"plain"`),
		"old/index.html":   htmlPage(redirectPage("/new/"), `"old"`),
	})
	ranged := http.Header{"Range": {"bytes=0-2"}}
	h := boundary(t, clientConfig(t, config.ClientRedirects{}), origin)
	for _, tc := range []struct {
		target, location string
		status           int
		calls            []string
	}{
		{"/plain", "/plain/", 308, []string{"HEAD plain", "GET plain", "HEAD plain/index.html", "GET plain/index.html"}},
		{"/old", "/new/", 301, []string{"HEAD old", "GET old", "HEAD old/index.html", "GET old/index.html"}},
	} {
		origin.take()
		w := request(h, http.MethodGet, tc.target, ranged.Clone())
		if w.Code != tc.status || w.Header().Get("Location") != tc.location || w.Header().Get("Content-Range") != "" {
			t.Fatalf("%s: %d Location %q %v", tc.target, w.Code, w.Header().Get("Location"), w.Header())
		}
		if calls := origin.take(); strings.Join(calls, "|") != strings.Join(tc.calls, "|") {
			t.Fatalf("%s: origin calls %q, want %q", tc.target, calls, tc.calls)
		}
	}
	c := clientConfig(t, config.ClientRedirects{})
	c.S3.Features.RangeReads = false
	h = boundary(t, c, origin)
	origin.take()
	w := request(h, http.MethodGet, "/plain/", ranged.Clone())
	if w.Code != http.StatusPartialContent || w.Body.String() != plain[:3] || w.Header().Get("Accept-Ranges") != "" {
		t.Fatalf("ranges off: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if calls := origin.take(); strings.Join(calls, "|") != "GET plain/index.html" {
		t.Fatalf("ranges off: origin calls %q", calls)
	}
	// Without client_redirects, the same request streams the whole page.
	c = testConfig(t)
	c.S3.Features.RangeReads = false
	if w := request(boundary(t, c, origin), http.MethodGet, "/plain/", ranged.Clone()); w.Code != http.StatusOK || w.Body.String() != plain {
		t.Fatalf("ranges off without client_redirects: %d %q", w.Code, w.Body.String())
	}
}
