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
	"github.com/gtriggiano/statica/internal/storage"
)

// rangeRig serves memOrigin through a handler, cached unless uncached is set.
type rangeRig struct {
	origin  *memOrigin
	handler http.Handler
	cache   *cache.Cache
	store   *cache.Store
	o       *Observation
}

func newRangeRig(t *testing.T, uncached bool, mutate func(*config.Config)) *rangeRig {
	t.Helper()
	cfg := cachedConfig(t, mutate)
	r := &rangeRig{origin: newMemOrigin(), o: &Observation{}}
	options := []HandlerOption{WithObserver(func(v Observation) { *r.o = v })}
	if !uncached {
		r.cache, r.store = newCache(t, cfg, r.origin, &testClock{t: time.Now()})
		options = append(options, WithCache(r.cache))
	}
	r.handler = boundary(t, cfg, r.origin, options...)
	return r
}

// get serves a GET of path with header given as name, value pairs.
func (r *rangeRig) get(path string, header ...string) served {
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Add(header[i], header[i+1])
	}
	return serve(r.handler, "GET", "http://example.org"+path, h)
}

// expectCalls requires the origin calls since the previous take.
func (r *rangeRig) expectCalls(t *testing.T, label string, want ...string) {
	t.Helper()
	if calls := r.origin.take(); strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("%s: origin calls %q, want %q", label, calls, want)
	}
}

const bigBody = "0123456789abcdef" // big.bin repeats it to 4 KiB, past max_object_bytes

// A range request reads the object's metadata first and never aborts a full
// GET: the bytes come from a guarded origin range read, or from a body the
// cache holds or fills.
func TestRangeRequestsReadMetadataFirst(t *testing.T) {
	ranged := `GET big.bin range=bytes=16-19 if-match="big"`
	t.Run("large object with the cache", func(t *testing.T) {
		r := newRangeRig(t, false, nil)
		for i, want := range []cache.Outcome{cache.OutcomeMiss, cache.OutcomeMetadataHit, cache.OutcomeMetadataHit, cache.OutcomeMetadataHit, cache.OutcomeMetadataHit} {
			w := r.get("/big.bin", "Range", "bytes=16-19")
			if w.status != 206 || w.body != "0123" || w.header.Get("Content-Range") != "bytes 16-19/4096" || w.header.Get("Accept-Ranges") != "bytes" || r.o.CacheOutcome != want || r.o.CacheBytes != 0 {
				t.Fatalf("range %d: %d %q %v %+v", i, w.status, w.body, w.header, *r.o)
			}
		}
		r.expectCalls(t, "five ranges", "HEAD big.bin", ranged, ranged, ranged, ranged, ranged)
		if _, kind, ok := r.store.Peek("big.bin"); !ok || kind != cache.KindMetadata {
			t.Fatalf("big.bin stored as %s, %t", kind, ok)
		}
	})
	for _, tc := range []struct {
		name     string
		uncached bool
		header   []string
		outcome  cache.Outcome
		reason   cache.Reason
	}{
		{name: "without a cache", uncached: true},
		{name: "request no-store", header: []string{"Cache-Control", "no-store"}, outcome: cache.OutcomeBypass, reason: cache.ReasonRequestNoStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRangeRig(t, tc.uncached, nil)
			for i := range 2 {
				for _, target := range []struct{ path, body, call string }{
					{"/big.bin", "0123", ranged},
					{"/file.txt", "o wo", `GET file.txt range=bytes=4-7 if-match="f1"`},
				} {
					spec := "bytes=16-19"
					if target.path == "/file.txt" {
						spec = "bytes=4-7"
					}
					w := r.get(target.path, append([]string{"Range", spec}, tc.header...)...)
					if w.status != 206 || w.body != target.body || r.o.CacheOutcome != tc.outcome || r.o.CacheReason != tc.reason {
						t.Fatalf("%s %d: %d %q %+v", target.path, i, w.status, w.body, *r.o)
					}
					r.expectCalls(t, target.path, "HEAD "+strings.TrimPrefix(target.path, "/"), target.call)
				}
			}
		})
	}
	t.Run("uncacheable origin metadata", func(t *testing.T) {
		r := newRangeRig(t, false, nil)
		r.origin.set("private.txt", memObject{"hello world", fixtureMeta(`"p1"`, "no-store", 0)})
		for i := range 2 {
			w := r.get("/private.txt", "Range", "bytes=0-4")
			if w.status != 206 || w.body != "hello" || r.o.CacheOutcome != cache.OutcomeBypass || r.o.CacheReason != cache.ReasonUncacheable {
				t.Fatalf("range %d: %d %q %+v", i, w.status, w.body, *r.o)
			}
			r.expectCalls(t, "uncacheable", "HEAD private.txt", `GET private.txt range=bytes=0-4 if-match="p1"`)
		}
	})
	t.Run("small object fills then hits", func(t *testing.T) {
		r := newRangeRig(t, false, nil)
		for i, want := range []cache.Outcome{cache.OutcomeMiss, cache.OutcomeHit} {
			w := r.get("/file.txt", "Range", "bytes=4-7")
			if w.status != 206 || w.body != "o wo" || w.header.Get("Content-Range") != "bytes 4-7/11" || r.o.CacheOutcome != want {
				t.Fatalf("range %d: %d %q %v %+v", i, w.status, w.body, w.header, *r.o)
			}
			if i == 0 {
				r.expectCalls(t, "cold", "HEAD file.txt", "GET file.txt")
			}
		}
		r.expectCalls(t, "warm")
	})
	t.Run("full fill slots stream: the origin serves the range", func(t *testing.T) {
		r := newRangeRig(t, false, func(c *config.Config) { c.Cache.MaxConcurrentFills = 1 })
		gate := make(chan struct{})
		r.origin.setHook(func(ctx context.Context, key string, _ storage.ReadOptions) error {
			if key == "style.css" {
				select {
				case <-gate:
				case <-ctx.Done():
				}
			}
			return nil
		})
		var wg sync.WaitGroup
		wg.Go(func() { r.get("/style.css") })
		eventually(t, "the only fill to run", func() bool { return r.cache.Stats().InFlight == 1 })
		w := r.get("/file.txt", "Range", "bytes=4-7")
		o := *r.o // before the held request's observation replaces it
		close(gate)
		wg.Wait()
		// The discarded stream supplied no bytes: the metadata lookup decided.
		if w.status != 206 || w.body != "o wo" || o.CacheOutcome != cache.OutcomeMiss {
			t.Fatalf("range %d %q %+v", w.status, w.body, o)
		}
		r.expectCalls(t, "fills full", "GET style.css", "HEAD file.txt", "GET file.txt", `GET file.txt range=bytes=4-7 if-match="f1"`)
	})
}

// Conditions and an unsatisfiable range are decided on metadata alone; a
// range that does not apply reads the full object.
func TestRangeRequestsDecidedOnMetadata(t *testing.T) {
	for _, uncached := range []bool{true, false} {
		t.Run("uncached "+strconvBool(uncached), func(t *testing.T) {
			r := newRangeRig(t, uncached, nil)
			w := r.get("/big.bin", "Range", "bytes=0-3", "If-None-Match", `"big"`)
			if w.status != 304 || w.body != "" || w.header.Get("ETag") != `"big"` || r.o.OriginKey != "big.bin" {
				t.Fatalf("304 %d %v %+v", w.status, w.header, *r.o)
			}
			r.expectCalls(t, "304", "HEAD big.bin")
			w = r.get("/big.bin", "Range", "bytes=0-3", "If-Match", `"other"`)
			if w.status != 412 || w.body != "" {
				t.Fatalf("412 %d %v", w.status, w.header)
			}
			w = r.get("/big.bin", "Range", "bytes=5000-")
			if w.status != 416 || w.header.Get("Content-Range") != "bytes */4096" || w.header.Get("Cache-Control") != "no-store" {
				t.Fatalf("416 %d %v", w.status, w.header)
			}
			w = r.get("/big.bin", "Range", "bytes=0-3", "If-Range", `"older"`)
			if w.status != 200 || len(w.body) != 4096 || w.header.Get("Content-Range") != "" || w.header.Get("Accept-Ranges") != "bytes" {
				t.Fatalf("If-Range mismatch %d, %d bytes, %v", w.status, len(w.body), w.header)
			}
			if uncached {
				r.expectCalls(t, "412, 416, If-Range", "HEAD big.bin", "HEAD big.bin", "HEAD big.bin", "GET big.bin")
			} else {
				r.expectCalls(t, "412, 416, If-Range", "GET big.bin")
			}
		})
	}
	for _, off := range []string{"range_reads", "conditional_reads"} {
		t.Run(off+" off", func(t *testing.T) {
			r := newRangeRig(t, true, func(c *config.Config) {
				c.S3.Features.RangeReads = off != "range_reads"
				c.S3.Features.ConditionalReads = off != "conditional_reads"
			})
			w := r.get("/big.bin", "Range", "bytes=0-3")
			if w.status != 200 || len(w.body) != 4096 || w.header.Get("Accept-Ranges") != "" {
				t.Fatalf("%d, %d bytes, %v", w.status, len(w.body), w.header)
			}
			r.expectCalls(t, "ignored range", "GET big.bin")
		})
	}
}

// Missing candidates of a range request cost what a HEAD pays: an ambiguous
// HEAD 404 is confirmed with a GET. A custom 404 page, which never answers a
// range, is read with its body.
func TestRangeRequestsResolveFallbacks(t *testing.T) {
	missing := []string{"HEAD missing.txt", "GET missing.txt", "HEAD missing.txt/index.html", "GET missing.txt/index.html"}
	for _, tc := range []struct {
		name, path string
		header     []string
		config     func(*config.Config)
		status     int
		body       string
		cold       []string
		coldCached []string // when the cache changes the cold reads
	}{
		{name: "built-in 404", path: "/missing.txt", status: 404, body: "Not Found\n", cold: missing},
		{name: "custom 404", path: "/missing.txt", config: func(c *config.Config) { c.NotFound = "404.html" }, status: 404, body: "<p>custom 404</p>",
			cold: append(append([]string(nil), missing...), "GET 404.html")},
		{name: "SPA shell", path: "/app/route", header: []string{"Accept", "text/html"}, config: func(c *config.Config) { c.SPA = &config.SPA{Fallback: "index.html"} }, status: 206, body: "<p>",
			cold:       []string{"HEAD app/route", "GET app/route", "HEAD app/route/index.html", "GET app/route/index.html", "HEAD index.html", `GET index.html range=bytes=0-2 if-match="shell"`},
			coldCached: []string{"HEAD app/route", "GET app/route", "HEAD app/route/index.html", "GET app/route/index.html", "HEAD index.html", "GET index.html"}},
	} {
		for _, uncached := range []bool{true, false} {
			t.Run(tc.name+" uncached "+strconvBool(uncached), func(t *testing.T) {
				r := newRangeRig(t, uncached, tc.config)
				for i := range 2 {
					w := r.get(tc.path, append([]string{"Range", "bytes=0-2"}, tc.header...)...)
					if w.status != tc.status || w.body != tc.body {
						t.Fatalf("request %d: %d %q", i, w.status, w.body)
					}
					switch {
					case uncached:
						r.expectCalls(t, "uncached", tc.cold...)
					case i == 0 && tc.coldCached != nil:
						// The small shell is filled, then sliced.
						r.expectCalls(t, "cold", tc.coldCached...)
					case i == 0:
						r.expectCalls(t, "cold", tc.cold...)
					default:
						// Negative entries and the bodies end the chain in memory.
						r.expectCalls(t, "warm")
					}
				}
			})
		}
	}
}

// A guarded range read that fails its If-Match restarts once, revalidating
// every lookup: metadata in memory cannot select the replaced object again.
// An object that keeps changing ends in a 503.
func TestRangeRetryRevalidatesMetadata(t *testing.T) {
	v2 := memObject{strings.Repeat("fedcba9876543210", 256), fixtureMeta(`"big2"`, "", 0)}
	t.Run("stale metadata in memory", func(t *testing.T) {
		r := newRangeRig(t, false, nil)
		if w := r.get("/big.bin", "Range", "bytes=0-3"); w.status != 206 {
			t.Fatalf("warm %d", w.status)
		}
		r.origin.take()
		r.origin.set("big.bin", v2)
		w := r.get("/big.bin", "Range", "bytes=0-3")
		if w.status != 206 || w.body != "fedc" || w.header.Get("ETag") != `"big2"` {
			t.Fatalf("retry %d %q %v", w.status, w.body, w.header)
		}
		r.expectCalls(t, "retry", `GET big.bin range=bytes=0-3 if-match="big"`, "HEAD big.bin", `GET big.bin range=bytes=0-3 if-match="big2"`)
		if w := r.get("/big.bin", "Range", "bytes=0-3"); w.status != 206 || w.body != "fedc" || r.o.CacheOutcome != cache.OutcomeMetadataHit {
			t.Fatalf("after retry %d %q %+v", w.status, w.body, *r.o)
		}
		r.expectCalls(t, "refreshed metadata", `GET big.bin range=bytes=0-3 if-match="big2"`)
	})
	t.Run("changed between the HEAD and the range", func(t *testing.T) {
		r := newRangeRig(t, false, nil)
		var once sync.Once
		r.origin.setHook(func(_ context.Context, key string, opts storage.ReadOptions) error {
			if key == "big.bin" && opts == (storage.ReadOptions{}) {
				once.Do(func() { r.origin.set("big.bin", v2) })
			}
			return nil
		})
		w := r.get("/big.bin", "Range", "bytes=0-3")
		if w.status != 206 || w.body != "fedc" || w.header.Get("ETag") != `"big2"` {
			t.Fatalf("retry %d %q %v", w.status, w.body, w.header)
		}
		r.expectCalls(t, "retry", "HEAD big.bin", `GET big.bin range=bytes=0-3 if-match="big"`, "HEAD big.bin", `GET big.bin range=bytes=0-3 if-match="big2"`)
	})
	t.Run("changed twice", func(t *testing.T) {
		cfg := cachedConfig(t, nil)
		origin := newMemOrigin()
		c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
		revision := 0
		origin.setHook(func(_ context.Context, key string, opts storage.ReadOptions) error {
			if key == "big.bin" && opts == (storage.ReadOptions{}) {
				// Every HEAD answers the revision a later range read misses.
				revision++
				origin.set("big.bin", memObject{strings.Repeat(bigBody, 256), fixtureMeta(`"r`+strconv.Itoa(revision)+`"`, "", 0)})
			}
			return nil
		})
		p := newPipeline(t, cfg, origin, WithCache(c))
		req := httptest.NewRequest("GET", "http://example.org/big.bin", nil)
		req.Header.Set("Range", "bytes=0-3")
		w := p.do(req)
		records := p.take()
		if w.Code != 503 || len(records) != 1 || records[0].ErrorClass != "origin_inconsistent" {
			t.Fatalf("status %d, records %+v", w.Code, records)
		}
		if calls := origin.take(); len(calls) != 4 {
			t.Fatalf("calls %q", calls)
		}
	})
}

// The whole object a range request reads after its metadata must be the
// selected revision: another ETag restarts resolution once, revalidating
// every lookup, and a second change ends in a 503.
func TestRangeBodyReadChecksTheSelectedRevision(t *testing.T) {
	warm := func(t *testing.T, r *rangeRig) {
		t.Helper()
		// A HEAD leaves a metadata entry for the small object, without its body.
		if w := serve(r.handler, "HEAD", "http://example.org/file.txt", nil); w.status != 200 {
			t.Fatalf("HEAD %d", w.status)
		}
		r.origin.take()
	}
	t.Run("changed after its metadata entry", func(t *testing.T) {
		r := newRangeRig(t, false, nil)
		warm(t, r)
		r.origin.set("file.txt", memObject{"HELLO WORLD", fixtureMeta(`"f2"`, "", 0)})
		w := r.get("/file.txt", "Range", "bytes=4-7")
		if w.status != 206 || w.body != "O WO" || w.header.Get("ETag") != `"f2"` || w.header.Get("Content-Range") != "bytes 4-7/11" {
			t.Fatalf("retry %d %q %v", w.status, w.body, w.header)
		}
		// The fill read f2 against f1 metadata; the retry refreshes the
		// metadata with a HEAD and revalidates the new body.
		r.expectCalls(t, "retry", "GET file.txt", "HEAD file.txt", `GET file.txt if-none-match="f2"`)
		if w := r.get("/file.txt", "Range", "bytes=4-7"); w.status != 206 || w.body != "O WO" || r.o.CacheOutcome != cache.OutcomeHit {
			t.Fatalf("after retry %d %q %+v", w.status, w.body, *r.o)
		}
		r.expectCalls(t, "after retry")
	})
	t.Run("changed twice", func(t *testing.T) {
		cfg := cachedConfig(t, nil)
		origin := newMemOrigin()
		c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
		p := newPipeline(t, cfg, origin, WithCache(c))
		head := httptest.NewRequest("HEAD", "http://example.org/file.txt", nil)
		if w := p.do(head); w.Code != 200 {
			t.Fatalf("HEAD %d", w.Code)
		}
		p.take()
		origin.take()
		revision := 0
		next := func() {
			revision++
			origin.set("file.txt", memObject{"hello world", fixtureMeta(`"x`+strconv.Itoa(revision)+`"`, "", 0)})
		}
		next()
		// Every origin read of file.txt answers a revision the next one misses.
		origin.setHook(func(_ context.Context, key string, _ storage.ReadOptions) error {
			if key == "file.txt" {
				next()
			}
			return nil
		})
		req := httptest.NewRequest("GET", "http://example.org/file.txt", nil)
		req.Header.Set("Range", "bytes=4-7")
		w := p.do(req)
		records := p.take()
		if w.Code != 503 || len(records) != 1 || records[0].ErrorClass != "origin_inconsistent" || records[0].OriginKey != "" {
			t.Fatalf("status %d, records %+v", w.Code, records)
		}
		if calls := origin.take(); strings.Join(calls, "|") != "GET file.txt|HEAD file.txt|GET file.txt" {
			t.Fatalf("calls %q", calls)
		}
	})
	t.Run("failed body read", func(t *testing.T) {
		cfg := cachedConfig(t, nil)
		origin := newMemOrigin()
		c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
		p := newPipeline(t, cfg, origin, WithCache(c))
		if w := p.do(httptest.NewRequest("HEAD", "http://example.org/file.txt", nil)); w.Code != 200 {
			t.Fatalf("HEAD %d", w.Code)
		}
		p.take()
		origin.setHook(func(context.Context, string, storage.ReadOptions) error {
			return &storage.Error{Kind: storage.Unavailable, Size: -1}
		})
		req := httptest.NewRequest("GET", "http://example.org/file.txt", nil)
		req.Header.Set("Range", "bytes=4-7")
		w := p.do(req)
		records := p.take()
		if w.Code != 502 || len(records) != 1 || records[0].ErrorClass != "origin_unavailable" || records[0].CacheOutcome != string(cache.OutcomeMiss) {
			t.Fatalf("status %d, records %+v", w.Code, records)
		}
	})
}

// A precompressed variant is selected on metadata like any object: its
// range counts encoded bytes, and its coding is checked on every body read.
func TestRangeOfPrecompressedVariant(t *testing.T) {
	for _, uncached := range []bool{true, false} {
		t.Run("uncached "+strconvBool(uncached), func(t *testing.T) {
			r := newRangeRig(t, uncached, nil)
			w := r.get("/app.js", "Range", "bytes=1-3", "Accept-Encoding", "br")
			if w.status != 206 || w.body != "rot" || w.header.Get("Content-Encoding") != "br" || w.header.Get("Content-Range") != "bytes 1-3/6" || w.header.Get("ETag") != `"app-br"` {
				t.Fatalf("range %d %q %v", w.status, w.body, w.header)
			}
			last := `GET app.js.br range=bytes=1-3 if-match="app-br"`
			if !uncached {
				last = "GET app.js.br" // filled, then sliced
			}
			r.expectCalls(t, "range", "HEAD app.js", "HEAD app.js.br", last)
			w = r.get("/app.js", "Range", "bytes=1-3", "Accept-Encoding", "br", "If-Range", `"other"`)
			if w.status != 200 || w.body != "brotli" || w.header.Get("Content-Encoding") != "br" || w.header.Get("Content-Range") != "" {
				t.Fatalf("If-Range mismatch %d %q %v", w.status, w.body, w.header)
			}
			if uncached {
				r.expectCalls(t, "If-Range mismatch", "HEAD app.js", "HEAD app.js.br", "GET app.js.br")
			} else {
				r.expectCalls(t, "If-Range mismatch")
			}
		})
	}
}
