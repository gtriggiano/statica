package app_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/go-logfmt/logfmt"
	"github.com/gtriggiano/statica/internal/app"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/metrics"
	origins3 "github.com/gtriggiano/statica/internal/storage/s3"
)

// Canary values that must never appear in logs.
const (
	bucket       = "canary-bucket-x9"
	accessKey    = "CANARYAKIDX9"
	secretKey    = "canary-secret-key-x9"
	sessionToken = "canary-session-token-x9"
)

// The configuration needs valid ports; the test binder replaces these
// placeholders with ephemeral ports and passes any other address through.
const (
	publicPlaceholder = "127.0.0.1:1"
	adminPlaceholder  = "127.0.0.1:2"
)

const waitTimeout = 10 * time.Second

var (
	slowText = strings.Repeat("s", 4<<10)  // cached: a fill holds the request until released
	slowBody = strings.Repeat("b", 64<<10) // streamed when the cache is disabled
	bigBody  = strings.Repeat("x", 32<<20) // larger than any socket buffers
)

// origin is an S3 protocol fixture for path-style requests signed with the
// canary static credentials. Slow objects send half their body, report
// their start and then wait for finish or the request's end.
type origin struct {
	server  *httptest.Server
	objects map[string]string
	healthy atomic.Bool
	started chan string
	release chan struct{}
	once    sync.Once

	mu    sync.Mutex
	heads map[string]int
}

func newOrigin(t *testing.T) *origin {
	t.Helper()
	o := &origin{
		objects: map[string]string{
			"pub/hello.txt":   "hello world",
			"pub/health.json": "{}",
			"pub/slow.txt":    slowText,
			"pub/slow.bin":    slowBody,
			"pub/big.bin":     bigBody,
		},
		started: make(chan string, 16), release: make(chan struct{}), heads: map[string]int{},
	}
	o.server = httptest.NewServer(o)
	t.Cleanup(func() {
		o.finish()
		o.server.Close()
	})
	return o
}

// finish lets every slow body complete.
func (o *origin) finish() { o.once.Do(func() { close(o.release) }) }

func (o *origin) headCount(key string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.heads[key]
}

func (o *origin) awaitSlow(t *testing.T) {
	t.Helper()
	select {
	case <-o.started:
	case <-time.After(waitTimeout):
		t.Fatal("the slow origin object was not requested")
	}
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, "/"+bucket+"/")
	if !ok || !strings.Contains(r.Header.Get("Authorization"), "Credential="+accessKey+"/") || r.Header.Get("X-Amz-Security-Token") != sessionToken {
		s3Error(w, http.StatusForbidden, "AccessDenied")
		return
	}
	if r.Method == http.MethodHead {
		o.mu.Lock()
		o.heads[key]++
		o.mu.Unlock()
	}
	if key == "pub/broken.txt" || key == "pub/health.json" && !o.healthy.Load() {
		s3Error(w, http.StatusInternalServerError, "InternalError")
		return
	}
	body, found := o.objects[key]
	if !found {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("ETag", `"v1"`)
	h.Set("Last-Modified", "Fri, 06 Nov 2026 12:00:00 GMT")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if key == "pub/slow.txt" || key == "pub/slow.bin" {
		half := len(body) / 2
		_, _ = io.WriteString(w, body[:half])
		w.(http.Flusher).Flush()
		o.started <- key
		select {
		case <-o.release:
		case <-r.Context().Done():
			return
		}
		body = body[half:]
	}
	_, _ = io.WriteString(w, body)
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>private-origin-message</Message></Error>", code)
}

// settings are YAML lines appended to each block, each indented by two
// spaces; s3 replaces the default "max_attempts: 1".
type settings struct{ server, admin, s3, top string }

func newConfig(t *testing.T, endpoint string, s settings) *config.Config {
	t.Helper()
	if s.s3 == "" {
		s.s3 = "  max_attempts: 1\n"
	}
	text := fmt.Sprintf("server:\n  listen: %s\n%sadmin:\n  listen: %s\n%ss3:\n  bucket: %s\n  region: eu-west-1\n  prefix: pub/\n  endpoint: %s\n  path_style: true\n  allow_http: true\n%s%s",
		publicPlaceholder, s.server, adminPlaceholder, s.admin, bucket, endpoint, s.s3, s.top)
	cfg, err := config.Parse([]byte(text), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// binder is the injected Listen: it records each listener by its configured
// address.
type binder struct {
	mu    sync.Mutex
	bound map[string]net.Listener
	calls []string
}

func (b *binder) listen(network, address string) (net.Listener, error) {
	b.mu.Lock()
	b.calls = append(b.calls, address)
	b.mu.Unlock()
	target := address
	if address == publicPlaceholder || address == adminPlaceholder {
		target = "127.0.0.1:0"
	}
	l, err := net.Listen(network, target)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.bound[address] = l
	b.mu.Unlock()
	return l, nil
}

func (b *binder) get(address string) net.Listener {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bound[address]
}

type instance struct {
	t             *testing.T
	logs          *syncBuffer
	signals       chan os.Signal
	ctx           context.Context
	cancel        context.CancelFunc
	origin        []origins3.Option                                   // extra origin client options
	listen        func(network, address string) (net.Listener, error) // replaces the binder
	binder        *binder
	finished      chan struct{}
	err           error
	public, admin string // bound host:port
	// adminClient keeps its connections alive, so frequent probes do not
	// churn through loopback ports.
	adminClient *http.Client
}

func staticCredentials() origins3.Option {
	return origins3.WithCredentials(credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken))
}

// newInstance prepares an application run; a test may queue signals,
// cancel the context or add origin options before run.
func newInstance(t *testing.T) *instance {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	return &instance{t: t, logs: &syncBuffer{}, signals: make(chan os.Signal, 2), ctx: ctx, cancel: cancel,
		binder: &binder{bound: map[string]net.Listener{}}, finished: make(chan struct{}),
		adminClient: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}}
}

// run starts the application without waiting for its startup.
func (i *instance) run(cfg *config.Config) *instance {
	i.t.Helper()
	opts := app.Options{
		Logs:          i.logs,
		Build:         metrics.BuildInfo{Version: "v-test", Revision: "rev-test", GoVersion: "go-test"},
		Signals:       i.signals,
		OriginOptions: append([]origins3.Option{staticCredentials()}, i.origin...),
		Listen:        i.binder.listen,
	}
	if i.listen != nil {
		opts.Listen = i.listen
	}
	go func() {
		defer close(i.finished)
		i.err = app.Run(i.ctx, cfg, opts)
	}()
	i.t.Cleanup(func() {
		i.cancel()
		select {
		case <-i.finished:
		case <-time.After(waitTimeout):
			i.t.Error("Run did not return after its context was cancelled")
		}
		i.adminClient.CloseIdleConnections()
	})
	return i
}

// launch runs the application without waiting for its startup.
func launch(t *testing.T, cfg *config.Config) *instance {
	t.Helper()
	return newInstance(t).run(cfg)
}

// start launches the application and waits for its startup event.
func start(t *testing.T, cfg *config.Config) *instance {
	t.Helper()
	return newInstance(t).start(cfg)
}

func (i *instance) start(cfg *config.Config) *instance {
	i.t.Helper()
	i.run(cfg)
	i.waitEvent("startup", nil)
	i.public = i.binder.get(publicPlaceholder).Addr().String()
	i.admin = i.binder.get(adminPlaceholder).Addr().String()
	return i
}

func (i *instance) wait(timeout time.Duration) error {
	i.t.Helper()
	select {
	case <-i.finished:
		return i.err
	case <-time.After(timeout):
		i.t.Fatal("Run did not return")
		return nil
	}
}

func (i *instance) running() bool {
	select {
	case <-i.finished:
		return false
	default:
		return true
	}
}

type event map[string]string

func parseEvents(t *testing.T, text string) []event {
	t.Helper()
	var events []event
	d := logfmt.NewDecoder(strings.NewReader(text))
	for d.ScanRecord() {
		e := event{}
		for d.ScanKeyval() {
			if _, dup := e[string(d.Key())]; dup {
				t.Fatalf("duplicate key %q in:\n%s", d.Key(), text)
			}
			e[string(d.Key())] = string(d.Value())
		}
		events = append(events, e)
	}
	if err := d.Err(); err != nil {
		t.Fatalf("invalid logfmt: %v\n%s", err, text)
	}
	return events
}

// find returns the first event named name that match accepts.
func (i *instance) find(name string, match func(event) bool) (event, bool) {
	for _, e := range parseEvents(i.t, i.logs.String()) {
		if e["event"] == name && (match == nil || match(e)) {
			return e, true
		}
	}
	return nil, false
}

func (i *instance) event(name string, match func(event) bool) event {
	i.t.Helper()
	e, ok := i.find(name, match)
	if !ok {
		i.t.Fatalf("missing %s event:\n%s", name, i.logs.String())
	}
	return e
}

func (i *instance) waitEvent(name string, match func(event) bool) event {
	i.t.Helper()
	var e event
	waitFor(i.t, name+" event", func() bool {
		var ok bool
		e, ok = i.find(name, match)
		return ok
	})
	return e
}

func path(p string) func(event) bool { return func(e event) bool { return e["path"] == p } }

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func want(t *testing.T, e event, fields map[string]string) {
	t.Helper()
	for key, value := range fields {
		if e[key] != value {
			t.Errorf("%s: %s=%q, want %q (event %v)", e["event"], key, e[key], value, e)
		}
	}
}

// client never reuses connections, so a draining or capacity-limited public
// server sees only the connections a test opens deliberately.
var client = &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

func get(t *testing.T, addr, p string) (int, string) {
	t.Helper()
	return fetch(t, client, "http://"+addr+p)
}

func wantGet(t *testing.T, addr, p string, status int, body string) {
	t.Helper()
	wantResponse(t, p, status, body, func() (int, string) { return get(t, addr, p) })
}

// probe requests an admin endpoint.
func (i *instance) probe(p string) (int, string) {
	i.t.Helper()
	return fetch(i.t, i.adminClient, "http://"+i.admin+p)
}

func (i *instance) wantProbe(p string, status int, body string) {
	i.t.Helper()
	wantResponse(i.t, p, status, body, func() (int, string) { return i.probe(p) })
}

func (i *instance) metrics() string {
	i.t.Helper()
	_, exposition := i.probe("/metrics")
	return exposition
}

func fetch(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()
	response, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("GET %s body: %v", url, err)
	}
	return response.StatusCode, string(body)
}

// wantResponse checks the status and, unless empty, the body.
func wantResponse(t *testing.T, p string, status int, body string, do func() (int, string)) {
	t.Helper()
	gotStatus, gotBody := do()
	if gotStatus != status || body != "" && gotBody != body {
		t.Fatalf("GET %s = %d %q, want %d %q", p, gotStatus, gotBody, status, body)
	}
}

// hasMetric reports whether a /metrics line starts with prefix.
func hasMetric(exposition, prefix string) bool {
	for _, line := range strings.Split(exposition, "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func wantMetrics(t *testing.T, exposition string, prefixes ...string) {
	t.Helper()
	for _, prefix := range prefixes {
		if !hasMetric(exposition, prefix) {
			t.Errorf("metrics lack %q", prefix)
		}
	}
}

// download starts a GET and returns once its first half arrived, leaving
// the rest to read from the returned body.
func download(t *testing.T, o *origin, addr, p string, size int) io.ReadCloser {
	t.Helper()
	response, err := client.Get("http://" + addr + p)
	if err != nil {
		t.Fatalf("GET %s: %v", p, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", p, response.StatusCode)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, size/2)); err != nil {
		t.Fatalf("GET %s first half: %v", p, err)
	}
	o.awaitSlow(t)
	return response.Body
}
