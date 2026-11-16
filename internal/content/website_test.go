package content

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/redirect"
	"github.com/gtriggiano/statica/internal/site"
	"github.com/gtriggiano/statica/internal/storage"
)

// websiteOrigin serves the objects of a map; every other key is missing. It
// records the keys it reads with a body and with HEAD.
type websiteOrigin struct {
	mu      sync.Mutex
	objects map[string]storage.Metadata
	gets    []string
	heads   []string
}

func (o *websiteOrigin) reader() fakeReader {
	return fakeReader{
		get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.gets = append(o.gets, key)
			m, ok := o.objects[key]
			if !ok {
				return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
			}
			return object(strings.Repeat("x", int(m.Size)), m), nil
		},
		head: func(_ context.Context, key string) (storage.Metadata, error) {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.heads = append(o.heads, key)
			m, ok := o.objects[key]
			if !ok {
				return storage.Metadata{}, &storage.Error{Kind: storage.AmbiguousNotFound, Size: -1}
			}
			return m, nil
		},
	}
}

func redirectObject(destination string) storage.Metadata {
	return storage.Metadata{Size: 0, ETag: `"d41d8cd98f00b204e9800998ecf8427e"`, ContentType: "text/html", WebsiteRedirectLocation: destination}
}

func websiteConfig(t *testing.T, w config.WebsiteRedirects) *config.Config {
	t.Helper()
	c := testConfig(t)
	c.WebsiteRedirects = &w
	return c
}

func TestWebsiteRedirectAnswersBeforeConditionsAndRanges(t *testing.T) {
	origin := &websiteOrigin{objects: map[string]storage.Metadata{
		"old.html":       redirectObject("/new/"),
		"old/index.html": redirectObject("/docs/#install"),
		"page.html":      {Size: 4, ETag: `"page"`, ContentType: "text/html"},
	}}
	var mu sync.Mutex
	var observed []Observation
	h := boundary(t, websiteConfig(t, config.WebsiteRedirects{Status: 301, Query: "preserve"}), origin.reader(), WithObserver(func(o Observation) {
		mu.Lock()
		observed = append(observed, o)
		mu.Unlock()
	}))
	for _, tc := range []struct {
		name, method, target string
		headers              http.Header
		location             string
	}{
		{"ordinary", http.MethodGet, "/old.html?ref=feed", nil, "/new/?ref=feed"},
		{"index", http.MethodGet, "/old/", nil, "/docs/#install"},
		{"index query before fragment", http.MethodGet, "/old/?ref=feed", nil, "/docs/?ref=feed#install"},
		{"index without trailing slash", http.MethodGet, "/old", nil, "/docs/#install"},
		{"head", http.MethodHead, "/old.html", nil, "/new/"},
		{"if-none-match", http.MethodGet, "/old.html", http.Header{"If-None-Match": {`"d41d8cd98f00b204e9800998ecf8427e"`}}, "/new/"},
		{"if-match failing", http.MethodGet, "/old.html", http.Header{"If-Match": {`"other"`}}, "/new/"},
		{"range", http.MethodGet, "/old.html", http.Header{"Range": {"bytes=0-0"}}, "/new/"},
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
	mu.Lock()
	last := observed[len(observed)-1]
	mu.Unlock()
	if last.Redirect != RedirectWebsite || last.Location != "/new/" || last.OriginKey != "old.html" || last.Resolution != "ordinary" || last.Status != http.StatusMovedPermanently {
		t.Fatalf("observation %+v", last)
	}
	origin.mu.Lock()
	gets := len(origin.gets)
	origin.mu.Unlock()
	if w := request(h, http.MethodHead, "/old/", nil); w.Code != http.StatusMovedPermanently {
		t.Fatalf("HEAD index %d", w.Code)
	}
	origin.mu.Lock()
	defer origin.mu.Unlock()
	if len(origin.gets) != gets {
		t.Fatalf("a HEAD read a body: %v", origin.gets[gets:])
	}
}

func TestWebsiteRedirectDestinationPolicy(t *testing.T) {
	origin := &websiteOrigin{objects: map[string]storage.Metadata{
		"same.html":      redirectObject("https://example.com/new/"),
		"other.html":     redirectObject("https://docs.example.com/new/"),
		"http.html":      redirectObject("http://example.com/new/"),
		"relative.html":  redirectObject("new/"),
		"long.html":      redirectObject("/" + strings.Repeat("a", redirect.MaxTargetBytes-1)),
		"dir/index.html": redirectObject("http://example.com/new/"),
	}}
	var mu sync.Mutex
	var observed Observation
	observe := WithObserver(func(o Observation) { mu.Lock(); observed = o; mu.Unlock() })
	local := boundary(t, websiteConfig(t, config.WebsiteRedirects{Status: 302, Query: "drop"}), origin.reader(), observe)
	external := boundary(t, websiteConfig(t, config.WebsiteRedirects{Status: 307, Query: "preserve", External: true}), origin.reader(), observe)
	for _, tc := range []struct {
		name     string
		h        http.Handler
		target   string
		status   int
		location string
	}{
		{"request host", local, "/same.html?ref=feed", 302, "https://example.com/new/"},
		{"other host refused", local, "/other.html", 200, ""},
		{"other host allowed", external, "/other.html?ref=feed", 307, "https://docs.example.com/new/?ref=feed"},
		{"http refused", external, "/http.html", 200, ""},
		{"relative refused", external, "/relative.html", 200, ""},
		{"too long", external, "/long.html?ref=feed", 414, ""},
		{"request host with port", local, "http://example.com:8080/same.html", 302, "https://example.com/new/"},
		{"refused falls back to the trailing slash", local, "/dir", 308, "/dir/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(tc.h, http.MethodGet, tc.target, nil)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("got %d Location %q, want %d %q", w.Code, w.Header().Get("Location"), tc.status, tc.location)
			}
			mu.Lock()
			defer mu.Unlock()
			sent := tc.status == 301 || tc.status == 302 || tc.status == 307
			if observed.Redirect != RedirectWebsite || sent && (observed.Location != tc.location || observed.OriginKey == "") || !sent && observed.Location != "" {
				t.Fatalf("observation %+v", observed)
			}
			if tc.status == 200 && observed.OriginKey == "" {
				t.Fatal("a refused destination must serve the object")
			}
			if tc.status >= 308 && observed.OriginKey != "" {
				t.Fatalf("origin_key on a %d: %+v", tc.status, observed)
			}
		})
	}
	other := request(local, http.MethodGet, "http://docs.example.com/other.html", nil)
	if other.Code != 302 || other.Header().Get("Location") != "https://docs.example.com/new/" {
		t.Fatalf("destination on the request's host: %d %q", other.Code, other.Header().Get("Location"))
	}
}

// A guarded retry selects again: the redirect source of an earlier revision
// must not describe the object finally served.
func TestWebsiteRedirectObservationFollowsTheSelection(t *testing.T) {
	h := &handler{website: &redirect.ObjectPolicy{Query: "drop"}, websiteStatus: 301}
	o := Observation{Redirect: RedirectWebsite}
	plain := &site.Selection{Key: "page.html", Status: http.StatusOK, Resolution: site.ResolutionOrdinary}
	if _, _, ok := h.websiteRedirect(plain, httpserver.RequestMetadata{Host: "example.com"}, &o); ok || o.Redirect != "" {
		t.Fatalf("got %v, observation %+v", ok, o)
	}
}

func TestWebsiteRedirectScope(t *testing.T) {
	origin := &websiteOrigin{objects: map[string]storage.Metadata{
		"old.html":      redirectObject("/new/"),
		"old.html.br":   {Size: 2, ETag: `"br"`, ContentType: "text/html", ContentEncoding: "br"},
		"shell.html":    redirectObject("/elsewhere/"),
		"404.html":      redirectObject("/elsewhere/"),
		"plain.html":    {Size: 3, ETag: `"plain"`, ContentType: "text/html"},
		"plain.html.br": {Size: 2, ETag: `"plain-br"`, ContentType: "text/html", ContentEncoding: "br", WebsiteRedirectLocation: "/variant/"},
	}}
	c := websiteConfig(t, config.WebsiteRedirects{Status: 301, Query: "preserve"})
	c.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br", "gzip"}}
	c.SPA = &config.SPA{Fallback: "shell.html"}
	c.NotFound = "404.html"
	h := boundary(t, c, origin.reader())
	br := http.Header{"Accept-Encoding": {"br"}}
	if w := request(h, http.MethodGet, "/old.html", br); w.Code != 301 || w.Header().Get("Location") != "/new/" {
		t.Fatalf("canonical metadata must decide for a variant: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := request(h, http.MethodGet, "/plain.html", br); w.Code != 200 || w.Header().Get("Location") != "" {
		t.Fatalf("a variant's own metadata must not decide: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := request(h, http.MethodGet, "/route", http.Header{"Accept": {"text/html"}}); w.Code != 200 || w.Header().Get("Location") != "" {
		t.Fatalf("SPA shell redirected: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := request(h, http.MethodGet, "/missing.txt", nil); w.Code != 404 || w.Header().Get("Location") != "" {
		t.Fatalf("not_found object redirected: %d %q", w.Code, w.Header().Get("Location"))
	}
	off := boundary(t, testConfig(t), origin.reader())
	if w := request(off, http.MethodGet, "/old.html", nil); w.Code != 200 || w.Header().Get("Location") != "" {
		t.Fatalf("redirect without website_redirects: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// A redirect object is usually empty, and every empty object has the same
// ETag: a changed destination must reach clients once the entry expires,
// even from an origin that answers a matching If-None-Match with 304.
func TestWebsiteRedirectThroughMemoryCache(t *testing.T) {
	origin := &websiteOrigin{objects: map[string]storage.Metadata{"old.html": redirectObject("/a/")}}
	reader := origin.reader()
	get := reader.get
	reader.get = func(ctx context.Context, key string, o storage.ReadOptions) (*storage.Object, error) {
		origin.mu.Lock()
		m := origin.objects[key]
		origin.mu.Unlock()
		if o.IfNoneMatch != "" && o.IfNoneMatch == m.ETag {
			return nil, &storage.Error{Kind: storage.NotModified, Size: -1, Metadata: &storage.Metadata{Size: -1, ETag: m.ETag}}
		}
		return get(ctx, key, o)
	}
	cfg := websiteConfig(t, config.WebsiteRedirects{Status: 301, Query: "drop"})
	clock := &testClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	memory, _ := newCache(t, cfg, reader, clock)
	h := boundary(t, cfg, reader, WithCache(memory))
	if w := request(h, http.MethodGet, "/old.html", nil); w.Code != 301 || w.Header().Get("Location") != "/a/" {
		t.Fatalf("first: %d %q", w.Code, w.Header().Get("Location"))
	}
	origin.mu.Lock()
	origin.objects["old.html"] = redirectObject("/b/")
	reads := len(origin.gets)
	origin.mu.Unlock()
	if w := request(h, http.MethodHead, "/old.html", nil); w.Code != 301 || w.Header().Get("Location") != "/a/" {
		t.Fatalf("fresh entry: %d %q", w.Code, w.Header().Get("Location"))
	}
	origin.mu.Lock()
	if len(origin.gets) != reads || len(origin.heads) != 0 {
		t.Fatalf("a fresh entry read the origin: %v %v", origin.gets[reads:], origin.heads)
	}
	origin.mu.Unlock()
	clock.Advance(cfg.Cache.TTL + time.Second)
	if w := request(h, http.MethodGet, "/old.html", nil); w.Code != 301 || w.Header().Get("Location") != "/b/" {
		t.Fatalf("after expiry: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// An empty object that gains the metadata keeps its ETag: the redirect must
// take effect once the entry expires, although the origin would answer 304.
func TestWebsiteRedirectAddedToAnEmptyObject(t *testing.T) {
	empty := redirectObject("")
	origin := &websiteOrigin{objects: map[string]storage.Metadata{"old/index.html": empty}}
	reader := origin.reader()
	get := reader.get
	reader.get = func(ctx context.Context, key string, o storage.ReadOptions) (*storage.Object, error) {
		origin.mu.Lock()
		m := origin.objects[key]
		origin.mu.Unlock()
		if o.IfNoneMatch != "" && o.IfNoneMatch == m.ETag {
			return nil, &storage.Error{Kind: storage.NotModified, Size: -1, Metadata: &storage.Metadata{Size: -1, ETag: m.ETag}}
		}
		return get(ctx, key, o)
	}
	cfg := websiteConfig(t, config.WebsiteRedirects{Status: 301, Query: "drop"})
	clock := &testClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
	memory, _ := newCache(t, cfg, reader, clock)
	h := boundary(t, cfg, reader, WithCache(memory))
	if w := request(h, http.MethodGet, "/old/", nil); w.Code != 200 {
		t.Fatalf("first: %d", w.Code)
	}
	origin.mu.Lock()
	origin.objects["old/index.html"] = redirectObject("/new/")
	origin.mu.Unlock()
	clock.Advance(cfg.Cache.TTL + time.Second)
	if w := request(h, http.MethodGet, "/old/", nil); w.Code != 301 || w.Header().Get("Location") != "/new/" {
		t.Fatalf("after expiry: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// A HEAD or a range request after expiry revalidates with HeadObject: a
// redirect changed, added or removed under the same ETag must take effect.
func TestWebsiteRedirectRevalidatedByHeadAndRange(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after string
		status        int
		location      string
	}{
		{"changed", "/a/", "/b/", 301, "/b/"},
		{"added", "", "/b/", 301, "/b/"},
		{"removed", "/a/", "", 200, ""},
	} {
		for _, probe := range []struct {
			name, method string
			header       http.Header
		}{{"head", http.MethodHead, nil}, {"range", http.MethodGet, http.Header{"Range": {"bytes=0-0"}}}} {
			t.Run(tc.name+" "+probe.name, func(t *testing.T) {
				origin := &websiteOrigin{objects: map[string]storage.Metadata{"old.html": redirectObject(tc.before)}}
				cfg := websiteConfig(t, config.WebsiteRedirects{Status: 301, Query: "drop"})
				clock := &testClock{t: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)}
				memory, _ := newCache(t, cfg, origin.reader(), clock)
				h := boundary(t, cfg, origin.reader(), WithCache(memory))
				request(h, http.MethodGet, "/old.html", nil)
				origin.mu.Lock()
				origin.objects["old.html"] = redirectObject(tc.after)
				origin.mu.Unlock()
				clock.Advance(cfg.Cache.TTL + time.Second)
				if w := request(h, probe.method, "/old.html", probe.header); w.Header().Get("Location") != tc.location {
					t.Fatalf("revalidating request: %d %q", w.Code, w.Header().Get("Location"))
				}
				if w := request(h, http.MethodGet, "/old.html", nil); w.Code != tc.status || w.Header().Get("Location") != tc.location {
					t.Fatalf("next GET: %d %q, want %d %q", w.Code, w.Header().Get("Location"), tc.status, tc.location)
				}
			})
		}
	}
}
