package httpserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/redirect"
)

func handlerConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("s3:\n  bucket: test-bucket\n  prefix: public\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func makeHandler(t *testing.T, cfg *config.Config, next http.HandlerFunc, options ...HandlerOption) http.Handler {
	t.Helper()
	h, err := NewHandler(cfg, next, options...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func boundaryRequest(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Host = "example.com"
	r.RemoteAddr = "192.0.2.10:12345"
	return r
}

func TestHandlerHostAdmission(t *testing.T) {
	for _, tc := range []struct {
		host, normalized string
		status           int
	}{
		{"EXAMPLE.com.:8080", "example.com", 200},
		{"127.0.0.1:80", "127.0.0.1", 200},
		{"[2001:0db8::1]:443", "2001:db8::1", 200},
		{"[::ffff:192.0.2.1]", "::ffff:192.0.2.1", 200},
		{"xn--caf-dma.example", "xn--caf-dma.example", 200},
		{"other.example", "", 421},
		{"café.example", "", 400},
		{"", "", 400}, {"example.com:", "", 400},
		{"example.com:0", "", 400}, {"example.com:65536", "", 400},
		{"example.com:+80", "", 400}, {"example.com:80:90", "", 400},
		{"example.com..", "", 400}, {"-example.com", "", 400},
		{"example_com", "", 400}, {"example.com/evil", "", 400},
		{"user@example.com", "", 400}, {"[fe80::1%eth0]", "", 400},
		{"2001:db8::1", "", 400}, {"[127.0.0.1]", "", 400},
	} {
		t.Run(tc.host, func(t *testing.T) {
			cfg := handlerConfig(t)
			cfg.Hosts = []string{"example.com", "127.0.0.1", "2001:db8::1", "::ffff:192.0.2.1", "xn--caf-dma.example"}
			cfg.Redirects = []redirect.Rule{{From: "/old", To: "/new", Status: 308, Query: "drop"}}
			calls := 0
			var events []Observation
			h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
				calls++
				m, ok := MetadataFromContext(r.Context())
				if !ok || m.Host != tc.normalized {
					t.Errorf("metadata: %+v, %v", m, ok)
				}
			}, WithObserver(func(o Observation) { events = append(events, o) }))
			r := boundaryRequest("GET", "/")
			r.Host = tc.host
			r.Header.Set("Forwarded", "host=example.com")
			r.Header.Set("X-Forwarded-Host", "example.com")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || calls != boolInt(tc.status == 200) {
				t.Fatalf("status=%d calls=%d", w.Code, calls)
			}
			if tc.status != 200 {
				for _, event := range events {
					if event.Host != "" {
						t.Errorf("rejected host leaked: %+v", event)
					}
				}
				r = boundaryRequest("GET", "/old")
				r.Host = tc.host
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.status || w.Header().Get("Location") != "" {
					t.Errorf("redirect ran before host admission")
				}
			}
		})
	}
	// An omitted allowlist still rejects malformed names, and Host-header map
	// values never substitute for the authority provided by net/http.
	h := makeHandler(t, handlerConfig(t), func(w http.ResponseWriter, r *http.Request) {})
	r := boundaryRequest("GET", "/")
	r.Host = "unlisted.example"
	r.Header.Set("Host", "evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("omitted allowlist: %d", w.Code)
	}
	r.Host = ""
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("header-map fallback: %d", w.Code)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestHandlerAbsoluteFormAuthority(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Hosts = []string{"example.com"}
	calls := 0
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) { calls++ })
	for _, tc := range []struct {
		target, hostHeader string
		status             int
	}{
		{"http://example.com/asset?x=1", "evil.example", 200},
		{"https://example.com", "evil.example", 200},
		{"http://evil.example/asset", "example.com", 421},
		{"http://user@example.com/", "example.com", 400},
		{"http://example.com/a%2fb", "example.com", 400},
	} {
		t.Run(tc.target, func(t *testing.T) {
			r, err := http.ReadRequest(bufio.NewReader(strings.NewReader("GET " + tc.target + " HTTP/1.1\r\nHost: " + tc.hostHeader + "\r\n\r\n")))
			if err != nil {
				t.Fatal(err)
			}
			r.RemoteAddr = "192.0.2.1:80"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d want %d", w.Code, tc.status)
			}
		})
	}
	if calls != 2 {
		t.Fatalf("content calls=%d", calls)
	}
}

func TestHandlerValidationRedirectsAndObjectMapping(t *testing.T) {
	cfg := handlerConfig(t)
	longPath := "/" + strings.Repeat("a", 1100)
	cfg.Redirects = []redirect.Rule{
		{From: "/old?x={x}", To: "/new/{x}", Status: 308, Query: "preserve"},
		{From: longPath, To: "/short", Status: 301, Query: "drop"},
	}
	calls := 0
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		calls++
		m, ok := MetadataFromContext(r.Context())
		if !ok || m.Object == nil || m.Object.Key != "public/café/index.html" || m.Object.Path != "/café/index.html" || m.Target.RawQuery != "versionId=other&response-content-type=evil" {
			t.Errorf("unsafe mapping: %+v", m)
		}
		m.Object.Key = "mutated"
		again, _ := MetadataFromContext(r.Context())
		if again.Object.Key != "public/café/index.html" {
			t.Error("mutable context metadata")
		}
	})
	for _, tc := range []struct {
		target   string
		status   int
		location string
	}{
		{"/old?x=a%20b&extra=%23tail", 308, "/new/a%20b?x=a%20b&extra=%23tail"},
		{"/old?x=a%2Fb", 400, ""},
		{"/old?x=a&x=b", 400, ""},
		{"/old?x=%0d%0aHeader:injected", 400, ""},
		{"/old?x=%ZZ", 400, ""},
		{"/.private", 404, ""},
		{"/a%2Fb", 400, ""}, {"/a%5Cb", 400, ""},
		{"/%252e%252e/secret", 400, ""}, {"//evil.example/file", 400, ""},
		{"/a/../b", 400, ""}, {"/a//b", 400, ""},
		{"/a?x=bad%0a", 400, ""},
		{longPath, 301, "/short"},
		{"/" + strings.Repeat("b", 1100), 414, ""},
		{"/" + strings.Repeat("a", 8192), 414, ""},
	} {
		t.Run(tc.target[:min(len(tc.target), 70)], func(t *testing.T) {
			r := boundaryRequest("GET", "/")
			// Keep malformed escapes as raw input without asking httptest's
			// constructor to parse an intentionally invalid URL.
			if !strings.Contains(tc.target, "%ZZ") {
				r = boundaryRequest("GET", tc.target)
			} else {
				r.RequestURI = tc.target
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Location") != tc.location {
				t.Fatalf("status=%d location=%q", w.Code, w.Header().Get("Location"))
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Error("generated response cacheable")
			}
		})
	}
	if calls != 0 {
		t.Fatalf("content called on rejected target: %d", calls)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, boundaryRequest("GET", "/caf%C3%A9/?versionId=other&response-content-type=evil"))
	if w.Code != 200 || calls != 1 {
		t.Fatalf("mapping status=%d calls=%d", w.Code, calls)
	}
}

type unreadBody struct{ reads atomic.Int32 }

func (b *unreadBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, errors.New("must not read") }
func (b *unreadBody) Close() error             { return nil }

func TestHandlerMethodsBodiesHEADAndHeaders(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Headers.Set["Referrer-Policy"] = "no-referrer"
	cfg.Redirects = []redirect.Rule{{From: "/old", To: "/new", Status: 302, Query: "drop"}}
	calls := 0
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("X-Content-Type-Options", "unsafe")
		w.Header().Set("X-Request-ID", "forged-by-next")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "7")
		_, _ = io.WriteString(w, "content")
	})
	for _, method := range []string{"POST", "PUT", "OPTIONS", "TRACE"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, boundaryRequest(method, "/"))
		if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s status=%d Allow=%q", method, w.Code, w.Header().Get("Allow"))
		}
	}
	for _, method := range []string{"GET", "HEAD"} {
		for _, length := range []int64{1, -1} {
			body := &unreadBody{}
			r := boundaryRequest(method, "/")
			r.Body = body
			r.ContentLength = length
			if length == -1 {
				r.TransferEncoding = []string{"chunked"}
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 400 || body.reads.Load() != 0 || w.Header().Get("Connection") != "close" || !r.Close {
				t.Errorf("body rejection %s %d status=%d reads=%d", method, length, w.Code, body.reads.Load())
			}
			if method == "HEAD" && w.Body.Len() != 0 {
				t.Error("HEAD error body")
			}
		}
	}
	if calls != 0 {
		t.Fatalf("unexpected content calls=%d", calls)
	}
	for _, target := range []string{"/", "/old", "/.hidden"} {
		get := httptest.NewRecorder()
		h.ServeHTTP(get, boundaryRequest("GET", target))
		head := httptest.NewRecorder()
		h.ServeHTTP(head, boundaryRequest("HEAD", target))
		if get.Code != head.Code || head.Body.Len() != 0 || get.Header().Get("Content-Length") != head.Header().Get("Content-Length") {
			t.Errorf("HEAD mismatch for %s: GET=%d HEAD=%d body=%q", target, get.Code, head.Code, head.Body.String())
		}
		for _, w := range []*httptest.ResponseRecorder{get, head} {
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" || !validRequestID(w.Header().Get("X-Request-ID")) || w.Header().Get("X-Request-ID") == "forged-by-next" || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("boundary headers: %+v", w.Header())
			}
		}
	}
	// A nonnil server-side Body alone is not evidence of payload (HTTP/2
	// commonly supplies one on a normal bodyless GET).
	r := boundaryRequest("GET", "/")
	body := &unreadBody{}
	r.Body = body
	r.ContentLength = 0
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || body.reads.Load() != 0 {
		t.Errorf("bodyless nonnil body: status=%d reads=%d", w.Code, body.reads.Load())
	}
}

func TestHandlerSnapshot(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Hosts = []string{"example.com"}
	cfg.Headers.Set["Referrer-Policy"] = "no-referrer"
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.Redirects = []redirect.Rule{{From: "/old/{id}", To: "/new/{id}", Status: 302, Query: "drop", Captures: map[string]redirect.Capture{"id": {Pattern: "[0-9]+"}}}}
	cfg.CORS = &config.CORS{AllowedOrigins: []string{"https://client.example"}}
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		m, _ := MetadataFromContext(r.Context())
		if m.Object.Key != "public/index.html" || m.ClientIP.String() != "198.51.100.2" || m.RequestID != "proxy-id" {
			t.Errorf("mutated snapshot: %+v", m)
		}
	})
	cfg.Hosts[0] = "evil.example"
	cfg.S3.Prefix = "evil/"
	cfg.Index = "evil.html"
	cfg.Headers.Set["Referrer-Policy"] = "unsafe-url"
	cfg.Server.TrustedProxies[0] = "192.0.2.0/24"
	cfg.Redirects[0].To = "/changed/{id}"
	cfg.Redirects[0].Captures["id"] = redirect.Capture{Pattern: "[a-z]+"}
	cfg.CORS.AllowedOrigins[0] = "https://evil.example"
	for _, target := range []string{"/", "/old/123"} {
		r := boundaryRequest("GET", target)
		r.RemoteAddr = "10.1.1.1:80"
		r.Header.Set("X-Forwarded-For", "198.51.100.2")
		r.Header.Set("X-Request-ID", "proxy-id")
		r.Header.Set("Origin", "https://client.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Referrer-Policy") != "no-referrer" || w.Header().Get("Access-Control-Allow-Origin") != "https://client.example" {
			t.Errorf("mutated headers: %+v", w.Header())
		}
		if target != "/" && (w.Code != 302 || w.Header().Get("Location") != "/new/123") {
			t.Errorf("mutated redirect: %d %s", w.Code, w.Header().Get("Location"))
		}
	}
}

func TestHandlerCapacityCancellationAndPanic(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Server.MaxConcurrentRequests = 1
	entered := make(chan struct{}, 1)
	var mu sync.Mutex
	var events []Observation
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("test panic")
		}
		if r.URL.Path == "/committed-panic" {
			w.WriteHeader(201)
			panic("test committed panic")
		}
		if r.URL.Path == "/wait" {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
	}, WithObserver(func(o Observation) { mu.Lock(); events = append(events, o); mu.Unlock() }))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), boundaryRequest("GET", "/wait").WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("content not entered")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, boundaryRequest("GET", "/"))
	if w.Code != 503 {
		t.Fatalf("capacity status=%d", w.Code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled request did not release")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("handler swallowed panic")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), boundaryRequest("GET", "/panic"))
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("committed panic swallowed")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), boundaryRequest("GET", "/committed-panic"))
	}()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, boundaryRequest("GET", "/"))
	if w.Code != 200 {
		t.Fatalf("slot leak: %d", w.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	var overload, panicFinished, committedPanic, zero bool
	for _, event := range events {
		if event.Inflight < 0 || event.Inflight > 1 {
			t.Errorf("invalid inflight: %+v", event)
		}
		if event.Event == ObservationReject && event.Reason == "request_capacity" {
			overload = true
		}
		if event.Event == ObservationFinish && event.Reason == "handler_panic" {
			if event.Status == 0 {
				panicFinished = true
			}
			if event.Status == 201 {
				committedPanic = true
			}
		}
		if event.Event == ObservationFinish && event.Inflight == 0 {
			zero = true
		}
	}
	if !overload || !panicFinished || !committedPanic || !zero {
		t.Errorf("missing observation: %+v", events)
	}
}

func TestHandlerRejectsMissingDependencies(t *testing.T) {
	if _, err := NewHandler(nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {
		t.Error("nil config accepted")
	}
	if _, err := NewHandler(handlerConfig(t), nil); err == nil {
		t.Error("nil content handler accepted")
	}
}

func TestHandlerClientIPAndRequestID(t *testing.T) {
	for _, tc := range []struct {
		name, peer, header               string
		forwarded, ids                   []string
		ip, source, reason, id, idSource string
	}{
		{"untrusted spoof", "192.0.2.1:80", "X-Forwarded-For", []string{"10.1.1.1"}, []string{"forged"}, "192.0.2.1", "peer", "", "", "generated"},
		{"rightmost untrusted", "10.1.1.1:80", "X-Forwarded-For", []string{"6.6.6.6, 198.51.100.8, 10.2.2.2"}, []string{"Proxy:123._-"}, "198.51.100.8", "x_forwarded_for", "", "Proxy:123._-", "proxy"},
		{"all trusted", "10.1.1.1:80", "X-Forwarded-For", []string{"10.2.2.2", "10.3.3.3"}, []string{"ok"}, "10.2.2.2", "x_forwarded_for", "", "ok", "proxy"},
		{"bad first hop", "10.1.1.1:80", "X-Forwarded-For", []string{"bad, 198.51.100.8"}, []string{"ok"}, "10.1.1.1", "peer", "invalid_forwarded_chain", "ok", "proxy"},
		{"empty hop", "10.1.1.1:80", "X-Forwarded-For", []string{"198.51.100.8,,10.2.2.2"}, nil, "10.1.1.1", "peer", "invalid_forwarded_chain", "", "generated"},
		{"overlong hops", "10.1.1.1:80", "X-Forwarded-For", []string{"1.1.1.1,2.2.2.2,3.3.3.3,4.4.4.4"}, nil, "10.1.1.1", "peer", "forwarded_chain_too_long", "", "generated"},
		{"overlong bytes", "10.1.1.1:80", "X-Forwarded-For", []string{strings.Repeat(" ", 256) + "1.1.1.1"}, nil, "10.1.1.1", "peer", "forwarded_chain_too_long", "", "generated"},
		{"port not IP", "10.1.1.1:80", "X-Forwarded-For", []string{"198.51.100.8:80"}, nil, "10.1.1.1", "peer", "invalid_forwarded_chain", "", "generated"},
		{"missing", "10.1.1.1:80", "X-Forwarded-For", nil, nil, "10.1.1.1", "peer", "missing_client_ip_header", "", "generated"},
		{"IPv6", "[2001:db8::1]:80", "X-Forwarded-For", []string{"2001:db9::2"}, []string{"ok"}, "2001:db9::2", "x_forwarded_for", "", "ok", "proxy"},
		{"mapped peer", "[::ffff:10.1.1.1]:80", "X-Forwarded-For", []string{"::ffff:198.51.100.8"}, []string{"ok"}, "198.51.100.8", "x_forwarded_for", "", "ok", "proxy"},
		{"real IP", "10.1.1.1:80", "X-Real-IP", []string{"198.51.100.8"}, []string{"ok"}, "198.51.100.8", "x_real_ip", "", "ok", "proxy"},
		{"real list", "10.1.1.1:80", "X-Real-IP", []string{"198.51.100.8, 10.2.2.2"}, []string{"ok"}, "10.1.1.1", "peer", "invalid_real_ip", "ok", "proxy"},
		{"duplicate real", "10.1.1.1:80", "X-Real-IP", []string{"198.51.100.8", "198.51.100.9"}, []string{"ok"}, "10.1.1.1", "peer", "invalid_real_ip", "ok", "proxy"},
		{"duplicate ID", "10.1.1.1:80", "X-Forwarded-For", nil, []string{"first", "second"}, "10.1.1.1", "peer", "missing_client_ip_header", "", "generated"},
		{"ID comma", "10.1.1.1:80", "X-Forwarded-For", nil, []string{"first,second"}, "10.1.1.1", "peer", "missing_client_ip_header", "", "generated"},
		{"ID max", "10.1.1.1:80", "X-Forwarded-For", nil, []string{strings.Repeat("a", 128)}, "10.1.1.1", "peer", "missing_client_ip_header", strings.Repeat("a", 128), "proxy"},
		{"ID overlong", "10.1.1.1:80", "X-Forwarded-For", nil, []string{strings.Repeat("a", 129)}, "10.1.1.1", "peer", "missing_client_ip_header", "", "generated"},
		{"ID invalid", "10.1.1.1:80", "X-Forwarded-For", nil, []string{"id café"}, "10.1.1.1", "peer", "missing_client_ip_header", "", "generated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := handlerConfig(t)
			cfg.Server.TrustedProxies = []string{"10.0.0.0/8", "2001:db8::/32"}
			cfg.Server.MaxForwardedHops = 3
			cfg.Server.ClientIPHeader = tc.header
			var m RequestMetadata
			h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) { m, _ = MetadataFromContext(r.Context()) })
			r := boundaryRequest("GET", "/")
			r.RemoteAddr = tc.peer
			for _, v := range tc.forwarded {
				r.Header.Add(tc.header, v)
			}
			for _, v := range tc.ids {
				r.Header.Add("X-Request-ID", v)
			}
			r.Header.Set("Forwarded", "for=6.6.6.6")
			if tc.header == "X-Real-IP" {
				r.Header.Set("X-Forwarded-For", "6.6.6.6")
			} else {
				r.Header.Set("X-Real-IP", "6.6.6.6")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 200 || m.ClientIP != netip.MustParseAddr(tc.ip) || m.ClientIPSource != tc.source || m.AttributionReason != tc.reason || m.RequestIDSource != tc.idSource {
				t.Fatalf("metadata=%+v status=%d", m, w.Code)
			}
			if !validRequestID(m.RequestID) || w.Header().Get("X-Request-ID") != m.RequestID || tc.id != "" && m.RequestID != tc.id {
				t.Errorf("request ID mismatch: %+v", m)
			}
			if tc.idSource == "generated" {
				if len(m.RequestID) != 32 {
					t.Errorf("generated ID length %d", len(m.RequestID))
				}
				w2 := httptest.NewRecorder()
				h.ServeHTTP(w2, r)
				if w2.Header().Get("X-Request-ID") == w.Header().Get("X-Request-ID") {
					t.Error("generated ID reused")
				}
			}
		})
	}
	if _, ok := MetadataFromContext(context.Background()); ok {
		t.Error("metadata on unrelated context")
	}
}

func TestHandlerConcurrentRequestBudget(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Server.MaxConcurrentRequests = 3
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-release })
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.ServeHTTP(httptest.NewRecorder(), boundaryRequest("GET", "/")) }()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("slot not entered")
		}
	}
	for i := 0; i < 16; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, boundaryRequest("GET", "/"))
		if w.Code != 503 {
			t.Errorf("overload %d: %d", i, w.Code)
		}
	}
	close(release)
	wg.Wait()
	// All requests can share an HTTP/2 connection/peer; the budget counts
	// requests and cannot be bypassed by stream multiplexing.
	if got := len(h.(*handler).slots); got != 0 {
		t.Fatal(fmt.Sprintf("slots still held: %d", got))
	}
}
