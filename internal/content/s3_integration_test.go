package content_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/content"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/storage"
	origins3 "github.com/gtriggiano/statica/internal/storage/s3"
)

type sdkFixtureObject struct{ body, contentType, coding, etag, cacheControl string }

// sdkFixtureOrigin answers an If-None-Match only when conditional is set, as
// cache revalidation does; otherwise one reaching it is a 500, since browser
// validators must be evaluated locally. Such calls are recorded with an
// " if-none-match" suffix. now, when set, stamps Date like an origin whose
// clock agrees with the cache's.
type sdkFixtureOrigin struct {
	mu          sync.Mutex
	calls       []string
	objects     map[string]sdkFixtureObject
	conditional bool
	now         func() time.Time
}

func (o *sdkFixtureOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/fixture-bucket/")
	call := r.Method + " " + key
	if r.Header.Get("If-None-Match") != "" {
		call += " if-none-match"
	}
	o.mu.Lock()
	o.calls = append(o.calls, call)
	o.mu.Unlock()
	if o.now != nil {
		w.Header().Set("Date", o.now().UTC().Format(http.TimeFormat))
	}
	if !strings.HasPrefix(r.URL.Path, "/fixture-bucket/") || r.Host == "visitor.example" || !strings.Contains(r.Header.Get("Authorization"), "Credential=pipeline-key/") || r.Header.Get("Cookie") != "" {
		w.WriteHeader(500)
		return
	}
	if key == "pub/denied" {
		sdkFixtureError(w, 403, "AccessDenied")
		return
	}
	object, ok := o.objects[key]
	if !ok {
		sdkFixtureError(w, 404, "NoSuchKey")
		return
	}
	payload := object.body
	w.Header().Set("Content-Type", object.contentType)
	w.Header().Set("ETag", object.etag)
	w.Header().Set("Content-Encoding", object.coding)
	w.Header().Set("Cache-Control", object.cacheControl)
	w.Header().Set("Last-Modified", "Fri, 06 Nov 2026 12:00:00 GMT")
	w.Header().Set("X-Amz-Meta-Private", "origin-only")
	status := 200
	if raw := r.Header.Get("Range"); raw != "" {
		if r.Header.Get("If-Match") != object.etag {
			sdkFixtureError(w, 412, "PreconditionFailed")
			return
		}
		var first, last int
		if _, err := fmt.Sscanf(raw, "bytes=%d-%d", &first, &last); err != nil || first < 0 || last >= len(payload) || last < first {
			sdkFixtureError(w, 416, "InvalidRange")
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(payload)))
		payload = payload[first : last+1]
		status = 206
	} else if o.conditional && r.Header.Get("If-None-Match") == object.etag && r.Header.Get("If-Match") == "" {
		w.WriteHeader(http.StatusNotModified)
		return
	} else if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Match") != "" {
		// Browser validators are evaluated against one GET snapshot, rather than
		// being blindly forwarded into representation discovery.
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		_, _ = io.WriteString(w, payload)
	}
}
func (o *sdkFixtureOrigin) takeCalls() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	calls := append([]string(nil), o.calls...)
	o.calls = nil
	return calls
}
func sdkFixtureError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>private-origin-message</Message></Error>", code)
}
func sdkPipelineConfig(t *testing.T, endpoint string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(fmt.Sprintf("s3:\n  bucket: fixture-bucket\n  region: eu-west-1\n  prefix: pub/\n  endpoint: %s\n  path_style: true\n  allow_http: true\n  max_attempts: 1\ncompression:\n  mode: precompressed\nspa:\n  fallback: index.html\n", endpoint)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
func sdkPipeline(t *testing.T, cfg *config.Config) (string, <-chan content.Observation) {
	t.Helper()
	return sdkPipelineWith(t, cfg, nil)
}

// sdkPipelineWith adds the handler options that options derives from the
// SDK origin reader.
func sdkPipelineWith(t *testing.T, cfg *config.Config, options func(storage.Reader) []content.HandlerOption) (string, <-chan content.Observation) {
	t.Helper()
	reader, err := origins3.New(context.Background(), cfg.S3, origins3.WithCredentials(credentials.NewStaticCredentialsProvider("pipeline-key", "pipeline-secret", "pipeline-token")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	observations := make(chan content.Observation, 16)
	handlerOptions := []content.HandlerOption{content.WithObserver(func(o content.Observation) { observations <- o })}
	if options != nil {
		handlerOptions = append(handlerOptions, options(reader)...)
	}
	objectHandler, err := content.NewHandler(cfg, reader, handlerOptions...)
	if err != nil {
		t.Fatal(err)
	}
	admission, err := httpserver.NewHandler(cfg, objectHandler)
	if err != nil {
		t.Fatal(err)
	}
	server, err := httpserver.NewServer(cfg.Server, admission)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-stopped:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("public server did not stop")
		}
	})
	return "http://" + listener.Addr().String(), observations
}
func sdkObservation(t *testing.T, ch <-chan content.Observation) content.Observation {
	t.Helper()
	select {
	case observation := <-ch:
		return observation
	case <-time.After(5 * time.Second): // the observer runs after the body, possibly late under load
		t.Fatal("missing content observation")
		return content.Observation{}
	}
}

func TestRealSDKPublicPipeline(t *testing.T) {
	origin := &sdkFixtureOrigin{objects: map[string]sdkFixtureObject{
		"pub/plain.txt":  {body: "hello", contentType: "text/plain; charset=utf-8", etag: `"plain-v1"`, cacheControl: "public, max-age=60"},
		"pub/index.html": {body: "<html>shell</html>", contentType: "text/html; charset=utf-8", etag: `"shell-v1"`, cacheControl: "public, max-age=60"},
		"pub/app.js":     {body: "console.log('identity');", contentType: "application/javascript", etag: `"identity-v1"`},
		"pub/app.js.br":  {body: "brotli-fixture", contentType: "application/octet-stream", coding: "br", etag: `"brotli-v1"`},
	}}
	upstream := httptest.NewServer(origin)
	defer upstream.Close()
	cfg := sdkPipelineConfig(t, upstream.URL)
	public, observations := sdkPipeline(t, cfg)
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	for _, tc := range []struct {
		name, method, path                             string
		headers                                        http.Header
		status                                         int
		body, coding, etag, cacheControl, contentRange string
		calls                                          []string
		outcome                                        content.Outcome
		resolution                                     string
	}{
		{name: "ordinary", method: "GET", path: "/plain.txt?visitor=query", status: 200, body: "hello", etag: `"plain-v1"`, cacheControl: "public, max-age=60", calls: []string{"GET pub/plain.txt"}, outcome: content.OutcomeSuccess, resolution: "ordinary"},
		{name: "head", method: "HEAD", path: "/plain.txt", status: 200, etag: `"plain-v1"`, cacheControl: "public, max-age=60", calls: []string{"HEAD pub/plain.txt"}, outcome: content.OutcomeSuccess, resolution: "ordinary"},
		{name: "range", method: "GET", path: "/plain.txt", headers: http.Header{"Range": {"bytes=1-3"}}, status: 206, body: "ell", etag: `"plain-v1"`, cacheControl: "public, max-age=60", contentRange: "bytes 1-3/5", calls: []string{"HEAD pub/plain.txt", "GET pub/plain.txt"}, outcome: content.OutcomeSuccess, resolution: "ordinary"},
		{name: "local validator", method: "GET", path: "/plain.txt", headers: http.Header{"If-None-Match": {`"plain-v1"`}}, status: 304, etag: `"plain-v1"`, cacheControl: "public, max-age=60", calls: []string{"GET pub/plain.txt"}, outcome: content.OutcomeSuccess, resolution: "ordinary"},
		{name: "denied stops shell", method: "GET", path: "/denied", headers: http.Header{"Accept": {"text/html"}}, status: 404, body: "Not Found\n", cacheControl: "no-store", calls: []string{"GET pub/denied"}, outcome: content.OutcomeOriginFailure},
		{name: "confirmed missing shell", method: "GET", path: "/orders/42?source=browser", headers: http.Header{"Accept": {"text/html;charset=utf-8"}}, status: 200, body: "<html>shell</html>", etag: `"shell-v1"`, cacheControl: "no-store", calls: []string{"GET pub/orders/42", "GET pub/orders/42/index.html", "GET pub/index.html"}, outcome: content.OutcomeSuccess, resolution: "spa"},
		{name: "precompressed", method: "GET", path: "/app.js", headers: http.Header{"Accept-Encoding": {"br,gzip;q=0.5"}}, status: 200, body: "brotli-fixture", coding: "br", etag: `"brotli-v1"`, cacheControl: "max-age=300", calls: []string{"GET pub/app.js", "GET pub/app.js.br"}, outcome: content.OutcomeSuccess, resolution: "ordinary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := http.NewRequest(tc.method, public+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header = tc.headers.Clone()
			if request.Header == nil {
				request.Header = make(http.Header)
			}
			request.Header.Set("Cookie", "visitor-secret=not-forwarded")
			request.Host = "visitor.example"
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if response.StatusCode != tc.status || string(body) != tc.body || response.Header.Get("Content-Encoding") != tc.coding || response.Header.Get("ETag") != tc.etag || response.Header.Get("Cache-Control") != tc.cacheControl || response.Header.Get("Content-Range") != tc.contentRange {
				t.Fatalf("status%d headers%v body%q", response.StatusCode, response.Header, body)
			}
			if response.Header.Get("X-Amz-Meta-Private") != "" || strings.Contains(string(body), "private-origin") {
				t.Fatal("origin metadata/error escaped")
			}
			calls := origin.takeCalls()
			if strings.Join(calls, "|") != strings.Join(tc.calls, "|") {
				t.Fatalf("origin calls %q want %q", calls, tc.calls)
			}
			observation := sdkObservation(t, observations)
			if observation.Outcome != tc.outcome || string(observation.Resolution) != tc.resolution {
				t.Fatalf("observation = %+v", observation)
			}
			if tc.name == "precompressed" && (response.Header.Get("Content-Type") != "application/javascript" || !strings.Contains(response.Header.Get("Vary"), "Accept-Encoding")) {
				t.Fatalf("precompressed headers %v", response.Header)
			}
			if tc.resolution == "spa" && !strings.Contains(response.Header.Get("Vary"), "Accept") {
				t.Fatalf("SPA headers %v", response.Header)
			}
		})
	}
}

func TestRealSDKPipelineTimeoutPhases(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			canceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(canceled)
				if phase == "body" {
					w.Header().Set("Content-Length", "6")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			defer upstream.Close()
			cfg := sdkPipelineConfig(t, upstream.URL)
			cfg.Server.ResponseHeaderTimeout = 100 * time.Millisecond
			cfg.S3.ResponseHeaderTimeout = time.Second
			cfg.S3.ReadIdleTimeout = 30 * time.Millisecond
			public, observations := sdkPipeline(t, cfg)
			transport := &http.Transport{DisableCompression: true}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			response, err := client.Get(public + "/slow.txt")
			if err != nil {
				t.Fatal(err)
			}
			payload, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			observation := sdkObservation(t, observations)
			if phase == "headers" {
				if response.StatusCode != 504 || readErr != nil || string(payload) != "Gateway Timeout\n" || observation.TerminationReason != content.ReasonPreparationTimeout {
					t.Fatalf("status%d body%q read%v observation%+v", response.StatusCode, payload, readErr, observation)
				}
			} else {
				if response.StatusCode != 200 || readErr == nil || len(payload) != 0 || observation.Outcome != content.OutcomeIncomplete || observation.TerminationReason != content.ReasonOriginReadTimeout {
					t.Fatalf("status%d body%q read%v observation%+v", response.StatusCode, payload, readErr, observation)
				}
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("origin request was not canceled")
			}
		})
	}
}

type sdkClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *sdkClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *sdkClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestRealSDKCachedPipeline(t *testing.T) {
	// The fixture stamps Date from the cache's fake clock, while the response
	// layer computes apparent age against the real clock. Starting the fake
	// clock an hour ahead makes that real-clock apparent age negative, hence
	// ignored, however long the test takes: Age comes only from the cache's
	// current age on the fake clock.
	clock := &sdkClock{t: time.Now().Add(time.Hour)}
	origin := &sdkFixtureOrigin{conditional: true, now: clock.Now, objects: map[string]sdkFixtureObject{
		"pub/plain.txt": {body: "hello", contentType: "text/plain; charset=utf-8", etag: `"plain-v1"`, cacheControl: "public, max-age=60"},
	}}
	upstream := httptest.NewServer(origin)
	defer upstream.Close()
	cfg := sdkPipelineConfig(t, upstream.URL)
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	public, observations := sdkPipelineWith(t, cfg, func(reader storage.Reader) []content.HandlerOption {
		c, err := cache.FromConfig(lifetime, cfg, reader, func(o *cache.Options) { o.Now = clock.Now })
		if err != nil || c == nil {
			t.Fatalf("cache: %v", err)
		}
		return []content.HandlerOption{content.WithCache(c)}
	})
	transport := &http.Transport{DisableCompression: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	for _, tc := range []struct {
		name         string
		advance      time.Duration
		headers      http.Header
		status       int
		body, age    string
		calls        []string
		outcome      cache.Outcome
		memoryBytes  int64
		cacheControl string
	}{
		{name: "cold", status: 200, body: "hello", calls: []string{"GET pub/plain.txt"}, outcome: cache.OutcomeMiss},
		{name: "warm", advance: 20 * time.Second, status: 200, body: "hello", age: "20", outcome: cache.OutcomeHit, memoryBytes: 5},
		{name: "local validator", status: 304, headers: http.Header{"If-None-Match": {`"plain-v1"`}}, age: "20", outcome: cache.OutcomeHit},
		{name: "expired revalidates", advance: 41 * time.Second, status: 200, body: "hello", calls: []string{"GET pub/plain.txt if-none-match"}, outcome: cache.OutcomeRevalidated, memoryBytes: 5},
		{name: "refreshed", advance: 10 * time.Second, status: 200, body: "hello", age: "10", outcome: cache.OutcomeHit, memoryBytes: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock.Advance(tc.advance)
			request, err := http.NewRequest("GET", public+"/plain.txt", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header = tc.headers.Clone()
			if request.Header == nil {
				request.Header = make(http.Header)
			}
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			// Consume this request's origin calls and observation before any
			// assertion, so that a failure cannot shift them onto later steps.
			calls := origin.takeCalls()
			observation := sdkObservation(t, observations)
			if readErr != nil {
				t.Error(readErr)
			}
			if response.StatusCode != tc.status || string(body) != tc.body || response.Header.Get("ETag") != `"plain-v1"` || response.Header.Get("Cache-Control") != "public, max-age=60" || response.Header.Get("Age") != tc.age {
				t.Errorf("status%d headers%v body%q", response.StatusCode, response.Header, body)
			}
			if strings.Join(calls, "|") != strings.Join(tc.calls, "|") {
				t.Errorf("origin calls %q want %q", calls, tc.calls)
			}
			if observation.CacheOutcome != tc.outcome || observation.CacheBytes != tc.memoryBytes || observation.OriginKey != "pub/plain.txt" {
				t.Errorf("observation = %+v", observation)
			}
		})
	}
}
