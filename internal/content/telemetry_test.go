package content

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

// series names one Prometheus series: name{k1="v1",k2="v2"}.
func series(name string, labels ...string) string {
	if len(labels) == 0 {
		return name
	}
	var b strings.Builder
	b.WriteString(name + "{")
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(labels[i] + "=" + strconv.Quote(labels[i+1]))
	}
	return b.String() + "}"
}

func requests(outcome string) string { return series("cache_requests_total", "outcome", outcome) }
func admissions(result cache.Result) string {
	return series("cache_admissions_total", "result", string(result))
}
func bypasses(reason cache.Reason) string {
	return series("cache_admissions_total", "result", "bypass", "reason", string(reason))
}
func fills(result cache.FillResult) string {
	return series("cache_fill_duration_seconds_count", "result", string(result))
}
func revalidations(result cache.RevalidationResult) string {
	return series("cache_revalidations_total", "result", string(result))
}
func evictions(kind cache.Kind) string { return series("cache_evictions_total", "kind", string(kind)) }
func expired(kind cache.Kind) string   { return series("cache_expirations_total", "kind", string(kind)) }

const responseBytes = "cache_response_bytes_total"

// gauges are the cache metrics whose value, not its change, is the signal.
var gauges = []string{"cache_bytes", "cache_retired_bytes", "cache_entries", "cache_fill_reserved_bytes",
	"cache_fills_in_flight", "cache_waiters", "cache_limit_bytes", "cache_limit_entries", "cache_fill_limit_bytes",
	"cache_fill_limit_concurrency", "cache_object_limit_bytes", "cache_waiter_limit_per_key", "cache_enabled"}

// collector maps each cache metric of the observability design onto its data
// source, as the Milestone 5 collectors will: request-level series from the
// content Observation (whose host comes from the httpserver observation of
// the same request), fill durations from cache.Options.OnFill, and everything
// else from Store.Stats, Cache.Stats and the validated configuration. A
// coalesced request is counted under outcome="coalesced" rather than under
// the outcome its flight produced, so that outcomes partition requests.
type collector struct {
	cfg   config.Cache
	cache *cache.Cache
	store *cache.Store

	mu   sync.Mutex
	fed  map[string]float64 // series fed by callbacks
	peak map[string]float64 // the largest value each gauge was sampled at
}

func (m *collector) observe(o Observation) {
	if o.CacheOutcome == "" {
		return
	}
	outcome := string(o.CacheOutcome)
	if o.CacheCoalesced {
		outcome = "coalesced"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fed[requests(outcome)]++
	m.fed[responseBytes] += float64(o.CacheBytes)
}

func (m *collector) onFill(e cache.FillEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fed[fills(e.Result)]++
	m.fed["cache_fill_duration_seconds_sum"] += e.Duration.Seconds()
}

// sample reads every series and records gauge peaks.
func (m *collector) sample() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := maps.Clone(m.fed)
	st, fs := m.store.Stats(), m.cache.Stats()
	out["cache_bytes"] = float64(st.ChargedBytes)
	out["cache_retired_bytes"] = float64(st.RetiredBytes)
	out["cache_entries"] = float64(st.ResidentEntries + st.RetiredEntries)
	out["cache_fill_reserved_bytes"] = float64(st.FillBytes)
	out["cache_fills_in_flight"] = float64(fs.InFlight)
	out["cache_waiters"] = float64(fs.Waiters)
	for r, n := range st.Admissions {
		out[admissions(r)] = float64(n)
	}
	for r, n := range fs.Bypasses {
		out[bypasses(r)] = float64(n)
	}
	for k, n := range st.Evictions {
		out[evictions(k)] = float64(n)
	}
	for k, n := range st.Expirations {
		out[expired(k)] = float64(n)
	}
	for r, n := range fs.Revalidations {
		out[revalidations(r)] = float64(n)
	}
	out["cache_limit_bytes"] = float64(st.Limits.MaxBytes)
	out["cache_limit_entries"] = float64(st.Limits.MaxEntries)
	out["cache_fill_limit_bytes"] = float64(st.Limits.MaxFillBytes)
	out["cache_object_limit_bytes"] = float64(st.Limits.MaxObjectBytes)
	out["cache_fill_limit_concurrency"] = float64(m.cfg.MaxConcurrentFills)
	out["cache_waiter_limit_per_key"] = float64(m.cfg.MaxWaitersPerKey)
	out["cache_enabled"] = 0
	if m.cfg.Enabled {
		out["cache_enabled"] = 1
	}
	for _, g := range gauges {
		m.peak[g] = max(m.peak[g], out[g])
	}
	return out
}

type telemetryRig struct {
	*collector
	origin  *slowOrigin
	handler http.Handler
	clock   *testClock
}

func newTelemetryRig(t *testing.T, mutate func(*config.Config)) *telemetryRig {
	cfg := cachedConfig(t, mutate)
	origin := &slowOrigin{memOrigin: newMemOrigin()}
	r := &telemetryRig{collector: &collector{cfg: cfg.Cache, fed: map[string]float64{}, peak: map[string]float64{}}, origin: origin, clock: &testClock{t: time.Now()}}
	store, err := cache.NewStore(cache.LimitsFrom(cfg.Cache))
	if err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	c, err := cache.New(cache.Options{Store: store, Origin: origin, Config: cfg.Cache, Prefix: cfg.S3.Prefix,
		ConditionalReads: cfg.S3.Features.ConditionalReads, Now: r.clock.Now, Lifetime: lifetime, OnFill: r.onFill})
	if err != nil {
		t.Fatal(err)
	}
	r.cache, r.store = c, store
	r.handler = boundary(t, cfg, origin, WithCache(c), WithObserver(r.observe))
	return r
}

func (r *telemetryRig) serve(t *testing.T, method, path string, status int, header ...string) {
	t.Helper()
	h := http.Header{}
	for i := 0; i+1 < len(header); i += 2 {
		h.Set(header[i], header[i+1])
	}
	if w := serve(r.handler, method, "http://example.org"+path, h); w.status != status {
		t.Fatalf("%s %s %v: status %d, want %d", method, path, h, w.status, status)
	}
}

// telemetryStep runs one scripted action. counts are the exact changes of
// every counter series; any series not listed must not move. gauges are exact
// gauge values once the action's flights have unwound.
type telemetryStep struct {
	name   string
	do     func(t *testing.T, r *telemetryRig)
	counts map[string]float64
	gauges map[string]float64
}

// play runs steps and returns the series that moved.
func (r *telemetryRig) play(t *testing.T, steps []telemetryStep) map[string]bool {
	moved := map[string]bool{}
	before := r.sample()
	for _, s := range steps {
		s.do(t, r)
		eventually(t, s.name+" flights to unwind", func() bool {
			st := r.cache.Stats()
			return st.InFlight == 0 && st.Waiters == 0
		})
		after := r.sample()
		got := map[string]float64{}
		for k, v := range after {
			if d := v - before[k]; d != 0 {
				moved[k] = true
				if !slices.Contains(gauges, k) && k != "cache_fill_duration_seconds_sum" {
					got[k] = d
				}
			}
		}
		if !maps.Equal(got, s.counts) {
			t.Fatalf("%s: counters moved by\n%v\nwant\n%v", s.name, got, s.counts)
		}
		for g, want := range s.gauges {
			if after[g] != want {
				t.Fatalf("%s: %s = %v, want %v", s.name, g, after[g], want)
			}
		}
		before = after
	}
	return moved
}

func setObject(body, etag string) func(*testing.T, *telemetryRig) {
	return func(_ *testing.T, r *telemetryRig) {
		r.origin.set("file.txt", memObject{body, fixtureMeta(etag, "", 0)})
	}
}

// answerConditional makes conditional reads of file.txt fail with err.
func answerConditional(r *telemetryRig, err error) {
	r.origin.setHook(func(_ context.Context, key string, opts storage.ReadOptions) error {
		if key == "file.txt" && opts.IfNoneMatch != "" {
			return err
		}
		return nil
	})
}

// TestCacheTelemetryIsComplete is the evidence that every cache metric of
// the observability design has an exact data source. It drives the full HTTP
// handler through scripted steps that move every series, checking the exact
// change of every counter at each step, then checks that each metric row of
// the design was observed moving, or holds its configured value.
func TestCacheTelemetryIsComplete(t *testing.T) {
	const expire = 6 * time.Minute // past the 5m default TTL and the 1m negative_ttl
	advance := func(d time.Duration) func(*testing.T, *telemetryRig) {
		return func(t *testing.T, r *telemetryRig) { r.clock.Advance(d) }
	}
	then := func(actions ...func(*testing.T, *telemetryRig)) func(*testing.T, *telemetryRig) {
		return func(t *testing.T, r *telemetryRig) {
			for _, a := range actions {
				a(t, r)
			}
		}
	}
	get := func(path string, status int, header ...string) func(*testing.T, *telemetryRig) {
		return func(t *testing.T, r *telemetryRig) { r.serve(t, "GET", path, status, header...) }
	}
	head := func(path string, status int) func(*testing.T, *telemetryRig) {
		return func(t *testing.T, r *telemetryRig) { r.serve(t, "HEAD", path, status) }
	}
	refreshed := map[string]float64{revalidations(cache.RevalidationNotModified): 1, fills(cache.FillRefreshed): 1, admissions(cache.ResultRefreshed): 1, requests("revalidated"): 1, responseBytes: 11}
	with := func(base map[string]float64, extra ...any) map[string]float64 {
		out := maps.Clone(base)
		for i := 0; i+1 < len(extra); i += 2 {
			out[extra[i].(string)] = float64(extra[i+1].(int))
		}
		return out
	}
	main := newTelemetryRig(t, func(c *config.Config) { c.Cache.StaleIfError = time.Hour })
	moved := main.play(t, []telemetryStep{
		{name: "cold GET fills", do: get("/file.txt", 200),
			counts: map[string]float64{requests("miss"): 1, admissions(cache.ResultAdmitted): 1, fills(cache.FillAdmitted): 1},
			gauges: map[string]float64{"cache_entries": 1, "cache_retired_bytes": 0, "cache_fill_reserved_bytes": 0}},
		{name: "warm GET hits", do: get("/file.txt", 200), counts: map[string]float64{requests("hit"): 1, responseBytes: 11}},
		{name: "HEAD from memory", do: head("/file.txt", 200), counts: map[string]float64{requests("metadata_hit"): 1}},
		{name: "304 from memory", do: get("/file.txt", 304, "If-None-Match", `"f1"`), counts: map[string]float64{requests("hit"): 1}},
		// The built-in 404 confirms missing.txt and missing.txt/index.html
		// absent, and its observation reports the last lookup.
		{name: "confirmed absence", do: get("/missing.txt", 404),
			counts: map[string]float64{requests("miss"): 1, admissions(cache.ResultAdmitted): 2, fills(cache.FillNotFound): 2},
			gauges: map[string]float64{"cache_entries": 3}},
		{name: "negative hits", do: then(get("/missing.txt", 404), head("/missing.txt", 404)), counts: map[string]float64{requests("negative_hit"): 2}},
		{name: "request no-store", do: get("/file.txt", 200, "Cache-Control", "no-store"),
			counts: map[string]float64{requests("bypass"): 1, bypasses(cache.ReasonRequestNoStore): 1}},
		{name: "disabled rule", do: get("/nocache.txt", 200), counts: map[string]float64{requests("bypass"): 1, bypasses(cache.ReasonDisabled): 1}},
		{name: "large object streams", do: get("/big.bin", 200),
			counts: map[string]float64{requests("bypass"): 1, bypasses(cache.ReasonTooLarge): 1, fills(cache.FillStreamed): 1}},
		// A range reads metadata, which the next range reuses: neither
		// starts a fill the large object would stream.
		{name: "large object ranges read metadata", do: then(get("/big.bin", 206, "Range", "bytes=0-3"), get("/big.bin", 206, "Range", "bytes=4-7")),
			counts: map[string]float64{requests("miss"): 1, requests("metadata_hit"): 1, admissions(cache.ResultAdmitted): 1}},
		{name: "forced revalidation expires nothing", do: get("/file.txt", 200, "Cache-Control", "no-cache"), counts: refreshed},
		{name: "expired entry revalidates", do: then(advance(expire), get("/file.txt", 200)), counts: with(refreshed, expired(cache.KindBody), 1)},
		{name: "changed object replaces", do: then(setObject("second", `"f2"`), advance(expire), get("/file.txt", 200)),
			counts: map[string]float64{expired(cache.KindBody): 1, revalidations(cache.RevalidationModified): 1, fills(cache.FillAdmitted): 1, admissions(cache.ResultReplaced): 1, requests("miss"): 1}},
		{name: "changed entity-tag refetches", do: then(setObject("third", `"f3"`), func(_ *testing.T, r *telemetryRig) {
			answerConditional(r, &storage.Error{Kind: storage.NotModified, Size: -1, Metadata: &storage.Metadata{Size: -1, ETag: `"other"`}})
		}, advance(expire), get("/file.txt", 200)),
			counts: map[string]float64{expired(cache.KindBody): 1, revalidations(cache.RevalidationETagChanged): 1, fills(cache.FillAdmitted): 1, admissions(cache.ResultReplaced): 1, requests("miss"): 1}},
		{name: "outage serves stale", do: then(func(_ *testing.T, r *telemetryRig) {
			answerConditional(r, &storage.Error{Kind: storage.Unavailable, Size: -1})
		}, advance(expire), get("/file.txt", 200)),
			counts: map[string]float64{expired(cache.KindBody): 1, revalidations(cache.RevalidationError): 1, fills(cache.FillFailed): 1, requests("stale"): 1, responseBytes: 5}},
		// The same expired version: no second expiration. The 404 then also
		// confirms file.txt/index.html absent.
		{name: "deleted object is missing", do: then(func(_ *testing.T, r *telemetryRig) {
			answerConditional(r, &storage.Error{Kind: storage.NotFound, Size: -1})
		}, get("/file.txt", 404)),
			counts: map[string]float64{revalidations(cache.RevalidationMissing): 1, fills(cache.FillNotFound): 2, admissions(cache.ResultReplaced): 1, admissions(cache.ResultAdmitted): 1, requests("miss"): 1}},
		{name: "cold HEAD stores metadata", do: then(head("/style.css", 200), head("/style.css", 200)),
			counts: map[string]float64{requests("miss"): 1, requests("metadata_hit"): 1, admissions(cache.ResultAdmitted): 1}},
		{name: "expired metadata", do: then(advance(expire), head("/style.css", 200)),
			counts: map[string]float64{expired(cache.KindMetadata): 1, admissions(cache.ResultReplaced): 1, requests("miss"): 1}},
		{name: "expired negatives", do: get("/missing.txt", 404),
			counts: map[string]float64{expired(cache.KindNegative): 2, fills(cache.FillNotFound): 2, admissions(cache.ResultReplaced): 2, requests("miss"): 1}},
		{name: "coalesced fill", do: func(t *testing.T, r *telemetryRig) {
			const n = 4
			r.origin.set("popular.txt", memObject{strings.Repeat("p", 100), fixtureMeta(`"pop"`, "", 0)})
			gate := make(chan struct{})
			r.origin.setHolds(nil, gate)
			var wg sync.WaitGroup
			for range n {
				wg.Go(func() { r.serve(t, "GET", "/popular.txt", 200) })
			}
			eventually(t, "the waiters and the reserved fill", func() bool {
				st := r.sample()
				return st["cache_waiters"] == n && st["cache_fill_reserved_bytes"] == 100 && st["cache_fills_in_flight"] == 1
			})
			close(gate)
			wg.Wait()
			r.origin.setHolds(nil, nil)
		},
			counts: map[string]float64{requests("miss"): 1, requests("coalesced"): 3, admissions(cache.ResultAdmitted): 1, fills(cache.FillAdmitted): 1},
			gauges: map[string]float64{"cache_fills_in_flight": 0, "cache_waiters": 0, "cache_fill_reserved_bytes": 0}},
		{name: "replaced while pinned", do: func(t *testing.T, r *telemetryRig) {
			pin, err := r.cache.Get(context.Background(), "popular.txt", httpcache.Request{})
			if err != nil || pin.Outcome != cache.OutcomeHit {
				t.Fatalf("pin %s, %v", pin.Outcome, err)
			}
			r.origin.set("popular.txt", memObject{strings.Repeat("q", 100), fixtureMeta(`"pop2"`, "", 0)})
			r.serve(t, "GET", "/popular.txt", 200, "Cache-Control", "no-cache")
			if st := r.sample(); st["cache_retired_bytes"] <= 100 {
				t.Fatalf("retired bytes %v while the replaced entry is pinned", st["cache_retired_bytes"])
			}
			pin.Release()
		},
			counts: map[string]float64{revalidations(cache.RevalidationModified): 1, fills(cache.FillAdmitted): 1, admissions(cache.ResultReplaced): 1, requests("miss"): 1},
			gauges: map[string]float64{"cache_retired_bytes": 0}},
	})
	if main.origin.fillReads.Load() == 0 || main.origin.reads.Load() == main.origin.fillReads.Load() {
		t.Fatalf("origin reads %d, of which cache_fill %d: want both purposes", main.origin.reads.Load(), main.origin.fillReads.Load())
	}

	// A one-entry cache evicts one entry of each kind in turn.
	small := newTelemetryRig(t, func(c *config.Config) { c.Cache.MaxEntries = 1 })
	for k := range small.play(t, []telemetryStep{
		{name: "metadata entry", do: head("/file.txt", 200), counts: map[string]float64{requests("miss"): 1, admissions(cache.ResultAdmitted): 1}},
		{name: "negatives evict metadata, then each other", do: get("/missing.txt", 404),
			counts: map[string]float64{requests("miss"): 1, admissions(cache.ResultAdmitted): 2, fills(cache.FillNotFound): 2, evictions(cache.KindMetadata): 1, evictions(cache.KindNegative): 1}},
		{name: "body evicts negative", do: get("/cc.txt", 200),
			counts: map[string]float64{requests("miss"): 1, admissions(cache.ResultAdmitted): 1, fills(cache.FillAdmitted): 1, evictions(cache.KindNegative): 1}},
		{name: "body evicts body", do: get("/style.css", 200),
			counts: map[string]float64{requests("miss"): 1, admissions(cache.ResultAdmitted): 1, fills(cache.FillAdmitted): 1, evictions(cache.KindBody): 1},
			gauges: map[string]float64{"cache_entries": 1}},
	}) {
		moved[k] = true
	}

	peak := func(g string) float64 { return max(main.peak[g], small.peak[g]) }
	for _, row := range []struct {
		metric string
		moved  []string // series that must have moved
		peaked []string // gauges that must have been sampled above zero
	}{
		{metric: "cache_requests_total", moved: []string{requests("hit"), requests("metadata_hit"), requests("miss"), requests("negative_hit"), requests("revalidated"), requests("stale"), requests("coalesced"), requests("bypass")}},
		{metric: "cache_response_bytes_total", moved: []string{responseBytes}},
		{metric: "cache_bytes", peaked: []string{"cache_bytes"}},
		{metric: "cache_retired_bytes", peaked: []string{"cache_retired_bytes"}},
		{metric: "cache_entries", peaked: []string{"cache_entries"}},
		{metric: "cache_fill_reserved_bytes", peaked: []string{"cache_fill_reserved_bytes"}},
		{metric: "cache_fills_in_flight / cache_waiters", peaked: []string{"cache_fills_in_flight", "cache_waiters"}},
		{metric: "cache_fill_duration_seconds", moved: []string{"cache_fill_duration_seconds_sum", fills(cache.FillAdmitted), fills(cache.FillRefreshed), fills(cache.FillStreamed), fills(cache.FillNotFound), fills(cache.FillFailed)}},
		{metric: "cache_admissions_total", moved: []string{admissions(cache.ResultAdmitted), admissions(cache.ResultReplaced), admissions(cache.ResultRefreshed), bypasses(cache.ReasonRequestNoStore), bypasses(cache.ReasonDisabled), bypasses(cache.ReasonTooLarge)}},
		{metric: "cache_evictions_total", moved: []string{evictions(cache.KindBody), evictions(cache.KindMetadata), evictions(cache.KindNegative)}},
		{metric: "cache_expirations_total", moved: []string{expired(cache.KindBody), expired(cache.KindMetadata), expired(cache.KindNegative)}},
		{metric: "cache_revalidations_total", moved: []string{revalidations(cache.RevalidationNotModified), revalidations(cache.RevalidationETagChanged), revalidations(cache.RevalidationModified), revalidations(cache.RevalidationMissing), revalidations(cache.RevalidationError)}},
	} {
		for _, s := range row.moved {
			if !moved[s] {
				t.Errorf("%s: %s never moved", row.metric, s)
			}
		}
		for _, g := range row.peaked {
			if peak(g) <= 0 {
				t.Errorf("%s: %s never sampled above zero", row.metric, g)
			}
		}
	}
	// The limits and enablement are the effective configuration, and never move.
	st := main.sample()
	for g, want := range map[string]float64{"cache_limit_bytes": float64(main.cfg.MaxBytes), "cache_limit_entries": float64(main.cfg.MaxEntries),
		"cache_fill_limit_bytes": float64(main.cfg.MaxFillBytes), "cache_fill_limit_concurrency": float64(main.cfg.MaxConcurrentFills),
		"cache_object_limit_bytes": float64(main.cfg.MaxObjectBytes), "cache_waiter_limit_per_key": float64(main.cfg.MaxWaitersPerKey), "cache_enabled": 1} {
		if st[g] != want || moved[g] || main.peak[g] != want {
			t.Errorf("%s = %v (peak %v, moved %t), want %v", g, st[g], main.peak[g], moved[g], want)
		}
	}
}
