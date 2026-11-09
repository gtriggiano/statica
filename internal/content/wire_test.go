package content

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/storage"
)

func wireCertificate(t *testing.T) (*config.TLS, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return &config.TLS{CertFile: certPath, KeyFile: keyPath, MinVersion: "1.2"}, roots
}

func wireServer(t *testing.T, cfg *config.Config, h http.Handler) string {
	t.Helper()
	s, err := httpserver.NewServer(cfg.Server, h)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	t.Cleanup(func() {
		_ = s.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	scheme := "http://"
	if cfg.Server.TLS != nil {
		scheme = "https://"
	}
	return scheme + l.Addr().String()
}

func wireClient(t *testing.T, major int, roots *x509.CertPool) *http.Client {
	t.Helper()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(major == 2)
	transport := &http.Transport{Protocols: protocols, TLSClientConfig: &tls.Config{RootCAs: roots}, MaxConnsPerHost: 1, DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func TestWireProgressAndEarlyFailuresHTTP1AndHTTP2(t *testing.T) {
	for _, major := range []int{1, 2} {
		t.Run(strconvBool(major == 2), func(t *testing.T) {
			c := testConfig(t)
			c.Server.ResponseHeaderTimeout = 20 * time.Millisecond
			c.Server.WriteIdleTimeout = 100 * time.Millisecond
			var roots *x509.CertPool
			c.Server.TLS, roots = wireCertificate(t)
			f := fakeReader{get: func(ctx context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
				switch key {
				case "prepare":
					<-ctx.Done()
					return nil, ctx.Err()
				case "short":
					return object("x", storage.Metadata{Size: 5}), nil
				default:
					return &storage.Object{Metadata: storage.Metadata{Size: 8}, Body: &progressBody{ctx: ctx, remaining: 8, pause: 10 * time.Millisecond}}, nil
				}
			}}
			base := wireServer(t, c, boundary(t, c, f))
			client := wireClient(t, major, roots)
			for _, path := range []string{"/progress", "/prepare", "/short"} {
				response, err := client.Get(base + path)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if response.ProtoMajor != major {
					t.Fatalf("protocol %s", response.Proto)
				}
				switch path {
				case "/progress":
					if response.StatusCode != 200 || readErr != nil || string(body) != "xxxxxxxx" {
						t.Fatalf("progress %d %q %v", response.StatusCode, body, readErr)
					}
				case "/prepare":
					if response.StatusCode != 504 || readErr != nil {
						t.Fatalf("preparation %d %q %v", response.StatusCode, body, readErr)
					}
				case "/short":
					if response.StatusCode != 200 || readErr == nil {
						t.Fatalf("short response %d %q %v", response.StatusCode, body, readErr)
					}
				}
			}
		})
	}
}

func TestWireHTTP2TotalDeadlineIsolatesOneStream(t *testing.T) {
	c := testConfig(t)
	c.Server.RequestTimeout = 120 * time.Millisecond
	var roots *x509.CertPool
	c.Server.TLS, roots = wireCertificate(t)
	stalled := newBlockedBody()
	observations := make(chan Observation, 4)
	f := fakeReader{get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
		if key == "stall" {
			return &storage.Object{Metadata: storage.Metadata{Size: -1}, Body: stalled}, nil
		}
		return object("ok", storage.Metadata{Size: 2}), nil
	}}
	base := wireServer(t, c, boundary(t, c, f, WithObserver(func(o Observation) { observations <- o })))
	client := wireClient(t, 2, roots)
	var first, second net.Conn
	req, _ := http.NewRequest("GET", base+"/stall", nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { first = info.Conn }}))
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.ProtoMajor != 2 {
		t.Fatalf("wanted HTTP/2, got %s", response.Proto)
	}
	req, _ = http.NewRequest("GET", base+"/fast", nil)
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { second = info.Conn }}))
	fast, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(fast.Body)
	_ = fast.Body.Close()
	if err != nil || string(body) != "ok" || first != second {
		t.Fatalf("fast stream body=%q err=%v sameconn=%t", body, err, first == second)
	}
	if _, err = io.ReadAll(response.Body); err == nil {
		t.Fatal("stalled stream completed after total deadline")
	}
	_ = response.Body.Close()
	var incomplete Observation
	for range 2 {
		select {
		case o := <-observations:
			if o.OriginKey == "stall" {
				incomplete = o
			}
		case <-time.After(time.Second):
			t.Fatal("missing observation")
		}
	}
	if incomplete.Status != 200 || incomplete.Outcome != OutcomeIncomplete || incomplete.TerminationReason != ReasonTotalDeadline || !stalled.closed.Load() {
		t.Fatalf("incomplete %+v closed=%t", incomplete, stalled.closed.Load())
	}
	// The existing HTTP/2 connection remains usable after the reset.
	fast, err = client.Get(base + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(fast.Body)
	_ = fast.Body.Close()
	if err != nil || string(body) != "ok" {
		t.Fatalf("post-timeout stream %q %v", body, err)
	}
}

type repeatedBody struct {
	size      int64
	closed    atomic.Bool
	readCalls atomic.Int64
	maxRead   atomic.Int64
}

func (b *repeatedBody) Read(p []byte) (int, error) {
	if b.size == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.size)
	for i := range p[:n] {
		p[i] = 'x'
	}
	b.size -= n
	b.readCalls.Add(1)
	b.maxRead.Store(max(b.maxRead.Load(), int64(len(p))))
	return int(n), nil
}
func (b *repeatedBody) Close() error { b.closed.Store(true); return nil }

func TestWireBlockedClientHasBoundedWriteAndBodyMemory(t *testing.T) {
	c := testConfig(t)
	c.Server.WriteIdleTimeout = 35 * time.Millisecond
	c.S3.ReadIdleTimeout = 10 * time.Millisecond
	body := &repeatedBody{size: 128 << 20}
	observations := make(chan Observation, 1)
	f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
		return &storage.Object{Metadata: storage.Metadata{Size: 128 << 20}, Body: body}, nil
	}}
	base := wireServer(t, c, boundary(t, c, f, WithObserver(func(o Observation) { observations <- o })))
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	if _, err = io.WriteString(conn, "GET /large HTTP/1.1\r\nHost: example.org\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// The peer deliberately never reads. Kernel buffering is finite, so a
	// bounded Write/Flush must eventually reach the inactivity deadline.
	select {
	case o := <-observations:
		if o.Status != 200 || o.Outcome != OutcomeIncomplete || o.TerminationReason != ReasonClientWriteTimeout || !body.closed.Load() {
			t.Fatalf("observation %+v closed=%t", o, body.closed.Load())
		}
		if o.Bytes >= 128<<20 || body.maxRead.Load() != 32<<10 || body.readCalls.Load() <= 1 {
			t.Fatalf("unbounded/whole-body transfer: %+v maxread=%d reads=%d", o, body.maxRead.Load(), body.readCalls.Load())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked downstream write did not terminate")
	}
}

func TestWireHTTP2BlockedWriteDoesNotAbortOtherStreams(t *testing.T) {
	c := testConfig(t)
	c.Server.WriteIdleTimeout = 60 * time.Millisecond
	var roots *x509.CertPool
	c.Server.TLS, roots = wireCertificate(t)
	large := &repeatedBody{size: 128 << 20}
	observations := make(chan Observation, 4)
	f := fakeReader{get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
		if key == "large" {
			return &storage.Object{Metadata: storage.Metadata{Size: 128 << 20}, Body: large}, nil
		}
		return object("ok", storage.Metadata{Size: 2}), nil
	}}
	base := wireServer(t, c, boundary(t, c, f, WithObserver(func(o Observation) { observations <- o })))
	client := wireClient(t, 2, roots)
	response, err := client.Get(base + "/large")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("protocol %s", response.Proto)
	}
	fast, err := client.Get(base + "/fast")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(fast.Body)
	_ = fast.Body.Close()
	if err != nil || string(data) != "ok" {
		t.Fatalf("fast stream %q %v", data, err)
	}
	for {
		select {
		case o := <-observations:
			if o.OriginKey != "large" {
				continue
			}
			if o.Status != 200 || o.Outcome != OutcomeIncomplete || o.TerminationReason != ReasonClientWriteTimeout || !large.closed.Load() {
				t.Fatalf("large stream %+v closed=%t", o, large.closed.Load())
			}
			fast, err = client.Get(base + "/fast")
			if err != nil {
				t.Fatal(err)
			}
			data, err = io.ReadAll(fast.Body)
			_ = fast.Body.Close()
			if err != nil || string(data) != "ok" {
				t.Fatalf("stream after write timeout %q %v", data, err)
			}
			return
		case <-time.After(2 * time.Second):
			t.Fatal("HTTP/2 write did not time out")
		}
	}
}

func TestWireForcedDrainReportsShutdownAndReleasesOriginBody(t *testing.T) {
	c := testConfig(t)
	c.Server.ShutdownTimeout = 30 * time.Millisecond
	body := newBlockedBody()
	observations := make(chan Observation, 1)
	f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
		return &storage.Object{Metadata: storage.Metadata{Size: -1}, Body: body}, nil
	}}
	server, err := httpserver.NewServer(c.Server, boundary(t, c, f, WithObserver(func(o Observation) { observations <- o })))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("serve %v", err)
			}
		case <-time.After(time.Second):
			t.Error("serve did not stop")
		}
	})
	client := wireClient(t, 1, nil)
	response, err := client.Get("http://" + listener.Addr().String() + "/download")
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Shutdown(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown result %v", err)
	}
	if _, err = io.ReadAll(response.Body); err == nil {
		t.Fatal("forced drain completed stalled response")
	}
	_ = response.Body.Close()
	select {
	case o := <-observations:
		if o.OriginKey != "download" || o.Status != 200 || o.Outcome != OutcomeIncomplete || o.TerminationReason != ReasonShutdown || !body.closed.Load() {
			t.Fatalf("shutdown %+v closed=%t", o, body.closed.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not release handler")
	}
}

func TestWireDisconnectCancelsOnlyClientWork(t *testing.T) {
	c := testConfig(t)
	body := newBlockedBody()
	observations := make(chan Observation, 1)
	f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
		return &storage.Object{Metadata: storage.Metadata{Size: -1}, Body: body}, nil
	}}
	base := wireServer(t, c, boundary(t, c, f, WithObserver(func(o Observation) { observations <- o })))
	client := wireClient(t, 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/download", nil)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case o := <-observations:
		if o.Status != 200 || o.OriginKey != "download" || o.TerminationReason != ReasonDisconnect || !body.closed.Load() {
			t.Fatalf("disconnect %+v closed=%t", o, body.closed.Load())
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not release handler")
	}
}
