package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
)

func serverConfig(t *testing.T) config.Server {
	t.Helper()
	cfg, err := config.Parse([]byte("s3:\n  bucket: test-bucket\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Server
}

type serverFixture struct {
	server *Server
	addr   string
	done   chan error
}

func startServer(t *testing.T, cfg config.Server, handler http.Handler) *serverFixture {
	t.Helper()
	s, err := NewServer(cfg, handler)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &serverFixture{s, l.Addr().String(), make(chan error, 1)}
	go func() { f.done <- s.Serve(l) }()
	t.Cleanup(func() {
		_ = s.Close()
		select {
		case err := <-f.done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve did not stop")
		}
	})
	return f
}

func dialServer(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func writeRequest(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
}

func readResponse(t *testing.T, r *bufio.Reader) *http.Response {
	t.Helper()
	response, err := http.ReadResponse(r, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func testHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", "2")
	_, _ = io.WriteString(w, "ok")
}

func certificateFixture(t *testing.T) (*config.TLS, *x509.CertPool) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := &config.TLS{CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem"), MinVersion: "1.2"}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(cfg.CertFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return cfg, pool
}

func TestServerTLSNegotiatesHTTP1AndHTTP2(t *testing.T) {
	cfg := serverConfig(t)
	var roots *x509.CertPool
	cfg.TLS, roots = certificateFixture(t)
	f := startServer(t, cfg, http.HandlerFunc(testHandler))
	for _, major := range []int{1, 2} {
		t.Run(fmt.Sprintf("HTTP%d", major), func(t *testing.T) {
			protocols := new(http.Protocols)
			protocols.SetHTTP1(true)
			protocols.SetHTTP2(major == 2)
			nextProtos := []string{"http/1.1"}
			if major == 2 {
				nextProtos = []string{"h2", "http/1.1"}
			}
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, NextProtos: nextProtos}, Protocols: protocols}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
			response, err := client.Get("https://" + f.addr + "/")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || string(body) != "ok" || response.ProtoMajor != major {
				t.Fatalf("protocol=%s body=%q err=%v", response.Proto, body, err)
			}
			wantALPN := "http/1.1"
			if major == 2 {
				wantALPN = "h2"
			}
			if response.TLS.NegotiatedProtocol != wantALPN {
				t.Fatalf("ALPN=%q, want %q", response.TLS.NegotiatedProtocol, wantALPN)
			}
		})
	}
}

func TestServerTLSMinimumVersion(t *testing.T) {
	for _, version := range []string{"1.2", "1.3"} {
		t.Run(version, func(t *testing.T) {
			cfg := serverConfig(t)
			var roots *x509.CertPool
			cfg.TLS, roots = certificateFixture(t)
			cfg.TLS.MinVersion = version
			f := startServer(t, cfg, http.HandlerFunc(testHandler))
			c, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", f.addr, &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
			if version == "1.3" {
				if err == nil {
					_ = c.Close()
					t.Fatal("TLS 1.2 accepted below configured minimum")
				}
			} else if err != nil {
				t.Fatal(err)
			} else {
				_ = c.Close()
			}
		})
	}
}

func TestServerRejectsBadTLSBeforeServing(t *testing.T) {
	for _, bad := range []string{"missing", "mismatch", "invalid-chain", "minimum"} {
		t.Run(bad, func(t *testing.T) {
			cfg := serverConfig(t)
			cfg.TLS, _ = certificateFixture(t)
			switch bad {
			case "missing":
				cfg.TLS.CertFile += "-missing"
			case "mismatch":
				other, _ := certificateFixture(t)
				cfg.TLS.KeyFile = other.KeyFile
			case "invalid-chain":
				f, err := os.OpenFile(cfg.TLS.CertFile, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("invalid DER")}))
				_ = f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "minimum":
				cfg.TLS.MinVersion = "1.1"
			}
			if s, err := NewServer(cfg, http.HandlerFunc(testHandler)); err == nil {
				_ = s.Close()
				t.Fatal("bad TLS accepted")
			} else if strings.Contains(err.Error(), cfg.TLS.CertFile) || strings.Contains(err.Error(), cfg.TLS.KeyFile) {
				t.Fatal("TLS error exposed file paths")
			}
		})
	}
}

func TestServerTLSHasNoCleartextFallback(t *testing.T) {
	cfg := serverConfig(t)
	cfg.TLS, _ = certificateFixture(t)
	var calls atomic.Int32
	f := startServer(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); testHandler(w, r) }))
	c := dialServer(t, f.addr)
	writeRequest(t, c)
	response := readResponse(t, bufio.NewReader(c))
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("cleartext status=%d handler calls=%d", response.StatusCode, calls.Load())
	}
}

func TestServerCleartextDoesNotEnableH2C(t *testing.T) {
	f := startServer(t, serverConfig(t), http.HandlerFunc(testHandler))
	c := dialServer(t, f.addr)
	_, _ = io.WriteString(c, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")
	response := readResponse(t, bufio.NewReader(c))
	defer response.Body.Close()
	// net/http can parse the HTTP/2 preface as an HTTP/1 request with method
	// PRI. Application method/target admission rejects that request; this
	// primitive must never upgrade it to an HTTP/2 connection.
	if response.ProtoMajor != 1 {
		t.Fatalf("h2c response=%s %d", response.Proto, response.StatusCode)
	}
}

func TestServerHeaderAndIdleLimits(t *testing.T) {
	t.Run("slow-headers", func(t *testing.T) {
		cfg := serverConfig(t)
		cfg.ReadHeaderTimeout = 150 * time.Millisecond
		f := startServer(t, cfg, http.HandlerFunc(testHandler))
		c := dialServer(t, f.addr)
		_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost:")
		var b [1]byte
		if _, err := c.Read(b[:]); err == nil {
			t.Fatal("incomplete headers were not closed")
		} else if e, ok := err.(net.Error); ok && e.Timeout() {
			t.Fatal("client deadline expired before server header deadline")
		}
	})
	t.Run("oversized-headers", func(t *testing.T) {
		cfg := serverConfig(t)
		cfg.MaxHeaderBytes = 1024
		f := startServer(t, cfg, http.HandlerFunc(testHandler))
		c := dialServer(t, f.addr)
		_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\nX-Large: "+strings.Repeat("a", 8192)+"\r\n\r\n")
		response := readResponse(t, bufio.NewReader(c))
		defer response.Body.Close()
		if response.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("oversized header status=%d", response.StatusCode)
		}
	})
	t.Run("idle-keepalive", func(t *testing.T) {
		cfg := serverConfig(t)
		cfg.IdleTimeout = 150 * time.Millisecond
		f := startServer(t, cfg, http.HandlerFunc(testHandler))
		c := dialServer(t, f.addr)
		writeRequest(t, c)
		r := bufio.NewReader(c)
		response := readResponse(t, r)
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if _, err := r.ReadByte(); !errors.Is(err, io.EOF) {
			t.Fatalf("idle connection was not closed: %v", err)
		}
	})
}

func TestServerTLSHandshakeIsBoundedAndReleasesCapacity(t *testing.T) {
	cfg := serverConfig(t)
	cfg.TLS, _ = certificateFixture(t)
	cfg.ReadHeaderTimeout = 150 * time.Millisecond
	cfg.MaxConnections = 1
	f := startServer(t, cfg, http.HandlerFunc(testHandler))
	c := dialServer(t, f.addr)
	var b [1]byte
	if _, err := c.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("stalled TLS handshake was not closed: %v", err)
	}
	// A subsequent connection must be admitted after handshake failure.
	next := dialServer(t, f.addr)
	if _, err := next.Read(b[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("handshake capacity was not released: %v", err)
	}
}

func TestServerShutdownDrainsThenForcesCancellation(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", forced), func(t *testing.T) {
			cfg := serverConfig(t)
			cfg.ShutdownTimeout = time.Second
			if forced {
				cfg.ShutdownTimeout = 150 * time.Millisecond
			}
			started := make(chan struct{})
			release := make(chan struct{})
			cancelled := make(chan struct{})
			f := startServer(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-release:
					testHandler(w, r)
				case <-r.Context().Done():
					close(cancelled)
				}
			}))
			c := dialServer(t, f.addr)
			writeRequest(t, c)
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("handler not started")
			}
			done := make(chan error, 1)
			go func() { done <- f.server.Shutdown(context.Background()) }()
			if !forced {
				close(release)
				response := readResponse(t, bufio.NewReader(c))
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("draining response status=%d", response.StatusCode)
				}
			}
			select {
			case err := <-done:
				if forced && !errors.Is(err, context.DeadlineExceeded) || !forced && err != nil {
					t.Fatalf("Shutdown error=%v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Shutdown did not finish")
			}
			if forced {
				select {
				case <-cancelled:
				case <-time.After(time.Second):
					t.Fatal("request context not cancelled")
				}
			}
		})
	}
}

type failingListener struct {
	net.Listener
	failed    chan struct{}
	closeOnce sync.Once
	passFirst bool
	accepted  atomic.Bool
}

func (l *failingListener) Accept() (net.Conn, error) {
	if l.passFirst && l.accepted.CompareAndSwap(false, true) {
		return l.Listener.Accept()
	}
	<-l.failed
	return nil, errors.New("listener failed")
}

func (l *failingListener) Close() error {
	l.closeOnce.Do(func() { close(l.failed) })
	return l.Listener.Close()
}

func TestServerFailedListenerClosesImmediately(t *testing.T) {
	s, err := NewServer(serverConfig(t), http.HandlerFunc(testHandler))
	if err != nil {
		t.Fatal(err)
	}
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &failingListener{Listener: inner, failed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	l.closeOnce.Do(func() { close(l.failed) })
	select {
	case err := <-done:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("listener failure error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener failure did not stop Serve")
	}
	if _, err := net.DialTimeout("tcp", inner.Addr().String(), time.Second); err == nil {
		t.Fatal("failed listener stayed open")
	}
}

func TestServerDoesNotApplyAbsoluteDownloadTimeouts(t *testing.T) {
	s, err := NewServer(serverConfig(t), http.HandlerFunc(testHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.http.ReadTimeout != 0 || s.http.WriteTimeout != 0 {
		t.Fatal("absolute connection deadlines would truncate active downloads")
	}
	if s.http.ErrorLog.Writer() != io.Discard {
		t.Fatal("uncontrolled net/http errors must not reach global logs")
	}
}

func TestServerListenerFailureCancelsActiveRequests(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	s, err := NewServer(serverConfig(t), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &failingListener{Listener: inner, failed: make(chan struct{}), passFirst: true}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	c := dialServer(t, inner.Addr().String())
	writeRequest(t, c)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not started")
	}
	l.closeOnce.Do(func() { close(l.failed) })
	select {
	case err := <-done:
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("listener failure error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not return after listener failure")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("listener failure orphaned an active request")
	}
}

func TestServerShutdownCancelsHTTP2Streams(t *testing.T) {
	cfg := serverConfig(t)
	var roots *x509.CertPool
	cfg.TLS, roots = certificateFixture(t)
	started := make(chan int, 2)
	cancelled := make(chan struct{}, 2)
	f := startServer(t, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.ProtoMajor
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	finished := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			response, err := client.Get("https://" + f.addr + "/")
			if err == nil {
				_ = response.Body.Close()
			}
			finished <- struct{}{}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case major := <-started:
			if major != 2 {
				t.Fatalf("expected HTTP/2, got HTTP/%d", major)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("HTTP/2 stream not started")
		}
	}
	// The caller's earlier deadline must cap the configured 30-second drain.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := f.server.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("HTTP/2 request context not cancelled")
		}
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatal("HTTP/2 client did not finish after forced drain")
		}
	}
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

func TestServerErrorLoggerReportsOnlyHandlerPanics(t *testing.T) {
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	panicking := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret panic value") })
	s, err := NewServer(serverConfig(t), panicking, WithErrorLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	defer func() {
		_ = s.Close()
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve: %v", err)
		}
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	if response, err := client.Get("http://" + l.Addr().String() + "/secret-path?token=secret"); err == nil {
		_ = response.Body.Close()
		t.Fatal("a panicking handler produced a response")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(logs.String(), "msg=http_server_error") {
		if time.Now().After(deadline) {
			t.Fatalf("missing panic event:\n%s", logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	out := logs.String()
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "level=WARN msg=http_server_error error_class=handler_panic\n") {
		t.Fatalf("unexpected events:\n%s", out)
	}
	for _, leak := range []string{"secret", "127.0.0.1", "goroutine", "panic serving"} {
		if strings.Contains(out, leak) {
			t.Fatalf("event copied %q from net/http:\n%s", leak, out)
		}
	}
	adapter := httpErrorLog{logger}
	for _, line := range []string{
		"http: TLS handshake error from 127.0.0.1:1234: secret client hello\n",
		"http: Accept error: secret; retrying in 5ms\n",
		"http: superfluous response.WriteHeader call from 127.0.0.1:1234\n",
		"http2: server: error reading preface from client 127.0.0.1:1234: secret\n",
	} {
		if n, err := adapter.Write([]byte(line)); n != len(line) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", line, n, err)
		}
	}
	if logs.String() != out {
		t.Fatalf("other net/http lines were not discarded:\n%s", logs.String())
	}
	_, _ = adapter.Write([]byte("http2: panic serving 127.0.0.1:1234: secret\ngoroutine 1 [running]:\n"))
	if got := logs.String(); strings.Count(got, "msg=http_server_error error_class=handler_panic") != 2 || strings.Contains(got, "secret") {
		t.Fatalf("HTTP/2 panic event:\n%s", got)
	}
	for _, option := range []ServerOption{nil, WithErrorLogger(nil)} {
		if _, err := NewServer(serverConfig(t), panicking, option); err != nil {
			t.Fatalf("a nil option or logger must discard: %v", err)
		}
	}
}

func TestServerDoneFollowsRequestCancellation(t *testing.T) {
	closed := func(s *Server) bool {
		select {
		case <-s.Done():
			return true
		default:
			return false
		}
	}
	type observation struct {
		cause      error
		doneClosed bool
	}
	started, observed := make(chan struct{}, 1), make(chan observation, 1)
	var s *Server
	f := startServer(t, serverConfig(t), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		observed <- observation{context.Cause(r.Context()), closed(s)}
	}))
	s = f.server
	go func() {
		c := dialServer(t, f.addr)
		_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n")
		_, _ = io.ReadAll(c)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("request not started")
	}
	if closed(s) {
		t.Fatal("Done closed while serving")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v", err)
	}
	if !closed(s) {
		t.Fatal("Done open after an expired drain")
	}
	select {
	case o := <-observed:
		if !errors.Is(o.cause, ErrShutdown) || !o.doneClosed {
			t.Fatalf("request saw cause %v with Done closed=%v", o.cause, o.doneClosed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request context not cancelled")
	}

	drained := startServer(t, serverConfig(t), http.NotFoundHandler()).server
	if err := drained.Shutdown(context.Background()); err != nil || !closed(drained) {
		t.Fatalf("drained Shutdown error=%v Done closed=%v", err, closed(drained))
	}
	closing := startServer(t, serverConfig(t), http.NotFoundHandler()).server
	if err := closing.Close(); err != nil || !closed(closing) {
		t.Fatalf("Close error=%v Done closed=%v", err, closed(closing))
	}
}
