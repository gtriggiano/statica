package httpserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
)

func boundaryClient(t *testing.T, major int, roots *x509.CertPool) *http.Client {
	t.Helper()
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetHTTP2(major == 2)
	transport := &http.Transport{Protocols: p, TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func TestServerBoundaryHTTP1AndHTTP2(t *testing.T) {
	for _, major := range []int{1, 2} {
		t.Run(fmt.Sprintf("HTTP%d", major), func(t *testing.T) {
			cfg := handlerConfig(t)
			cfg.Hosts = []string{"example.com"}
			cfg.Headers.Set["Referrer-Policy"] = "no-referrer"
			cfg.CORS = &config.CORS{AllowedOrigins: []string{"https://client.example"}}
			var roots *x509.CertPool
			cfg.Server.TLS, roots = certificateFixture(t)
			var calls atomic.Int32
			h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				m, ok := MetadataFromContext(r.Context())
				if !ok || m.Host != "example.com" || m.Object.Key != "public/index.html" {
					t.Errorf("metadata: %+v, present=%t", m, ok)
				}
				testHandler(w, r)
			})
			f := startServer(t, cfg.Server, h)
			client := boundaryClient(t, major, roots)
			for _, tc := range []struct {
				method, target, host string
				status               int
				body                 string
			}{
				{"GET", "/", "EXAMPLE.com.:443", 200, "ok"},
				{"HEAD", "/", "example.com", 200, ""},
				{"GET", "/", "other.example", 421, "Misdirected Request\n"},
				{"GET", "/a%2fb", "example.com", 400, "Bad Request\n"},
				{"GET", "/.secret", "example.com", 404, "Not Found\n"},
			} {
				r, err := http.NewRequest(tc.method, "https://"+f.addr+tc.target, nil)
				if err != nil {
					t.Fatal(err)
				}
				r.Host = tc.host
				r.Header.Set("Origin", "https://client.example")
				response, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				body, readErr := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if readErr != nil || response.StatusCode != tc.status || response.ProtoMajor != major || string(body) != tc.body {
					t.Fatalf("%s %s: status=%d proto=%s body=%q err=%v", tc.method, tc.target, response.StatusCode, response.Proto, body, readErr)
				}
				if response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Referrer-Policy") != "no-referrer" || !validRequestID(response.Header.Get("X-Request-ID")) {
					t.Fatalf("boundary headers missing: %+v", response.Header)
				}
				if tc.status == 200 && (response.Header.Get("Access-Control-Allow-Origin") != "https://client.example" || response.Header.Get("Content-Length") != "2") {
					t.Fatalf("content headers: %+v", response.Header)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("rejected requests reached content: %d calls", calls.Load())
			}
		})
	}
}

func TestServerBoundaryRejectsDelayedHTTP1Bodies(t *testing.T) {
	for _, framing := range []string{"Content-Length: 100\r\n", "Transfer-Encoding: chunked\r\n"} {
		t.Run(strings.TrimSpace(framing), func(t *testing.T) {
			cfg := handlerConfig(t)
			var calls atomic.Int32
			h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); testHandler(w, r) })
			f := startServer(t, cfg.Server, h)
			c := dialServer(t, f.addr)
			// Send only headers. The boundary must return its rejection without
			// waiting for body bytes, even for chunked framing of unknown length.
			_, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: example.com\r\n"+framing+"\r\n")
			if err != nil {
				t.Fatal(err)
			}
			r := bufio.NewReader(c)
			response := readResponse(t, r)
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != 400 || string(body) != "Bad Request\n" || !response.Close {
				t.Fatalf("body rejection: status=%d close=%t body=%q err=%v", response.StatusCode, response.Close, body, err)
			}
			// An unread payload cannot become a second request on this socket.
			_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: example.com\r\n\r\n")
			if _, err := r.ReadByte(); err == nil {
				t.Fatal("body-bearing connection remained reusable")
			}
			if calls.Load() != 0 {
				t.Fatal("body-bearing request reached content")
			}
		})
	}
}

func TestServerBoundaryHTTP2AdmissionAndBodyIsolation(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Server.MaxConcurrentRequests = 1
	cfg.Server.MaxConnections = 1
	var roots *x509.CertPool
	cfg.Server.TLS, roots = certificateFixture(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	waitFinished := make(chan struct{}, 1)
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/wait" {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		testHandler(w, r)
	}, WithObserver(func(o Observation) {
		if o.Event == ObservationFinish && calls.Load() == 2 {
			waitFinished <- struct{}{}
		}
	}))
	f := startServer(t, cfg.Server, h)
	client := boundaryClient(t, 2, roots)
	// Warm the sole allowed connection so both later requests share it.
	response, err := client.Get("https://" + f.addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatalf("expected H2, got %s", response.Proto)
	}
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	waitDone := make(chan error, 1)
	go func() {
		r, _ := http.NewRequestWithContext(waitCtx, http.MethodGet, "https://"+f.addr+"/wait", nil)
		response, err := client.Do(r)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
		}
		waitDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("waiting stream did not start")
	}
	// Rejected body-bearing overload must not alter the waiting stream's
	// connection deadline. Payload is withheld until rejection has arrived.
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	bodyRequest, _ := http.NewRequest(http.MethodGet, "https://"+f.addr+"/", reader)
	bodyRequest.ContentLength = 100
	response, err = client.Do(bodyRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, bodyErr := io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || bodyErr != nil {
		t.Fatalf("overload response=%d body error=%v", response.StatusCode, bodyErr)
	}
	select {
	case err := <-waitDone:
		t.Fatalf("rejected body's deadline affected another stream: %v", err)
	default:
	}
	cancelWait()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("cancelled stream did not finish")
	}
	select {
	case <-waitFinished:
	case <-time.After(time.Second):
		t.Fatal("cancelled stream did not release its admission slot")
	}
	// Cancellation and rejection both release capacity; a bodyless H2 GET
	// must still succeed on the same connection afterwards.
	response, err = client.Get("https://" + f.addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || err != nil || calls.Load() != 3 {
		t.Fatalf("post-cancellation status=%d calls=%d err=%v", response.StatusCode, calls.Load(), err)
	}
	// Independently exercise a framed body once admission is available.
	r, _ := http.NewRequest(http.MethodGet, "https://"+f.addr+"/", strings.NewReader("payload"))
	response, err = client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 400 || err != nil || calls.Load() != 3 {
		t.Fatalf("H2 body rejection status=%d calls=%d err=%v", response.StatusCode, calls.Load(), err)
	}
}
