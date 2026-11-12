package content

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/site"
	"github.com/gtriggiano/statica/internal/storage"
)

// fakeReader serves Get with get. Without head, a Head returns the metadata
// get returns without options and closes its body, as S3 sends a HEAD the
// headers of the GET; the get calls it makes count as HEADs.
type fakeReader struct {
	get  func(context.Context, string, storage.ReadOptions) (*storage.Object, error)
	head func(context.Context, string) (storage.Metadata, error)
}

func (f fakeReader) Get(ctx context.Context, key string, o storage.ReadOptions) (*storage.Object, error) {
	return f.get(ctx, key, o)
}
func (f fakeReader) Head(ctx context.Context, key string) (storage.Metadata, error) {
	if f.head != nil {
		return f.head(ctx, key)
	}
	obj, err := f.get(ctx, key, storage.ReadOptions{})
	if err != nil {
		return storage.Metadata{}, err
	}
	if obj == nil || obj.Body == nil {
		return storage.Metadata{}, errors.New("unexpected HEAD")
	}
	_ = obj.Body.Close()
	return obj.Metadata, nil
}

func testConfig(t testing.TB) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte("s3:\n  bucket: test\n  region: eu-west-1\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func boundary(t testing.TB, c *config.Config, reader storage.Reader, options ...HandlerOption) http.Handler {
	t.Helper()
	content, err := NewHandler(c, reader, options...)
	if err != nil {
		t.Fatal(err)
	}
	h, err := httpserver.NewHandler(c, content)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func request(h http.Handler, method, target string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if headers != nil {
		r.Header = headers
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type trackedBody struct {
	io.Reader
	closed atomic.Bool
}

func (b *trackedBody) Close() error { b.closed.Store(true); return nil }

func object(body string, m storage.Metadata) *storage.Object {
	return &storage.Object{Metadata: m, Body: io.NopCloser(strings.NewReader(body))}
}

func TestPreconditionOrderAndEntityTagGrammar(t *testing.T) {
	m := storage.Metadata{ETag: `"a,b"`, LastModified: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	for _, tc := range []struct {
		name    string
		headers http.Header
		status  int
	}{
		{"weak match fails", http.Header{"If-Match": {`W/"a,b"`}}, 412},
		{"quoted comma", http.Header{"If-Match": {`"other", "a,b"`}}, 0},
		{"wildcard existence", http.Header{"If-Match": {"*"}}, 0},
		{"weak none match", http.Header{"If-None-Match": {`"other", W/"a,b"`}}, 304},
		{"match before none", http.Header{"If-Match": {`"bad"`}, "If-None-Match": {"*"}}, 412},
		{"match suppresses date", http.Header{"If-Match": {`"a,b"`}, "If-Unmodified-Since": {"Thu, 01 Jan 1970 00:00:00 GMT"}}, 0},
		{"none suppresses date", http.Header{"If-None-Match": {`"bad"`}, "If-Modified-Since": {"Fri, 02 Jan 2099 03:04:05 GMT"}}, 0},
		{"malformed match ignored whole", http.Header{"If-Match": {`"bad", broken`}, "If-Unmodified-Since": {"Thu, 01 Jan 1970 00:00:00 GMT"}}, 0},
		{"modified date", http.Header{"If-Modified-Since": {m.LastModified.Format(http.TimeFormat)}}, 304},
		{"unmodified date", http.Header{"If-Unmodified-Since": {"Thu, 01 Jan 1970 00:00:00 GMT"}}, 412},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := precondition(tc.headers, m); got != tc.status {
				t.Fatalf("got %d want %d", got, tc.status)
			}
		})
	}
	if precondition(http.Header{"If-None-Match": {"*"}}, storage.Metadata{}) != 304 {
		t.Fatal("wildcard must test existence without needing an ETag")
	}
}

func TestHEADAndConditionsCloseSnapshotWithoutReading(t *testing.T) {
	c := testConfig(t)
	var gets, heads atomic.Int32
	body := &trackedBody{Reader: strings.NewReader("hello")}
	m := storage.Metadata{Size: 5, ETag: `"v1"`, LastModified: time.Now().Add(-time.Hour)}
	f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
		gets.Add(1)
		return &storage.Object{Metadata: m, Body: body}, nil
	}, head: func(context.Context, string) (storage.Metadata, error) { heads.Add(1); return m, nil }}
	var observation Observation
	h := boundary(t, c, f, WithObserver(func(o Observation) { observation = o }))
	w := request(h, "HEAD", "http://example.org/file.txt", http.Header{"Range": {"bytes=1-2"}})
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "5" || gets.Load() != 0 || heads.Load() != 1 {
		t.Fatalf("HEAD %d %s calls %d/%d", w.Code, w.Header(), gets.Load(), heads.Load())
	}
	// A range request resolves on metadata: its 304 opens no body.
	w = request(h, "GET", "http://example.org/file.txt", http.Header{"If-None-Match": {`W/"v1"`}, "Range": {"bytes=1-2"}})
	if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != `"v1"` || w.Header().Get("Content-Length") != "" || gets.Load() != 0 || heads.Load() != 2 || observation.OriginKey != "file.txt" {
		t.Fatalf("range 304 %d %+v calls %d/%d observation %+v", w.Code, w.Header(), gets.Load(), heads.Load(), observation)
	}
	w = request(h, "GET", "http://example.org/file.txt", http.Header{"If-None-Match": {`W/"v1"`}})
	if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != `"v1"` || w.Header().Get("Content-Length") != "" || !body.closed.Load() || observation.OriginKey != "file.txt" {
		t.Fatalf("304 %d %+v observation %+v", w.Code, w.Header(), observation)
	}
	w = request(h, "GET", "http://example.org/file.txt", http.Header{"If-Match": {`"other"`}})
	if w.Code != 412 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "0" {
		t.Fatalf("412 %d %+v %q", w.Code, w.Header(), w.Body.String())
	}
}

func TestRangesAreGuardedAndMutationRestartsAreBounded(t *testing.T) {
	for _, mutations := range []int{0, 1, 2} {
		t.Run(string(rune('0'+mutations)), func(t *testing.T) {
			c := testConfig(t)
			calls := 0
			guards := 0
			snapshots := 0
			f := fakeReader{get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				calls++
				if o.Range == "" {
					t.Fatal("a range request read the full object")
				}
				guards++
				if o.Range != "bytes=1-3" || o.IfMatch != `"v1"` {
					t.Fatalf("unguarded range: %+v", o)
				}
				if guards <= mutations {
					return nil, &storage.Error{Kind: storage.Precondition}
				}
				return object("bcd", storage.Metadata{Size: 3, ETag: `"v1"`, ContentRange: "bytes 1-3/6"}), nil
			}, head: func(context.Context, string) (storage.Metadata, error) {
				calls++
				snapshots++
				return storage.Metadata{Size: 6, ETag: `"v1"`}, nil
			}}
			w := request(boundary(t, c, f), "GET", "http://example.org/f", http.Header{"Range": {"bytes=1-3"}})
			if mutations < 2 {
				if w.Code != 206 || w.Body.String() != "bcd" || w.Header().Get("Content-Range") != "bytes 1-3/6" || w.Header().Get("Content-Length") != "3" {
					t.Fatalf("range %d %+v %q", w.Code, w.Header(), w.Body.String())
				}
			} else if w.Code != 503 {
				t.Fatalf("repeated mutation status %d", w.Code)
			}
			if guards != min(mutations+1, 2) || snapshots != guards || calls != 2*guards {
				t.Fatalf("unbounded/missing reads %d/%d/%d", calls, snapshots, guards)
			}
		})
	}
}

func TestRangeParsingAndSafeFullResponseFallbacks(t *testing.T) {
	for _, tc := range []struct {
		value, ifRange      string
		conditional, ranges bool
		etag                string
		status              int
		body                string
	}{
		{"bytes=-2", "", true, true, `"v"`, 206, "ef"},
		{"bytes=4-", "", true, true, `"v"`, 206, "ef"},
		{"bytes=6-", "", true, true, `"v"`, 416, "Requested Range Not Satisfiable\n"},
		{"bytes=6-", `"other"`, true, true, `"v"`, 200, "abcdef"}, // If-Range is decided before satisfiability
		{"bytes=6-", "", true, true, `W/"v"`, 200, "abcdef"},
		{"bytes=0-1,3-4", "", true, true, `"v"`, 200, "abcdef"},
		{"bytes=+0-1", "", true, true, `"v"`, 200, "abcdef"},
		{"bytes=3-1", "", true, true, `"v"`, 200, "abcdef"},
		{"bytes=0-1", `W/"v"`, true, true, `"v"`, 200, "abcdef"},
		{"bytes=0-1", `"other"`, true, true, `"v"`, 200, "abcdef"},
		{"bytes=0-1", "", false, true, `"v"`, 200, "abcdef"},
		{"bytes=0-1", "", true, false, `"v"`, 200, "abcdef"},
		{"bytes=0-1", "", true, true, `W/"v"`, 200, "abcdef"},
		{"bytes=0-1", "", true, true, "", 200, "abcdef"},
	} {
		t.Run(tc.value+tc.ifRange+tc.etag+strconvBool(tc.conditional)+strconvBool(tc.ranges), func(t *testing.T) {
			c := testConfig(t)
			c.S3.Features.ConditionalReads = tc.conditional
			c.S3.Features.RangeReads = tc.ranges
			heads, full, ranged := 0, 0, 0
			f := fakeReader{get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range != "" {
					ranged++
					return object("ef", storage.Metadata{Size: 2, ETag: tc.etag, ContentRange: "bytes 4-5/6"}), nil
				}
				full++
				return object("abcdef", storage.Metadata{Size: 6, ETag: tc.etag}), nil
			}, head: func(context.Context, string) (storage.Metadata, error) {
				heads++
				return storage.Metadata{Size: 6, ETag: tc.etag}, nil
			}}
			headers := http.Header{"Range": {tc.value}}
			if tc.ifRange != "" {
				headers.Set("If-Range", tc.ifRange)
			}
			w := request(boundary(t, c, f), "GET", "http://example.org/f", headers)
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("got %d %q want %d %q", w.Code, w.Body.String(), tc.status, tc.body)
			}
			// A single range with both capabilities resolves on metadata;
			// only a 200 then reads the full object, only a 206 a range.
			_, single, _ := parseRange(tc.value, math.MaxInt64)
			want := map[bool]int{false: 0, true: 1}
			if heads != want[single && tc.conditional && tc.ranges] || full != want[tc.status == 200] || ranged != want[tc.status == 206] {
				t.Fatalf("reads: %d HEAD, %d full GET, %d ranged GET", heads, full, ranged)
			}
		})
	}
}
func strconvBool(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func TestBrowserCachePolicyAndMetadataAllowlist(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		name                      string
		m                         storage.Metadata
		override                  string
		want, expires, diagnostic string
	}{
		{"origin", storage.Metadata{CacheControl: "public, max-age=3600", Expires: now.Add(time.Hour).Format(http.TimeFormat), Date: now.Add(-30 * time.Second), Age: 10 * time.Second}, "", "public, max-age=3600", now.Add(time.Hour).Format(http.TimeFormat), ""},
		{"expires", storage.Metadata{Expires: now.Add(time.Hour).Format(http.TimeFormat), Date: now.Add(-30 * time.Second)}, "", "", now.Add(time.Hour).Format(http.TimeFormat), ""},
		{"ttl", storage.Metadata{}, "", "max-age=300", "", ""},
		{"ttl age", storage.Metadata{Date: now.Add(-120 * time.Second), Age: 60 * time.Second}, "", "max-age=300", "", ""},
		{"override", storage.Metadata{CacheControl: "max-age=300", Expires: now.Add(time.Hour).Format(http.TimeFormat)}, "private, max-age=10", "private, max-age=10", "", ""},
		{"bad directives", storage.Metadata{CacheControl: "max-age=-1"}, "", "no-store", "", "malformed_origin_cache_metadata"},
		{"bad expires", storage.Metadata{Expires: "tomorrow"}, "", "no-store", "", "malformed_origin_cache_metadata"},
		{"bad date", storage.Metadata{RawDate: "yesterday"}, "", "no-store", "", "malformed_origin_cache_metadata"},
		{"bad age", storage.Metadata{RawAge: "-1"}, "", "no-store", "", "malformed_origin_cache_metadata"},
		// RFC 9111 §1.2.2: delta-seconds that overflow saturate to 2^31 rather
		// than make the metadata malformed. These two were no-store before the
		// predicate moved to httpcache, shared with the memory cache.
		{"saturated age", storage.Metadata{RawAge: "10000000000"}, "", "max-age=300", "", ""},
		{"saturated max-age", storage.Metadata{CacheControl: "max-age=99999999999999999999"}, "", "max-age=99999999999999999999", "", ""},
		// The shared grammar allows only spaces and tabs around members and
		// valid UTF-8: a trailing U+00A0 (no-break space) or an invalid byte in
		// a quoted argument fails closed like any other stray byte.
		{"unicode space", storage.Metadata{CacheControl: "max-age=60\u00a0"}, "", "no-store", "", "malformed_origin_cache_metadata"},
		{"invalid utf-8", storage.Metadata{CacheControl: "ext=\"\xff\""}, "", "no-store", "", "malformed_origin_cache_metadata"},
		// RFC 9111 §1.2.2: every Age source saturates together at 2^31 seconds.
		{"saturated raw and decoded age", storage.Metadata{RawAge: "9223372036", Age: 100 * 365 * 24 * time.Hour}, "", "max-age=300", "", ""},
		{"saturated decoded age", storage.Metadata{Age: 100 * 365 * 24 * time.Hour}, "", "max-age=300", "", ""},
		{"saturated apparent age", storage.Metadata{Date: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)}, "", "max-age=300", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			if tc.override != "" {
				c.CacheControl.Default = tc.override
			}
			tc.m.Size = 1
			tc.m.ContentType = "bad\r\ninjection"
			tc.m.ContentDisposition = "inline\nInjected: true"
			tc.m.ETag = "bad"
			var o Observation
			f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
				return object("x", tc.m), nil
			}}
			w := request(boundary(t, c, f, WithObserver(func(v Observation) { o = v })), "GET", "http://example.org/thing.js", nil)
			if w.Header().Get("Cache-Control") != tc.want || w.Header().Get("Expires") != tc.expires || o.CacheDiagnostic != tc.diagnostic {
				t.Fatalf("headers %+v observation %+v", w.Header(), o)
			}
			if w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || w.Header().Get("Content-Disposition") != "" || w.Header().Get("ETag") != "" {
				t.Fatalf("unsafe metadata %+v", w.Header())
			}
			// The apparent age from Date grows with the real seconds elapsed
			// since now, whatever delays the test meets.
			ageWithin := func(base int64) bool {
				age, err := strconv.ParseInt(w.Header().Get("Age"), 10, 64)
				return err == nil && age >= base && age <= base+time.Now().Unix()-now.Unix()
			}
			if tc.name == "origin" && !ageWithin(30) {
				t.Fatalf("age counted twice or reset: %s", w.Header().Get("Age"))
			}
			if tc.name == "ttl age" && !ageWithin(120) {
				t.Fatalf("TTLprojection reset age: %+v", w.Header())
			}
			saturated := map[string]bool{"saturated age": true, "saturated raw and decoded age": true, "saturated decoded age": true, "saturated apparent age": true}
			if saturated[tc.name] && w.Header().Get("Age") != "2147483648" {
				t.Fatalf("Age did not saturate at 2^31: %+v", w.Header())
			}
		})
	}
}

func TestCustomErrorAndSPAHeadersAndVary(t *testing.T) {
	for _, spa := range []bool{false, true} {
		t.Run(strconvBool(spa), func(t *testing.T) {
			c := testConfig(t)
			c.NotFound = "404.html"
			if spa {
				c.SPA = &config.SPA{Fallback: "shell.html"}
			}
			c.CORS = &config.CORS{AllowedOrigins: []string{"https://client.example"}}
			f := fakeReader{get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
				if key == "404.html" || key == "shell.html" {
					return object("html", storage.Metadata{Size: 4, ETag: `"v"`, CacheControl: "public, max-age=3600"}), nil
				}
				return nil, &storage.Error{Kind: storage.NotFound}
			}}
			var o Observation
			w := request(boundary(t, c, f, WithObserver(func(v Observation) { o = v })), "GET", "http://example.org/route", http.Header{"Accept": {"text/html"}, "Origin": {"https://client.example"}})
			status := 404
			resolution := site.ResolutionNotFound
			key := "404.html"
			if spa {
				status = 200
				resolution = site.ResolutionSPA
				key = "shell.html"
			}
			if w.Code != status || w.Body.String() != "html" || w.Header().Get("Cache-Control") != "no-store" || o.OriginKey != key || o.Resolution != resolution {
				t.Fatalf("got %d %+v %q %+v", w.Code, w.Header(), w.Body.String(), o)
			}
			if !spa && w.Header().Get("ETag") != "" {
				t.Fatal("custom404 sent success validator")
			}
			joined := strings.Join(w.Header().Values("Vary"), ",")
			if !strings.Contains(joined, "Origin") || !strings.Contains(joined, "Accept-Encoding") || spa && !strings.Contains(joined, "Accept") {
				t.Fatalf("lost Vary: %q", joined)
			}
		})
	}
}

func TestPreparationAndTotalDeadlineScopes(t *testing.T) {
	t.Run("preparation", func(t *testing.T) {
		c := testConfig(t)
		c.Server.ResponseHeaderTimeout = 15 * time.Millisecond
		f := fakeReader{get: func(ctx context.Context, _ string, _ storage.ReadOptions) (*storage.Object, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		var o Observation
		w := request(boundary(t, c, f, WithObserver(func(v Observation) { o = v })), "GET", "http://example.org/f", nil)
		if w.Code != 504 || o.TerminationReason != ReasonPreparationTimeout {
			t.Fatalf("got %d %+v", w.Code, o)
		}
	})
	t.Run("progress beyond preparation", func(t *testing.T) {
		c := testConfig(t)
		c.Server.ResponseHeaderTimeout = 10 * time.Millisecond
		body := &progressBody{remaining: 5, pause: 8 * time.Millisecond}
		f := fakeReader{get: func(ctx context.Context, _ string, _ storage.ReadOptions) (*storage.Object, error) {
			body.ctx = ctx
			return &storage.Object{Metadata: storage.Metadata{Size: 5}, Body: body}, nil
		}}
		w := request(boundary(t, c, f), "GET", "http://example.org/f", nil)
		if w.Code != 200 || w.Body.String() != "xxxxx" || !body.closed.Load() {
			t.Fatalf("progressing body canceled: %d %q", w.Code, w.Body.String())
		}
	})
	t.Run("total interrupts read and closes body", func(t *testing.T) {
		c := testConfig(t)
		c.Server.RequestTimeout = 20 * time.Millisecond
		body := newBlockedBody()
		f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return &storage.Object{Metadata: storage.Metadata{Size: -1}, Body: body}, nil
		}}
		var o Observation
		h := boundary(t, c, f, WithObserver(func(v Observation) { o = v }))
		mustAbort(t, func() { request(h, "GET", "http://example.org/f", nil) })
		if o.Status != 200 || o.TerminationReason != ReasonTotalDeadline || !body.closed.Load() {
			t.Fatalf("total deadline %+v closed=%t", o, body.closed.Load())
		}
	})
}

type progressBody struct {
	ctx       context.Context
	remaining int
	pause     time.Duration
	closed    atomic.Bool
}

func (b *progressBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	time.Sleep(b.pause)
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	b.remaining--
	p[0] = 'x'
	return 1, nil
}
func (b *progressBody) Close() error { b.closed.Store(true); return nil }

type blockedBody struct {
	done   chan struct{}
	once   sync.Once
	closed atomic.Bool
}

func newBlockedBody() *blockedBody              { return &blockedBody{done: make(chan struct{})} }
func (b *blockedBody) Read([]byte) (int, error) { <-b.done; return 0, context.Canceled }
func (b *blockedBody) Close() error {
	b.closed.Store(true)
	b.once.Do(func() { close(b.done) })
	return nil
}

func mustAbort(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if p := recover(); p != http.ErrAbortHandler {
			t.Fatalf("panic %v want ErrAbortHandler", p)
		}
	}()
	fn()
	t.Fatal("stream did not abort")
}

func TestReadFailuresAfterHeadersAbortAndClose(t *testing.T) {
	for _, tc := range []struct {
		name   string
		size   int64
		reader io.Reader
		reason TerminationReason
	}{
		{"short", 5, strings.NewReader("abc"), ReasonLengthMismatch},
		{"long", 1, strings.NewReader("abc"), ReasonLengthMismatch},
		{"checksum", -1, errorReader{&storage.Error{Kind: storage.ChecksumMismatch}}, ReasonChecksumMismatch},
		{"origin idle", -1, errorReader{&storage.Error{Kind: storage.Timeout}}, ReasonOriginReadTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			b := &trackedBody{Reader: tc.reader}
			var o Observation
			f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
				return &storage.Object{Metadata: storage.Metadata{Size: tc.size}, Body: b}, nil
			}}
			h := boundary(t, c, f, WithObserver(func(v Observation) { o = v }))
			mustAbort(t, func() { request(h, "GET", "http://example.org/file", nil) })
			if o.Status != 200 || o.Outcome != OutcomeIncomplete || o.TerminationReason != tc.reason || !b.closed.Load() || o.OriginKey != "file" {
				t.Fatalf("observation %+v closed %t", o, b.closed.Load())
			}
		})
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

// leavingWriter cancels its request with cause as soon as the client has
// received after bytes of the body: a nil cause is a client that goes away.
type leavingWriter struct {
	*httptest.ResponseRecorder
	after  int
	cause  error
	cancel context.CancelCauseFunc
}

func (w *leavingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if w.Body.Len() >= w.after {
		w.cancel(w.cause)
	}
	return n, err
}

// leave serves a GET of target whose request ends with cause after after
// body bytes, and reports whether the handler aborted the response.
func leave(h http.Handler, target string, after int, cause error) (aborted bool) {
	r := httptest.NewRequest("GET", target, nil)
	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	w := &leavingWriter{ResponseRecorder: httptest.NewRecorder(), after: after, cause: cause, cancel: cancel}
	defer func() {
		if p := recover(); p != nil {
			if p != http.ErrAbortHandler {
				panic(p)
			}
			aborted = true
		}
	}()
	h.ServeHTTP(w, r.WithContext(ctx))
	return false
}

func TestClientLeavingAfterTheWholeBodyIsSuccess(t *testing.T) {
	const size = transferChunk + 100
	for _, tc := range []struct {
		name   string
		reader io.Reader
		after  int
	}{
		// The origin ends its body before the handler sees the client leave.
		{"origin end", strings.NewReader(strings.Repeat("x", size)), size},
		// The client's departure closes the origin body before its end.
		{"origin body closed", io.MultiReader(strings.NewReader(strings.Repeat("x", size)), errorReader{&storage.Error{Kind: storage.Canceled, Size: -1}}), size},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &trackedBody{Reader: tc.reader}
			var o Observation
			f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
				return &storage.Object{Metadata: storage.Metadata{Size: size}, Body: b}, nil
			}}
			h := boundary(t, testConfig(t), f, WithObserver(func(v Observation) { o = v }))
			if leave(h, "http://example.org/file", tc.after, nil) {
				t.Fatalf("aborted a complete response: %+v", o)
			}
			if o.Status != 200 || o.Outcome != OutcomeSuccess || o.TerminationReason != ReasonNone || o.Bytes != size || !b.closed.Load() {
				t.Fatalf("observation %+v closed %t", o, b.closed.Load())
			}
		})
	}

	t.Run("memory", func(t *testing.T) {
		cfg := cachedConfig(t, nil)
		origin := newMemOrigin()
		c, _ := newCache(t, cfg, origin, &testClock{t: time.Now()})
		var o Observation
		h := boundary(t, cfg, origin, WithCache(c), WithObserver(func(v Observation) { o = v }))
		if w := serve(h, "GET", "http://example.org/file.txt", nil); w.status != 200 {
			t.Fatalf("warm-up status %d", w.status)
		}
		if leave(h, "http://example.org/file.txt", len("hello world"), nil) {
			t.Fatalf("aborted a complete response: %+v", o)
		}
		if o.Status != 200 || o.Outcome != OutcomeSuccess || o.TerminationReason != ReasonNone || o.CacheOutcome != cache.OutcomeHit || o.CacheBytes != 11 {
			t.Fatalf("observation %+v", o)
		}
	})

	// A client that leaves before the last byte still cuts the response.
	t.Run("before the end", func(t *testing.T) {
		var o Observation
		f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return object(strings.Repeat("x", size), storage.Metadata{Size: size}), nil
		}}
		h := boundary(t, testConfig(t), f, WithObserver(func(v Observation) { o = v }))
		if !leave(h, "http://example.org/file", transferChunk, nil) {
			t.Fatalf("did not abort a cut response: %+v", o)
		}
		if o.Status != 200 || o.Outcome != OutcomeIncomplete || o.TerminationReason != ReasonDisconnect || o.Bytes != transferChunk {
			t.Fatalf("observation %+v", o)
		}
	})

	// A shutdown still cuts a streamed body whose origin end is unconfirmed.
	t.Run("shutdown after the end", func(t *testing.T) {
		var o Observation
		f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return object(strings.Repeat("x", size), storage.Metadata{Size: size}), nil
		}}
		h := boundary(t, testConfig(t), f, WithObserver(func(v Observation) { o = v }))
		if !leave(h, "http://example.org/file", size, httpserver.ErrShutdown) {
			t.Fatalf("did not abort on shutdown: %+v", o)
		}
		if o.Status != 200 || o.Outcome != OutcomeIncomplete || o.TerminationReason != ReasonShutdown || o.Bytes != size {
			t.Fatalf("observation %+v", o)
		}
	})
}

func TestRepeatedFieldsAndIfRangeDateStrength(t *testing.T) {
	lm := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := storage.Metadata{Size: 6, ETag: `"v"`, LastModified: lm, Date: lm.Add(2 * time.Minute)}
	for _, tc := range []struct {
		name    string
		headers http.Header
		status  int
	}{
		{"repeated range", http.Header{"Range": {"bytes=0-1", "bytes=3-4"}}, 200},
		{"repeated ifrange", http.Header{"Range": {"bytes=0-1"}, "If-Range": {`"v"`, `"other"`}}, 200},
		{"repeated modified date", http.Header{"If-Modified-Since": {lm.Format(http.TimeFormat), lm.Format(http.TimeFormat)}}, 200},
		{"repeated unmodified date", http.Header{"If-Unmodified-Since": {lm.Add(-time.Hour).Format(http.TimeFormat), lm.Add(-time.Hour).Format(http.TimeFormat)}}, 200},
		{"equal strong date", http.Header{"Range": {"bytes=0-1"}, "If-Range": {lm.Format(http.TimeFormat)}}, 206},
		{"future date", http.Header{"Range": {"bytes=0-1"}, "If-Range": {lm.Add(time.Hour).Format(http.TimeFormat)}}, 200},
		{"older date", http.Header{"Range": {"bytes=0-1"}, "If-Range": {lm.Add(-time.Hour).Format(http.TimeFormat)}}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := fakeReader{get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range != "" {
					partial := m
					partial.Size = 2
					partial.ContentRange = "bytes 0-1/6"
					return object("ab", partial), nil
				}
				return object("abcdef", m), nil
			}}
			w := request(boundary(t, testConfig(t), f), "GET", "http://example.org/file", tc.headers)
			if w.Code != tc.status {
				t.Fatalf("got %d want %d", w.Code, tc.status)
			}
		})
	}
	for _, date := range []time.Time{{}, lm.Add(59 * time.Second)} {
		weak := m
		weak.Date = date
		if ifRangeMatches(lm.Format(http.TimeFormat), weak) {
			t.Fatal("weak date used for If-Range")
		}
	}
}

func TestGuardedRangeUsesActualMetadataAndRestartsCodingChange(t *testing.T) {
	t.Run("metadata-only update", func(t *testing.T) {
		f := fakeReader{get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
			if o.Range != "" {
				return object("ab", storage.Metadata{Size: 2, ETag: `"v"`, ContentRange: "bytes 0-1/6", ContentType: "text/updated", CacheControl: "private, max-age=10"}), nil
			}
			return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`, ContentType: "text/old", CacheControl: "public, max-age=600"}), nil
		}}
		w := request(boundary(t, testConfig(t), f), "GET", "http://example.org/file", http.Header{"Range": {"bytes=0-1"}})
		if w.Code != 206 || w.Header().Get("Content-Type") != "text/updated" || w.Header().Get("Cache-Control") != "private, max-age=10" {
			t.Fatalf("mixed snapshots: %d %+v", w.Code, w.Header())
		}
	})
	t.Run("coding change", func(t *testing.T) {
		calls := 0
		f := fakeReader{get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
			calls++
			if o.Range != "" {
				return object("ab", storage.Metadata{Size: 2, ETag: `"v"`, ContentRange: "bytes 0-1/6", ContentEncoding: "gzip"}), nil
			}
			return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
		}}
		w := request(boundary(t, testConfig(t), f), "GET", "http://example.org/file", http.Header{"Range": {"bytes=0-1"}})
		if w.Code != 503 || calls != 4 || w.Header().Get("Content-Encoding") != "" {
			t.Fatalf("mislabeled/unbounded range: %d calls=%d %+v", w.Code, calls, w.Header())
		}
	})
}

func TestEffectiveCachePolicyUsesResolvedVariantAndCopiesPointers(t *testing.T) {
	c := testConfig(t)
	c.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br"}}
	ttl := 2 * time.Second
	c.Cache.Rules = []config.CacheRule{{Match: config.Match{Exact: "/file.js.br"}, TTL: &ttl}}
	f := fakeReader{get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
		if key == "file.js.br" {
			return object("br", storage.Metadata{Size: 2}), nil
		}
		return object("identity", storage.Metadata{Size: 8}), nil
	}}
	h := boundary(t, c, f)
	ttl = time.Hour
	c.Cache.Rules[0].Match.Exact = "/changed"
	c.Cache.TTL = time.Hour
	w := request(h, "GET", "http://example.org/file.js", http.Header{"Accept-Encoding": {"br"}})
	if w.Code != 200 || w.Body.String() != "br" || w.Header().Get("Cache-Control") != "max-age=2" || w.Header().Get("Content-Encoding") != "br" {
		t.Fatalf("policy did not use copied variant: %d %+v %q", w.Code, w.Header(), w.Body.String())
	}
}

// A regex sees the same path as exact and prefix: cache.rules the selected
// object, so a directory's index or a precompressed variant, and
// cache_control.rules the request path.
func TestRegexRulesMatchTheirOwnPaths(t *testing.T) {
	f := fakeReader{get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
		switch key {
		case "app.js", "app.js.br", "docs/index.html":
			return object(key, storage.Metadata{Size: int64(len(key))}), nil
		}
		return nil, &storage.Error{Kind: storage.NotFound}
	}}
	for _, tc := range []struct {
		name, rules string
		want        map[string]string // target and Accept-Encoding → Cache-Control
	}{
		{"file names only", `cache:
  rules:
    - match: {regex: '.*\.js'}
      ttl: 1h
    - match: {regex: '.*\.html'}
      ttl: 2m
cache_control:
  rules:
    - match: {regex: '.*\.html'}
      value: no-cache
`, map[string]string{"/app.js": "max-age=3600", "/app.js br": "max-age=300", "/docs/": "max-age=120", "/docs/index.html": "no-cache"}},
		{"directories and variants", `cache:
  rules:
    - match: {regex: '.*\.js(?:\.br|\.gz)?'}
      ttl: 1h
cache_control:
  rules:
    - match: {regex: '.*/|.*\.html'}
      value: no-cache
`, map[string]string{"/app.js": "max-age=3600", "/app.js br": "max-age=3600", "/docs/": "no-cache", "/docs/index.html": "no-cache"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := config.Parse([]byte("s3: {bucket: example-websites}\ncompression: {mode: precompressed, encodings: [br]}\n"+tc.rules), func(string) (string, bool) { return "", false })
			if err != nil {
				t.Fatal(err)
			}
			h := boundary(t, c, f)
			for target, want := range tc.want {
				path, encoding, _ := strings.Cut(target, " ")
				w := request(h, "GET", "http://example.com"+path, http.Header{"Accept-Encoding": {encoding}})
				if w.Code != 200 || w.Header().Get("Content-Encoding") != encoding || w.Header().Get("Cache-Control") != want {
					t.Errorf("%s: %d %q %q, want 200 %q %q", target, w.Code, w.Header().Get("Content-Encoding"), w.Header().Get("Cache-Control"), encoding, want)
				}
			}
		})
	}
}

func TestPreparationCommitChecksClockWithoutTimerCallback(t *testing.T) {
	p := startPreparation(context.Background(), time.Now().Add(time.Hour))
	defer p.close()
	p.deadline = time.Now().Add(-time.Second)
	if p.commit() == nil || !errors.Is(context.Cause(p.ctx), errPreparation) {
		t.Fatal("late preparation committed before timer callback")
	}
}

func TestShutdownBeforeHeadersUsesSafeStatusAndReason(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(httpserver.ErrShutdown)
	var observation Observation
	f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
		t.Fatal("shutdown context reached origin")
		return nil, nil
	}}
	h := boundary(t, testConfig(t), f, WithObserver(func(o Observation) { observation = o }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://example.org/file", nil).WithContext(ctx))
	if w.Code != 503 || observation.TerminationReason != ReasonShutdown || observation.OriginKey != "" {
		t.Fatalf("status=%d observation=%+v", w.Code, observation)
	}
}

func TestOriginErrorMappingStopsFallbackAndNeverClaimsKey(t *testing.T) {
	for _, tc := range []struct {
		kind   storage.Kind
		policy string
		status int
	}{{storage.Denied, "not_found", 404}, {storage.Denied, "bad_gateway", 502}, {storage.Throttled, "not_found", 503}, {storage.Timeout, "not_found", 504}, {storage.Unavailable, "not_found", 502}} {
		t.Run(string(tc.kind)+tc.policy, func(t *testing.T) {
			c := testConfig(t)
			c.Errors.AccessDenied = tc.policy
			c.NotFound = "404.html"
			calls := 0
			var o Observation
			f := fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
				calls++
				return nil, &storage.Error{Kind: tc.kind}
			}}
			w := request(boundary(t, c, f, WithObserver(func(v Observation) { o = v })), "GET", "http://example.org/missing", nil)
			if w.Code != tc.status || calls != 1 || o.OriginKey != "" || o.Outcome != OutcomeOriginFailure || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("got %d calls=%d %+v", w.Code, calls, o)
			}
		})
	}
}
