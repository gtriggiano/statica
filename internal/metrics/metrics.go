// Package metrics exports statica's Prometheus metrics, as the observability
// design's metrics contract defines them, from a private registry served by
// the admin listener's /metrics endpoint.
//
// Request-level series are derived from the per-request telemetry.Record, the
// same record the access log is derived from, so the two cannot disagree. The
// in-flight gauge comes from the httpserver observer, fill durations from
// cache.Options.OnFill and origin series from the S3 adapter's observer.
// Cache, readiness and log state is read from its owner's snapshot at scrape
// time by custom collectors.
//
// # Labels
//
// Host-labelled series carry the actual normalized hostname of each admitted
// request, never a cap, hash or "_other", and are never evicted: with a host
// allowlist they are bounded by it, without one they intentionally grow with
// every accepted hostname, the explicitly accepted trade-off (D13: metrics
// are part of the memory budget). Rejected requests only count under a fixed
// reason, without a host, even when admission had already normalized one.
// Every other label value is a finite code constant or a normalized method
// (GET, HEAD, OPTIONS or OTHER); paths, keys, queries, addresses and request
// IDs never become labels. Global cache state, fills and origin operations
// have no host: one fill may serve several hostnames.
//
// Per accepted host, the series are: http_requests_total for each method,
// status and outcome seen; one request duration histogram per method seen;
// one TTFB histogram; one response bytes counter; at most one cache_requests
// counter per cache outcome, one cache bytes counter and one stream failure
// counter per termination reason seen.
//
// # Buckets
//
// The histogram buckets are initial, documented choices to be tuned with D13
// capacity evidence; they are not service-level objectives.
package metrics

import (
	"errors"
	"net/http"
	"runtime"
	"sync"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "statica"

var (
	// requestDurationBuckets cover small assets served from memory in
	// milliseconds up to long downloads of ten minutes: a successful long
	// transfer is not an error and must not fall off the top bucket early.
	requestDurationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}
	// latencyBuckets cover time to first byte and origin time to headers,
	// from sub-millisecond memory hits to the 10s origin and 30s server
	// response header timeouts of the default configuration.
	latencyBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
	// fillDurationBuckets cover shared fills from 1ms to the default 30s
	// fill_timeout.
	fillDurationBuckets = []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
)

// BuildInfo identifies the binary for statica_build_info. Empty values use
// the same local development defaults as the version command.
type BuildInfo struct {
	Version, Revision, GoVersion string
}

// Metrics owns statica's private registry and every series in it. Its
// methods are safe for concurrent use. RecordSink, HTTPObserver, OnFill and
// the origin observer run on request and fill paths; on a warm series they do
// not allocate.
type Metrics struct {
	registry     *prometheus.Registry
	handler      http.Handler
	cacheEnabled bool

	inflight prometheus.Gauge

	exchanges     *children[exchangeKey, exchangeSeries]
	ttfb          *children[string, prometheus.Observer]
	rejections    *children[string, prometheus.Counter]
	failures      *children[hostLabel, prometheus.Counter]
	cacheRequests *children[hostLabel, prometheus.Counter]
	cacheBytes    *children[string, prometheus.Counter]
	fills         *children[string, prometheus.Observer]

	operations *children[originKey, prometheus.Counter]
	attempts   *children[originKey, prometheus.Counter]
	headers    *children[originKey, prometheus.Observer]
	bytes      *children[string, prometheus.Counter]
}

// New returns the metrics of the validated configuration cfg, with the build,
// Go runtime and process collectors and every request-path series family
// registered. Slice-specific state is added with RegisterCache,
// RegisterReadiness and RegisterLogOutput.
func New(cfg *config.Config, build BuildInfo) (*Metrics, error) {
	if cfg == nil {
		return nil, errors.New("metrics: configuration is required")
	}
	if build.Version == "" {
		build.Version = "dev"
	}
	if build.Revision == "" {
		build.Revision = "unknown"
	}
	if build.GoVersion == "" {
		build.GoVersion = runtime.Version()
	}
	reg := prometheus.NewRegistry()
	m := &Metrics{registry: reg, handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), cacheEnabled: cfg.Cache.Enabled}

	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "build_info",
		Help:        "Constant 1, labelled with the build identity.",
		ConstLabels: prometheus.Labels{"version": build.Version, "revision": build.Revision, "go_version": build.GoVersion}})
	buildInfo.Set(1)
	cacheEnabled := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "cache_enabled",
		Help: "1 when the memory cache is enabled, 0 when it is bypassed globally (then no other global cache series is exported)."})
	if cfg.Cache.Enabled {
		cacheEnabled.Set(1)
	}
	m.inflight = prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "http_requests_in_flight",
		Help: "Public request slots in use."})

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "http_requests_total",
		Help: "Admitted requests by normalized host, method, committed status (0 when none) and outcome (success, origin_failure, incomplete)."},
		[]string{"host", "method", "status", "outcome"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "http_request_duration_seconds",
		Help: "Full response or abort duration of admitted requests.", Buckets: requestDurationBuckets}, []string{"host", "method"})
	ttfb := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "http_time_to_first_byte_seconds",
		Help: "Time until the response was committed, for admitted requests that committed one.", Buckets: latencyBuckets}, []string{"host"})
	responseBytes := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "http_response_bytes_total",
		Help: "Body bytes actually written to admitted requests, partial transfers included."}, []string{"host"})
	rejected := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "http_rejected_requests_total",
		Help: "Requests rejected before ordinary serving, by fixed reason; never labelled by host."}, []string{"reason"})
	failures := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "stream_failures_total",
		Help: "Admitted requests whose response did not complete (outcome incomplete), by termination reason."}, []string{"host", "reason"})
	cacheRequests := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "cache_requests_total",
		Help: "Admitted requests that reached the cache, by outcome: hit, metadata_hit, negative_hit, miss, revalidated, stale, bypass, or coalesced for a request that joined another's fill, so that outcomes partition requests."},
		[]string{"host", "outcome"})
	cacheBytes := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "cache_response_bytes_total",
		Help: "Body bytes written from snapshots a request reused from memory (hit, revalidated, stale), whole or sliced."}, []string{"host"})
	fills := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "cache_fill_duration_seconds",
		Help: "Shared fill duration, from its creation to the publication of its result, by the fill's result.", Buckets: fillDurationBuckets}, []string{"result"})
	operations := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "origin_operations_total",
		Help: "Logical origin operations (get, head) by result (success or the storage error kind) and purpose (request, cache_fill, health)."},
		[]string{"operation", "result", "purpose"})
	attempts := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "origin_attempts_total",
		Help: "Origin HTTP round trips, retries included, by operation, result and purpose. Only round trips count: an operation that failed before reaching the transport (e.g. a credential or signing error) has no attempt, so operations may exceed attempts."},
		[]string{"operation", "result", "purpose"})
	headers := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: "origin_header_duration_seconds",
		Help: "Time from a logical origin operation's start to its response headers or failure, retries and backoff included.", Buckets: latencyBuckets},
		[]string{"operation", "purpose"})
	originBytes := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "origin_bytes_total",
		Help: "Body bytes actually read from the origin, by the purpose of the reader: a fill stream handed to a request counts as request."},
		[]string{"purpose"})

	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo, cacheEnabled, m.inflight, requests, duration, ttfb, responseBytes, rejected, failures,
		cacheRequests, cacheBytes, fills, operations, attempts, headers, originBytes,
	} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}

	m.exchanges = newChildren(func(k exchangeKey) exchangeSeries {
		return exchangeSeries{
			requests: requests.WithLabelValues(k.host, k.method, statusLabel(k.status), k.outcome),
			duration: duration.WithLabelValues(k.host, k.method),
			bytes:    responseBytes.WithLabelValues(k.host),
		}
	})
	m.ttfb = newChildren(func(host string) prometheus.Observer { return ttfb.WithLabelValues(host) })
	m.rejections = newChildren(func(reason string) prometheus.Counter { return rejected.WithLabelValues(reason) })
	m.failures = newChildren(func(k hostLabel) prometheus.Counter { return failures.WithLabelValues(k.host, k.label) })
	m.cacheRequests = newChildren(func(k hostLabel) prometheus.Counter { return cacheRequests.WithLabelValues(k.host, k.label) })
	m.cacheBytes = newChildren(func(host string) prometheus.Counter { return cacheBytes.WithLabelValues(host) })
	m.fills = newChildren(func(result string) prometheus.Observer { return fills.WithLabelValues(result) })
	m.operations = newChildren(func(k originKey) prometheus.Counter {
		return operations.WithLabelValues(k.operation, k.result, k.purpose)
	})
	m.attempts = newChildren(func(k originKey) prometheus.Counter {
		return attempts.WithLabelValues(k.operation, k.result, k.purpose)
	})
	m.headers = newChildren(func(k originKey) prometheus.Observer { return headers.WithLabelValues(k.operation, k.purpose) })
	m.bytes = newChildren(func(purpose string) prometheus.Counter { return originBytes.WithLabelValues(purpose) })
	return m, nil
}

// Handler serves the registry in the Prometheus exposition formats, for the
// admin /metrics endpoint. Scrapes only read in-memory state.
func (m *Metrics) Handler() http.Handler { return m.handler }

// children memoizes the child metrics of one vector by a comparable key, so a
// warm series costs one read-locked map lookup and no allocation, unlike
// WithLabelValues, which allocates its label slice on every call. Entries
// are never removed: a series lives as long as its vector, exactly as the
// host-label contract requires.
type children[K comparable, M any] struct {
	create func(K) M
	mu     sync.RWMutex
	m      map[K]M
}

func newChildren[K comparable, M any](create func(K) M) *children[K, M] {
	return &children[K, M]{create: create, m: map[K]M{}}
}

func (c *children[K, M]) get(k K) M {
	c.mu.RLock()
	v, ok := c.m[k]
	c.mu.RUnlock()
	if ok {
		return v
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok = c.m[k]; !ok {
		v = c.create(k)
		c.m[k] = v
	}
	return v
}
