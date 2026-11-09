package content

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

// Warm/cold capacity evidence for D13: deterministic tests through the full
// HTTP handler, run in CI, and benchmarks against a latency-injected origin.

// slowOrigin delays an S3-like memOrigin. Every read waits header before its
// headers and then, while hold is set, until hold closes. Every body read
// waits chunkDelay and returns at most chunk bytes; the first one first waits
// until bodyHold, when set, closes. reads counts GETs and HEADs, fillReads
// those carrying storage.PurposeCacheFill.
type slowOrigin struct {
	*memOrigin
	header, chunkDelay time.Duration
	chunk              int
	mu                 sync.Mutex
	hold, bodyHold     chan struct{}
	reads, fillReads   atomic.Int64
}

func newSlowOrigin(objects map[string]memObject) *slowOrigin {
	return &slowOrigin{memOrigin: &memOrigin{objects: objects, denied: map[string]bool{}}}
}

func (o *slowOrigin) setHolds(hold, bodyHold chan struct{}) {
	o.mu.Lock()
	o.hold, o.bodyHold = hold, bodyHold
	o.mu.Unlock()
}

// begin counts a read and waits for its headers.
func (o *slowOrigin) begin(ctx context.Context) (bodyHold chan struct{}, err error) {
	o.reads.Add(1)
	if storage.PurposeFrom(ctx) == storage.PurposeCacheFill {
		o.fillReads.Add(1)
	}
	o.mu.Lock()
	hold, bodyHold := o.hold, o.bodyHold
	o.mu.Unlock()
	if err := pause(ctx, o.header); err != nil {
		return nil, err
	}
	return bodyHold, await(ctx, hold)
}

func (o *slowOrigin) Get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	bodyHold, err := o.begin(ctx)
	if err != nil {
		return nil, err
	}
	obj, err := o.memOrigin.Get(ctx, key, opts)
	if err == nil && (o.chunkDelay > 0 || o.chunk > 0 || bodyHold != nil) {
		obj.Body = &slowBody{ctx: ctx, body: obj.Body, chunk: o.chunk, delay: o.chunkDelay, hold: bodyHold}
	}
	return obj, err
}

func (o *slowOrigin) Head(ctx context.Context, key string) (storage.Metadata, error) {
	if _, err := o.begin(ctx); err != nil {
		return storage.Metadata{}, err
	}
	return o.memOrigin.Head(ctx, key)
}

type slowBody struct {
	ctx   context.Context
	body  io.ReadCloser
	chunk int
	delay time.Duration
	hold  chan struct{}
}

func (b *slowBody) Read(p []byte) (int, error) {
	if err := await(b.ctx, b.hold); err != nil {
		return 0, err
	}
	b.hold = nil
	if err := pause(b.ctx, b.delay); err != nil {
		return 0, err
	}
	if b.chunk > 0 && len(p) > b.chunk {
		p = p[:b.chunk]
	}
	return b.body.Read(p)
}

func (b *slowBody) Close() error { return b.body.Close() }

func pause(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return &storage.Error{Kind: storage.Canceled, Size: -1}
	}
}

// await waits until gate, when set, closes.
func await(ctx context.Context, gate chan struct{}) error {
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return &storage.Error{Kind: storage.Canceled, Size: -1}
	}
}

// timedWriter discards the body and times the response.
type timedWriter struct {
	header  http.Header
	start   time.Time
	ttfb    time.Duration
	status  int
	written int64
}

func (w *timedWriter) Header() http.Header { return w.header }

func (w *timedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status, w.ttfb = status, time.Since(w.start)
	}
}

func (w *timedWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.written += int64(len(p))
	return len(p), nil
}

func (w *timedWriter) Flush() {}

type timing struct {
	status      int
	written     int64
	ttfb, total time.Duration
	large       bool
}

// rig is the full public handler, httpserver.NewHandler around the content
// handler, over a slowOrigin, with or without a cache on a test clock.
type rig struct {
	origin  *slowOrigin
	handler http.Handler
	cache   *cache.Cache
	store   *cache.Store
	clock   *testClock
	keep    bool // record every Observation, for tests

	cacheBytes   atomic.Int64
	mu           sync.Mutex
	observations []Observation
}

func newRig(tb testing.TB, origin *slowOrigin, cached, keep bool, mutate func(*config.Config)) *rig {
	tb.Helper()
	cfg := testConfig(tb)
	if mutate != nil {
		mutate(cfg)
	}
	r := &rig{origin: origin, clock: &testClock{t: time.Now()}, keep: keep}
	options := []HandlerOption{WithObserver(r.observe)}
	if cached {
		store, err := cache.NewStore(cache.LimitsFrom(cfg.Cache))
		if err != nil {
			tb.Fatal(err)
		}
		lifetime, stop := context.WithCancel(context.Background())
		tb.Cleanup(stop)
		c, err := cache.New(cache.Options{Store: store, Origin: origin, Config: cfg.Cache, Prefix: cfg.S3.Prefix,
			ConditionalReads: cfg.S3.Features.ConditionalReads, Now: r.clock.Now, Lifetime: lifetime})
		if err != nil {
			tb.Fatal(err)
		}
		r.cache, r.store = c, store
		options = append(options, WithCache(c))
	}
	r.handler = boundary(tb, cfg, origin, options...)
	return r
}

func (r *rig) observe(o Observation) {
	r.cacheBytes.Add(o.CacheBytes)
	if r.keep {
		r.mu.Lock()
		r.observations = append(r.observations, o)
		r.mu.Unlock()
	}
}

// takeObservations returns the observations recorded since the previous call.
func (r *rig) takeObservations() []Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	o := r.observations
	r.observations = nil
	return o
}

func (r *rig) get(path string) timing {
	req := httptest.NewRequest(http.MethodGet, "http://example.org"+path, nil)
	w := &timedWriter{header: http.Header{}, start: time.Now()}
	r.handler.ServeHTTP(w, req)
	return timing{status: w.status, written: w.written, ttfb: w.ttfb, total: time.Since(w.start)}
}

// burst sends n concurrent GETs of path, all started together.
func (r *rig) burst(n int, path string, into []timing) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			into[i] = r.get(path)
		})
	}
	close(start)
	wg.Wait()
}

// settle waits until no flight or waiter remains, so that every OnFill and
// flight counter has been published.
func (r *rig) settle(tb testing.TB) {
	tb.Helper()
	eventually(tb, "flights to unwind", func() bool { st := r.cache.Stats(); return st.InFlight == 0 && st.Waiters == 0 })
}

func eventually(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func asset(size int, etag string) memObject {
	return memObject{strings.Repeat("a", size), fixtureMeta(etag, "", 0)}
}

const burstSize = 64

// burstLimits lets burstSize requests be admitted at once and all wait on
// one key's flight, whatever the defaults.
func burstLimits(c *config.Config) {
	c.Cache.MaxWaitersPerKey = burstSize
	c.Server.MaxConcurrentRequests = max(c.Server.MaxConcurrentRequests, burstSize)
}

// TestExpiryBurstRevalidatesOnce sends burstSize concurrent requests for one
// expired key: one conditional origin read, owned by no request, serves them
// all from memory.
func TestExpiryBurstRevalidatesOnce(t *testing.T) {
	origin := newSlowOrigin(map[string]memObject{"hot.js": asset(4096, `"hot"`)})
	r := newRig(t, origin, true, true, burstLimits)
	if got := r.get("/hot.js"); got.status != 200 {
		t.Fatalf("priming %d", got.status)
	}
	r.settle(t)
	origin.take()
	r.takeObservations()
	fills := origin.fillReads.Load()
	r.clock.Advance(6 * time.Minute) // past the 5m default TTL
	gate := make(chan struct{})
	origin.setHolds(gate, nil)
	results := make([]timing, burstSize)
	done := make(chan struct{})
	go func() { r.burst(burstSize, "/hot.js", results); close(done) }()
	eventually(t, "every request to wait on the revalidation", func() bool { return r.cache.Stats().Waiters == burstSize })
	close(gate)
	<-done
	r.settle(t)
	if calls := origin.take(); !slices.Equal(calls, []string{`GET hot.js if-none-match="hot"`}) || origin.fillReads.Load()-fills != 1 {
		t.Fatalf("origin calls %q, %d cache_fill reads", calls, origin.fillReads.Load()-fills)
	}
	for i, got := range results {
		if got.status != 200 || got.written != 4096 {
			t.Fatalf("request %d: %d, %d bytes", i, got.status, got.written)
		}
	}
	coalesced := 0
	for _, o := range r.takeObservations() {
		if o.CacheOutcome != cache.OutcomeRevalidated || o.CacheBytes != 4096 {
			t.Fatalf("observation %+v", o)
		}
		if o.CacheCoalesced {
			coalesced++
		}
	}
	st, store := r.cache.Stats(), r.store.Stats()
	if coalesced != burstSize-1 || st.Coalesced != burstSize-1 || st.Revalidations[cache.RevalidationNotModified] != 1 || store.Expirations[cache.KindBody] != 1 {
		t.Fatalf("%d coalesced; stats %+v; expirations %v", coalesced, st, store.Expirations)
	}
}

// TestPopularColdKeyFillsOnce sends burstSize concurrent requests for a key
// the cache has never seen: one origin GET fills it for all of them.
func TestPopularColdKeyFillsOnce(t *testing.T) {
	origin := newSlowOrigin(map[string]memObject{"hot.js": asset(4096, `"hot"`)})
	r := newRig(t, origin, true, true, burstLimits)
	gate := make(chan struct{})
	origin.setHolds(gate, nil)
	results := make([]timing, burstSize)
	done := make(chan struct{})
	go func() { r.burst(burstSize, "/hot.js", results); close(done) }()
	eventually(t, "every request to wait on the fill", func() bool { return r.cache.Stats().Waiters == burstSize })
	close(gate)
	<-done
	r.settle(t)
	if calls := origin.take(); !slices.Equal(calls, []string{"GET hot.js"}) || origin.fillReads.Load() != 1 {
		t.Fatalf("origin calls %q, %d cache_fill reads", calls, origin.fillReads.Load())
	}
	for i, got := range results {
		if got.status != 200 || got.written != 4096 {
			t.Fatalf("request %d: %d, %d bytes", i, got.status, got.written)
		}
	}
	coalesced := 0
	for _, o := range r.takeObservations() {
		if o.CacheOutcome != cache.OutcomeMiss || o.CacheBytes != 0 {
			t.Fatalf("observation %+v", o)
		}
		if o.CacheCoalesced {
			coalesced++
		}
	}
	if st := r.cache.Stats(); coalesced != burstSize-1 || st.Coalesced != burstSize-1 || st.Fills[cache.FillAdmitted] != 1 {
		t.Fatalf("%d coalesced; stats %+v", coalesced, st)
	}
}

// TestMixedLoadKeepsHotSetResident interleaves a hot set of small assets,
// which fills most of max_bytes, with an object above max_object_bytes. The
// large object streams every time and never enters the cache, and no hot
// entry is evicted or refetched.
func TestMixedLoadKeepsHotSetResident(t *testing.T) {
	const (
		hot        = 16
		assetSize  = 16 << 10
		largeSize  = 1 << 20
		clients    = 8
		perClient  = 40
		largeEvery = 8
	)
	objects := map[string]memObject{"large.bin": asset(largeSize, `"large"`)}
	for i := range hot {
		objects["hot-"+strconv.Itoa(i)+".js"] = asset(assetSize, `"h`+strconv.Itoa(i)+`"`)
	}
	origin := newSlowOrigin(objects)
	origin.chunk, origin.chunkDelay = 32<<10, 100*time.Microsecond
	r := newRig(t, origin, true, true, func(c *config.Config) {
		c.Cache.MaxBytes, c.Cache.MaxEntries = 320<<10, hot+1
		c.Cache.MaxObjectBytes, c.Cache.MaxFillBytes = 64<<10, 128<<10
	})
	versions := map[string]cache.Version{}
	for i := range hot {
		key := "hot-" + strconv.Itoa(i) + ".js"
		if got := r.get("/" + key); got.status != 200 || got.written != assetSize {
			t.Fatalf("priming %s: %+v", key, got)
		}
		r.settle(t)
		v, kind, ok := r.store.Peek(key)
		if !ok || kind != cache.KindBody {
			t.Fatalf("%s not resident", key)
		}
		versions[key] = v
	}
	origin.take()
	r.takeObservations()
	var wg sync.WaitGroup
	var failures atomic.Int64
	for c := range clients {
		wg.Go(func() {
			for i := range perClient {
				path, size := "/hot-"+strconv.Itoa((c*perClient+i)%hot)+".js", int64(assetSize)
				if i%largeEvery == largeEvery-1 {
					path, size = "/large.bin", largeSize
				}
				if got := r.get(path); got.status != 200 || got.written != size {
					failures.Add(1)
				}
			}
		})
	}
	wg.Wait()
	r.settle(t)
	const larges = clients * perClient / largeEvery
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d requests failed", n)
	}
	if calls := origin.take(); len(calls) != larges || slices.ContainsFunc(calls, func(c string) bool { return c != "GET large.bin" }) {
		t.Fatalf("origin calls %q", calls)
	}
	for _, o := range r.takeObservations() {
		switch o.OriginKey {
		case "large.bin":
			if o.CacheOutcome != cache.OutcomeBypass || o.CacheReason != cache.ReasonTooLarge || o.CacheBytes != 0 {
				t.Fatalf("large observation %+v", o)
			}
		default:
			if o.CacheOutcome != cache.OutcomeHit || o.CacheBytes != assetSize {
				t.Fatalf("hot observation %+v", o)
			}
		}
	}
	if _, _, ok := r.store.Peek("large.bin"); ok {
		t.Fatal("the large object entered the cache")
	}
	for key, v := range versions {
		if got, kind, ok := r.store.Peek(key); !ok || kind != cache.KindBody || got != v {
			t.Fatalf("hot entry %s changed or left", key)
		}
	}
	st, store := r.cache.Stats(), r.store.Stats()
	none := map[cache.Kind]uint64{cache.KindBody: 0, cache.KindMetadata: 0, cache.KindNegative: 0}
	if st.Bypasses[cache.ReasonTooLarge] != larges || store.ResidentEntries != hot || !reflect.DeepEqual(store.Evictions, none) || store.FillBytes != 0 {
		t.Fatalf("stats %+v; store %+v", st, store)
	}
}

// Benchmark scenarios share one origin profile: a 5 ms header latency, a hot
// set of small assets and a large object that streams at about 32 MiB/s.
const (
	benchHeader     = 5 * time.Millisecond
	benchHot        = 32
	benchAssetSize  = 16 << 10
	benchLargeSize  = 4 << 20 // above the 2 MiB default max_object_bytes
	benchClients    = 16
	benchColdFanout = 4  // concurrent first touches of each cold key
	benchLargeEvery = 50 // one large download per this many requests
)

func benchOrigin(extra map[string]memObject) *slowOrigin {
	objects := map[string]memObject{"large.bin": asset(benchLargeSize, `"large"`)}
	for i := range benchHot {
		objects[hotKey(i)] = asset(benchAssetSize, `"h`+strconv.Itoa(i)+`"`)
	}
	for k, v := range extra {
		objects[k] = v
	}
	o := newSlowOrigin(objects)
	o.header, o.chunk, o.chunkDelay = benchHeader, 32<<10, time.Millisecond
	return o
}

func hotKey(i int) string { return "hot-" + strconv.Itoa(i%benchHot) + ".js" }

// load makes b.N requests from benchClients goroutines; fn makes request i.
func load(b *testing.B, fn func(i int) timing) []timing {
	results := make([]timing, b.N)
	var next atomic.Int64
	var wg sync.WaitGroup
	for range benchClients {
		wg.Go(func() {
			for i := int(next.Add(1)) - 1; i < b.N; i = int(next.Add(1)) - 1 {
				results[i] = fn(i)
			}
		})
	}
	wg.Wait()
	return results
}

func percentile(ds []time.Duration, p float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	i := int(float64(len(s))*p+0.5) - 1
	return float64(s[min(max(i, 0), len(s)-1)]) / float64(time.Millisecond)
}

// report states origin calls and bytes served from the cache per request, and
// TTFB percentiles over every request and, when the load mixes them, over hot
// assets and large downloads apart.
func report(b *testing.B, r *rig, reads0 int64, results []timing) {
	b.StopTimer()
	var all, hot, large, largeTotal []time.Duration
	for i, t := range results {
		if t.status != http.StatusOK {
			b.Fatalf("request %d: status %d", i, t.status)
		}
		all = append(all, t.ttfb)
		if t.large {
			large, largeTotal = append(large, t.ttfb), append(largeTotal, t.total)
		} else {
			hot = append(hot, t.ttfb)
		}
	}
	n := float64(b.N)
	b.ReportMetric(float64(r.origin.reads.Load()-reads0)/n, "origin_calls/op")
	b.ReportMetric(float64(r.cacheBytes.Load())/n, "cache_B/op")
	b.ReportMetric(percentile(all, 0.50), "p50_ttfb_ms")
	b.ReportMetric(percentile(all, 0.99), "p99_ttfb_ms")
	if len(large) > 0 {
		b.ReportMetric(percentile(hot, 0.50), "hot_p50_ttfb_ms")
		b.ReportMetric(percentile(hot, 0.99), "hot_p99_ttfb_ms")
		b.ReportMetric(percentile(large, 0.50), "large_p50_ttfb_ms")
		b.ReportMetric(percentile(largeTotal, 0.50), "large_p50_total_ms")
	}
}

func prime(b *testing.B, r *rig) {
	for i := range benchHot {
		if got := r.get("/" + hotKey(i)); got.status != http.StatusOK {
			b.Fatalf("priming %s: %d", hotKey(i), got.status)
		}
	}
	r.settle(b)
}

// BenchmarkCacheCapacity measures the D13 warm/cold scenarios through the
// full HTTP handler. One op is one request; see report for the metrics.
//   - uncached: hot assets without a cache, every request reads the origin;
//   - cold: every key is new and first touched by benchColdFanout concurrent
//     requests, as a hot set right after a restart;
//   - warm: the hot set is resident;
//   - expiry_burst: waves of burstSize concurrent requests for one key that
//     has just expired, each wave revalidated by one conditional read;
//   - mixed: the warm hot set plus one large download, which streams, every
//     benchLargeEvery requests.
func BenchmarkCacheCapacity(b *testing.B) {
	b.Run("uncached", func(b *testing.B) {
		r := newRig(b, benchOrigin(nil), false, false, nil)
		reads0 := r.origin.reads.Load()
		b.ReportAllocs()
		b.ResetTimer()
		results := load(b, func(i int) timing { return r.get("/" + hotKey(i)) })
		report(b, r, reads0, results)
	})
	b.Run("cold", func(b *testing.B) {
		extra := map[string]memObject{}
		for k := 0; k <= b.N/benchColdFanout; k++ {
			extra["cold-"+strconv.Itoa(k)+".js"] = asset(benchAssetSize, `"c`+strconv.Itoa(k)+`"`)
		}
		r := newRig(b, benchOrigin(extra), true, false, nil)
		reads0 := r.origin.reads.Load()
		b.ReportAllocs()
		b.ResetTimer()
		results := load(b, func(i int) timing { return r.get("/cold-" + strconv.Itoa(i/benchColdFanout) + ".js") })
		report(b, r, reads0, results)
	})
	b.Run("warm", func(b *testing.B) {
		r := newRig(b, benchOrigin(nil), true, false, nil)
		prime(b, r)
		reads0, bytes0 := r.origin.reads.Load(), r.cacheBytes.Load()
		r.cacheBytes.Add(-bytes0)
		b.ReportAllocs()
		b.ResetTimer()
		results := load(b, func(i int) timing { return r.get("/" + hotKey(i)) })
		report(b, r, reads0, results)
	})
	b.Run("expiry_burst", func(b *testing.B) {
		r := newRig(b, benchOrigin(nil), true, false, burstLimits)
		prime(b, r)
		reads0, bytes0 := r.origin.reads.Load(), r.cacheBytes.Load()
		r.cacheBytes.Add(-bytes0)
		results := make([]timing, b.N)
		b.ReportAllocs()
		b.ResetTimer()
		for done := 0; done < b.N; done += burstSize {
			r.clock.Advance(6 * time.Minute) // past the 5m default TTL
			n := min(burstSize, b.N-done)
			r.burst(n, "/"+hotKey(0), results[done:done+n])
		}
		report(b, r, reads0, results)
	})
	b.Run("mixed", func(b *testing.B) {
		r := newRig(b, benchOrigin(nil), true, false, nil)
		prime(b, r)
		reads0, bytes0 := r.origin.reads.Load(), r.cacheBytes.Load()
		r.cacheBytes.Add(-bytes0)
		b.ReportAllocs()
		b.ResetTimer()
		results := load(b, func(i int) timing {
			if i%benchLargeEvery == benchLargeEvery-1 {
				t := r.get("/large.bin")
				t.large = true
				return t
			}
			return r.get("/" + hotKey(i))
		})
		report(b, r, reads0, results)
	})
}
