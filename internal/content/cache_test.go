package content

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type memObject struct {
	body string
	meta storage.Metadata // Size is derived from body
}

// memOrigin is an S3-like origin over a map. A missing key is a confirmed
// NotFound for GET and an AmbiguousNotFound for HEAD, as an S3 HEAD 404 has
// no error code. Ranges, If-Match and If-None-Match behave like S3; like the
// S3 adapter, it refuses them as Unsupported when noRanges or noConditionals
// disables the provider capability. hook, when set, runs first and fails the
// read with a non-nil error.
type memOrigin struct {
	mu                       sync.Mutex
	objects                  map[string]memObject
	denied                   map[string]bool
	hook                     func(ctx context.Context, key string, opts storage.ReadOptions) error
	calls                    []string
	noRanges, noConditionals bool
}

func (o *memOrigin) set(key string, obj memObject) {
	o.mu.Lock()
	o.objects[key] = obj
	o.mu.Unlock()
}

func (o *memOrigin) deny(key string) {
	o.mu.Lock()
	o.denied[key] = true
	o.mu.Unlock()
}

func (o *memOrigin) setHook(hook func(context.Context, string, storage.ReadOptions) error) {
	o.mu.Lock()
	o.hook = hook
	o.mu.Unlock()
}

// take returns the calls recorded since the previous take.
func (o *memOrigin) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	calls := o.calls
	o.calls = nil
	return calls
}

func (o *memOrigin) lookup(call, key string) (memObject, bool, bool, func(context.Context, string, storage.ReadOptions) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls = append(o.calls, call)
	obj, ok := o.objects[key]
	return obj, ok, o.denied[key], o.hook
}

func (o *memOrigin) Get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	call := "GET " + key
	for _, option := range []struct{ name, value string }{{"range", opts.Range}, {"if-match", opts.IfMatch}, {"if-none-match", opts.IfNoneMatch}} {
		if option.value != "" {
			call += " " + option.name + "=" + option.value
		}
	}
	obj, ok, denied, hook := o.lookup(call, key)
	if hook != nil {
		if err := hook(ctx, key, opts); err != nil {
			return nil, err
		}
	}
	switch {
	case o.noRanges && opts.Range != "" || o.noConditionals && (opts.IfMatch != "" || opts.IfNoneMatch != ""):
		return nil, &storage.Error{Kind: storage.Unsupported, Size: -1}
	case denied:
		return nil, &storage.Error{Kind: storage.Denied, Size: -1}
	case !ok:
		return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
	}
	m, body := obj.meta, obj.body
	m.Size = int64(len(body))
	if opts.IfNoneMatch != "" && opts.IfNoneMatch == m.ETag {
		fields := storage.Metadata{Size: -1, ETag: m.ETag, CacheControl: m.CacheControl, Expires: m.Expires, Date: m.Date, RawDate: m.RawDate, Age: m.Age, RawAge: m.RawAge, LastModified: m.LastModified}
		return nil, &storage.Error{Kind: storage.NotModified, Size: -1, Metadata: &fields}
	}
	if opts.IfMatch != "" && opts.IfMatch != m.ETag {
		return nil, &storage.Error{Kind: storage.Precondition, Size: -1}
	}
	if opts.Range != "" {
		br, valid, satisfiable := parseRange(opts.Range, m.Size)
		if valid && !satisfiable {
			return nil, &storage.Error{Kind: storage.InvalidRange, Size: m.Size}
		}
		if valid {
			body, m.Size, m.ContentRange = body[br.start:br.end+1], br.length(), br.contentRange()
		}
	}
	return &storage.Object{Metadata: m, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (o *memOrigin) Head(ctx context.Context, key string) (storage.Metadata, error) {
	obj, ok, denied, hook := o.lookup("HEAD "+key, key)
	if hook != nil {
		if err := hook(ctx, key, storage.ReadOptions{}); err != nil {
			return storage.Metadata{}, err
		}
	}
	switch {
	case denied:
		return storage.Metadata{}, &storage.Error{Kind: storage.Denied, Size: -1}
	case !ok:
		return storage.Metadata{}, &storage.Error{Kind: storage.AmbiguousNotFound, Size: -1}
	}
	m := obj.meta
	m.Size = int64(len(obj.body))
	return m, nil
}

var fixtureModified = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func fixtureMeta(etag, cacheControl string, age int) storage.Metadata {
	m := storage.Metadata{ETag: etag, LastModified: fixtureModified, CacheControl: cacheControl}
	if age > 0 {
		m.Age, m.RawAge = time.Duration(age)*time.Second, strconv.Itoa(age)
	}
	return m
}

func newMemOrigin() *memOrigin {
	br, gz := fixtureMeta(`"app-br"`, "", 0), fixtureMeta(`"app-gz"`, "", 0)
	br.ContentEncoding, gz.ContentEncoding = "br", "gzip"
	shell := fixtureMeta(`"shell"`, "public, max-age=60", 0)
	shell.ContentType = "text/html; charset=utf-8"
	now := time.Now().Truncate(time.Second)
	// The origin clock runs an hour ahead: Date adds no apparent age, and it
	// is late enough after Last-Modified to make that a strong validator.
	ahead := fixtureMeta(`"ahead"`, "", 2)
	ahead.Date, ahead.RawDate = now.Add(time.Hour), now.Add(time.Hour).Format(http.TimeFormat)
	behind := fixtureMeta(`"behind"`, "", 0)
	behind.Date, behind.RawDate = now.Add(-30*time.Second), now.Add(-30*time.Second).Format(http.TimeFormat)
	expires := fixtureMeta(`"exp"`, "", 0)
	expires.Expires = now.Add(time.Hour).Format(http.TimeFormat)
	override := fixtureMeta(`"ovr"`, "public, max-age=600", 6)
	override.Expires = now.Add(time.Hour).Format(http.TimeFormat)
	return &memOrigin{denied: map[string]bool{"denied.txt": true}, objects: map[string]memObject{
		"ahead.txt":       {"origin clock ahead", ahead},
		"behind.txt":      {"origin clock behind", behind},
		"expires.txt":     {"origin expires", expires},
		"override.txt":    {"policy override", override},
		"empty.txt":       {"", fixtureMeta(`"empty"`, "", 0)},
		"file.txt":        {"hello world", fixtureMeta(`"f1"`, "", 5)},
		"cc.txt":          {"origin policy", fixtureMeta(`"c1"`, "public, max-age=60", 3)},
		"weak.txt":        {"weak validator", fixtureMeta(`W/"w1"`, "", 0)},
		"docs/index.html": {"<p>docs</p>", fixtureMeta(`"d1"`, "", 0)},
		"index.html":      {"<p>shell</p>", shell},
		"404.html":        {"<p>custom 404</p>", fixtureMeta(`"nf"`, "public, max-age=60", 0)},
		"app.js":          {"identity()", fixtureMeta(`"app"`, "", 2)},
		"app.js.br":       {"brotli", br},
		"app.js.gz":       {"gzip", gz},
		"style.css":       {"body{}", fixtureMeta(`"css"`, "", 0)},
		"nocache.txt":     {"never cached", fixtureMeta(`"nc"`, "", 4)},
		"assets/logo.svg": {"<svg/>", fixtureMeta(`"logo"`, "", 0)},
		"big.bin":         {strings.Repeat("0123456789abcdef", 256), fixtureMeta(`"big"`, "", 1)},
	}}
}

// cachedConfig enables precompressed negotiation and caps memory objects at
// 1 KiB so big.bin streams. Negative entries outlive the warm request.
func cachedConfig(t *testing.T, mutate func(*config.Config)) *config.Config {
	t.Helper()
	c := testConfig(t)
	c.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br", "gzip"}}
	c.Cache.MaxObjectBytes = 1 << 10
	c.Cache.NegativeTTL = time.Minute
	disabled := false
	assetTTL := time.Hour
	c.Cache.Rules = []config.CacheRule{{Match: config.Match{Exact: "/nocache.txt"}, Enabled: &disabled}, {Match: config.Match{Prefix: "/assets/"}, TTL: &assetTTL}}
	c.CacheControl.Rules = []config.CacheControlRule{{Match: config.Match{Exact: "/override.txt"}, Value: "private, max-age=10"}}
	if mutate != nil {
		mutate(c)
	}
	return c
}

// newCache builds a Cache over origin on clock and returns its Store.
func newCache(t *testing.T, cfg *config.Config, origin storage.Reader, clock *testClock) (*cache.Cache, *cache.Store) {
	t.Helper()
	store, err := cache.NewStore(cache.LimitsFrom(cfg.Cache))
	if err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	c, err := cache.New(cache.Options{Store: store, Origin: origin, Config: cfg.Cache, Prefix: cfg.S3.Prefix,
		ConditionalReads: cfg.S3.Features.ConditionalReads, Now: clock.Now, Lifetime: lifetime})
	if err != nil {
		t.Fatal(err)
	}
	return c, store
}

type served struct {
	status int
	header http.Header
	body   string
}

func serve(h http.Handler, method, target string, header http.Header) served {
	if header == nil {
		header = http.Header{}
	}
	w := request(h, method, target, header.Clone())
	return served{w.Code, w.Result().Header, w.Body.String()}
}

func ageOf(h http.Header) int64 {
	age, _ := strconv.ParseInt(h.Get("Age"), 10, 64)
	return age
}

// sameResponse requires identical status, body and headers, except an Age
// that must exceed the uncached one by shift to shift+extra seconds, and the
// request ID generated for each request.
func sameResponse(t *testing.T, label string, uncached, cached served, shift, extra int64) {
	t.Helper()
	a, b := uncached.header.Clone(), cached.header.Clone()
	for _, name := range []string{"Age", "X-Request-Id"} {
		a.Del(name)
		b.Del(name)
	}
	if cached.status != uncached.status || cached.body != uncached.body || !reflect.DeepEqual(a, b) {
		short := func(s string) string { return s[:min(len(s), 64)] }
		t.Fatalf("%s differs:\nuncached %d %q %v\ncached   %d %q %v", label, uncached.status, short(uncached.body), uncached.header, cached.status, short(cached.body), cached.header)
	}
	if got, want := ageOf(cached.header), ageOf(uncached.header)+shift; got < want || got > want+extra {
		t.Fatalf("%s Age %d, want %d (uncached %v, cached %v)", label, got, want, uncached.header, cached.header)
	}
}

// TestCachedHandlerMatchesUncached runs every request through an uncached
// handler and a cached one, cold and then warm, with provider ranges and
// conditional reads both on and both off. The cached responses must be the
// uncached ones, with Age advanced by the residence of a snapshot served from
// memory. The one intended exception is a local row with the capabilities
// off: the uncached handler cannot range-read and sends a full 200, while a
// body the cache holds in memory is a complete snapshot that it slices. Its
// response must then be what an uncached handler with the capabilities on
// sends, except that Accept-Ranges, which follows the provider capabilities
// alone, stays absent.
func TestCachedHandlerMatchesUncached(t *testing.T) {
	const warmAfter = 7 * time.Second
	notFound := func(c *config.Config) { c.NotFound = "404.html" }
	spa := func(c *config.Config) { c.SPA = &config.SPA{Fallback: "index.html"} }
	badGateway := func(c *config.Config) { c.Errors.AccessDenied = "bad_gateway" }
	html := http.Header{"Accept": {"text/html"}}
	br := http.Header{"Accept-Encoding": {"br, gzip"}}
	ranged := func(spec string, more ...string) http.Header {
		h := http.Header{"Range": {spec}}
		for i := 0; i+1 < len(more); i += 2 {
			h.Set(more[i], more[i+1])
		}
		return h
	}
	modified := fixtureModified.Format(http.TimeFormat)
	for _, tc := range []struct {
		name, method, target string
		header               http.Header
		config               func(*config.Config)
		status               int  // with the capabilities on
		statusOff            int  // with them off, when different and not local
		memory               bool // the warm response presents a snapshot's age
		local                bool // with the capabilities off, the cached handler slices memory
		warmCalls            int  // origin reads of the warm request
		warmCallsOff         int  // with the capabilities off, when different
		realDate             bool // a past origin Date makes Age follow the real clock too
	}{
		{name: "get", method: "GET", target: "/file.txt", status: 200, memory: true},
		{name: "head", method: "HEAD", target: "/file.txt", status: 200, memory: true},
		{name: "head ignores range", method: "HEAD", target: "/file.txt", header: ranged("bytes=1-3"), status: 200, memory: true},
		{name: "index", method: "GET", target: "/docs/", status: 200, memory: true},
		{name: "redirect", method: "GET", target: "/docs?x=1", status: 308},
		{name: "builtin 404", method: "GET", target: "/missing.txt", status: 404},
		{name: "builtin 404 head", method: "HEAD", target: "/missing.txt", status: 404},
		{name: "custom 404", method: "GET", target: "/missing.txt", config: notFound, status: 404},
		{name: "custom 404 head", method: "HEAD", target: "/missing", config: notFound, status: 404},
		{name: "spa shell", method: "GET", target: "/app/route", header: html, config: spa, status: 200},
		{name: "spa shell head", method: "HEAD", target: "/app/other", header: html, config: spa, status: 200},
		{name: "brotli", method: "GET", target: "/app.js", header: br, status: 200, memory: true},
		{name: "gzip", method: "GET", target: "/app.js", header: http.Header{"Accept-Encoding": {"gzip"}}, status: 200, memory: true},
		{name: "identity", method: "GET", target: "/app.js", header: http.Header{"Accept-Encoding": {"identity"}}, status: 200, memory: true},
		{name: "not acceptable", method: "GET", target: "/style.css", header: http.Header{"Accept-Encoding": {"br, identity;q=0"}}, status: 406},
		{name: "brotli range", method: "GET", target: "/app.js", header: ranged("bytes=1-3", "Accept-Encoding", "br"), status: 206, memory: true, local: true},
		{name: "if-none-match", method: "GET", target: "/file.txt", header: http.Header{"If-None-Match": {`W/"f1"`}}, status: 304, memory: true},
		{name: "if-modified-since", method: "GET", target: "/file.txt", header: http.Header{"If-Modified-Since": {modified}}, status: 304, memory: true},
		{name: "if-match", method: "GET", target: "/file.txt", header: http.Header{"If-Match": {`"other"`}}, status: 412, memory: true},
		{name: "range", method: "GET", target: "/file.txt", header: ranged("bytes=1-3"), status: 206, memory: true, local: true},
		{name: "suffix range", method: "GET", target: "/file.txt", header: ranged("bytes=-4"), status: 206, memory: true, local: true},
		{name: "unsatisfiable range", method: "GET", target: "/file.txt", header: ranged("bytes=100-"), status: 416, local: true},
		{name: "if-range etag match", method: "GET", target: "/file.txt", header: ranged("bytes=0-1", "If-Range", `"f1"`), status: 206, memory: true, local: true},
		{name: "if-range etag stale", method: "GET", target: "/file.txt", header: ranged("bytes=0-1", "If-Range", `"f0"`), status: 200, memory: true},
		{name: "if-range date match", method: "GET", target: "/ahead.txt", header: ranged("bytes=0-1", "If-Range", modified), status: 206, memory: true, local: true},
		{name: "if-range date stale", method: "GET", target: "/ahead.txt", header: ranged("bytes=0-1", "If-Range", fixtureModified.Add(time.Hour).Format(http.TimeFormat)), status: 200, memory: true},
		{name: "weak etag range", method: "GET", target: "/weak.txt", header: ranged("bytes=0-1"), status: 200, memory: true},
		{name: "empty object", method: "GET", target: "/empty.txt", status: 200, memory: true},
		{name: "empty object range", method: "GET", target: "/empty.txt", header: ranged("bytes=0-0"), status: 416, local: true},
		{name: "origin cache-control", method: "GET", target: "/cc.txt", status: 200, memory: true},
		{name: "origin expires", method: "GET", target: "/expires.txt", status: 200, memory: true},
		{name: "cache_control override", method: "GET", target: "/override.txt", status: 200, memory: true},
		{name: "origin date ahead", method: "GET", target: "/ahead.txt", status: 200, memory: true},
		{name: "origin date behind", method: "GET", target: "/behind.txt", status: 200, memory: true, realDate: true},
		{name: "rule ttl projection", method: "GET", target: "/assets/logo.svg", status: 200, memory: true},
		{name: "rule disabled", method: "GET", target: "/nocache.txt", status: 200, warmCalls: 1},
		{name: "denied not_found", method: "GET", target: "/denied.txt", status: 404, warmCalls: 1},
		{name: "denied bad_gateway", method: "GET", target: "/denied.txt", config: badGateway, status: 502, warmCalls: 1},
		{name: "large object", method: "GET", target: "/big.bin", status: 200, warmCalls: 1},
		{name: "large object range", method: "GET", target: "/big.bin", header: ranged("bytes=10-19"), status: 206, statusOff: 200, warmCalls: 2, warmCallsOff: 1},
	} {
		for _, capable := range []bool{true, false} {
			t.Run(tc.name+" capabilities "+strconvBool(capable), func(t *testing.T) {
				cfg := cachedConfig(t, tc.config)
				cfg.S3.Features.RangeReads, cfg.S3.Features.ConditionalReads = capable, capable
				origin := newMemOrigin()
				origin.noRanges, origin.noConditionals = !capable, !capable
				// With a past origin Date, the response layer adds the real time
				// elapsed since that Date to Age, and the cache its fake-clock
				// age, which starts from real time too. Both only grow from the
				// uncached response's, by at most the real seconds elapsed since
				// start, whatever delays the test meets.
				start := time.Now()
				realExtra := func() int64 {
					if !tc.realDate {
						return 0
					}
					return time.Now().Unix() - start.Unix()
				}
				uncached := serve(boundary(t, cfg, origin), tc.method, "http://example.org"+tc.target, tc.header)
				want, status, warmCalls := uncached, tc.status, tc.warmCalls
				if !capable && tc.warmCallsOff != 0 {
					warmCalls = tc.warmCallsOff
				}
				if !capable && tc.statusOff != 0 {
					status = tc.statusOff
				}
				if !capable && tc.local {
					// The intended exception: compare with a range-capable uncached handler.
					if uncached.status != http.StatusOK {
						t.Fatalf("uncached status %d without range capability", uncached.status)
					}
					// The same objects, whose Date and Expires were stamped once.
					capableCfg := cachedConfig(t, tc.config)
					sameObjects := &memOrigin{objects: origin.objects, denied: origin.denied}
					want = serve(boundary(t, capableCfg, sameObjects), tc.method, "http://example.org"+tc.target, tc.header)
					want.header = want.header.Clone()
					want.header.Del("Accept-Ranges")
				}
				if want.status != status {
					t.Fatalf("expected status %d, want %d", want.status, status)
				}
				clock := &testClock{t: time.Now()}
				lifetime, stop := context.WithCancel(context.Background())
				defer stop()
				c, err := cache.FromConfig(lifetime, cfg, origin, func(o *cache.Options) { o.Now = clock.Now })
				if err != nil {
					t.Fatal(err)
				}
				cached := boundary(t, cfg, origin, WithCache(c))
				origin.take()
				cold := serve(cached, tc.method, "http://example.org"+tc.target, tc.header)
				sameResponse(t, "cold", want, cold, 0, realExtra())
				origin.take()
				clock.Advance(warmAfter)
				shift := int64(0)
				if tc.memory {
					shift = int64(warmAfter / time.Second)
				}
				warm := serve(cached, tc.method, "http://example.org"+tc.target, tc.header)
				sameResponse(t, "warm", want, warm, shift, realExtra())
				if calls := origin.take(); len(calls) != warmCalls {
					t.Fatalf("warm origin calls %q, want %d", calls, warmCalls)
				}
			})
		}
	}
}

func TestCacheAvoidsOriginReads(t *testing.T) {
	cfg := cachedConfig(t, func(c *config.Config) { c.SPA = &config.SPA{Fallback: "index.html"} })
	origin := newMemOrigin()
	clock := &testClock{t: time.Now()}
	c, store := newCache(t, cfg, origin, clock)
	h := boundary(t, cfg, origin, WithCache(c))
	html := http.Header{"Accept": {"text/html"}}

	t.Run("cold HEAD reads metadata only", func(t *testing.T) {
		if w := serve(h, "HEAD", "http://example.org/file.txt", nil); w.status != 200 || w.body != "" {
			t.Fatalf("HEAD %d", w.status)
		}
		if calls := origin.take(); strings.Join(calls, "|") != "HEAD file.txt" {
			t.Fatalf("cold HEAD calls %q", calls)
		}
	})
	t.Run("repeated GET and HEAD after GET", func(t *testing.T) {
		serve(h, "GET", "http://example.org/file.txt", nil)
		if calls := origin.take(); len(calls) != 1 {
			t.Fatalf("GET after a metadata entry: %q", calls)
		}
		for _, method := range []string{"GET", "HEAD", "GET"} {
			if w := serve(h, method, "http://example.org/file.txt", nil); w.status != 200 {
				t.Fatalf("%s %d", method, w.status)
			}
		}
		if calls := origin.take(); len(calls) != 0 {
			t.Fatalf("warm reads reached the origin: %q", calls)
		}
	})
	t.Run("negative entries end fallback chains", func(t *testing.T) {
		for _, target := range []string{"/missing.txt", "/missing.txt"} {
			if w := serve(h, "GET", "http://example.org"+target, nil); w.status != 404 {
				t.Fatalf("404 status %d", w.status)
			}
		}
		if calls := origin.take(); len(calls) != 2 {
			t.Fatalf("a repeated 404 reached the origin: %q", calls)
		}
	})
	t.Run("one SPA shell entry serves every route", func(t *testing.T) {
		for _, route := range []string{"/a", "/b/c", "/d", "/a", "/b/c"} {
			if w := serve(h, "GET", "http://example.org"+route, html); w.status != 200 || w.body != "<p>shell</p>" {
				t.Fatalf("%s: %d %q", route, w.status, w.body)
			}
		}
		shell := 0
		calls := origin.take()
		for _, call := range calls {
			if call == "GET index.html" {
				shell++
			}
		}
		if shell != 1 || len(calls) != 7 { // the shell, and two confirmed misses per new route
			t.Fatalf("SPA routes read %q", calls)
		}
	})
	t.Run("precompressed variants are separate entries", func(t *testing.T) {
		for range 2 {
			for _, coding := range []string{"br", "gzip", "identity"} {
				w := serve(h, "GET", "http://example.org/app.js", http.Header{"Accept-Encoding": {coding}})
				if want := map[string]string{"br": "brotli", "gzip": "gzip", "identity": "identity()"}[coding]; w.body != want {
					t.Fatalf("%s served %q", coding, w.body)
				}
			}
		}
		if calls := origin.take(); strings.Join(calls, "|") != "GET app.js|GET app.js.br|GET app.js.gz" {
			t.Fatalf("variants read %q", calls)
		}
		for _, key := range []string{"app.js", "app.js.br", "app.js.gz"} {
			if _, kind, ok := store.Peek(key); !ok || kind != cache.KindBody {
				t.Fatalf("%s stored as %s, %t", key, kind, ok)
			}
		}
	})
}

func TestCachedAgeAndProjection(t *testing.T) {
	t.Run("origin age plus residence", func(t *testing.T) {
		cfg := cachedConfig(t, nil)
		origin := newMemOrigin()
		origin.set("page.txt", memObject{"page", fixtureMeta(`"p"`, "max-age=300", 120)})
		clock := &testClock{t: time.Now()}
		c, _ := newCache(t, cfg, origin, clock)
		h := boundary(t, cfg, origin, WithCache(c))
		for _, step := range []struct {
			advance time.Duration
			age     string
		}{{0, "120"}, {30 * time.Second, "150"}, {100 * time.Second, "250"}} {
			clock.Advance(step.advance)
			w := serve(h, "GET", "http://example.org/page.txt", nil)
			if w.header.Get("Age") != step.age || w.header.Get("Cache-Control") != "max-age=300" {
				t.Fatalf("after %v: %v", step.advance, w.header)
			}
		}
		if calls := origin.take(); len(calls) != 1 {
			t.Fatalf("calls %q", calls)
		}
	})
	t.Run("projected TTL", func(t *testing.T) {
		cfg := cachedConfig(t, func(c *config.Config) { c.Cache.TTL = 10 * time.Minute })
		origin := newMemOrigin()
		clock := &testClock{t: time.Now()}
		c, _ := newCache(t, cfg, origin, clock)
		h := boundary(t, cfg, origin, WithCache(c))
		serve(h, "GET", "http://example.org/style.css", nil)
		clock.Advance(100 * time.Second)
		w := serve(h, "GET", "http://example.org/style.css", nil)
		if w.header.Get("Age") != "100" || w.header.Get("Cache-Control") != "max-age=600" {
			t.Fatalf("projection %v", w.header)
		}
		clock.Advance(100 * time.Second)
		w = serve(h, "GET", "http://example.org/assets/logo.svg", nil) // the rule's 1h TTL
		if w.header.Get("Age") != "" || w.header.Get("Cache-Control") != "max-age=3600" {
			t.Fatalf("rule projection %v", w.header)
		}
	})
	t.Run("stale-if-error reports the true age", func(t *testing.T) {
		cfg := cachedConfig(t, func(c *config.Config) { c.Cache.StaleIfError = time.Minute })
		origin := newMemOrigin()
		origin.set("page.txt", memObject{"page", fixtureMeta(`"p"`, "max-age=60", 10)})
		clock := &testClock{t: time.Now()}
		c, _ := newCache(t, cfg, origin, clock)
		var o Observation
		h := boundary(t, cfg, origin, WithCache(c), WithObserver(func(v Observation) { o = v }))
		serve(h, "GET", "http://example.org/page.txt", nil)
		clock.Advance(80 * time.Second)
		origin.setHook(func(context.Context, string, storage.ReadOptions) error {
			return &storage.Error{Kind: storage.Unavailable, Size: -1}
		})
		w := serve(h, "GET", "http://example.org/page.txt", nil)
		if w.status != 200 || w.body != "page" || w.header.Get("Age") != "90" || w.header.Get("Cache-Control") != "max-age=60" || o.CacheOutcome != cache.OutcomeStale {
			t.Fatalf("stale %d %v %+v", w.status, w.header, o)
		}
	})
}

func TestCachedRangesFromMemory(t *testing.T) {
	for _, flags := range []struct{ ranges, conditional bool }{{false, true}, {true, false}, {false, false}} {
		t.Run("ranges "+strconvBool(flags.ranges)+" conditional "+strconvBool(flags.conditional), func(t *testing.T) {
			cfg := cachedConfig(t, func(c *config.Config) {
				c.S3.Features.RangeReads, c.S3.Features.ConditionalReads = flags.ranges, flags.conditional
			})
			origin := newMemOrigin()
			origin.noRanges, origin.noConditionals = !flags.ranges, !flags.conditional
			c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
			var o Observation
			h := boundary(t, cfg, origin, WithCache(c), WithObserver(func(v Observation) { o = v }))
			ranged := http.Header{"Range": {"bytes=1-3"}}
			// The fill buffers the small object completely, so even the cold
			// request is sliced from memory.
			for i, want := range []cache.Outcome{cache.OutcomeMiss, cache.OutcomeHit} {
				w := serve(h, "GET", "http://example.org/file.txt", ranged)
				if w.status != 206 || w.body != "ell" || w.header.Get("Content-Range") != "bytes 1-3/11" || w.header.Get("Accept-Ranges") != "" || o.CacheOutcome != want {
					t.Fatalf("range %d: %d %q %v %+v", i, w.status, w.body, w.header, o)
				}
			}
			for _, tc := range []struct {
				header                  http.Header
				status                  int
				body, cr, contentLength string
			}{
				{header: http.Header{"Range": {"bytes=-5"}}, status: 206, body: "world", cr: "bytes 6-10/11", contentLength: "5"},
				{header: http.Header{"Range": {"bytes=0-1"}, "If-Range": {`"f1"`}}, status: 206, body: "he", cr: "bytes 0-1/11", contentLength: "2"},
				{header: http.Header{"Range": {"bytes=0-1"}, "If-Range": {`"f0"`}}, status: 200, body: "hello world", contentLength: "11"},
				{header: http.Header{"Range": {"bytes=0-1"}, "If-Range": {`W/"f1"`}}, status: 200, body: "hello world", contentLength: "11"},
				{header: http.Header{"Range": {"bytes=0-1,4-5"}}, status: 200, body: "hello world", contentLength: "11"},
				{header: http.Header{"Range": {"bytes=11-"}}, status: 416, body: "Requested Range Not Satisfiable\n", cr: "bytes */11", contentLength: "32"},
			} {
				w := serve(h, "GET", "http://example.org/file.txt", tc.header)
				if w.status != tc.status || w.body != tc.body || w.header.Get("Content-Range") != tc.cr || w.header.Get("Content-Length") != tc.contentLength || w.header.Get("Accept-Ranges") != "" {
					t.Fatalf("%v: %d %q %v", tc.header, w.status, w.body, w.header)
				}
			}
			if calls := origin.take(); len(calls) != 1 {
				t.Fatalf("ranges reached the origin: %q", calls)
			}
			// A streamed object is not buffered to manufacture range support.
			for range 2 {
				if w := serve(h, "GET", "http://example.org/big.bin", ranged); w.status != 200 || len(w.body) != 4096 {
					t.Fatalf("streamed range %d, %d bytes", w.status, len(w.body))
				}
			}
		})
	}
	t.Run("provider ranges enabled", func(t *testing.T) {
		cfg := cachedConfig(t, nil)
		origin := newMemOrigin()
		c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
		var o Observation
		h := boundary(t, cfg, origin, WithCache(c), WithObserver(func(v Observation) { o = v }))
		ranged := http.Header{"Range": {"bytes=1-3"}}
		for i, tc := range []struct {
			outcome cache.Outcome
			bytes   int64
		}{{cache.OutcomeMiss, 0}, {cache.OutcomeHit, 3}} {
			w := serve(h, "GET", "http://example.org/file.txt", ranged)
			if w.status != 206 || w.body != "ell" || w.header.Get("Content-Range") != "bytes 1-3/11" || w.header.Get("Accept-Ranges") != "bytes" || o.CacheOutcome != tc.outcome || o.CacheBytes != tc.bytes {
				t.Fatalf("range %d: %d %q %v %+v", i, w.status, w.body, w.header, o)
			}
		}
		// Neither request needs a ranged origin read.
		if calls := origin.take(); strings.Join(calls, "|") != "GET file.txt" {
			t.Fatalf("calls %q", calls)
		}
	})
}

func TestCachedOriginOutcomes(t *testing.T) {
	setup := func(t *testing.T, mutate func(*config.Config)) (*memOrigin, *testClock, *cache.Store, http.Handler, *Observation) {
		cfg := cachedConfig(t, mutate)
		origin := newMemOrigin()
		clock := &testClock{t: time.Now()}
		c, store := newCache(t, cfg, origin, clock)
		o := &Observation{}
		h := boundary(t, cfg, origin, WithCache(c), WithObserver(func(v Observation) { *o = v }))
		origin.set("page.txt", memObject{"page", fixtureMeta(`"p"`, "max-age=60", 0)})
		serve(h, "GET", "http://example.org/page.txt", nil)
		origin.take()
		return origin, clock, store, h, o
	}
	t.Run("revalidation refreshes metadata", func(t *testing.T) {
		origin, clock, _, h, o := setup(t, nil)
		clock.Advance(2 * time.Minute)
		origin.set("page.txt", memObject{"changed bytes the 304 must not serve", fixtureMeta(`"p"`, "max-age=120", 4)})
		w := serve(h, "GET", "http://example.org/page.txt", nil)
		if w.status != 200 || w.body != "page" || w.header.Get("Cache-Control") != "max-age=120" || w.header.Get("Age") != "4" || o.CacheOutcome != cache.OutcomeRevalidated || o.CacheBytes != 4 {
			t.Fatalf("revalidated %d %q %v %+v", w.status, w.body, w.header, o)
		}
		if calls := origin.take(); strings.Join(calls, "|") != `GET page.txt if-none-match="p"` {
			t.Fatalf("calls %q", calls)
		}
	})
	for _, policy := range []struct {
		name   string
		status int
	}{{"not_found", 404}, {"bad_gateway", 502}} {
		t.Run("denied revalidation "+policy.name, func(t *testing.T) {
			origin, clock, store, h, o := setup(t, func(c *config.Config) {
				c.Errors.AccessDenied = policy.name
				c.Cache.StaleIfError = time.Hour
			})
			clock.Advance(2 * time.Minute)
			origin.deny("page.txt")
			w := serve(h, "GET", "http://example.org/page.txt", nil)
			if w.status != policy.status || w.header.Get("Cache-Control") != "no-store" || o.Outcome != OutcomeOriginFailure || o.OriginKey != "" {
				t.Fatalf("denied %d %v %+v", w.status, w.header, o)
			}
			if _, _, ok := store.Peek("page.txt"); ok {
				t.Fatal("a denied revalidation kept the entry")
			}
		})
	}
	t.Run("request directives", func(t *testing.T) {
		origin, _, _, h, o := setup(t, nil)
		for _, tc := range []struct {
			cacheControl string
			outcome      cache.Outcome
			reason       cache.Reason
			calls        string
		}{
			{"no-store", cache.OutcomeBypass, cache.ReasonRequestNoStore, "GET page.txt"},
			{"no-cache", cache.OutcomeRevalidated, cache.ReasonNone, `GET page.txt if-none-match="p"`},
			{"max-age=0", cache.OutcomeRevalidated, cache.ReasonNone, `GET page.txt if-none-match="p"`},
			{"", cache.OutcomeHit, cache.ReasonNone, ""},
		} {
			w := serve(h, "GET", "http://example.org/page.txt", http.Header{"Cache-Control": {tc.cacheControl}})
			if w.status != 200 || w.body != "page" || o.CacheOutcome != tc.outcome || o.CacheReason != tc.reason {
				t.Fatalf("%q: %d %+v", tc.cacheControl, w.status, o)
			}
			if calls := origin.take(); strings.Join(calls, "|") != tc.calls {
				t.Fatalf("%q: calls %q", tc.cacheControl, calls)
			}
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		cfg := cachedConfig(t, nil)
		origin := newMemOrigin()
		store, err := cache.NewStore(cache.LimitsFrom(cfg.Cache))
		if err != nil {
			t.Fatal(err)
		}
		lifetime, stop := context.WithCancel(context.Background())
		defer stop()
		c, err := cache.New(cache.Options{Store: store, Origin: origin, Config: cfg.Cache, Lifetime: lifetime})
		if err != nil {
			t.Fatal(err)
		}
		origin.setHook(func(ctx context.Context, _ string, _ storage.ReadOptions) error {
			stop()
			<-ctx.Done()
			return ctx.Err()
		})
		var o Observation
		w := serve(boundary(t, cfg, origin, WithCache(c), WithObserver(func(v Observation) { o = v })), "GET", "http://example.org/file.txt", nil)
		if w.status != 503 || o.TerminationReason != ReasonShutdown || o.Outcome != OutcomeOriginFailure || w.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("shutdown %d %+v", w.status, o)
		}
	})
}

func TestCachedObservation(t *testing.T) {
	cfg := cachedConfig(t, nil)
	origin := newMemOrigin()
	clock := &testClock{t: time.Now()}
	c, _ := newCache(t, cfg, origin, clock)
	var o Observation
	h := boundary(t, cfg, origin, WithCache(c), WithObserver(func(v Observation) { o = v }))
	for _, tc := range []struct {
		method, target string
		header         http.Header
		status         int
		outcome        cache.Outcome
		reason         cache.Reason
		memoryBytes    int64
		originKey      string
	}{
		{"GET", "/file.txt", nil, 200, cache.OutcomeMiss, "", 0, "file.txt"}, // the request's own fill supplied the bytes
		{"GET", "/file.txt", nil, 200, cache.OutcomeHit, "", 11, "file.txt"},
		{"HEAD", "/file.txt", nil, 200, cache.OutcomeMetadataHit, "", 0, "file.txt"},
		{"GET", "/file.txt", http.Header{"If-None-Match": {`"f1"`}}, 304, cache.OutcomeHit, "", 0, "file.txt"},
		{"GET", "/file.txt", http.Header{"If-Match": {`"x"`}}, 412, cache.OutcomeHit, "", 0, "file.txt"},
		{"GET", "/file.txt", http.Header{"Range": {"bytes=2-5"}}, 206, cache.OutcomeHit, "", 4, "file.txt"},
		{"GET", "/file.txt", http.Header{"Range": {"bytes=50-"}}, 416, cache.OutcomeHit, "", 0, ""},
		{"GET", "/file.txt", http.Header{"Cache-Control": {"no-store"}}, 200, cache.OutcomeBypass, cache.ReasonRequestNoStore, 0, "file.txt"},
		{"GET", "/missing.txt", nil, 404, cache.OutcomeMiss, "", 0, ""},
		{"GET", "/missing.txt", nil, 404, cache.OutcomeNegativeHit, "", 0, ""},
		{"HEAD", "/missing.txt", nil, 404, cache.OutcomeNegativeHit, "", 0, ""},
		{"GET", "/docs", nil, 308, cache.OutcomeMiss, "", 0, ""},
		{"GET", "/docs", nil, 308, cache.OutcomeHit, "", 0, ""},
		{"GET", "/nocache.txt", nil, 200, cache.OutcomeBypass, cache.ReasonDisabled, 0, "nocache.txt"},
		{"GET", "/big.bin", nil, 200, cache.OutcomeBypass, cache.ReasonTooLarge, 0, "big.bin"},
		{"GET", "/denied.txt", nil, 404, cache.OutcomeMiss, "", 0, ""},
	} {
		w := serve(h, tc.method, "http://example.org"+tc.target, tc.header)
		if w.status != tc.status || o.CacheOutcome != tc.outcome || o.CacheReason != tc.reason || o.CacheBytes != tc.memoryBytes || o.OriginKey != tc.originKey {
			t.Fatalf("%s %s %v: %d %+v", tc.method, tc.target, tc.header, w.status, o)
		}
		if tc.memoryBytes > 0 && o.Bytes != tc.memoryBytes {
			t.Fatalf("%s %s: memory bytes %d of %d written", tc.method, tc.target, o.CacheBytes, o.Bytes)
		}
	}
	// A handler without a cache reports no cache fields.
	h = boundary(t, cfg, origin, WithCache(nil), WithObserver(func(v Observation) { o = v }))
	if w := serve(h, "GET", "http://example.org/file.txt", nil); w.status != 200 || o.CacheOutcome != "" || o.CacheReason != "" || o.CacheBytes != 0 {
		t.Fatalf("uncached observation %+v", o)
	}
}

// benchmarkHandler serves a small object through httpserver.NewHandler and
// the content handler, with or without a cache. distinct requests a new key
// each time, so that every cached request is a cold miss.
func benchmarkHandler(b *testing.B, cached, distinct bool) {
	cfg := testConfig(b)
	origin := fakeReader{get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
		return object("hello, cached world", storage.Metadata{Size: 19, ETag: `"v1"`, LastModified: fixtureModified, CacheControl: "public, max-age=60"}), nil
	}}
	var options []HandlerOption
	if cached {
		c, err := cache.FromConfig(context.Background(), cfg, origin)
		if err != nil {
			b.Fatal(err)
		}
		options = append(options, WithCache(c))
	}
	h := boundary(b, cfg, origin, options...)
	if !distinct {
		w := request(h, "GET", "http://example.org/file.txt", nil)
		if w.Code != 200 {
			b.Fatalf("warmup %d", w.Code)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		target := "http://example.org/file.txt"
		if distinct {
			target = "http://example.org/file-" + strconv.Itoa(i) + ".txt"
		}
		if w := request(h, "GET", target, nil); w.Code != 200 {
			b.Fatalf("status %d", w.Code)
		}
	}
}

func BenchmarkHandlerUncached(b *testing.B)       { benchmarkHandler(b, false, false) }
func BenchmarkHandlerCachedWarmHit(b *testing.B)  { benchmarkHandler(b, true, false) }
func BenchmarkHandlerCachedColdMiss(b *testing.B) { benchmarkHandler(b, true, true) }
