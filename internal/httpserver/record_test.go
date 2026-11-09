package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/redirect"
	"github.com/gtriggiano/statica/internal/telemetry"
)

// recordLog collects delivered records and, in order, observer events and
// deliveries.
type recordLog struct {
	mu      sync.Mutex
	records []*telemetry.Record
	order   []string
}

func (l *recordLog) observe(o Observation) {
	l.mu.Lock()
	l.order = append(l.order, string(o.Event))
	l.mu.Unlock()
}

func (l *recordLog) sink(r *telemetry.Record) {
	l.mu.Lock()
	l.records = append(l.records, r)
	l.order = append(l.order, "record")
	l.mu.Unlock()
}

// brokenWriter fails every body write, as a reset connection does.
type brokenWriter struct {
	header http.Header
	status int
}

func (w *brokenWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *brokenWriter) WriteHeader(status int)    { w.status = status }
func (w *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

// take returns the single record and event order since the previous take.
func (l *recordLog) take(t *testing.T) (*telemetry.Record, string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.records) != 1 {
		t.Fatalf("%d records delivered, want 1", len(l.records))
	}
	r, order := l.records[0], strings.Join(l.order, " ")
	l.records, l.order = nil, nil
	return r, order
}

func TestHandlerRecordLifecycle(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Hosts = []string{"example.com"}
	cfg.Redirects = []redirect.Rule{{From: "/old", To: "/new?dest=secret", Status: 301, Query: "drop"}}
	var log recordLog
	var seen *telemetry.Record
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		seen = telemetry.RecordFrom(r.Context())
		switch r.URL.Path {
		case "/partial":
			_, _ = w.Write([]byte("abc"))
			panic(http.ErrAbortHandler)
		case "/panic":
			panic("boom")
		default:
			_, _ = w.Write([]byte("hello"))
		}
	}, WithObserver(log.observe), WithRecordSink(log.sink))
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		func() {
			defer func() { _ = recover() }()
			h.ServeHTTP(w, r)
		}()
		return w
	}
	check := func(t *testing.T, r *telemetry.Record) {
		t.Helper()
		if r.Started.IsZero() || r.Finished.Before(r.Started) || !r.FirstByte.IsZero() && (r.FirstByte.Before(r.Started) || r.FirstByte.After(r.Finished)) {
			t.Fatalf("times %v %v %v", r.Started, r.FirstByte, r.Finished)
		}
		if ttfb, ok := r.TTFB(); ok && ttfb > r.Duration() {
			t.Fatalf("ttfb %v exceeds duration %v", ttfb, r.Duration())
		}
		if len(r.RequestID) != 32 || r.RequestIDSource != "generated" || r.PeerIP.String() != "192.0.2.10" || r.ClientIPSource != "peer" {
			t.Fatalf("identity %+v", r)
		}
	}

	t.Run("served", func(t *testing.T) {
		w := serve(boundaryRequest("GET", "/ok?q=1&token=x"))
		r, order := log.take(t)
		check(t, r)
		if r != seen || order != "start finish record" || r.Status != 200 || r.BytesWritten != 5 || r.Outcome != "success" ||
			r.Reason != "" || r.Rejected || r.Host != "example.com" || r.Authority != "" || r.EscapedPath != "/ok" ||
			r.RawQuery != "q=1&token=x" || r.Admitted.IsZero() || r.FirstByte.IsZero() || r.Method != "GET" ||
			w.Header().Get("X-Request-ID") != r.RequestID {
			t.Fatalf("order %q record %+v", order, r)
		}
	})
	t.Run("HEAD writes no bytes", func(t *testing.T) {
		serve(boundaryRequest("HEAD", "/ok"))
		if r, _ := log.take(t); r.Status != 200 || r.BytesWritten != 0 || r.Method != "HEAD" {
			t.Fatalf("record %+v", r)
		}
	})
	t.Run("partial transfer", func(t *testing.T) {
		serve(boundaryRequest("GET", "/partial"))
		r, order := log.take(t)
		check(t, r)
		if order != "start finish record" || r.Status != 200 || r.BytesWritten != 3 || r.Outcome != "incomplete" || r.Reason != "response_aborted" {
			t.Fatalf("order %q record %+v", order, r)
		}
	})
	t.Run("panic", func(t *testing.T) {
		serve(boundaryRequest("GET", "/panic"))
		r, _ := log.take(t)
		check(t, r)
		if r.Status != 0 || !r.FirstByte.IsZero() || r.BytesWritten != 0 || r.Outcome != "incomplete" || r.Reason != "handler_panic" {
			t.Fatalf("record %+v", r)
		}
	})
	t.Run("redirect", func(t *testing.T) {
		seen = nil
		w := serve(boundaryRequest("GET", "/old"))
		r, _ := log.take(t)
		if seen != nil || r.Status != 301 || r.Reason != "redirect" || r.Outcome != "success" || int(r.BytesWritten) != w.Body.Len() || r.OriginKey != "" {
			t.Fatalf("record %+v", r)
		}
	})
	t.Run("unknown host", func(t *testing.T) {
		req := boundaryRequest("GET", "/ok?a=b")
		req.Host = "Evil.Example:8443"
		w := serve(req)
		r, order := log.take(t)
		if order != "start reject finish record" || r.Status != 421 || !r.Rejected || r.Reason != "unknown_host" || r.Host != "" ||
			r.Authority != "Evil.Example:8443" || r.EscapedPath != "/ok" || r.RawQuery != "a=b" || r.Outcome != "" || int(r.BytesWritten) != w.Body.Len() {
			t.Fatalf("order %q record %+v", order, r)
		}
	})
	t.Run("unsupported method keeps the admitted host", func(t *testing.T) {
		serve(boundaryRequest("POST", "/ok"))
		if r, _ := log.take(t); r.Status != 405 || r.Reason != "unsupported_method" || r.Host != "example.com" || r.Authority != "" || r.Method != "POST" {
			t.Fatalf("record %+v", r)
		}
	})
	t.Run("failed responses", func(t *testing.T) {
		broken := func(r *http.Request) {
			defer func() {
				if v := recover(); v != http.ErrAbortHandler {
					t.Fatalf("panic %v", v)
				}
			}()
			h.ServeHTTP(&brokenWriter{}, r)
		}
		broken(boundaryRequest("GET", "/old"))
		r, _ := log.take(t)
		if r.Status != 301 || r.Reason != "redirect" || r.Outcome != "incomplete" || r.TerminationReason != "client_write_failure" || r.BytesWritten != 0 {
			t.Fatalf("aborted redirect %+v", r)
		}
		req := boundaryRequest("GET", "/ok")
		req.Host = "evil.example"
		broken(req)
		r, order := log.take(t)
		if order != "start reject finish record" || !r.Rejected || r.Reason != "unknown_host" || r.Outcome != "" || r.TerminationReason != "client_write_failure" {
			t.Fatalf("order %q aborted rejection %+v", order, r)
		}
	})
	t.Run("unparseable target", func(t *testing.T) {
		req := boundaryRequest("GET", "/")
		req.RequestURI = "/a/../b?token=x"
		serve(req)
		if r, _ := log.take(t); r.Status != 400 || r.Reason != "invalid_target" || r.EscapedPath != "" || r.RawQuery != "" {
			t.Fatalf("record %+v", r)
		}
	})

	t.Run("overload", func(t *testing.T) {
		cfg := handlerConfig(t)
		cfg.Server.MaxConcurrentRequests = 1
		var log recordLog
		entered, release := make(chan struct{}), make(chan struct{})
		h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
			close(entered)
			<-release
		}, WithObserver(log.observe), WithRecordSink(log.sink))
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.ServeHTTP(httptest.NewRecorder(), boundaryRequest("GET", "/held"))
		}()
		<-entered
		w := httptest.NewRecorder()
		h.ServeHTTP(w, boundaryRequest("GET", "/busy?x=1"))
		r, order := log.take(t)
		if w.Code != 503 || order != "start reject record" || r.Status != 503 || !r.Rejected || r.Reason != "request_capacity" ||
			!r.Admitted.IsZero() || r.Host != "" || r.Authority != "example.com" || r.EscapedPath != "/busy" || r.RawQuery != "x=1" {
			t.Fatalf("order %q record %+v", order, r)
		}
		func() {
			defer func() {
				if v := recover(); v != http.ErrAbortHandler {
					t.Fatalf("panic %v", v)
				}
			}()
			h.ServeHTTP(&brokenWriter{}, boundaryRequest("GET", "/busy"))
		}()
		r, order = log.take(t)
		if order != "reject record" || !r.Rejected || r.Reason != "request_capacity" || r.Outcome != "" || r.TerminationReason != "client_write_failure" {
			t.Fatalf("order %q aborted overload %+v", order, r)
		}
		close(release)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("held request did not finish")
		}
		if r, _ := log.take(t); r.EscapedPath != "/held" || r.Rejected {
			t.Fatalf("record %+v", r)
		}
	})

	t.Run("no sink, no record", func(t *testing.T) {
		found := true
		h := makeHandler(t, handlerConfig(t), func(w http.ResponseWriter, r *http.Request) { found = telemetry.RecordFrom(r.Context()) != nil })
		h.ServeHTTP(httptest.NewRecorder(), boundaryRequest("GET", "/"))
		if found {
			t.Fatal("record without a sink")
		}
	})
}
