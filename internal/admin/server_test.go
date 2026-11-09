package admin

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/storage"
)

type observingListener struct {
	net.Listener
	accepted chan struct{}
}

func (l *observingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		select {
		case l.accepted <- struct{}{}:
		default:
		}
	}
	return c, err
}

type adminFixture struct {
	server   *Server
	listener *observingListener
	url      string
	client   *http.Client
}

func startAdmin(t *testing.T, r *Readiness, metrics http.Handler, l limits) *adminFixture {
	t.Helper()
	return startLoggedAdmin(t, r, metrics, nil, l)
}

func startLoggedAdmin(t *testing.T, r *Readiness, metrics http.Handler, logger *slog.Logger, l limits) *adminFixture {
	t.Helper()
	s, err := newServer(r, metrics, logger, l)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &adminFixture{server: s, listener: &observingListener{Listener: inner, accepted: make(chan struct{}, 16)}, url: "http://" + inner.Addr().String(), client: &http.Client{Timeout: 3 * time.Second}}
	done := make(chan error, 1)
	go func() { done <- s.Serve(f.listener) }()
	t.Cleanup(func() {
		f.client.CloseIdleConnections()
		_ = s.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return f
}

func (f *adminFixture) do(t *testing.T, method, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, f.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%s %s: Cache-Control %q", method, path, resp.Header.Get("Cache-Control"))
	}
	return resp, string(body)
}

func (f *adminFixture) want(t *testing.T, method, path string, status int, body string) {
	t.Helper()
	if resp, got := f.do(t, method, path); resp.StatusCode != status || got != body {
		t.Fatalf("%s %s = %d %q; want %d %q", method, path, resp.StatusCode, got, status, body)
	}
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func get(t *testing.T, c net.Conn, path string) *http.Response {
	t.Helper()
	if _, err := io.WriteString(c, "GET "+path+" HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestAdminEndpoints(t *testing.T) {
	r := NewReadiness(nil)
	r.SetServing()
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "statica_ready 1\n") })
	f := startAdmin(t, r, metrics, defaultLimits)
	for _, c := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/livez", 200, "ok\n"},
		{"HEAD", "/livez", 200, ""},
		{"GET", "/readyz", 200, "ready\n"},
		{"HEAD", "/readyz", 200, ""},
		{"GET", "/readyz?verbose", 200, "ready\n"},
		{"GET", "/metrics", 200, "statica_ready 1\n"},
		{"HEAD", "/metrics", 200, ""},
		{"GET", "/", 404, "not found\n"},
		{"GET", "/livez/", 404, "not found\n"},
		{"GET", "/readyz/extra", 404, "not found\n"},
		{"GET", "/LIVEZ", 404, "not found\n"},
		{"GET", "/live%7A", 404, "not found\n"},
		{"GET", "//livez", 404, "not found\n"},
		{"GET", "/debug/pprof/", 404, "not found\n"},
		{"POST", "/nowhere", 404, "not found\n"},
		{"HEAD", "/nowhere", 404, ""},
	} {
		resp, body := f.do(t, c.method, c.path)
		if resp.StatusCode != c.status || body != c.body {
			t.Errorf("%s %s = %d %q; want %d %q", c.method, c.path, resp.StatusCode, body, c.status, c.body)
		}
	}
	for _, path := range []string{"/livez", "/readyz", "/metrics"} {
		for _, method := range []string{"POST", "PUT", "DELETE", "OPTIONS"} {
			resp, body := f.do(t, method, path)
			if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET, HEAD" || body != "method not allowed\n" {
				t.Errorf("%s %s = %d %q, Allow %q", method, path, resp.StatusCode, body, resp.Header.Get("Allow"))
			}
		}
	}
	resp, _ := f.do(t, "HEAD", "/readyz")
	if resp.ContentLength != int64(len("ready\n")) || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("HEAD /readyz headers: length %d, type %q", resp.ContentLength, resp.Header.Get("Content-Type"))
	}
}

func TestAdminMetricsDelegation(t *testing.T) {
	r := NewReadiness(nil)
	t.Run("nil", func(t *testing.T) {
		f := startAdmin(t, r, nil, defaultLimits)
		f.want(t, "GET", "/metrics", 404, "not found\n")
		f.want(t, "POST", "/metrics", 404, "not found\n")
	})
	t.Run("injected", func(t *testing.T) {
		var methods []string
		metrics := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			methods = append(methods, req.Method+" "+req.URL.RequestURI())
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			_, _ = io.WriteString(w, "# scrape\n")
		})
		f := startAdmin(t, r, metrics, defaultLimits)
		resp, body := f.do(t, "GET", "/metrics?name=x")
		if resp.StatusCode != 200 || body != "# scrape\n" || resp.Header.Get("Content-Type") != "text/plain; version=0.0.4" {
			t.Fatalf("GET /metrics = %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
		}
		f.want(t, "HEAD", "/metrics", 200, "")
		if strings.Join(methods, ",") != "GET /metrics?name=x,HEAD /metrics" {
			t.Fatalf("delegated %v", methods)
		}
	})
}

func TestAdminProbesFollowReadiness(t *testing.T) {
	r := NewReadiness(healthConfig()) // failure_threshold 3, success_threshold 2
	c := newClock(r)
	var next error
	w := newWorker(t, r, &fakeReader{head: func(context.Context, string) (storage.Metadata, error) { return storage.Metadata{}, next }})
	f := startAdmin(t, r, nil, defaultLimits)
	expect := func(status int, body string) {
		t.Helper()
		f.want(t, "GET", "/readyz", status, body)
		f.want(t, "HEAD", "/readyz", status, "")
		f.want(t, "GET", "/livez", 200, "ok\n")
	}
	expect(503, "not ready: initializing\n")
	r.SetServing()
	expect(503, "not ready: origin_unknown\n")
	w.check(context.Background())
	expect(200, "ready\n")
	next = &storage.Error{Kind: storage.Unavailable, Size: -1}
	for range 3 {
		w.check(context.Background())
	}
	expect(503, "not ready: origin_unhealthy\n") // liveness holds during the outage
	next = nil
	w.check(context.Background())
	w.check(context.Background())
	expect(200, "ready\n")
	c.advance(healthConfig().MaxStaleness + time.Second)
	expect(503, "not ready: origin_stale\n")
	w.check(context.Background())
	expect(200, "ready\n")
	r.SetDraining()
	expect(503, "not ready: draining\n") // liveness holds during drain
}

func TestAdminProbesNeverCallOrigin(t *testing.T) {
	r := NewReadiness(healthConfig()) // a one-hour interval: the first check is the only one
	reader := &fakeReader{}
	w := newWorker(t, r, reader)
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if s := r.Snapshot(); s.Ready {
			_, _ = io.WriteString(w, "statica_ready 1\n")
		}
	})
	f := startAdmin(t, r, metrics, defaultLimits)
	r.SetServing()
	w.Start()
	t.Cleanup(func() { stopWorker(t, w) })
	waitFor(t, "a first success", func() bool { return r.Snapshot().Checks[ResultSuccess] == 1 })
	for range 20 {
		f.want(t, "GET", "/readyz", 200, "ready\n")
		f.want(t, "HEAD", "/readyz", 200, "")
		f.want(t, "GET", "/livez", 200, "ok\n")
		f.want(t, "GET", "/metrics", 200, "statica_ready 1\n")
	}
	if heads, gets := reader.heads.Load(), reader.gets.Load(); heads != 1 || gets != 0 {
		t.Fatalf("probes and scrapes reached the origin: %d heads, %d gets", heads, gets)
	}
}

func TestAdminServerConnectionCap(t *testing.T) {
	l := defaultLimits
	l.connections = 1
	f := startAdmin(t, NewReadiness(nil), nil, l)
	addr := f.listener.Addr().String()
	first := dial(t, addr)
	if resp := get(t, first, "/livez"); resp.StatusCode != 200 {
		t.Fatalf("first connection status %d", resp.StatusCode)
	}
	<-f.listener.accepted
	second := dial(t, addr)
	if _, err := io.WriteString(second, "GET /livez HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// The first connection is an idle keep-alive holding the only slot, so the
	// second must wait in the OS backlog until it closes.
	select {
	case <-f.listener.accepted:
		t.Fatal("accepted a connection beyond the cap")
	case <-time.After(150 * time.Millisecond):
	}
	_ = first.Close()
	resp, err := http.ReadResponse(bufio.NewReader(second), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("second connection status %d", resp.StatusCode)
	}
}

func TestAdminServerTimeouts(t *testing.T) {
	short := defaultLimits
	short.readHeader, short.idle, short.write = 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	slow := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, "late\n")
	})
	f := startAdmin(t, NewReadiness(nil), slow, short)
	addr := f.listener.Addr().String()
	t.Run("header", func(t *testing.T) {
		c := dial(t, addr)
		_, _ = io.WriteString(c, "GET /livez HTTP/1.1\r\nHost:")
		var b [1]byte
		if _, err := c.Read(b[:]); err == nil {
			t.Fatal("incomplete headers were not closed")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("client deadline expired before the header deadline")
		}
	})
	t.Run("header-size", func(t *testing.T) {
		c := dial(t, addr)
		_, _ = io.WriteString(c, "GET /livez HTTP/1.1\r\nHost: localhost\r\nX-Large: "+strings.Repeat("a", 16<<10)+"\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("oversized headers status %d", resp.StatusCode)
		}
	})
	t.Run("idle", func(t *testing.T) {
		c := dial(t, addr)
		get(t, c, "/livez")
		var b [1]byte
		if _, err := c.Read(b[:]); !errors.Is(err, io.EOF) {
			t.Fatalf("idle keep-alive was not closed: %v", err)
		}
	})
	t.Run("write", func(t *testing.T) {
		c := dial(t, addr)
		_, _ = io.WriteString(c, "GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n")
		if _, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodGet}); err == nil {
			t.Fatal("a response outlived the write deadline")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("client deadline expired before the write deadline")
		}
	})
}

func TestAdminServerShutdown(t *testing.T) {
	t.Run("graceful", func(t *testing.T) {
		f := startAdmin(t, NewReadiness(nil), nil, defaultLimits)
		f.want(t, "GET", "/livez", 200, "ok\n") // leaves an idle keep-alive
		start := time.Now()
		if err := f.server.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Shutdown took %v", elapsed)
		}
		if c, err := net.DialTimeout("tcp", f.listener.Addr().String(), time.Second); err == nil {
			_ = c.Close()
			t.Fatal("listener still accepting after Shutdown")
		}
	})
	for name, budget := range map[string]struct{ server, ctx time.Duration }{
		"server-budget":  {100 * time.Millisecond, time.Hour},
		"context-budget": {time.Hour, 100 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			l := defaultLimits
			l.shutdown = budget.server
			started, cancelled := make(chan struct{}), make(chan struct{})
			stuck := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				close(started)
				<-req.Context().Done()
				close(cancelled)
			})
			f := startAdmin(t, NewReadiness(nil), stuck, l)
			c := dial(t, f.listener.Addr().String())
			_, _ = io.WriteString(c, "GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n")
			<-started
			ctx, cancel := context.WithTimeout(context.Background(), budget.ctx)
			defer cancel()
			start := time.Now()
			if err := f.server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Shutdown = %v; want an exhausted budget", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("Shutdown took %v with a 100ms budget", elapsed)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("the remaining request was not cancelled")
			}
		})
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestAdminServerLogsHandlerPanics(t *testing.T) {
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret panic value") })
	f := startLoggedAdmin(t, NewReadiness(nil), panicking, logger, defaultLimits)
	if resp, err := f.client.Get(f.url + "/metrics?token=secret"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a panicking handler produced a response")
	}
	waitFor(t, "the panic event", func() bool { return strings.Contains(logs.String(), "msg=admin_http_error") })
	f.want(t, "GET", "/livez", 200, "ok\n")
	out := logs.String()
	wantEvents(t, out, "level=WARN msg=admin_http_error error_class=handler_panic")
	for _, leak := range []string{"secret", "127.0.0.1", "goroutine", "panic serving"} {
		if strings.Contains(out, leak) {
			t.Fatalf("event copied %q from net/http:\n%s", leak, out)
		}
	}
	_, _ = httpErrorLog{logger}.Write([]byte("http: superfluous response.WriteHeader call from 127.0.0.1:1234\n"))
	_, _ = httpErrorLog{logger}.Write([]byte("http: Accept error: secret; retrying in 5ms\n"))
	if logs.String() != out {
		t.Fatalf("other net/http lines were not discarded:\n%s", logs.String())
	}
}

func TestAdminServerLifecycle(t *testing.T) {
	if _, err := NewServer(nil, nil); err == nil {
		t.Fatal("accepted a nil readiness model")
	}
	noSlots := defaultLimits
	noSlots.connections = 0
	if _, err := newServer(NewReadiness(nil), nil, nil, noSlots); err == nil {
		t.Fatal("accepted a zero connection limit")
	}
	if _, err := NewServerWithLogger(NewReadiness(nil), nil, nil); err != nil {
		t.Fatalf("a nil logger must discard: %v", err)
	}
	s, err := NewServer(NewReadiness(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(nil); err == nil {
		t.Fatal("served a nil listener")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve after Shutdown = %v", err)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve did not close its listener: %v", err)
	}
}
