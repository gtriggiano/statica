package metrics

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/admin"
	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/storage"
	"github.com/gtriggiano/statica/internal/storage/s3"
	"github.com/gtriggiano/statica/internal/telemetry"
	dto "github.com/prometheus/client_model/go"
)

func parseConfig(t testing.TB, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("s3:\n  bucket: test-bucket\n  region: eu-west-1\n"+extra), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newMetrics(t testing.TB, cfg *config.Config) *Metrics {
	t.Helper()
	if cfg == nil {
		cfg = parseConfig(t, "")
	}
	m, err := New(cfg, BuildInfo{Version: "v1.2.3", Revision: "abc123", GoVersion: "go1.27.3"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// series renders a series key with its label pairs sorted by name, as
// scrape renders gathered series.
func series(name string, pairs ...string) string {
	if len(pairs) == 0 {
		return name
	}
	type pair struct{ k, v string }
	var ps []pair
	for i := 0; i < len(pairs); i += 2 {
		ps = append(ps, pair{pairs[i], pairs[i+1]})
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].k < ps[j].k })
	var b strings.Builder
	b.WriteString(name + "{")
	for i, p := range ps {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", p.k, p.v)
	}
	return b.String() + "}"
}

func gather(t testing.TB, m *Metrics) []*dto.MetricFamily {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	return families
}

// scrape gathers every series: counters, gauges and untyped values by name,
// histograms as name_count and name_sum.
func scrape(t testing.TB, m *Metrics) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, f := range gather(t, m) {
		for _, metric := range f.GetMetric() {
			var pairs []string
			for _, l := range metric.GetLabel() {
				pairs = append(pairs, l.GetName(), l.GetValue())
			}
			name := f.GetName()
			switch f.GetType() {
			case dto.MetricType_COUNTER:
				out[series(name, pairs...)] = metric.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				out[series(name, pairs...)] = metric.GetGauge().GetValue()
			case dto.MetricType_UNTYPED:
				out[series(name, pairs...)] = metric.GetUntyped().GetValue()
			case dto.MetricType_HISTOGRAM:
				out[series(name+"_count", pairs...)] = float64(metric.GetHistogram().GetSampleCount())
				out[series(name+"_sum", pairs...)] = metric.GetHistogram().GetSampleSum()
			case dto.MetricType_SUMMARY:
				out[series(name+"_count", pairs...)] = float64(metric.GetSummary().GetSampleCount())
				out[series(name+"_sum", pairs...)] = metric.GetSummary().GetSampleSum()
			}
		}
	}
	return out
}

// changes reports the statica series that appeared or changed value.
func changes(before, after map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range after {
		if !strings.HasPrefix(k, "statica_") {
			continue
		}
		if old, ok := before[k]; !ok || old != v {
			out[k] = v - before[k]
		}
	}
	return out
}

func assertChanges(t *testing.T, step string, got, want map[string]float64) {
	t.Helper()
	for k, v := range want {
		g, ok := got[k]
		if !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s: %s changed by %v (present %v), want %v", step, k, g, ok, v)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected change %s by %v", step, k, v)
		}
	}
}

var epoch = time.Unix(1_800_000_000, 0)

// admitted returns a finalized admitted record: 50ms long and, when it has a
// status, committed after 10ms.
func admitted(host, method string, status int, outcome string) *telemetry.Record {
	r := &telemetry.Record{Method: method, Host: host, Status: status, Outcome: outcome, Started: epoch, Admitted: epoch, Finished: epoch.Add(50 * time.Millisecond)}
	if status != 0 {
		r.FirstByte = epoch.Add(10 * time.Millisecond)
	}
	return r
}

func with(r *telemetry.Record, change func(*telemetry.Record)) *telemetry.Record {
	change(r)
	return r
}

func requests(host, method, status, outcome string) string {
	return series("statica_http_requests_total", "host", host, "method", method, "status", status, "outcome", outcome)
}
func durationCount(host, method string) string {
	return series("statica_http_request_duration_seconds_count", "host", host, "method", method)
}
func durationSum(host, method string) string {
	return series("statica_http_request_duration_seconds_sum", "host", host, "method", method)
}
func ttfbCount(host string) string {
	return series("statica_http_time_to_first_byte_seconds_count", "host", host)
}
func ttfbSum(host string) string {
	return series("statica_http_time_to_first_byte_seconds_sum", "host", host)
}
func responseBytes(host string) string {
	return series("statica_http_response_bytes_total", "host", host)
}
func cacheRequests(host, outcome string) string {
	return series("statica_cache_requests_total", "host", host, "outcome", outcome)
}
func cacheBytes(host string) string {
	return series("statica_cache_response_bytes_total", "host", host)
}
func streamFailures(host, reason string) string {
	return series("statica_stream_failures_total", "host", host, "reason", reason)
}
func rejected(reason string) string {
	return series("statica_http_rejected_requests_total", "reason", reason)
}

// exchange is the change of the common series of one admitted request.
func exchange(host, method, status, outcome string, bytes float64, committed bool) map[string]float64 {
	out := map[string]float64{requests(host, method, status, outcome): 1, durationCount(host, method): 1, durationSum(host, method): .05, responseBytes(host): bytes}
	if committed {
		out[ttfbCount(host)], out[ttfbSum(host)] = 1, .01
	}
	return out
}

func merge(ms ...map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, m := range ms {
		for k, v := range m {
			out[k] += v
		}
	}
	return out
}

func TestRecordScenario(t *testing.T) {
	m := newMetrics(t, nil)
	steps := []struct {
		name   string
		record *telemetry.Record
		want   map[string]float64
	}{
		{"GET hit", with(admitted("a.example", "GET", 200, "success"), func(r *telemetry.Record) {
			r.BytesWritten, r.CacheOutcome, r.CacheBytes = 1000, "hit", 1000
		}), merge(exchange("a.example", "GET", "200", "success", 1000, true), map[string]float64{cacheRequests("a.example", "hit"): 1, cacheBytes("a.example"): 1000})},

		// The response bytes series already exists, so an empty body changes
		// nothing; HEAD is its own method.
		{"HEAD metadata hit", with(admitted("a.example", "HEAD", 200, "success"), func(r *telemetry.Record) { r.CacheOutcome = "metadata_hit" }),
			map[string]float64{requests("a.example", "HEAD", "200", "success"): 1, durationCount("a.example", "HEAD"): 1, durationSum("a.example", "HEAD"): .05,
				ttfbCount("a.example"): 1, ttfbSum("a.example"): .01, cacheRequests("a.example", "metadata_hit"): 1}},

		{"incomplete stream", with(admitted("a.example", "GET", 200, "incomplete"), func(r *telemetry.Record) {
			r.BytesWritten, r.TerminationReason, r.CacheOutcome = 300, "client_write_timeout", "miss"
		}), merge(exchange("a.example", "GET", "200", "incomplete", 300, true), map[string]float64{streamFailures("a.example", "client_write_timeout"): 1, cacheRequests("a.example", "miss"): 1})},

		{"abort before commit", with(admitted("a.example", "GET", 0, "incomplete"), func(r *telemetry.Record) { r.TerminationReason = "disconnect" }),
			map[string]float64{requests("a.example", "GET", "0", "incomplete"): 1, durationCount("a.example", "GET"): 1, durationSum("a.example", "GET"): .05,
				streamFailures("a.example", "disconnect"): 1}},

		{"handler panic", with(admitted("a.example", "GET", 200, "incomplete"), func(r *telemetry.Record) { r.Reason = "handler_panic" }),
			map[string]float64{requests("a.example", "GET", "200", "incomplete"): 1, durationCount("a.example", "GET"): 1, durationSum("a.example", "GET"): .05,
				ttfbCount("a.example"): 1, ttfbSum("a.example"): .01, streamFailures("a.example", "handler_panic"): 1}},

		{"masked origin failure", with(admitted("a.example", "GET", 404, "origin_failure"), func(r *telemetry.Record) {
			r.BytesWritten, r.ErrorClass, r.CacheOutcome = 9, "origin_denied", "miss"
		}), map[string]float64{requests("a.example", "GET", "404", "origin_failure"): 1, durationCount("a.example", "GET"): 1, durationSum("a.example", "GET"): .05,
			ttfbCount("a.example"): 1, ttfbSum("a.example"): .01, responseBytes("a.example"): 9, cacheRequests("a.example", "miss"): 1}},

		{"coalesced miss", with(admitted("b.example", "GET", 200, "success"), func(r *telemetry.Record) {
			r.BytesWritten, r.CacheOutcome, r.CacheCoalesced = 10, "miss", true
		}), merge(exchange("b.example", "GET", "200", "success", 10, true), map[string]float64{cacheRequests("b.example", "coalesced"): 1, cacheBytes("b.example"): 0})},

		{"stale with cache bytes", with(admitted("b.example", "GET", 206, "success"), func(r *telemetry.Record) {
			r.BytesWritten, r.CacheOutcome, r.CacheBytes = 4, "stale", 4
		}), merge(exchange("b.example", "GET", "206", "success", 4, true), map[string]float64{cacheRequests("b.example", "stale"): 1, cacheBytes("b.example"): 4})},

		{"OPTIONS and other methods", with(admitted("b.example", "OPTIONS", 204, "success"), func(*telemetry.Record) {}),
			map[string]float64{requests("b.example", "OPTIONS", "204", "success"): 1, durationCount("b.example", "OPTIONS"): 1, durationSum("b.example", "OPTIONS"): .05,
				ttfbCount("b.example"): 1, ttfbSum("b.example"): .01}},
		{"OTHER method", with(admitted("b.example", "PROPFIND", 200, "success"), func(*telemetry.Record) {}),
			map[string]float64{requests("b.example", "OTHER", "200", "success"): 1, durationCount("b.example", "OTHER"): 1, durationSum("b.example", "OTHER"): .05,
				ttfbCount("b.example"): 1, ttfbSum("b.example"): .01}},

		{"rejected unknown host", &telemetry.Record{Method: "GET", Authority: "evil.example", Rejected: true, Reason: "unknown_host", Status: 421, Started: epoch, Finished: epoch, FirstByte: epoch},
			map[string]float64{rejected("unknown_host"): 1}},
		// A request rejected after host admission still has no host label.
		{"rejected method", &telemetry.Record{Method: "DELETE", Host: "a.example", Rejected: true, Reason: "unsupported_method", Status: 405, BytesWritten: 20, Started: epoch, Finished: epoch, FirstByte: epoch},
			map[string]float64{rejected("unsupported_method"): 1}},
		{"rejected overload", &telemetry.Record{Method: "GET", Rejected: true, Reason: "request_capacity", Status: 503, Started: epoch, Finished: epoch, FirstByte: epoch},
			map[string]float64{rejected("request_capacity"): 1}},
	}
	before := scrape(t, m)
	for _, step := range steps {
		m.RecordSink(step.record)
		after := scrape(t, m)
		assertChanges(t, step.name, changes(before, after), step.want)
		before = after
	}
}

func TestInFlightGauge(t *testing.T) {
	m := newMetrics(t, nil)
	key := "statica_http_requests_in_flight"
	if got := scrape(t, m)[key]; got != 0 {
		t.Fatalf("initial in-flight %v", got)
	}
	m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationStart, Inflight: 1})
	m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationStart, Inflight: 2})
	// An overload rejection never acquired a slot; an admitted rejection
	// still finishes its slot.
	m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationReject, Status: 503, Reason: "request_capacity", Inflight: 2})
	if got := scrape(t, m)[key]; got != 2 {
		t.Fatalf("in-flight %v want 2", got)
	}
	// Snapshots delivered out of order do not leave the gauge stale.
	m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationFinish, Host: "a.example", Status: 200, Inflight: 0})
	m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationFinish, Host: "a.example", Status: 200, Inflight: 1})
	if got := scrape(t, m)[key]; got != 0 {
		t.Fatalf("in-flight %v want 0", got)
	}
}

type fakeOrigin struct {
	mu    sync.Mutex
	calls int
}

func (o *fakeOrigin) Get(ctx context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
	o.mu.Lock()
	o.calls++
	o.mu.Unlock()
	if key == "missing" {
		return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
	}
	return &storage.Object{Metadata: storage.Metadata{Size: 5, ETag: `"v1"`, Checksum: storage.ChecksumUnavailable}, Body: io.NopCloser(strings.NewReader("hello"))}, nil
}

func (o *fakeOrigin) Head(ctx context.Context, key string) (storage.Metadata, error) {
	return storage.Metadata{Size: 5, ETag: `"v1"`, Checksum: storage.ChecksumUnavailable}, nil
}

func admissions(result, reason string) string {
	return series("statica_cache_admissions_total", "result", result, "reason", reason)
}

func TestCacheCollectorReflectsStats(t *testing.T) {
	cfg := parseConfig(t, "cache:\n  max_bytes: 1MiB\n  max_entries: 100\n  max_object_bytes: 64KiB\n  max_fill_bytes: 256KiB\n  max_concurrent_fills: 3\n  max_waiters_per_key: 7\n")
	m := newMetrics(t, cfg)
	c, err := cache.FromConfig(context.Background(), cfg, &fakeOrigin{}, func(o *cache.Options) { o.OnFill = m.OnFill })
	if err != nil || c == nil {
		t.Fatalf("cache %v %v", c, err)
	}
	if err := m.RegisterCache(c, cfg.Cache); err != nil {
		t.Fatal(err)
	}
	got := scrape(t, m)
	want := map[string]float64{
		"statica_cache_enabled": 1, "statica_cache_bytes": 0, "statica_cache_retired_bytes": 0, "statica_cache_entries": 0,
		"statica_cache_fill_reserved_bytes": 0, "statica_cache_fills_in_flight": 0, "statica_cache_waiters": 0,
		"statica_cache_limit_bytes": 1 << 20, "statica_cache_limit_entries": 100, "statica_cache_object_limit_bytes": 64 << 10,
		"statica_cache_fill_limit_bytes": 256 << 10, "statica_cache_fill_limit_concurrency": 3, "statica_cache_waiter_limit_per_key": 7,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v want %v", k, got[k], v)
		}
	}
	// Every label of every cache counter exists at zero from the start.
	for _, r := range []string{"admitted", "replaced", "refreshed", "too_large", "no_space", "invalid", "conflict"} {
		if v, ok := got[admissions(r, "none")]; !ok || v != 0 {
			t.Errorf("missing initial admissions result %s", r)
		}
	}
	for _, r := range []string{"disabled", "request_no_store", "shutdown", "fills_full", "waiters_full", "unknown_size", "too_large", "uncacheable", "fill_budget"} {
		if v, ok := got[admissions("bypass", r)]; !ok || v != 0 {
			t.Errorf("missing initial bypass reason %s", r)
		}
	}
	for _, k := range []string{"body", "metadata", "negative"} {
		for _, name := range []string{"statica_cache_evictions_total", "statica_cache_expirations_total"} {
			if _, ok := got[series(name, "kind", k)]; !ok {
				t.Errorf("missing %s{kind=%q}", name, k)
			}
		}
	}
	for _, r := range []string{"not_modified", "etag_changed", "modified", "missing", "error"} {
		if _, ok := got[series("statica_cache_revalidations_total", "result", r)]; !ok {
			t.Errorf("missing revalidation result %s", r)
		}
	}
	for _, r := range []string{"admitted", "detached", "refreshed", "unstored", "streamed", "not_found", "denied", "timeout", "canceled", "failed"} {
		if _, ok := got[series("statica_cache_fill_duration_seconds_count", "result", r)]; !ok {
			t.Errorf("missing fill duration result %s", r)
		}
	}

	get := func(key string, req httpcache.Request) {
		l, err := c.Get(context.Background(), key, req)
		if l != nil {
			if o := l.TakeObject(); o != nil {
				_, _ = io.Copy(io.Discard, o.Body)
				_ = o.Body.Close()
			}
			l.Release()
		}
		if err != nil && key != "missing" {
			t.Fatal(err)
		}
	}
	before := got
	get("index.html", httpcache.Request{})              // miss: one admitted fill
	get("index.html", httpcache.Request{})              // hit
	get("index.html", httpcache.Request{NoStore: true}) // bypass
	get("missing", httpcache.Request{})                 // negative fill
	after := scrape(t, m)
	st := c.StoreStats()
	diff := changes(before, after)
	wantDiff := map[string]float64{
		admissions("admitted", "none"):           2,
		admissions("bypass", "request_no_store"): 1,
		"statica_cache_bytes":                    float64(st.ChargedBytes),
		"statica_cache_entries":                  2,
		series("statica_cache_fill_duration_seconds_count", "result", "admitted"):  1,
		series("statica_cache_fill_duration_seconds_count", "result", "not_found"): 1,
	}
	for k, v := range wantDiff {
		if diff[k] != v {
			t.Errorf("%s changed by %v want %v", k, diff[k], v)
		}
	}
	if st.ChargedBytes <= 0 || st.ResidentEntries != 2 {
		t.Fatalf("store stats %+v", st)
	}
	for k := range diff {
		if _, ok := wantDiff[k]; !ok && !strings.HasPrefix(k, "statica_cache_fill_duration_seconds_sum") {
			t.Errorf("unexpected change %s by %v", k, diff[k])
		}
	}
}

func TestDisabledCacheExportsOnlyEnabledGauge(t *testing.T) {
	cfg := parseConfig(t, "cache:\n  enabled: false\n")
	m := newMetrics(t, cfg)
	c, err := cache.FromConfig(context.Background(), cfg, &fakeOrigin{}, func(o *cache.Options) { o.OnFill = m.OnFill })
	if err != nil || c != nil {
		t.Fatalf("disabled cache %v %v", c, err)
	}
	if err := m.RegisterCache(c, cfg.Cache); err != nil {
		t.Fatal(err)
	}
	m.RecordSink(admitted("a.example", "GET", 200, "success"))
	var cacheSeries []string
	for k, v := range scrape(t, m) {
		if strings.HasPrefix(k, "statica_cache_") {
			cacheSeries = append(cacheSeries, fmt.Sprintf("%s=%v", k, v))
		}
	}
	if !slices.Equal(cacheSeries, []string{"statica_cache_enabled=0"}) {
		t.Fatalf("cache series with the cache disabled: %v", cacheSeries)
	}

	enabled := parseConfig(t, "")
	live, err := cache.FromConfig(context.Background(), enabled, &fakeOrigin{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterCache(live, enabled.Cache); err == nil {
		t.Fatal("registered a cache the configuration disables")
	}
	other := newMetrics(t, enabled)
	// An enabled configuration without a cache is a wiring defect too.
	if err := other.RegisterCache(nil, enabled.Cache); err == nil {
		t.Fatal("registered no cache while the configuration enables it")
	}
	if err := other.RegisterCache(nil, cfg.Cache); err == nil {
		t.Fatal("registered no cache while the constructor's configuration enables it")
	}
	if err := other.RegisterCache(live, enabled.Cache); err != nil {
		t.Fatal(err)
	}
	if err := other.RegisterCache(live, enabled.Cache); err == nil {
		t.Fatal("registered the cache collector twice")
	}
}

func TestOnFill(t *testing.T) {
	m := newMetrics(t, nil)
	m.OnFill(cache.FillEvent{Result: cache.FillAdmitted, Duration: 20 * time.Millisecond})
	m.OnFill(cache.FillEvent{Result: cache.FillTimeout, Revalidation: cache.RevalidationError, Duration: 30 * time.Second})
	got := scrape(t, m)
	if got[series("statica_cache_fill_duration_seconds_count", "result", "admitted")] != 1 ||
		math.Abs(got[series("statica_cache_fill_duration_seconds_sum", "result", "admitted")]-.02) > 1e-9 ||
		got[series("statica_cache_fill_duration_seconds_count", "result", "timeout")] != 1 {
		t.Fatalf("fill durations %v", got)
	}
}

type staticReader struct{ err error }

func (r staticReader) Get(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
	return nil, r.err
}
func (r staticReader) Head(context.Context, string) (storage.Metadata, error) {
	return storage.Metadata{Size: 1}, r.err
}

func TestReadinessCollector(t *testing.T) {
	m := newMetrics(t, nil)
	health := &config.OriginHealth{Enabled: true, Key: "index.html", Interval: time.Hour, Timeout: time.Second, FailureThreshold: 3, SuccessThreshold: 1, MaxStaleness: time.Hour}
	r := admin.NewReadiness(health)
	if err := m.RegisterReadiness(r); err != nil {
		t.Fatal(err)
	}
	got := scrape(t, m)
	if got["statica_ready"] != 0 || got["statica_origin_health_enabled"] != 1 || got["statica_origin_health_last_success_timestamp_seconds"] != 0 {
		t.Fatalf("initial readiness %v", got)
	}
	for _, res := range []string{"success", "not_found", "ambiguous_not_found", "denied", "throttled", "unavailable", "configuration", "timeout", "failed"} {
		if v, ok := got[series("statica_origin_health_checks_total", "result", res)]; !ok || v != 0 {
			t.Fatalf("missing initial check result %s", res)
		}
	}
	r.SetServing()
	w, err := admin.NewWorker(r, staticReader{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	w.Start()
	deadline := time.Now().Add(5 * time.Second)
	for r.Snapshot().Checks[admin.ResultSuccess] == 0 {
		if time.Now().After(deadline) {
			t.Fatal("health check did not run")
		}
		time.Sleep(time.Millisecond)
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	got = scrape(t, m)
	last := got["statica_origin_health_last_success_timestamp_seconds"]
	if got["statica_ready"] != 1 || got[series("statica_origin_health_checks_total", "result", "success")] != 1 ||
		last < float64(started.Unix())-1 || last > float64(time.Now().Unix())+1 {
		t.Fatalf("readiness after a success %v (last %v)", got, last)
	}
	r.SetDraining()
	if got := scrape(t, m)["statica_ready"]; got != 0 {
		t.Fatalf("ready while draining: %v", got)
	}

	disabled := newMetrics(t, nil)
	if err := disabled.RegisterReadiness(admin.NewReadiness(nil)); err != nil {
		t.Fatal(err)
	}
	if got := scrape(t, disabled); got["statica_origin_health_enabled"] != 0 || got["statica_ready"] != 0 {
		t.Fatalf("disabled health %v", got)
	}
	if err := disabled.RegisterReadiness(nil); err == nil {
		t.Fatal("registered a nil readiness")
	}
}

// blockedWriter blocks every write until released.
type blockedWriter struct{ release chan struct{} }

func (w blockedWriter) Write(p []byte) (int, error) { <-w.release; return len(p), nil }

func TestLogEventsDropped(t *testing.T) {
	m := newMetrics(t, nil)
	w := blockedWriter{release: make(chan struct{})}
	out := telemetry.NewOutput(w, telemetry.OutputOptions{MaxEvents: 1, MaxBytes: 64})
	if err := m.RegisterLogOutput(out); err != nil {
		t.Fatal(err)
	}
	if got := scrape(t, m)["statica_log_events_dropped_total"]; got != 0 {
		t.Fatalf("initial drops %v", got)
	}
	_, _ = out.Write(bytes.Repeat([]byte("x"), 100)) // larger than MaxBytes: always dropped
	for i := 0; i < 4; i++ {
		_, _ = out.Write([]byte("event\n"))
	}
	dropped := out.Dropped()
	if dropped == 0 {
		t.Fatal("no event dropped")
	}
	if got := scrape(t, m)["statica_log_events_dropped_total"]; got != float64(dropped) {
		t.Fatalf("dropped metric %v want %d", got, dropped)
	}
	close(w.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = out.Close(ctx)
	if err := m.RegisterLogOutput(nil); err == nil {
		t.Fatal("registered a nil output")
	}
}

func TestBuildAndRuntimeCollectors(t *testing.T) {
	got := scrape(t, newMetrics(t, nil))
	if got[series("statica_build_info", "version", "v1.2.3", "revision", "abc123", "go_version", "go1.27.3")] != 1 {
		t.Fatalf("build info missing: %v", got)
	}
	m, err := New(parseConfig(t, ""), BuildInfo{})
	if err != nil {
		t.Fatal(err)
	}
	got = scrape(t, m)
	if got[series("statica_build_info", "version", "dev", "revision", "unknown", "go_version", runtime.Version())] != 1 {
		t.Fatalf("default build info missing")
	}
	var goSeries, processSeries bool
	for k := range got {
		goSeries = goSeries || strings.HasPrefix(k, "go_goroutines")
		processSeries = processSeries || strings.HasPrefix(k, "process_")
	}
	if !goSeries || !processSeries {
		t.Fatalf("runtime collectors missing: go %v process %v", goSeries, processSeries)
	}
	if _, err := New(nil, BuildInfo{}); err == nil {
		t.Fatal("constructed without configuration")
	}
}

func TestMetricNamesFollowConventions(t *testing.T) {
	cfg := parseConfig(t, "")
	m := newMetrics(t, cfg)
	c, err := cache.FromConfig(context.Background(), cfg, &fakeOrigin{}, func(o *cache.Options) { o.OnFill = m.OnFill })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterCache(c, cfg.Cache); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterReadiness(admin.NewReadiness(nil)); err != nil {
		t.Fatal(err)
	}
	out := telemetry.NewOutput(io.Discard, telemetry.OutputOptions{})
	defer out.Close(context.Background())
	if err := m.RegisterLogOutput(out); err != nil {
		t.Fatal(err)
	}
	m.RecordSink(with(admitted("a.example", "GET", 200, "incomplete"), func(r *telemetry.Record) {
		r.CacheOutcome, r.TerminationReason = "hit", "disconnect"
	}))
	m.RecordSink(&telemetry.Record{Rejected: true, Reason: "unknown_host"})
	o := m.OriginObserver()
	o.ObserveOperation(s3.OperationGet, storage.PurposeRequest, "", time.Millisecond)
	o.ObserveAttempt(s3.OperationGet, storage.PurposeRequest, "")
	o.ObserveBodyRead(storage.PurposeRequest, 1)

	want := []string{"build_info", "cache_admissions_total", "cache_bytes", "cache_enabled", "cache_entries", "cache_evictions_total",
		"cache_expirations_total", "cache_fill_duration_seconds", "cache_fill_limit_bytes", "cache_fill_limit_concurrency",
		"cache_fill_reserved_bytes", "cache_fills_in_flight", "cache_limit_bytes", "cache_limit_entries", "cache_object_limit_bytes",
		"cache_requests_total", "cache_response_bytes_total", "cache_retired_bytes", "cache_revalidations_total", "cache_waiter_limit_per_key",
		"cache_waiters", "http_rejected_requests_total", "http_request_duration_seconds", "http_requests_in_flight", "http_requests_total",
		"http_response_bytes_total", "http_time_to_first_byte_seconds", "log_events_dropped_total", "origin_attempts_total", "origin_bytes_total",
		"origin_header_duration_seconds", "origin_health_checks_total", "origin_health_enabled", "origin_health_last_success_timestamp_seconds",
		"origin_operations_total", "ready", "stream_failures_total"}
	var names []string
	for _, f := range gather(t, m) {
		name := f.GetName()
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "process_") {
			continue
		}
		if !strings.HasPrefix(name, "statica_") {
			t.Errorf("metric %s lacks the statica_ prefix", name)
		}
		name = strings.TrimPrefix(name, "statica_")
		names = append(names, name)
		switch f.GetType() {
		case dto.MetricType_COUNTER:
			if !strings.HasSuffix(name, "_total") {
				t.Errorf("counter %s lacks _total", name)
			}
		case dto.MetricType_HISTOGRAM:
			if !strings.HasSuffix(name, "_seconds") {
				t.Errorf("histogram %s is not in seconds", name)
			}
		}
	}
	sort.Strings(names)
	if !slices.Equal(names, want) {
		t.Fatalf("metric families\n got %v\nwant %v", names, want)
	}
}

func TestBucketsCoverDocumentedRanges(t *testing.T) {
	for _, tc := range []struct {
		name     string
		buckets  []float64
		low, top float64
	}{
		{"request duration", requestDurationBuckets, .005, 600},
		{"latency", latencyBuckets, .0005, 30},
		{"fill duration", fillDurationBuckets, .001, 30},
	} {
		if !slices.IsSorted(tc.buckets) || tc.buckets[0] > tc.low || tc.buckets[len(tc.buckets)-1] < tc.top {
			t.Errorf("%s buckets %v do not cover [%v, %v]", tc.name, tc.buckets, tc.low, tc.top)
		}
	}
}

func TestHandlerServesRegistry(t *testing.T) {
	m := newMetrics(t, nil)
	m.RecordSink(admitted("a.example", "GET", 200, "success"))
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `statica_http_requests_total{host="a.example",method="GET",outcome="success",status="200"} 1`) {
		t.Fatalf("scrape %d:\n%s", rec.Code, body)
	}
	if reflect.ValueOf(m.Handler()).Pointer() != reflect.ValueOf(m.Handler()).Pointer() {
		t.Fatal("Handler builds a new handler per call")
	}
}

func TestUnexpectedEmptyLabelsStayFinite(t *testing.T) {
	cfg := parseConfig(t, "")
	m := newMetrics(t, cfg)
	// An admitted record without a host is a boundary defect: it counts under
	// one finite host="_unknown" series of every host-labelled request metric.
	m.RecordSink(with(admitted("", "GET", 200, "incomplete"), func(r *telemetry.Record) {
		r.BytesWritten, r.TerminationReason, r.CacheOutcome, r.CacheBytes = 7, "disconnect", "hit", 7
	}))
	m.RecordSink(with(admitted("", "GET", 200, "incomplete"), func(r *telemetry.Record) { r.TerminationReason = "disconnect" }))
	// "unknown" is a valid hostname: without an allowlist a real request for
	// it gets its own series, distinct from the defect's.
	if got := serve(boundary(t, cfg, m), http.MethodGet, "unknown", "/", nil); got != 200 {
		t.Fatalf("Host: unknown status %d", got)
	}
	o := m.OriginObserver()
	o.ObserveOperation(s3.OperationGet, "", "", time.Millisecond)
	o.ObserveAttempt(s3.OperationHead, "", storage.Timeout)
	o.ObserveBodyRead("", 5)
	m.OnFill(cache.FillEvent{Duration: time.Millisecond})

	got := scrape(t, m)
	want := map[string]float64{
		requests("_unknown", "GET", "200", "incomplete"): 2, durationCount("_unknown", "GET"): 2, ttfbCount("_unknown"): 2,
		responseBytes("_unknown"): 7, streamFailures("_unknown", "disconnect"): 2, cacheRequests("_unknown", "hit"): 1, cacheBytes("_unknown"): 7,
		requests("unknown", "GET", "200", "success"): 1, durationCount("unknown", "GET"): 1, ttfbCount("unknown"): 1,
		responseBytes("unknown"): 2, cacheRequests("unknown", "hit"): 1, cacheBytes("unknown"): 2,
		operations("get", "success", "unknown"): 1, attempts("head", "timeout", "unknown"): 1, headerCount("get", "unknown"): 1,
		originBytes("unknown"): 5, series("statica_cache_fill_duration_seconds_count", "result", "unknown"): 1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v want %v", k, got[k], v)
		}
	}
	if _, ok := got[streamFailures("unknown", "disconnect")]; ok {
		t.Error("the defect's stream failures leaked into the real host=\"unknown\" series")
	}
	for _, f := range gather(t, m) {
		if !strings.HasPrefix(f.GetName(), "statica_") {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetValue() == "" {
					t.Errorf("%s has an empty %s label", f.GetName(), l.GetName())
				}
			}
		}
	}
	if hosts := labelValues(t, m, "host"); !slices.Equal(hosts, []string{"_unknown", "unknown"}) {
		t.Fatalf("host labels %v", hosts)
	}
}

func TestConcurrentRecordsAndScrapes(t *testing.T) {
	m := newMetrics(t, nil)
	if err := m.RegisterReadiness(admin.NewReadiness(nil)); err != nil {
		t.Fatal(err)
	}
	const workers, iterations = 16, 300
	hosts := []string{"a.example", "b.example", "c.example", "d.example"}
	stop := make(chan struct{})
	var scrapers sync.WaitGroup
	for i := 0; i < 4; i++ {
		scrapers.Add(1)
		go func() {
			defer scrapers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				rec := httptest.NewRecorder()
				m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				if rec.Code != 200 {
					t.Errorf("scrape status %d", rec.Code)
					return
				}
				_ = scrape(t, m)
			}
		}()
	}
	var feeders sync.WaitGroup
	o := m.OriginObserver()
	for w := 0; w < workers; w++ {
		feeders.Add(1)
		go func() {
			defer feeders.Done()
			for i := 0; i < iterations; i++ {
				host := hosts[(w+i)%len(hosts)]
				m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationStart})
				m.RecordSink(with(admitted(host, "GET", 200, "success"), func(r *telemetry.Record) {
					r.BytesWritten, r.CacheOutcome, r.CacheBytes, r.CacheCoalesced = 2, "hit", 2, i%2 == 0
				}))
				m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationFinish, Host: host})
				m.RecordSink(&telemetry.Record{Rejected: true, Reason: "unknown_host"})
				m.OnFill(cache.FillEvent{Result: cache.FillAdmitted, Duration: time.Millisecond})
				o.ObserveOperation(s3.OperationGet, storage.PurposeCacheFill, "", time.Millisecond)
				o.ObserveAttempt(s3.OperationGet, storage.PurposeCacheFill, storage.Throttled)
				o.ObserveBodyRead(storage.PurposeRequest, 3)
			}
		}()
	}
	feeders.Wait()
	close(stop)
	scrapers.Wait()

	got := scrape(t, m)
	total := float64(workers * iterations)
	var requestsTotal, responseBytesTotal, cacheTotal float64
	for k, v := range got {
		switch {
		case strings.HasPrefix(k, "statica_http_requests_total{"):
			requestsTotal += v
		case strings.HasPrefix(k, "statica_http_response_bytes_total{"):
			responseBytesTotal += v
		case strings.HasPrefix(k, "statica_cache_requests_total{"):
			cacheTotal += v
		}
	}
	if requestsTotal != total || responseBytesTotal != 2*total || cacheTotal != total ||
		got[rejected("unknown_host")] != total || got["statica_http_requests_in_flight"] != 0 ||
		got[series("statica_cache_fill_duration_seconds_count", "result", "admitted")] != total ||
		got[series("statica_origin_operations_total", "operation", "get", "result", "success", "purpose", "cache_fill")] != total ||
		got[series("statica_origin_attempts_total", "operation", "get", "result", "throttled", "purpose", "cache_fill")] != total ||
		got[series("statica_origin_bytes_total", "purpose", "request")] != 3*total {
		t.Fatalf("lost updates: requests %v bytes %v cache %v series %v", requestsTotal, responseBytesTotal, cacheTotal, got)
	}
}

// warmRecord is a typical cached GET of a warm host.
func warmRecord() *telemetry.Record {
	return with(admitted("warm.example", "GET", 200, "success"), func(r *telemetry.Record) {
		r.BytesWritten, r.CacheOutcome, r.CacheBytes, r.RequestID, r.EscapedPath = 512, "hit", 512, "req-1", "/index.html"
	})
}

func TestRequestPathDoesNotAllocateOnWarmSeries(t *testing.T) {
	m := newMetrics(t, nil)
	r := warmRecord()
	failed := with(admitted("warm.example", "GET", 200, "incomplete"), func(r *telemetry.Record) { r.TerminationReason = "disconnect" })
	reject := &telemetry.Record{Rejected: true, Reason: "unknown_host", Authority: "cold.example"}
	o := m.OriginObserver()
	run := func() {
		m.RecordSink(r)
		m.RecordSink(failed)
		m.RecordSink(reject)
		m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationStart})
		m.HTTPObserver(httpserver.Observation{Event: httpserver.ObservationFinish})
		m.OnFill(cache.FillEvent{Result: cache.FillAdmitted, Duration: time.Millisecond})
		o.ObserveOperation(s3.OperationGet, storage.PurposeRequest, storage.NotFound, time.Millisecond)
		o.ObserveAttempt(s3.OperationGet, storage.PurposeRequest, storage.NotFound)
		o.ObserveBodyRead(storage.PurposeCacheFill, 1024)
	}
	run() // warm every series
	if allocs := testing.AllocsPerRun(200, run); allocs != 0 {
		t.Fatalf("warm request path allocates %v times per run", allocs)
	}
}

func BenchmarkRecordSinkWarmHost(b *testing.B) {
	m := newMetrics(b, nil)
	r := warmRecord()
	m.RecordSink(r)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		m.RecordSink(r)
	}
}

func BenchmarkRecordSinkWarmHostParallel(b *testing.B) {
	m := newMetrics(b, nil)
	m.RecordSink(warmRecord())
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		r := warmRecord()
		for pb.Next() {
			m.RecordSink(r)
		}
	})
}
