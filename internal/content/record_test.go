package content

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logfmt/logfmt"
	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/redirect"
	"github.com/gtriggiano/statica/internal/storage"
	"github.com/gtriggiano/statica/internal/telemetry"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// pipeline is the public handler, httpserver.NewHandler around the content
// handler, wired as the application will be: each request's record goes to
// an AccessLogger, here over a synchronous logfmt handler, and is kept.
type pipeline struct {
	handler http.Handler
	log     syncBuffer
	mu      sync.Mutex
	records []*telemetry.Record
}

func newPipeline(t testing.TB, cfg *config.Config, reader storage.Reader, options ...HandlerOption) *pipeline {
	t.Helper()
	p := &pipeline{}
	p.handler = pipelineWith(t, cfg, reader, telemetry.NewHandler(&p.log, &telemetry.HandlerOptions{Level: slog.LevelDebug}), func(r *telemetry.Record) {
		p.mu.Lock()
		p.records = append(p.records, r)
		p.mu.Unlock()
	}, options...)
	return p
}

func pipelineWith(t testing.TB, cfg *config.Config, reader storage.Reader, h slog.Handler, keep func(*telemetry.Record), options ...HandlerOption) http.Handler {
	t.Helper()
	access := telemetry.NewAccessLogger(h, cfg.Observability)
	content, err := NewHandler(cfg, reader, options...)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpserver.NewHandler(cfg, content, httpserver.WithRecordSink(func(r *telemetry.Record) {
		access.Sink(r)
		if keep != nil {
			keep(r)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

// do serves r, recovering the abort panic of an interrupted transfer.
func (p *pipeline) do(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	func() {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				panic(v)
			}
		}()
		p.handler.ServeHTTP(w, r)
	}()
	return w
}

func (p *pipeline) take() []*telemetry.Record {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.records
	p.records = nil
	return r
}

// logEvents decodes the access log with the go-logfmt decoder, requiring
// one record per line and unique keys.
func logEvents(t testing.TB, text string) []map[string]string {
	t.Helper()
	dec := logfmt.NewDecoder(strings.NewReader(text))
	var events []map[string]string
	for dec.ScanRecord() {
		e := map[string]string{}
		for dec.ScanKeyval() {
			if _, dup := e[string(dec.Key())]; dup {
				t.Fatalf("duplicate key %q", dec.Key())
			}
			e[string(dec.Key())] = string(dec.Value())
		}
		events = append(events, e)
	}
	if err := dec.Err(); err != nil || strings.Count(text, "\n") != len(events) {
		t.Fatalf("decode %v, %d lines for %d events", err, strings.Count(text, "\n"), len(events))
	}
	return events
}

// checkRecord checks the record's times and that an origin failure always
// carries an error class.
func checkRecord(t testing.TB, r *telemetry.Record) {
	t.Helper()
	if r.Outcome == telemetry.OutcomeOriginFailure && r.ErrorClass == "" {
		t.Fatalf("origin failure without an error class: %+v", r)
	}
	ttfb, committed := r.TTFB()
	if r.Started.IsZero() || r.Finished.Before(r.Started) || committed && (ttfb < 0 || ttfb > r.Duration()) {
		t.Fatalf("times started %v first byte %v finished %v", r.Started, r.FirstByte, r.Finished)
	}
}

func TestRecordsAndAccessEventsThroughThePipeline(t *testing.T) {
	notFound := func(c *config.Config) { c.NotFound = "404.html" }
	spa := func(c *config.Config) { c.SPA = &config.SPA{Fallback: "index.html"} }
	redirects := func(c *config.Config) {
		c.Redirects = []redirect.Rule{{From: "/old", To: "/new", Status: 301, Query: "drop"}}
	}
	hosts := func(c *config.Config) { c.Hosts = []string{"example.org"} }
	proxy := func(c *config.Config) { c.Server.TrustedProxies = []string{"192.0.2.0/24"} }
	none := "" // the field is absent
	for _, tc := range []struct {
		name, method, target string
		header               http.Header
		mutate               func(*config.Config)
		warm                 bool
		status               int
		want                 map[string]string
	}{
		{name: "miss", method: "GET", target: "/file.txt", status: 200, want: map[string]string{"level": "info", "host": "example.org", "method": "GET",
			"path": "/file.txt", "outcome": "success", "resolution": "ordinary", "cache": "miss", "origin_key": "file.txt", "bytes_written": "11"}},
		{name: "hit", method: "GET", target: "/file.txt?token=abc&v=2", warm: true, status: 200, want: map[string]string{"level": "info",
			"path": "/file.txt?token=[REDACTED]&v=2", "cache": "hit", "origin_key": "file.txt", "bytes_written": "11"}},
		{name: "HEAD", method: "HEAD", target: "/file.txt", warm: true, status: 200, want: map[string]string{"level": "info", "method": "HEAD",
			"cache": "metadata_hit", "origin_key": "file.txt", "bytes_written": "0"}},
		{name: "304", method: "GET", target: "/file.txt", header: http.Header{"If-None-Match": {`"f1"`}}, warm: true, status: 304,
			want: map[string]string{"level": "info", "status": "304", "cache": "hit", "origin_key": "file.txt", "bytes_written": "0"}},
		{name: "206", method: "GET", target: "/file.txt", header: http.Header{"Range": {"bytes=1-3"}}, warm: true, status: 206,
			want: map[string]string{"level": "info", "status": "206", "cache": "hit", "origin_key": "file.txt", "bytes_written": "3"}},
		{name: "configured redirect", method: "GET", target: "/old?token=abc", mutate: redirects, status: 301, want: map[string]string{"level": "info",
			"path": "/old?token=[REDACTED]", "outcome": "success", "origin_key": none, "cache": none, "resolution": none, "error_class": none}},
		{name: "trailing slash redirect", method: "GET", target: "/docs", status: 308, want: map[string]string{"level": "info",
			"resolution": "index", "origin_key": none, "cache": "miss", "outcome": "success"}},
		{name: "custom 404", method: "GET", target: "/missing.txt", mutate: notFound, status: 404, want: map[string]string{"level": "info",
			"resolution": "not_found", "origin_key": "404.html", "outcome": "success", "bytes_written": "17"}},
		{name: "builtin 404", method: "GET", target: "/missing.txt", status: 404, want: map[string]string{"level": "info",
			"resolution": "not_found", "origin_key": none, "outcome": "success", "error_class": none}},
		{name: "SPA shell", method: "GET", target: "/app/route", header: http.Header{"Accept": {"text/html"}}, mutate: spa, status: 200,
			want: map[string]string{"level": "info", "path": "/app/route", "resolution": "spa", "origin_key": "index.html"}},
		{name: "D05 masked denial", method: "GET", target: "/denied.txt", status: 404, want: map[string]string{"level": "error",
			"outcome": "origin_failure", "error_class": "origin_denied", "termination_reason": "origin_failure", "origin_key": none}},
		{name: "rejected host", method: "GET", target: "http://other.example/file.txt", mutate: hosts, status: 421, want: map[string]string{
			"level": "warn", "host": none, "authority": "other.example", "path": "/file.txt", "error_class": "unknown_host", "outcome": none}},
		{name: "rejected method", method: "POST", target: "/file.txt", status: 405, want: map[string]string{"level": "warn",
			"host": "example.org", "method": "POST", "error_class": "unsupported_method", "origin_key": none}},
		{name: "rejected target", method: "GET", target: "/a/../b?token=abc", status: 400, want: map[string]string{"level": "warn",
			"path": none, "error_class": "invalid_target"}},
		{name: "trusted proxy request ID", method: "GET", target: "/file.txt", mutate: proxy,
			header: http.Header{"X-Request-Id": {"edge-123"}, "X-Forwarded-For": {"198.51.100.7"}}, status: 200,
			want: map[string]string{"request_id": "edge-123", "request_id_source": "proxy", "client_ip": "198.51.100.7",
				"peer_ip": "192.0.2.1", "client_ip_source": "x_forwarded_for"}},
		{name: "ignored untrusted request ID", method: "GET", target: "/file.txt",
			header: http.Header{"X-Request-Id": {"forged"}, "X-Forwarded-For": {"198.51.100.7"}}, status: 200,
			want: map[string]string{"request_id_source": "generated", "client_ip": "192.0.2.1", "peer_ip": "192.0.2.1", "client_ip_source": "peer"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cachedConfig(t, tc.mutate)
			origin := newMemOrigin()
			c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
			p := newPipeline(t, cfg, origin, WithCache(c))
			newRequest := func() *http.Request {
				target := tc.target
				if !strings.HasPrefix(target, "http") {
					target = "http://example.org" + target
				}
				r := httptest.NewRequest(tc.method, "http://example.org/", nil)
				if !strings.Contains(target, "/../") {
					r = httptest.NewRequest(tc.method, target, nil)
				} else {
					r.RequestURI = strings.TrimPrefix(target, "http://example.org")
				}
				r.Header = tc.header.Clone()
				if r.Header == nil {
					r.Header = http.Header{}
				}
				return r
			}
			if tc.warm {
				p.do(newRequest())
				p.take()
				p.log.reset()
			}
			w := p.do(newRequest())
			records := p.take()
			if w.Code != tc.status || len(records) != 1 {
				t.Fatalf("status %d, %d records", w.Code, len(records))
			}
			r := records[0]
			checkRecord(t, r)
			if r.Status != w.Code || r.BytesWritten != int64(w.Body.Len()) || r.RequestID != w.Header().Get("X-Request-ID") {
				t.Fatalf("record %+v for %d %q", r, w.Code, w.Body.String())
			}
			events := logEvents(t, p.log.String())
			if len(events) != 1 {
				t.Fatalf("%d events", len(events))
			}
			e := events[0]
			if e["event"] != "http_request" || e["request_id"] != r.RequestID || e["status"] != fmt.Sprint(tc.status) {
				t.Fatalf("event %v", e)
			}
			for k, v := range tc.want {
				if got, ok := e[k]; v == none && ok || v != none && got != v {
					t.Errorf("%s = %q (present %t), want %q", k, got, ok, v)
				}
			}
		})
	}
}

func TestInterruptedTransferRecord(t *testing.T) {
	c := testConfig(t)
	f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
		return &storage.Object{Metadata: storage.Metadata{Size: 10, ETag: `"v"`},
			Body: io.NopCloser(io.MultiReader(strings.NewReader("hello"), errorReader{&storage.Error{Kind: storage.Unavailable}}))}, nil
	}}
	p := newPipeline(t, c, f)
	w := p.do(httptest.NewRequest("GET", "http://example.org/big.bin", nil))
	records := p.take()
	if len(records) != 1 || w.Code != 200 || w.Body.String() != "hello" {
		t.Fatalf("%d records, %d %q", len(records), w.Code, w.Body.String())
	}
	r := records[0]
	checkRecord(t, r)
	if r.Status != 200 || r.BytesWritten != 5 || r.Outcome != "incomplete" || r.TerminationReason != "origin_read_failure" ||
		r.OriginKey != "big.bin" || r.Reason != "response_aborted" {
		t.Fatalf("record %+v", r)
	}
	e := logEvents(t, p.log.String())[0]
	if e["level"] != "error" || e["outcome"] != "incomplete" || e["termination_reason"] != "origin_read_failure" || e["origin_key"] != "big.bin" || e["bytes_written"] != "5" {
		t.Fatalf("event %v", e)
	}
}

func TestOverloadRecord(t *testing.T) {
	c := testConfig(t)
	c.Server.MaxConcurrentRequests = 1
	origin := newSlowOrigin(map[string]memObject{"file.txt": {"hello", fixtureMeta(`"f"`, "", 0)}})
	gate := make(chan struct{})
	origin.setHolds(gate, nil)
	p := newPipeline(t, c, origin)
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.do(httptest.NewRequest("GET", "http://example.org/file.txt", nil))
	}()
	eventually(t, "the held request", func() bool { return origin.reads.Load() == 1 })
	w := p.do(httptest.NewRequest("GET", "http://example.org/file.txt?token=abc", nil))
	records := p.take()
	if w.Code != 503 || len(records) != 1 {
		t.Fatalf("status %d, %d records", w.Code, len(records))
	}
	if r := records[0]; !r.Rejected || r.Reason != "request_capacity" || !r.Admitted.IsZero() || r.BytesWritten != int64(w.Body.Len()) {
		t.Fatalf("record %+v", r)
	}
	e := logEvents(t, p.log.String())[0]
	if e["level"] != "warn" || e["error_class"] != "request_capacity" || e["status"] != "503" || e["authority"] != "example.org" ||
		e["path"] != "/file.txt?token=[REDACTED]" {
		t.Fatalf("event %v", e)
	}
	close(gate)
	<-done
	if records := p.take(); len(records) != 1 || records[0].Status != 200 {
		t.Fatalf("held request records %v", records)
	}
}

func TestCoalescedRequestsKeepTheirOwnRequestIDs(t *testing.T) {
	const n = 4
	cfg := cachedConfig(t, func(c *config.Config) { c.Server.TrustedProxies = []string{"192.0.2.0/24"} })
	origin := newSlowOrigin(map[string]memObject{"popular.txt": {strings.Repeat("p", 100), fixtureMeta(`"pop"`, "", 0)}})
	gate := make(chan struct{})
	origin.setHolds(gate, nil)
	c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
	p := newPipeline(t, cfg, origin, WithCache(c))
	headers := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			r := httptest.NewRequest("GET", "http://example.org/popular.txt", nil)
			r.Header.Set("X-Request-ID", fmt.Sprint("visitor-", i))
			headers[i] = p.do(r).Header().Get("X-Request-ID")
		})
	}
	eventually(t, "the coalesced waiters", func() bool { return c.Stats().Waiters == n })
	close(gate)
	wg.Wait()
	for i, h := range headers {
		if h != fmt.Sprint("visitor-", i) {
			t.Fatalf("response %d carried request ID %q", i, h)
		}
	}
	records := p.take()
	if len(records) != n {
		t.Fatalf("%d records", len(records))
	}
	var ids, logged []string
	coalesced := 0
	for _, r := range records {
		if r.CacheOutcome != string(cache.OutcomeMiss) || r.OriginKey != "popular.txt" || r.BytesWritten != 100 {
			t.Fatalf("record %+v", r)
		}
		if r.CacheCoalesced {
			coalesced++
		}
		ids = append(ids, r.RequestID)
	}
	for _, e := range logEvents(t, p.log.String()) {
		logged = append(logged, e["request_id"])
	}
	slices.Sort(ids)
	slices.Sort(logged)
	if want := "visitor-0 visitor-1 visitor-2 visitor-3"; strings.Join(ids, " ") != want || strings.Join(logged, " ") != want || coalesced != n-1 {
		t.Fatalf("record IDs %v, logged %v, %d coalesced", ids, logged, coalesced)
	}
}

// sdkErrorOrigin fails one key with an error carrying provider details.
type sdkErrorOrigin struct{ *memOrigin }

func (o sdkErrorOrigin) Get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	if key == "sdk-error.txt" {
		return nil, errors.New("CANARY-SDK GET https://bucket.s3.example/sdk-error.txt?X-Amz-Credential=CANARY-CRED: AccessDenied")
	}
	return o.memOrigin.Get(ctx, key, opts)
}

func TestAccessLogNeverLeaksSecrets(t *testing.T) {
	cfg := cachedConfig(t, func(c *config.Config) {
		c.Redirects = []redirect.Rule{{From: "/go", To: "/CANARY-LOCATION/dest", Status: 302, Query: "drop"}}
	})
	p := newPipeline(t, cfg, sdkErrorOrigin{newMemOrigin()})
	canaries := []string{"CANARY-AUTH", "CANARY-COOKIE", "CANARY-REFERER", "CANARY-UA", "CANARY-TOKEN", "CANARY-PASSWORD",
		"CANARY-SIG", "CANARY-ENCODED", "CANARY-KEY", "CANARY-LOCATION", "CANARY-SDK", "CANARY-CRED", "CANARY-SECRET", "CANARY-REJECTED"}
	header := http.Header{"Authorization": {"Bearer CANARY-AUTH"}, "Cookie": {"session=CANARY-COOKIE"},
		"Referer": {"https://CANARY-REFERER.example/"}, "User-Agent": {"CANARY-UA/1.0"}}
	for _, tc := range []struct {
		method, target string
		status         int
	}{
		{"GET", "/file.txt?token=CANARY-TOKEN&password=CANARY-PASSWORD&X-Amz-Signature=CANARY-SIG&%74oken=CANARY-ENCODED", 200},
		{"GET", "/go?api_key=CANARY-KEY", 302},
		{"GET", "/sdk-error.txt", 502},
		{"GET", "/denied.txt", 404},
		{"GET", "/docs?secret=CANARY-SECRET", 308},
		{"POST", "/file.txt?token=CANARY-REJECTED", 405},
	} {
		r := httptest.NewRequest(tc.method, "http://example.org"+tc.target, nil)
		r.Header = header.Clone()
		if w := p.do(r); w.Code != tc.status {
			t.Fatalf("%s %s: status %d", tc.method, tc.target, w.Code)
		}
	}
	out := p.log.String()
	for _, canary := range canaries {
		if strings.Contains(out, canary) {
			t.Errorf("access log contains %s:\n%s", canary, out)
		}
	}
	events := logEvents(t, out)
	if len(events) != 6 || events[0]["path"] != "/file.txt?token=[REDACTED]&password=[REDACTED]&X-Amz-Signature=[REDACTED]&%74oken=[REDACTED]" ||
		events[2]["level"] != "error" || events[2]["outcome"] != "origin_failure" || events[2]["error_class"] != "origin_error" {
		t.Fatalf("events %v", events)
	}
}

func TestAccessLogClientIPSwitch(t *testing.T) {
	for _, logIP := range []bool{true, false} {
		cfg := testConfig(t)
		cfg.Observability.LogClientIP = logIP
		p := newPipeline(t, cfg, newMemOrigin())
		p.do(httptest.NewRequest("GET", "http://example.org/file.txt", nil))
		e := logEvents(t, p.log.String())[0]
		for _, k := range []string{"client_ip", "peer_ip", "client_ip_source"} {
			if _, ok := e[k]; ok != logIP {
				t.Fatalf("log_client_ip %t: %s present %t", logIP, k, ok)
			}
		}
		if r := p.take()[0]; !r.ClientIP.IsValid() || !r.PeerIP.IsValid() {
			t.Fatal("addresses must still be resolved")
		}
	}
}

// stuckWriter blocks every Write until released.
type stuckWriter struct {
	release chan struct{}
	entered chan struct{}
}

func (w *stuckWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return len(p), nil
}

// TestBlockedLogOutputDoesNotSlowRequests is the resource test of blocked
// log output: with stdout stuck, requests complete at the latency they have
// with a working destination, access events are dropped and counted, and
// the output closes at its deadline.
func TestBlockedLogOutputDoesNotSlowRequests(t *testing.T) {
	const n, queue = 300, 16
	cfg := cachedConfig(t, nil)
	origin := newMemOrigin()
	c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
	latencies := func(h http.Handler) []time.Duration {
		ds := make([]time.Duration, n)
		for i := range ds {
			start := time.Now()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("GET", "http://example.org/file.txt", nil))
			ds[i] = time.Since(start)
			if w.Code != 200 {
				t.Fatalf("status %d", w.Code)
			}
		}
		slices.Sort(ds)
		return ds
	}
	working := telemetry.NewOutput(io.Discard, telemetry.OutputOptions{})
	baseline := latencies(pipelineWith(t, cfg, origin, telemetry.NewHandler(working, nil), nil, WithCache(c)))
	if err := working.Close(context.Background()); err != nil || working.Dropped() != 0 {
		t.Fatalf("working output: %v, %d dropped", err, working.Dropped())
	}

	w := &stuckWriter{release: make(chan struct{}), entered: make(chan struct{}, 1)}
	stuck := telemetry.NewOutput(w, telemetry.OutputOptions{MaxEvents: queue, DropReportInterval: time.Hour})
	defer close(w.release)
	stuck.Write([]byte("event=first\n"))
	<-w.entered
	blocked := latencies(pipelineWith(t, cfg, origin, telemetry.NewHandler(stuck, nil), nil, WithCache(c)))
	if stuck.Dropped() != n-queue {
		t.Fatalf("dropped %d, want %d", stuck.Dropped(), n-queue)
	}
	median := func(ds []time.Duration) time.Duration { return ds[len(ds)/2] }
	if median(blocked) > 4*median(baseline)+2*time.Millisecond || blocked[n-1] > time.Second {
		t.Fatalf("latency with blocked output: median %v, max %v; working output median %v", median(blocked), blocked[n-1], median(baseline))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := stuck.Close(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("Close = %v after %v", err, time.Since(start))
	}
}

func TestEveryOriginFailureHasAnErrorClass(t *testing.T) {
	ranged := http.Header{"Range": {"bytes=0-1"}}
	for _, tc := range []struct {
		name   string
		get    func(context.Context, string, storage.ReadOptions) (*storage.Object, error)
		header http.Header
		status int
		class  string
		reason string
	}{
		{name: "object keeps changing under its guarded range read", header: ranged, status: 503, class: "origin_inconsistent", reason: "origin_failure",
			get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range != "" {
					return nil, &storage.Error{Kind: storage.Precondition}
				}
				return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
			}},
		{name: "range coding keeps changing", header: ranged, status: 503, class: "origin_inconsistent", reason: "origin_failure",
			get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range != "" {
					return object("ab", storage.Metadata{Size: 2, ETag: `"v"`, ContentRange: "bytes 0-1/6", ContentEncoding: "gzip"}), nil
				}
				return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
			}},
		{name: "storage kind", status: 503, class: "origin_throttled", reason: "origin_failure",
			get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
				return nil, &storage.Error{Kind: storage.Throttled}
			}},
		{name: "unclassified error", status: 502, class: "origin_error", reason: "origin_failure",
			get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
				return nil, errors.New("provider text")
			}},
		{name: "preparation deadline", status: 504, class: "origin_timeout", reason: "preparation_timeout",
			get: func(ctx context.Context, _ string, _ storage.ReadOptions) (*storage.Object, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.Server.ResponseHeaderTimeout = 20 * time.Millisecond
			p := newPipeline(t, c, fakeReader{get: tc.get})
			r := httptest.NewRequest("GET", "http://example.org/file", nil)
			if tc.header != nil {
				r.Header = tc.header.Clone()
			}
			w := p.do(r)
			records := p.take()
			if w.Code != tc.status || len(records) != 1 {
				t.Fatalf("status %d, %d records", w.Code, len(records))
			}
			rec := records[0]
			checkRecord(t, rec)
			if rec.Outcome != "origin_failure" || rec.ErrorClass != tc.class || rec.TerminationReason != tc.reason || rec.OriginKey != "" {
				t.Fatalf("record %+v", rec)
			}
			e := logEvents(t, p.log.String())[0]
			if e["level"] != "error" || e["error_class"] != tc.class || e["termination_reason"] != tc.reason {
				t.Fatalf("event %v", e)
			}
		})
	}
}
