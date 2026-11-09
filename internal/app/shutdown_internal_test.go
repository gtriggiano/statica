package app

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
	origins3 "github.com/gtriggiano/statica/internal/storage/s3"
)

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

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDelayedShutdownExpiresTheBudgetInOrder delays the shutdown goroutine
// past the whole budget, so the drain expiry, the overall deadline and the
// public server's own deadline have all passed when it waits for the drain,
// whichever the select then picks. The outcome must still be a budget
// expiry, and the cache lifetime must end only after the public server has
// cancelled its request contexts.
func TestDelayedShutdownExpiresTheBudgetInOrder(t *testing.T) {
	requested := make(chan struct{}, 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		w.(http.Flusher).Flush()
		select {
		case requested <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer origin.Close()
	cfg, err := config.Parse([]byte("server:\n  listen: 127.0.0.1:1\n  shutdown_timeout: 400ms\nadmin:\n  listen: 127.0.0.1:2\ns3:\n  bucket: delay-bucket\n  region: eu-west-1\n  endpoint: "+origin.URL+"\n  path_style: true\n  allow_http: true\n  max_attempts: 1\n"),
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	signals := make(chan os.Signal, 1)
	listeners := make(chan net.Listener, 2)
	opts := Options{Logs: &logs, Signals: signals,
		OriginOptions: []origins3.Option{origins3.WithCredentials(credentials.NewStaticCredentialsProvider("delay-key", "delay-secret", ""))},
		Listen: func(network, _ string) (net.Listener, error) {
			l, err := net.Listen(network, "127.0.0.1:0")
			if err == nil {
				listeners <- l
			}
			return l, err
		}}
	ctx := context.Background()
	watch := watchStartup(ctx, signals)
	defer watch.finish()
	a, err := build(ctx, cfg, opts, watch)
	if err != nil {
		t.Fatal(err)
	}
	var ordered, ended atomic.Bool
	cancelLifetime := a.cancelLifetime
	a.cancelLifetime = func() {
		select {
		case <-a.public.Done():
			ordered.Store(true)
		default:
		}
		ended.Store(true)
		cancelLifetime()
	}
	a.beforeDrainWait = func() { time.Sleep(500 * time.Millisecond) }
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()
	public := <-listeners
	<-listeners
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	go func() {
		// The request waits on a shared cache fill the origin never ends.
		if response, err := client.Get("http://" + public.Addr().String() + "/slow.txt"); err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
	}()
	select {
	case <-requested:
	case <-time.After(10 * time.Second):
		t.Fatal("the origin was not requested")
	}
	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a signal shutdown must succeed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	eventually(t, "the end of the cache lifetime", ended.Load)
	if !ordered.Load() {
		t.Fatal("the cache lifetime ended before the public server cancelled its request contexts")
	}
	// The flush had no budget left, but the event was queued before it.
	var complete string
	eventually(t, "shutdown_complete", func() bool {
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, " event=shutdown_complete ") {
				complete = line
				return true
			}
		}
		return false
	})
	for _, field := range []string{"level=warn", "reason=signal", "outcome=forced", "forced_by=budget", "cancelled_requests="} {
		if !strings.Contains(complete, field) {
			t.Fatalf("shutdown_complete lacks %s: %s", field, complete)
		}
	}
}

// TestConstructionFailureOutranksPendingSignal fails a real construction
// stage while a signal is queued. The watch goroutine is not started, so the
// signal cannot cancel construction first; finish still takes it.
func TestConstructionFailureOutranksPendingSignal(t *testing.T) {
	cfg, err := config.Parse([]byte("server:\n  listen: 127.0.0.1:1\nadmin:\n  listen: 127.0.0.1:2\ns3:\n  bucket: failure-bucket\n  region: eu-west-1\n  endpoint: http://127.0.0.1:1\n  allow_http: true\n"),
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	// TLS material is read when the public server is prepared.
	cfg.Server.TLS = &config.TLS{CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem", MinVersion: "1.2"}
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	close(done)
	watch := &startupWatch{ctx: ctx, cancel: cancel, signals: signals, stop: make(chan struct{}), done: done}
	var logs lockedBuffer
	opts := Options{Logs: &logs, Signals: signals,
		OriginOptions: []origins3.Option{origins3.WithCredentials(credentials.NewStaticCredentialsProvider("failure-key", "failure-secret", ""))}}
	if _, err := build(context.Background(), cfg, opts, watch); err == nil || err.Error() != "cannot initialize the public server" {
		t.Fatalf("build = %v", err)
	}
	if !watch.signalled.Load() {
		t.Fatal("the queued signal was not taken")
	}
	out := logs.String()
	if !strings.Contains(out, " event=startup_failed stage=public_server signal_pending=true\n") || strings.Contains(out, "startup_aborted") {
		t.Fatalf("events:\n%s", out)
	}
}

// TestOriginErrorAfterStartupSignalIsACleanAbort feeds abortOrigin the
// configuration error origins3.New returns when a signal cancels its context
// during configuration loading, which no origin option can inject from
// outside the s3 package.
func TestOriginErrorAfterStartupSignalIsACleanAbort(t *testing.T) {
	cfg, err := config.Parse([]byte("server:\n  listen: 127.0.0.1:1\nadmin:\n  listen: 127.0.0.1:2\ns3:\n  bucket: abort-bucket\n  region: eu-west-1\n  endpoint: http://127.0.0.1:1\n  allow_http: true\n"),
		func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	configuration := &storage.Error{Kind: storage.Configuration, Size: -1}
	for _, tc := range []struct {
		name, want, event string
		signal, timeout   bool
	}{
		{name: "signal", signal: true, event: " event=startup_aborted reason=signal\n"},
		{name: "timeout", timeout: true, want: "origin client initialization timed out", event: " event=startup_failed stage=origin_client\n"},
		{name: "configuration", want: "cannot initialize the origin client", event: " event=startup_failed stage=origin_client\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signals := make(chan os.Signal, 1)
			var logs lockedBuffer
			watch := watchStartup(context.Background(), signals)
			a, err := build(context.Background(), cfg, Options{Logs: &logs, Signals: signals,
				OriginOptions: []origins3.Option{origins3.WithCredentials(credentials.NewStaticCredentialsProvider("abort-key", "abort-secret", ""))}}, watch)
			if err != nil {
				t.Fatal(err)
			}
			startup, cancel := context.WithTimeout(watch.ctx, time.Hour)
			defer cancel()
			switch {
			case tc.signal:
				signals <- syscall.SIGTERM
				<-startup.Done() // the signal watch cancels construction
			case tc.timeout:
				startup, cancel = context.WithTimeout(context.Background(), 0)
				defer cancel()
			}
			err = a.abortOrigin("origin_client", startup, configuration)
			if tc.signal && err != errAborted || !tc.signal && (err == nil || err.Error() != tc.want) {
				t.Fatalf("abortOrigin = %v", err)
			}
			if out := logs.String(); !strings.Contains(out, tc.event) {
				t.Fatalf("events:\n%s", out)
			}
		})
	}
}
