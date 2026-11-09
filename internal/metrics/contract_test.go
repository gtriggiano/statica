package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/admin"
	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/storage"
	"github.com/gtriggiano/statica/internal/storage/s3"
	"github.com/gtriggiano/statica/internal/telemetry"
)

// boundary returns the real public HTTP boundary feeding m, over a content
// stand-in that reports a cache hit and writes a small body.
func boundary(t *testing.T, cfg *config.Config, m *Metrics) http.Handler {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec := telemetry.RecordFrom(r.Context()); rec != nil {
			rec.OriginKey, rec.CacheOutcome, rec.CacheBytes = "canary-origin-key.html", "hit", 2
		}
		_, _ = io.WriteString(w, "ok")
	})
	h, err := httpserver.NewHandler(cfg, next, httpserver.WithObserver(m.HTTPObserver), httpserver.WithRecordSink(m.RecordSink))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func serve(h http.Handler, method, host, target string, header http.Header) int {
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	r.RemoteAddr = "203.0.113.77:40000"
	for k, v := range header {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// labelValues returns the sorted distinct values of label across every
// gathered statica series that has it.
func labelValues(t *testing.T, m *Metrics, label string) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, f := range gather(t, m) {
		if !strings.HasPrefix(f.GetName(), "statica_") {
			continue
		}
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				if l.GetName() == label {
					seen[l.GetValue()] = true
				}
			}
		}
	}
	var out []string
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func TestHostLabelsWithAllowlist(t *testing.T) {
	cfg := parseConfig(t, "hosts: [a.example, B.Example.]\n")
	m := newMetrics(t, cfg)
	h := boundary(t, cfg, m)
	for _, tc := range []struct {
		host   string
		status int
	}{
		{"a.example", 200}, {"A.EXAMPLE.:8080", 200}, {"b.example", 200},
		{"evil.example", 421}, {"other.a.example", 421}, {"10.0.0.1", 421}, {"[2001:db8::1]", 421}, {"bad host", 400}, {"", 400},
	} {
		if got := serve(h, http.MethodGet, tc.host, "/", nil); got != tc.status {
			t.Fatalf("host %q: status %d want %d", tc.host, got, tc.status)
		}
	}
	// Arbitrary hosts in many requests never create host series.
	for i := 0; i < 100; i++ {
		serve(h, http.MethodGet, fmt.Sprintf("random-%d.example", i), "/", nil)
	}
	if hosts := labelValues(t, m, "host"); !slices.Equal(hosts, []string{"a.example", "b.example"}) {
		t.Fatalf("host labels %v", hosts)
	}
	got := scrape(t, m)
	if got[requests("a.example", "GET", "200", "success")] != 2 || got[requests("b.example", "GET", "200", "success")] != 1 {
		t.Fatalf("admitted requests %v", got)
	}
	if got[rejected("unknown_host")] != 104 || got[rejected("malformed_host")] != 2 {
		t.Fatalf("rejections %v %v", got[rejected("unknown_host")], got[rejected("malformed_host")])
	}
	if reasons := labelValues(t, m, "reason"); !slices.Equal(reasons, []string{"malformed_host", "unknown_host"}) {
		t.Fatalf("reason labels %v", reasons)
	}
	if got["statica_http_requests_in_flight"] != 0 {
		t.Fatalf("in-flight %v", got["statica_http_requests_in_flight"])
	}
}

func TestHostLabelsWithoutAllowlist(t *testing.T) {
	cfg := parseConfig(t, "")
	m := newMetrics(t, cfg)
	h := boundary(t, cfg, m)
	// Without an allowlist every accepted host is labelled as normalized,
	// without a cap: series grow with distinct hosts, the accepted trade-off.
	const distinct = 50
	want := []string{"10.0.0.1", "2001:db8::1", "www.example"}
	for i := 0; i < distinct; i++ {
		host := fmt.Sprintf("site-%02d.example", i)
		want = append(want, host)
		serve(h, http.MethodGet, host, "/", nil)
	}
	for _, host := range []string{"WWW.Example.", "www.example:8443", "10.0.0.1:80", "[2001:0DB8::1]:443"} {
		if got := serve(h, http.MethodGet, host, "/", nil); got != 200 {
			t.Fatalf("host %q: status %d", host, got)
		}
	}
	serve(h, http.MethodGet, "bad host", "/", nil)
	sort.Strings(want)
	if hosts := labelValues(t, m, "host"); !slices.Equal(hosts, want) {
		t.Fatalf("host labels\n got %v\nwant %v", hosts, want)
	}
	got := scrape(t, m)
	if got[requests("www.example", "GET", "200", "success")] != 2 || got[cacheRequests("site-07.example", "hit")] != 1 {
		t.Fatalf("normalized host series %v", got)
	}
	var hostSeries int
	for k := range got {
		if strings.HasPrefix(k, "statica_http_requests_total{") {
			hostSeries++
		}
	}
	if hostSeries != len(want) {
		t.Fatalf("request series %d want one per host (%d)", hostSeries, len(want))
	}
}

// canaries appear in request input, records and origin traffic, and must
// never appear in any label value.
var canaries = []string{"canary-path", "canary-query", "canary-secret", "canary-origin-key", "canary-request-id", "canary-authority",
	"canary-diagnostic", "203.0.113.77", "198.51.100.23", "192.0.2.99"}

func TestNoRequestDataInLabels(t *testing.T) {
	cfg := parseConfig(t, "server:\n  trusted_proxies: ['203.0.113.0/24']\n")
	m := newMetrics(t, cfg)
	h := boundary(t, cfg, m)
	proxied := http.Header{"X-Request-Id": {"canary-request-id-1"}, "X-Forwarded-For": {"198.51.100.23"}}
	serve(h, http.MethodGet, "site.example", "/canary-path/index.html?canary-query=canary-secret", proxied)
	serve(h, http.MethodHead, "site.example", "/canary-path/?token=canary-secret", proxied)
	serve(h, http.MethodGet, "site.example", "/%2e%2e/canary-path", proxied)          // invalid target
	serve(h, http.MethodDelete, "site.example", "/canary-path?canary-query", proxied) // unsupported method
	serve(h, http.MethodGet, "canary-authority:99999", "/canary-path", proxied)       // malformed host
	serve(h, http.MethodGet, "site.example", "/canary-path", http.Header{"Content-Length": {"3"}})

	m.RecordSink(&telemetry.Record{Method: "GET", Host: "site.example", EscapedPath: "/canary-path/a", RawQuery: "canary-query=canary-secret",
		RequestID: "canary-request-id-2", ClientIP: netip.MustParseAddr("198.51.100.23"), PeerIP: netip.MustParseAddr("192.0.2.99"),
		OriginKey: "pub/canary-origin-key", Resolution: "spa", Outcome: "origin_failure", ErrorClass: "origin_denied", Status: 404,
		CacheOutcome: "negative_hit", CacheDiagnostic: "canary-diagnostic", Started: epoch, FirstByte: epoch, Finished: epoch})
	m.RecordSink(&telemetry.Record{Method: "GET", Authority: "canary-authority.example", EscapedPath: "/canary-path", RawQuery: "canary-query",
		RequestID: "canary-request-id-3", ClientIP: netip.MustParseAddr("198.51.100.23"), Rejected: true, Reason: "unknown_host", Status: 421})

	// Origin traffic for a canary key, through the real adapter.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()
	client := newS3Client(t, srv.URL, 1, m)
	o, err := client.Get(storage.WithPurpose(context.Background(), storage.PurposeCacheFill), "pub/canary-origin-key.html", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, o.Body)
	_ = o.Body.Close()
	_, _ = client.Head(context.Background(), "pub/canary-origin-key-missing")

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

	var values int
	for _, f := range gather(t, m) {
		for _, metric := range f.GetMetric() {
			for _, l := range metric.GetLabel() {
				values++
				v := l.GetValue()
				for _, c := range canaries {
					if strings.Contains(v, c) {
						t.Errorf("%s{%s=%q} carries request data %q", f.GetName(), l.GetName(), v, c)
					}
				}
				if strings.ContainsAny(v, "/?=&%") {
					t.Errorf("%s{%s=%q} looks like a path or query", f.GetName(), l.GetName(), v)
				}
				if _, err := netip.ParseAddr(v); err == nil {
					t.Errorf("%s{%s=%q} is an address", f.GetName(), l.GetName(), v)
				}
			}
		}
	}
	if values == 0 {
		t.Fatal("no label values gathered")
	}
	if hosts := labelValues(t, m, "host"); !slices.Equal(hosts, []string{"site.example"}) {
		t.Fatalf("host labels %v", hosts)
	}
}

func newS3Client(t *testing.T, endpoint string, attempts int, m *Metrics) *s3.Client {
	t.Helper()
	cfg := config.S3{Bucket: "fixture-bucket", Region: "eu-west-1", Prefix: "pub/", Endpoint: endpoint, PathStyle: true, AllowHTTP: true,
		ResponseHeaderTimeout: time.Second, ReadIdleTimeout: time.Second, MaxAttempts: attempts, Features: config.Features{ConditionalReads: true, RangeReads: true}}
	c, err := s3.New(context.Background(), cfg, s3.WithCredentials(staticCredentials{}), s3.WithObserver(m.OriginObserver()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
