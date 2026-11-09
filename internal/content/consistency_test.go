package content

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

// A 200 and an origin 206 of the same object use one extension table when
// the stored Content-Type is unusable.
func TestFullAndRangedResponsesAgreeOnContentType(t *testing.T) {
	for _, name := range []string{"movie.mp4", "clip.webm", "song.mp3", "sound.ogg", "data.csv", "bundle.zip", "font.ttf", "font.otf", "page.html", "blob.unknown"} {
		t.Run(name, func(t *testing.T) {
			f := fakeReader{get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range != "" {
					return object("bc", storage.Metadata{Size: 2, ETag: `"v"`, ContentRange: "bytes 1-2/6"}), nil
				}
				return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
			}}
			h := boundary(t, testConfig(t), f)
			full := request(h, "GET", "http://example.org/"+name, nil)
			ranged := request(h, "GET", "http://example.org/"+name, http.Header{"Range": {"bytes=1-2"}})
			if full.Code != 200 || ranged.Code != 206 {
				t.Fatalf("status %d / %d", full.Code, ranged.Code)
			}
			if a, b := full.Header().Get("Content-Type"), ranged.Header().Get("Content-Type"); a != b {
				t.Fatalf("200 Content-Type %q, 206 Content-Type %q", a, b)
			}
		})
	}
	if got := request(boundary(t, testConfig(t), fakeReader{get: func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
		return object("x", storage.Metadata{Size: 1}), nil
	}}), "GET", "http://example.org/movie.mp4", nil).Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("movie.mp4 Content-Type %q", got)
	}
}

// Accept-Ranges is sent only on 200 and 206 object responses.
func TestAcceptRangesOnlyOnFullAndPartialResponses(t *testing.T) {
	c := testConfig(t)
	c.NotFound = "404.html"
	f := fakeReader{get: func(_ context.Context, key string, o storage.ReadOptions) (*storage.Object, error) {
		switch {
		case key == "404.html":
			return object("gone", storage.Metadata{Size: 4, ETag: `"n"`}), nil
		case key != "file.txt":
			return nil, &storage.Error{Kind: storage.NotFound}
		case o.Range != "":
			return object("bc", storage.Metadata{Size: 2, ETag: `"v"`, ContentRange: "bytes 1-2/6"}), nil
		}
		return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
	}, head: func(context.Context, string) (storage.Metadata, error) {
		return storage.Metadata{Size: 6, ETag: `"v"`}, nil
	}}
	h := boundary(t, c, f)
	for _, tc := range []struct {
		name, method, target string
		header               http.Header
		status               int
		advertised           bool
	}{
		{"200", "GET", "/file.txt", nil, 200, true},
		{"HEAD", "HEAD", "/file.txt", nil, 200, true},
		{"206", "GET", "/file.txt", http.Header{"Range": {"bytes=1-2"}}, 206, true},
		{"304", "GET", "/file.txt", http.Header{"If-None-Match": {`"v"`}}, 304, false},
		{"412", "GET", "/file.txt", http.Header{"If-Match": {`"other"`}}, 412, false},
		{"custom 404", "GET", "/missing.txt", nil, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(h, tc.method, "http://example.org"+tc.target, tc.header)
			if w.Code != tc.status || (w.Header().Get("Accept-Ranges") == "bytes") != tc.advertised {
				t.Fatalf("status %d, Accept-Ranges %q", w.Code, w.Header().Get("Accept-Ranges"))
			}
		})
	}
}

// A 416 and a 406 log the resolution of the resource they answer for.
func TestUnsatisfiableAndUnacceptableRecordResolution(t *testing.T) {
	c := testConfig(t)
	c.SPA = &config.SPA{Fallback: "index.html"}
	f := fakeReader{get: func(_ context.Context, key string, _ storage.ReadOptions) (*storage.Object, error) {
		switch key {
		case "file.txt", "index.html":
			return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
		}
		return nil, &storage.Error{Kind: storage.NotFound}
	}}
	for _, tc := range []struct {
		name, target string
		header       http.Header
		status       int
		resolution   string
	}{
		{"416", "/file.txt", http.Header{"Range": {"bytes=10-20"}}, 416, "ordinary"},
		{"406 ordinary", "/file.txt", http.Header{"Accept-Encoding": {"identity;q=0"}}, 406, "ordinary"},
		{"406 SPA shell", "/app/route", http.Header{"Accept": {"text/html"}, "Accept-Encoding": {"identity;q=0"}}, 406, "spa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPipeline(t, c, f)
			r := httptest.NewRequest("GET", "http://example.org"+tc.target, nil)
			r.Header = tc.header.Clone()
			w := p.do(r)
			records := p.take()
			if w.Code != tc.status || len(records) != 1 || records[0].Resolution != tc.resolution {
				t.Fatalf("status %d, records %+v", w.Code, records)
			}
			e := logEvents(t, p.log.String())[0]
			if e["resolution"] != tc.resolution {
				t.Fatalf("event %v", e)
			}
		})
	}
}

// When the object headers cannot be committed in time, the 504 keeps none
// of them, Vary included.
func TestDeadlineAfterPreparedHeadersDropsVary(t *testing.T) {
	c := testConfig(t)
	c.Server.ResponseHeaderTimeout = 30 * time.Millisecond
	f := fakeReader{get: func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
		if o.Range != "" {
			// The guarded range read succeeds after the preparation deadline,
			// so committing the prepared object headers fails.
			time.Sleep(60 * time.Millisecond)
			return object("bc", storage.Metadata{Size: 2, ETag: `"v"`, ContentRange: "bytes 1-2/6"}), nil
		}
		return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
	}}
	w := request(boundary(t, c, f), "GET", "http://example.org/file.txt", http.Header{"Range": {"bytes=1-2"}})
	if w.Code != 504 || len(w.Header().Values("Vary")) != 0 || w.Header().Get("Accept-Ranges") != "" {
		t.Fatalf("status %d, headers %v", w.Code, w.Header())
	}
}

// A deadline that ends the request logs origin_timeout wherever it strikes,
// including during the guarded range read.
func TestDeadlineErrorClassDuringGuardedRangeRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config)
		reason string
	}{
		{"preparation", func(c *config.Config) { c.Server.ResponseHeaderTimeout = 20 * time.Millisecond }, "preparation_timeout"},
		{"total", func(c *config.Config) { c.Server.RequestTimeout = 20 * time.Millisecond }, "total_deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			tc.mutate(c)
			f := fakeReader{get: func(ctx context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range != "" {
					<-ctx.Done()
					return nil, &storage.Error{Kind: storage.Canceled}
				}
				return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
			}}
			p := newPipeline(t, c, f)
			r := httptest.NewRequest("GET", "http://example.org/file.txt", nil)
			r.Header.Set("Range", "bytes=1-2")
			w := p.do(r)
			records := p.take()
			if w.Code != 504 || len(records) != 1 {
				t.Fatalf("status %d, %d records", w.Code, len(records))
			}
			if rec := records[0]; rec.ErrorClass != "origin_timeout" || rec.TerminationReason != tc.reason {
				t.Fatalf("record %+v", rec)
			}
		})
	}
}

// A client that disconnects before the response headers receives nothing:
// the request is recorded as incomplete with status 0, at warn level, and
// without an origin error class, wherever preparation was.
func TestDisconnectBeforeHeadersIsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
		get    func(cancel context.CancelFunc) func(context.Context, string, storage.ReadOptions) (*storage.Object, error)
	}{
		{name: "during resolution", get: func(cancel context.CancelFunc) func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return func(ctx context.Context, _ string, _ storage.ReadOptions) (*storage.Object, error) {
				cancel()
				<-ctx.Done()
				return nil, &storage.Error{Kind: storage.Canceled}
			}
		}},
		{name: "before a generated 404", get: func(cancel context.CancelFunc) func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
				cancel()
				return nil, &storage.Error{Kind: storage.NotFound}
			}
		}},
		{name: "during the guarded range read", header: http.Header{"Range": {"bytes=1-2"}}, get: func(cancel context.CancelFunc) func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return func(ctx context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range == "" {
					return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
				}
				cancel()
				<-ctx.Done()
				return nil, ctx.Err()
			}
		}},
		{name: "before the object headers", header: http.Header{"Range": {"bytes=1-2"}}, get: func(cancel context.CancelFunc) func(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
			return func(_ context.Context, _ string, o storage.ReadOptions) (*storage.Object, error) {
				if o.Range == "" {
					return object("abcdef", storage.Metadata{Size: 6, ETag: `"v"`}), nil
				}
				cancel()
				return object("bc", storage.Metadata{Size: 2, ETag: `"v"`, ContentRange: "bytes 1-2/6"}), nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := newPipeline(t, testConfig(t), fakeReader{get: tc.get(cancel)})
			r := httptest.NewRequest("GET", "http://example.org/file.txt", nil).WithContext(ctx)
			if tc.header != nil {
				r.Header = tc.header.Clone()
			}
			w := p.do(r)
			records := p.take()
			if len(records) != 1 || w.Body.Len() != 0 || w.Flushed {
				t.Fatalf("%d records, %d body bytes, flushed %t", len(records), w.Body.Len(), w.Flushed)
			}
			rec := records[0]
			if rec.Status != 0 || rec.Outcome != "incomplete" || rec.TerminationReason != "disconnect" || rec.ErrorClass != "" {
				t.Fatalf("record %+v", rec)
			}
			e := logEvents(t, p.log.String())[0]
			if e["level"] != "warn" || e["status"] != "0" || e["outcome"] != "incomplete" || e["termination_reason"] != "disconnect" {
				t.Fatalf("event %v", e)
			}
			if _, ok := e["error_class"]; ok {
				t.Fatalf("event %v", e)
			}
		})
	}
}
