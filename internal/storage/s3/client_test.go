package s3

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

func testConfig(endpoint string) config.S3 {
	return config.S3{Bucket: "public-bucket", Region: "eu-west-1", Prefix: "docs/", Endpoint: endpoint, PathStyle: true, AllowHTTP: true, ResponseHeaderTimeout: time.Second, ReadIdleTimeout: time.Second, MaxAttempts: 1, Features: config.Features{ConditionalReads: true, RangeReads: true}}
}
func newClient(t *testing.T, cfg config.S3, options ...Option) *Client {
	t.Helper()
	options = append([]Option{WithCredentials(credentials.NewStaticCredentialsProvider("fixture-key", "fixture-secret", "fixture-token"))}, options...)
	c, err := New(context.Background(), cfg, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func xmlError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>private-origin-secret</Message></Error>", code)
}
func object(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	w.Header().Set("ETag", `"revision"`)
	w.Header().Set("Last-Modified", "Fri, 06 Nov 2026 12:00:00 GMT")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, payload)
}

func TestRealSDKRequestAndMetadata(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/public-bucket/literal%2f/space + café.txt" || r.URL.RawQuery != "x-id=GetObject" {
			t.Errorf("unexpected destination %q query %q", r.URL.Path, r.URL.RawQuery)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=fixture-key/") || r.Header.Get("X-Amz-Security-Token") != "fixture-token" {
			t.Error("missing SDK signing/session token")
		}
		if r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Cookie") != "" {
			t.Errorf("unwanted header: %v", r.Header)
		}
		if r.Header.Get("X-Amz-Checksum-Mode") != "" {
			t.Error("checksums unexpectedly requested")
		}
		w.Header().Set("Age", "3")
		w.Header().Set("Expires", "Fri, 06 Nov 2026 12:01:00 GMT")
		w.Header().Set("X-Amz-Meta-Secret", "do-not-forward")
		w.Header().Set("Content-Encoding", "gzip")
		object(w, "encoded bytes")
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.Prefix = "literal%2f/"
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")
	t.Setenv("AWS_ENDPOINT_URL_S3", "http://127.0.0.1:2")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_supported")
	c := newClient(t, cfg)
	o, err := c.Get(context.Background(), "literal%2f/space + café.txt", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(o.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Body.Close()
	if string(b) != "encoded bytes" || o.Metadata.ContentEncoding != "gzip" || o.Metadata.Size != 13 || o.Metadata.ETag != `"revision"` || o.Metadata.Age != 3*time.Second || o.Metadata.Expires == "" || o.Metadata.RawDate == "" || o.Metadata.Checksum != storage.ChecksumUnavailable {
		t.Fatalf("metadata=%+v body=%q", o.Metadata, b)
	}
	if calls.Load() != 1 || c.Region() != "eu-west-1" || !c.Capabilities().RangeReads {
		t.Fatal("unexpected config/operations")
	}
}

func TestTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		status     int
		head       bool
		kind       storage.Kind
	}{
		{"missing", "NoSuchKey", 404, false, storage.NotFound}, {"inconsistent-missing", "NoSuchKey", 503, false, storage.Unavailable}, {"inconsistent-not-modified", "NotModified", 503, false, storage.Unavailable}, {"inconsistent-precondition", "PreconditionFailed", 503, false, storage.Unavailable}, {"inconsistent-range", "InvalidRange", 503, false, storage.Unavailable}, {"bucket", "NoSuchBucket", 404, false, storage.Configuration}, {"denied", "AccessDenied", 403, false, storage.Denied}, {"denied-missing", "NoSuchKey", 403, false, storage.Denied}, {"head-ambiguous", "NotFound", 404, true, storage.AmbiguousNotFound}, {"get-ambiguous", "NotFound", 404, false, storage.AmbiguousNotFound}, {"unchanged", "NotModified", 304, false, storage.NotModified}, {"precondition", "PreconditionFailed", 412, false, storage.Precondition}, {"range", "InvalidRange", 416, false, storage.InvalidRange}, {"throttle", "SlowDown", 503, false, storage.Throttled}, {"outage", "InternalError", 500, false, storage.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 416 {
					w.Header().Set("Content-Range", "bytes */42")
				}
				xmlError(w, tc.status, tc.code)
			}))
			defer srv.Close()
			c := newClient(t, testConfig(srv.URL))
			var err error
			if tc.head {
				_, err = c.Head(context.Background(), "docs/key")
			} else {
				_, err = c.Get(context.Background(), "docs/key", storage.ReadOptions{})
			}
			if !storage.IsKind(err, tc.kind) {
				t.Fatalf("got %v want %s", err, tc.kind)
			}
			if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "fixture") {
				t.Fatal("leaked provider error")
			}
			if tc.status == 416 && err.(*storage.Error).Size != 42 {
				t.Fatal("missing range size")
			}
		})
	}
}

func TestFeaturesAndRanges(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Range") != "bytes=1-2" || r.Header.Get("If-Match") != `"v1"` || r.Header.Get("If-None-Match") != `"v0"` {
			t.Error("conditions missing")
		}
		w.Header().Set("Content-Range", "bytes 1-2/5")
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "bc")
	}))
	defer srv.Close()
	opts := storage.ReadOptions{Range: "bytes=1-2", IfMatch: `"v1"`, IfNoneMatch: `"v0"`}
	cfg := testConfig(srv.URL)
	c := newClient(t, cfg)
	o, err := c.Get(context.Background(), "docs/key", opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(o.Body)
	if err != nil || string(b) != "bc" || o.Metadata.ContentRange != "bytes 1-2/5" || o.Metadata.Size != 2 {
		t.Fatalf("range: %+v %q %v", o.Metadata, b, err)
	}
	cfg.Features = config.Features{}
	off := newClient(t, cfg)
	for _, v := range []storage.ReadOptions{{Range: "bytes=1-2"}, {IfMatch: `"v1"`}, {IfNoneMatch: `"v1"`}} {
		if _, err := off.Get(context.Background(), "docs/key", v); !storage.IsKind(err, storage.Unsupported) {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("disabled capability caused origin operation")
	}
}

func TestChecksumModes(t *testing.T) {
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte("hello"))
	valid := base64.StdEncoding.EncodeToString(crc.Sum(nil))
	for _, tc := range []struct {
		name, sum string
		enabled   bool
		want      storage.ChecksumStatus
		kind      storage.Kind
	}{
		{"valid", valid, true, storage.ChecksumVerified, ""}, {"mismatch", "AAAAAA==", true, storage.ChecksumPending, storage.ChecksumMismatch}, {"absent", "", true, storage.ChecksumUnavailable, ""}, {"composite", valid + "-2", true, storage.ChecksumUnavailable, ""}, {"off", "AAAAAA==", false, storage.ChecksumUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("X-Amz-Checksum-Mode"); (got == "ENABLED") != tc.enabled {
					t.Errorf("checksum request=%q", got)
				}
				if tc.sum != "" {
					w.Header().Set("X-Amz-Checksum-Crc32", tc.sum)
				}
				object(w, "hello")
			}))
			defer srv.Close()
			cfg := testConfig(srv.URL)
			cfg.Features.Checksums = tc.enabled
			c := newClient(t, cfg)
			o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.ReadAll(o.Body)
			defer o.Body.Close()
			if tc.kind != "" {
				if !storage.IsKind(err, tc.kind) {
					t.Fatalf("checksum error=%v", err)
				}
				if _, again := o.Body.Read(make([]byte, 1)); !storage.IsKind(again, tc.kind) {
					t.Fatalf("lost terminal checksum diagnostic: %v", again)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if o.Metadata.Checksum != tc.want {
				t.Fatalf("status=%s want=%s", o.Metadata.Checksum, tc.want)
			}
		})
	}
}

func TestRetriesAndCancellation(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			xmlError(w, 503, "SlowDown")
			return
		}
		object(w, "ok")
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.MaxAttempts = 3
	c := newClient(t, cfg)
	o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Body.Close()
	if calls.Load() != 3 {
		t.Fatal(calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "docs/key", storage.ReadOptions{}); !storage.IsKind(err, storage.Canceled) {
		t.Fatal(err)
	}
}

func TestHeaderAndReadTimeouts(t *testing.T) {
	t.Run("headers", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer srv.Close()
		cfg := testConfig(srv.URL)
		cfg.ResponseHeaderTimeout = 20 * time.Millisecond
		c := newClient(t, cfg)
		if _, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{}); !storage.IsKind(err, storage.Timeout) {
			t.Fatal(err)
		}
	})
	t.Run("active-read", func(t *testing.T) {
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(done)
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer srv.Close()
		cfg := testConfig(srv.URL)
		cfg.ReadIdleTimeout = 20 * time.Millisecond
		c := newClient(t, cfg)
		o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(o.Body)
		if !storage.IsKind(err, storage.Timeout) {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("origin body not canceled")
		}
	})
	t.Run("consumer-paused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { object(w, "ok") }))
		defer srv.Close()
		cfg := testConfig(srv.URL)
		cfg.ReadIdleTimeout = 10 * time.Millisecond
		c := newClient(t, cfg)
		o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(40 * time.Millisecond)
		b, err := io.ReadAll(o.Body)
		if err != nil || string(b) != "ok" {
			t.Fatalf("paused read: %q %v", b, err)
		}
	})
}

func TestTLSIsolationAndRedirects(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { object(w, "ok") }))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	cfg := testConfig(srv.URL)
	secure := newClient(t, cfg)
	if _, err := secure.Get(context.Background(), "docs/key", storage.ReadOptions{}); !storage.IsKind(err, storage.Unavailable) {
		t.Fatal(err)
	}
	cfg.InsecureSkipVerify = true
	c := newClient(t, cfg)
	o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Body.Close()
	if c.credentialTransport.TLSClientConfig.InsecureSkipVerify || !c.transport.TLSClientConfig.InsecureSkipVerify || c.credentialTransport == c.transport {
		t.Fatal("TLS exception leaked")
	}
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 302) }))
	defer origin.Close()
	redirect := newClient(t, testConfig(origin.URL))
	if _, err := redirect.Get(context.Background(), "docs/key", storage.ReadOptions{}); err == nil || redirected.Load() != 0 {
		t.Fatal("followed origin redirect")
	}
}

func TestVirtualHostAndAWSDefaultIgnoreAmbient(t *testing.T) {
	var host string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		if r.URL.Path != "/docs/key" {
			t.Error(r.URL.Path)
		}
		object(w, "ok")
	}))
	defer srv.Close()
	address := strings.TrimPrefix(srv.URL, "http://")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addressIgnored string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	defer transport.CloseIdleConnections()
	cfg := testConfig("http://fixture.example")
	cfg.PathStyle = false
	c := newClient(t, cfg, WithHTTPClient(&http.Client{Transport: transport, CheckRedirect: noRedirect}))
	o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Body.Close()
	if host != "public-bucket.fixture.example" {
		t.Fatal(host)
	}
	t.Setenv("AWS_ENDPOINT_URL", srv.URL)
	t.Setenv("AWS_ENDPOINT_URL_S3", srv.URL)
	cfg.Endpoint = ""
	cfg.PathStyle = true
	c = newClient(t, cfg, WithHTTPClient(httpClientFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "s3.eu-west-1.amazonaws.com" {
			t.Errorf("ambient endpoint selected: %s", r.URL.Host)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {"0"}}, ContentLength: 0, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})))
	o, err = c.Get(context.Background(), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Body.Close()
}

type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestKeyAndConstructorValidation(t *testing.T) {
	c := newClient(t, testConfig("http://127.0.0.1:1"))
	for _, key := range []string{"other/key", "docs/../key", "docs//key", "docs/\\key", "docs/\x00key", "docs/" + strings.Repeat("x", 1020), "docs/\xff"} {
		if _, err := c.Get(context.Background(), key, storage.ReadOptions{}); !storage.IsKind(err, storage.InvalidKey) {
			t.Fatalf("key %q: %v", key, err)
		}
	}
	for _, change := range []func(*config.S3){func(c *config.S3) { c.AllowHTTP = false }, func(c *config.S3) { c.InsecureSkipVerify = true }, func(c *config.S3) { c.Prefix = "docs" }, func(c *config.S3) { c.Endpoint += "?secret=yes" }, func(c *config.S3) { c.MaxAttempts = 4 }, func(c *config.S3) { c.Region = "bad/region" }} {
		cfg := testConfig("http://127.0.0.1:1")
		change(&cfg)
		if _, err := New(context.Background(), cfg); !storage.IsKind(err, storage.Configuration) {
			t.Fatal(err)
		}
	}
}

func TestRenewableCredentials(t *testing.T) {
	var retrieved atomic.Int32
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		n := retrieved.Add(1)
		return aws.Credentials{AccessKeyID: fmt.Sprint("key", n), SecretAccessKey: "secret", CanExpire: true, Expires: time.Now().Add(30 * time.Millisecond)}, nil
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { object(w, "ok") }))
	defer srv.Close()
	c := newClient(t, testConfig(srv.URL), WithCredentials(provider))
	for i := 0; i < 2; i++ {
		o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_ = o.Body.Close()
		if i == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if retrieved.Load() < 2 {
		t.Fatal("credentials did not refresh")
	}
}

func TestHeadAndBodyOwnership(t *testing.T) {
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "500")
			w.Header().Set("ETag", `"head"`)
			return
		}
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer srv.Close()
	c := newClient(t, testConfig(srv.URL))
	m, err := c.Head(context.Background(), "docs/key")
	if err != nil || m.Size != 500 || m.ETag != `"head"` {
		t.Fatalf("HEAD %+v %v", m, err)
	}
	o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Body.Close()
	_ = o.Body.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel origin")
	}
}

func TestMalformedAndUnknownLengthResponses(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		status                        int
		length, contentRange, payload string
		want                          storage.Kind
	}{
		{"truncated", 200, "5", "", "ab", storage.InvalidResponse},
		{"bad-partial", 206, "2", "bytes 1-3/5", "ab", storage.InvalidResponse},
		{"bad-range", 206, "2", "invalid", "ab", storage.InvalidResponse},
		{"unsolicited-partial", 206, "2", "bytes 1-2/5", "ab", storage.InvalidResponse},
		{"unknown", 200, "", "", "abcd", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.length != "" {
					w.Header().Set("Content-Length", tc.length)
				}
				if tc.contentRange != "" {
					w.Header().Set("Content-Range", tc.contentRange)
				}
				w.WriteHeader(tc.status)
				w.(http.Flusher).Flush()
				_, _ = io.WriteString(w, tc.payload)
			}))
			defer srv.Close()
			c := newClient(t, testConfig(srv.URL))
			o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
			if err == nil {
				_, err = io.ReadAll(o.Body)
				_ = o.Body.Close()
			}
			if tc.want != "" && !storage.IsKind(err, tc.want) {
				t.Fatal(err)
			}
			if tc.want == "" && (err != nil || o.Metadata.Size != -1) {
				t.Fatalf("unknown length: %+v %v", o, err)
			}
		})
	}
}

func TestCallerCancellationDuringBodyRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := newClient(t, testConfig(srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o, err := c.Get(ctx, "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()
	_, err = io.ReadAll(o.Body)
	if !storage.IsKind(err, storage.Canceled) {
		t.Fatal(err)
	}
}

func TestPrivateTransportAndReservedBuckets(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("global transport used")
		return nil, fmt.Errorf("unused")
	})
	defer func() { http.DefaultTransport = previous }()
	c := newClient(t, testConfig("http://127.0.0.1:1"))
	if c.transport == c.credentialTransport {
		t.Fatal("shared private transport")
	}
	for _, bucket := range []string{"directory--x-s3", "point.mrap", "point-s3alias", "table--table-s3"} {
		cfg := testConfig("http://127.0.0.1:1")
		cfg.Bucket = bucket
		if _, err := New(context.Background(), cfg); !storage.IsKind(err, storage.Configuration) {
			t.Fatalf("bucket %q: %v", bucket, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTP2ReadTimeoutIsStreamLocal(t *testing.T) {
	var connections atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("expected HTTP/2, got %s", r.Proto)
		}
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		if strings.HasSuffix(r.URL.Path, "slow") {
			<-r.Context().Done()
			return
		}
		<-release
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.InsecureSkipVerify = true
	cfg.ReadIdleTimeout = 30 * time.Millisecond
	c := newClient(t, cfg)
	slow, err := c.Get(context.Background(), "docs/slow", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fast, err := c.Get(context.Background(), "docs/fast", storage.ReadOptions{})
	if err != nil {
		_ = slow.Body.Close()
		close(release)
		t.Fatal(err)
	}
	_, err = io.ReadAll(slow.Body)
	close(release)
	if !storage.IsKind(err, storage.Timeout) {
		t.Fatal(err)
	}
	b, err := io.ReadAll(fast.Body)
	if err != nil || string(b) != "ok" {
		t.Fatalf("unrelated stream interrupted: %q %v", b, err)
	}
	if connections.Load() != 1 {
		t.Fatalf("expected multiplexed connection, got %d", connections.Load())
	}
}

func TestStandardCredentialAndRegionChain(t *testing.T) {
	file := filepath.Join(t.TempDir(), "empty-aws-config")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"AWS_CONFIG_FILE": file, "AWS_SHARED_CREDENTIALS_FILE": file, "AWS_PROFILE": "", "AWS_REGION": "us-west-2", "AWS_DEFAULT_REGION": "", "AWS_ACCESS_KEY_ID": "chain-key", "AWS_SECRET_ACCESS_KEY": "chain-secret", "AWS_SESSION_TOKEN": "chain-session", "AWS_EC2_METADATA_DISABLED": "true"} {
		t.Setenv(key, value)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "Credential=chain-key/") || !strings.Contains(r.Header.Get("Authorization"), "/us-west-2/s3/") || r.Header.Get("X-Amz-Security-Token") != "chain-session" {
			t.Error("default credential/signing chain missing")
		}
		object(w, "ok")
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.Region = ""
	c, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Region() != "us-west-2" {
		t.Fatal(c.Region())
	}
	o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = o.Body.Close()
	t.Setenv("AWS_REGION", "")
	if _, err := New(context.Background(), cfg, WithCredentials(credentials.NewStaticCredentialsProvider("key", "secret", ""))); !storage.IsKind(err, storage.Configuration) {
		t.Fatalf("missing region: %v", err)
	}
	cfg.Region = "explicit-region"
	c, err = New(context.Background(), cfg, WithCredentials(credentials.NewStaticCredentialsProvider("key", "secret", "")))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Region() != "explicit-region" {
		t.Fatal(c.Region())
	}
}

func TestNotModifiedKeepsCacheFields(t *testing.T) {
	const date, expires, modified = "Fri, 06 Nov 2026 12:00:00 GMT", "Fri, 06 Nov 2026 12:01:00 GMT", "Fri, 06 Nov 2026 11:00:00 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != `"revision"` {
			t.Errorf("If-None-Match = %q", r.Header.Get("If-None-Match"))
		}
		h := w.Header()
		h.Set("ETag", `"revision"`)
		h["Cache-Control"] = []string{"max-age=60", "must-revalidate"}
		h.Set("Expires", expires)
		h.Set("Date", date)
		h.Set("Age", "7")
		h.Set("Last-Modified", modified)
		h.Set("Content-Language", "en")
		h.Set("Content-Disposition", "attachment")
		h.Set("X-Amz-Meta-Secret", "do-not-forward")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	c := newClient(t, testConfig(srv.URL))
	_, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{IfNoneMatch: `"revision"`})
	var e *storage.Error
	if !errors.As(err, &e) || e.Kind != storage.NotModified || e.Size != -1 || e.Metadata == nil {
		t.Fatalf("got %#v", err)
	}
	parsed := func(v string) time.Time {
		t.Helper()
		p, err := http.ParseTime(v)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	want := storage.Metadata{Size: -1, ETag: `"revision"`, CacheControl: "max-age=60, must-revalidate", Expires: expires, RawDate: date, Date: parsed(date), RawAge: "7", Age: 7 * time.Second, LastModified: parsed(modified), Checksum: storage.ChecksumUnavailable}
	if !reflect.DeepEqual(*e.Metadata, want) {
		t.Fatalf("304 metadata = %+v\nwant %+v", *e.Metadata, want)
	}
	if err.Error() != "origin: not_modified" {
		t.Fatalf("error text %q", err.Error())
	}
}

func TestNotModifiedRejectsUnsafeFields(t *testing.T) {
	for _, tc := range []struct {
		name, field, value string
		want               storage.Kind
	}{
		{"etag-crlf", "ETag", "\"a\"\r\nX-Injected: 1", storage.InvalidResponse},
		{"cache-control-lf", "Cache-Control", "max-age=60\nprivate", storage.InvalidResponse},
		{"expires-nul", "Expires", "Fri, 06 Nov 2026 12:01:00 GMT\x00", storage.InvalidResponse},
		{"date-cr", "Date", "Fri, 06 Nov 2026 12:00:00 GMT\r", storage.InvalidResponse},
		{"age-lf", "Age", "1\n2", storage.InvalidResponse},
		{"last-modified-unparseable", "Last-Modified", "yesterday", storage.InvalidResponse},
		{"ignored-field", "Content-Type", "text/plain\r\nX-Injected: 1", storage.NotModified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, testConfig("http://fixture.example"), WithHTTPClient(httpClientFunc(func(r *http.Request) (*http.Response, error) {
				h := http.Header{"Etag": {`"a"`}}
				h[http.CanonicalHeaderKey(tc.field)] = []string{tc.value}
				return &http.Response{StatusCode: http.StatusNotModified, Header: h, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})))
			_, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{IfNoneMatch: `"a"`})
			var e *storage.Error
			if !errors.As(err, &e) || e.Kind != tc.want {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			if (e.Metadata != nil) != (tc.want == storage.NotModified) {
				t.Fatalf("metadata %+v for %s", e.Metadata, e.Kind)
			}
			if tc.want == storage.NotModified && (e.Metadata.ETag != `"a"` || e.Metadata.ContentType != "") {
				t.Fatalf("metadata %+v", *e.Metadata)
			}
		})
	}
}

// Connecting to the origin, TLS handshake included, is bounded by
// s3.response_header_timeout, for origin and credential traffic alike.
func TestConnectionSetupBoundedByResponseHeaderTimeout(t *testing.T) {
	// The listener accepts TCP connections but never answers the handshake.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	defer func() { _ = l.Close(); <-stopped }()
	go func() {
		defer close(stopped)
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			held = append(held, c)
		}
	}()
	cfg := testConfig("https://" + l.Addr().String())
	cfg.AllowHTTP = false
	cfg.ResponseHeaderTimeout = 50 * time.Millisecond
	c := newClient(t, cfg)
	for _, transport := range []*http.Transport{c.transport, c.credentialTransport} {
		if transport.TLSHandshakeTimeout != cfg.ResponseHeaderTimeout {
			t.Fatalf("TLS handshake timeout %v", transport.TLSHandshakeTimeout)
		}
		if transport.MaxIdleConns != 100 || transport.MaxIdleConnsPerHost != 100 {
			t.Fatalf("idle pool limits: total=%d perHost=%d", transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
		}
	}
	started := time.Now()
	// Like a missing response, a stalled handshake is an origin timeout.
	if _, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{}); !storage.IsKind(err, storage.Timeout) {
		t.Fatalf("stalled handshake: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("stalled handshake took %v", elapsed)
	}
	// Longer budgets keep the fixed connection limits.
	cfg.ResponseHeaderTimeout = time.Minute
	if long := newClient(t, cfg); long.transport.TLSHandshakeTimeout != 10*time.Second || long.credentialTransport.TLSHandshakeTimeout != 10*time.Second {
		t.Fatalf("TLS handshake timeout %v", long.transport.TLSHandshakeTimeout)
	}
}

func TestDialBoundedByResponseHeaderTimeout(t *testing.T) {
	limit := 100 * time.Millisecond
	transport := privateTransport(limit)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	started := time.Now()
	// TEST-NET-1 is not routed: where the network drops the SYN, only the
	// dial timeout ends the attempt.
	conn, err := transport.DialContext(ctx, "tcp", "192.0.2.1:443")
	elapsed := time.Since(started)
	if err == nil {
		_ = conn.Close()
		t.Skip("192.0.2.1 is reachable from this network")
	}
	var timeout interface{ Timeout() bool }
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Skipf("the network rejected the dial at once: %v", err)
	}
	// Far below the fixed 30s limit, with room for slow test hosts.
	if elapsed > 5*time.Second {
		t.Fatalf("dial took %v with a %v limit", elapsed, limit)
	}
}
