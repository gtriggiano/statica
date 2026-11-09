package site

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

type resolverFixture struct {
	metadata storage.Metadata
	err      error
	headErr  error
}
type resolverBody struct{ closed, reads int }

func (b *resolverBody) Read(p []byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *resolverBody) Close() error               { b.closed++; return nil }

type resolverStore struct {
	objects map[string]resolverFixture
	calls   []string
	bodies  []*resolverBody
	cancel  context.CancelFunc
}

func (s *resolverStore) Get(ctx context.Context, key string, options storage.ReadOptions) (*storage.Object, error) {
	s.calls = append(s.calls, "GET "+key)
	if options != (storage.ReadOptions{}) {
		panic("resolver sent browser conditions or range to origin")
	}
	fixture, ok := s.objects[key]
	if !ok {
		return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
	}
	if fixture.err != nil {
		return nil, fixture.err
	}
	body := &resolverBody{}
	s.bodies = append(s.bodies, body)
	if s.cancel != nil {
		s.cancel()
	}
	return &storage.Object{Metadata: fixture.metadata, Body: body}, nil
}
func (s *resolverStore) Head(ctx context.Context, key string) (storage.Metadata, error) {
	s.calls = append(s.calls, "HEAD "+key)
	fixture, ok := s.objects[key]
	if !ok {
		return storage.Metadata{}, &storage.Error{Kind: storage.NotFound, Size: -1}
	}
	if fixture.headErr != nil {
		return storage.Metadata{}, fixture.headErr
	}
	return fixture.metadata, fixture.err
}
func resolverConfig() *config.Config {
	return &config.Config{Index: "index.html", S3: config.S3{Prefix: "pub/", Features: config.Features{RangeReads: true}}, Compression: config.Compression{Mode: "off"}}
}
func fixture(coding string) resolverFixture {
	return resolverFixture{metadata: storage.Metadata{Size: 42, ETag: `"revision"`, ContentEncoding: coding}}
}
func newResolverTest(t *testing.T, cfg *config.Config, objects map[string]resolverFixture) (*Resolver, *resolverStore) {
	t.Helper()
	store := &resolverStore{objects: objects}
	resolver, err := NewResolver(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	return resolver, store
}
func resolverRequest(t *testing.T, raw, method string, header http.Header) Request {
	t.Helper()
	target, err := ParseTarget(request(raw))
	if err != nil {
		t.Fatal(err)
	}
	return Request{Target: target, Method: method, Header: header}
}
func mustResolve(t *testing.T, r *Resolver, req Request) *Selection {
	t.Helper()
	selected, err := r.Resolve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return selected
}
func assertCalls(t *testing.T, s *resolverStore, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(s.calls, want) {
		t.Fatalf("calls = %q, want %q", s.calls, want)
	}
}
func assertBodies(t *testing.T, s *resolverStore, closed ...int) {
	t.Helper()
	if len(s.bodies) != len(closed) {
		t.Fatalf("body count = %d want %d", len(s.bodies), len(closed))
	}
	for i, b := range s.bodies {
		if b.closed != closed[i] || b.reads != 0 {
			t.Fatalf("body %d: closed %d reads %d", i, b.closed, b.reads)
		}
	}
}

func TestResolverOrdinaryIndexRedirectAndOwnership(t *testing.T) {
	for _, tc := range []struct {
		raw, key   string
		resolution Resolution
		status     int
		location   string
	}{
		{"/asset.js?v=1", "pub/asset.js", ResolutionOrdinary, 200, ""},
		{"/", "pub/index.html", ResolutionIndex, 200, ""},
		{"/guide/", "pub/guide/index.html", ResolutionIndex, 200, ""},
		{"/a%20b?x=%2F", "pub/a b/index.html", ResolutionIndex, 308, "/a%20b/?x=%2F"},
		{"/guide?", "pub/guide/index.html", ResolutionIndex, 308, "/guide/?"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			r, store := newResolverTest(t, resolverConfig(), map[string]resolverFixture{tc.key: fixture("")})
			selected := mustResolve(t, r, resolverRequest(t, tc.raw, "GET", nil))
			if selected.Key != tc.key || selected.Status != tc.status || selected.Resolution != tc.resolution || selected.Location != tc.location {
				t.Fatalf("selection = %+v", selected)
			}
			if tc.status == 308 {
				assertBodies(t, store, 1)
				if selected.Body != nil {
					t.Fatal("redirect owns body")
				}
			} else {
				assertBodies(t, store, 0)
				if !selected.RangeCapable || selected.Metadata.ETag != `"revision"` {
					t.Fatalf("representation = %+v", selected)
				}
				closeSelection(selected)
				assertBodies(t, store, 1)
			}
			for _, call := range store.calls {
				if strings.Contains(call, "?") {
					t.Fatalf("query in key: %q", call)
				}
			}
		})
	}
}

func TestResolverSPARouteEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, path, accept string
		includes, excludes []string
		spa                bool
	}{
		{"html extensionless", "/orders/42", "text/html", nil, nil, true},
		{"trailing slash", "/orders/42/", "text/html", nil, nil, true},
		{"explicit quality", "/orders/42", "text/html;q=0.1,application/json", nil, nil, true},
		{"wildcard", "/orders/42", "*/*", nil, nil, false},
		{"json", "/orders/42", "application/json", nil, nil, false},
		{"html refused", "/orders/42", "text/html;q=0,*/*;q=1", nil, nil, false},
		{"html malformed", "/orders/42", "text/html;q=wrong", nil, nil, false},
		{"dotted implicit", "/app/release/1.2", "text/html", nil, nil, false},
		{"dotted included", "/app/release/1.2", "text/html", []string{"/app/"}, nil, true},
		{"root explicit", "/bundle.js", "text/html", []string{"/"}, nil, true},
		{"excluded wins", "/app/assets/bundle.js", "text/html", []string{"/app/"}, []string{"/app/assets/"}, false},
		{"prefix boundary", "/application/route", "text/html", []string{"/app/"}, nil, false},
		{"prefix root", "/app", "text/html", []string{"/app/"}, nil, true},
		{"exclude prefix root", "/api", "text/html", nil, []string{"/api/"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolverConfig()
			cfg.SPA = &config.SPA{Fallback: "shell.html", IncludePrefixes: tc.includes, ExcludePrefixes: tc.excludes}
			cfg.NotFound = "404.html"
			r, store := newResolverTest(t, cfg, map[string]resolverFixture{"pub/shell.html": fixture(""), "pub/404.html": fixture("")})
			selected := mustResolve(t, r, resolverRequest(t, tc.path, "GET", http.Header{"Accept": {tc.accept}}))
			want := ResolutionNotFound
			key := "pub/404.html"
			if tc.spa {
				want = ResolutionSPA
				key = "pub/shell.html"
			}
			if selected.Resolution != want || selected.Key != key {
				t.Fatalf("selection = %+v", selected)
			}
			if selected.VaryAccept != r.spaRoute(tc.path) {
				t.Fatalf("Vary Accept = %v", selected.VaryAccept)
			}
			closeSelection(selected)
			assertBodies(t, store, 1)
		})
	}
}

func TestResolverSPAOrdinaryWinsAndMissingShellDoesNotRecurse(t *testing.T) {
	cfg := resolverConfig()
	cfg.SPA = &config.SPA{Fallback: "shell.html"}
	cfg.NotFound = "404.html"
	r, s := newResolverTest(t, cfg, map[string]resolverFixture{"pub/route": fixture("")})
	selected := mustResolve(t, r, resolverRequest(t, "/route", "GET", http.Header{"Accept": {"text/html"}}))
	assertCalls(t, s, "GET pub/route")
	if selected.Resolution != ResolutionOrdinary || selected.VaryAccept {
		t.Fatalf("selection = %+v", selected)
	}
	closeSelection(selected)
	r, s = newResolverTest(t, cfg, nil)
	selected = mustResolve(t, r, resolverRequest(t, "/route", "GET", http.Header{"Accept": {"text/html"}}))
	assertCalls(t, s, "GET pub/route", "GET pub/route/index.html", "GET pub/shell.html", "GET pub/404.html")
	if selected.Status != 404 || selected.Key != "" || selected.Body != nil || !selected.VaryAccept {
		t.Fatalf("selection = %+v", selected)
	}
}

func TestResolverTerminalErrorsNeverFallBack(t *testing.T) {
	for _, kind := range []storage.Kind{storage.Denied, storage.Unavailable, storage.Configuration, storage.Timeout, storage.Unsupported} {
		for stage, key := range []string{"pub/route", "pub/route/index.html", "pub/shell.html", "pub/404.html"} {
			t.Run(string(kind)+key, func(t *testing.T) {
				cfg := resolverConfig()
				cfg.SPA = &config.SPA{Fallback: "shell.html"}
				cfg.NotFound = "404.html"
				originErr := &storage.Error{Kind: kind, Size: -1}
				r, s := newResolverTest(t, cfg, map[string]resolverFixture{key: {err: originErr}})
				selected, err := r.Resolve(context.Background(), resolverRequest(t, "/route", "GET", http.Header{"Accept": {"text/html"}}))
				if selected != nil || !storage.IsKind(err, kind) {
					t.Fatalf("selection %v error %v", selected, err)
				}
				if len(s.calls) != stage+1 {
					t.Fatalf("calls = %q", s.calls)
				}
			})
		}
	}
}

func TestResolverEncodingNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		present      bool
		canonical    string
		want         string
		status       int
		keys         []string
	}{
		{name: "absent canonical", canonical: "identity", want: "pub/app.js", keys: []string{"GET pub/app.js"}},
		{name: "absent encoded", canonical: "gzip", want: "pub/app.js", keys: []string{"GET pub/app.js"}},
		{name: "empty", present: true, canonical: "identity", want: "pub/app.js", keys: []string{"GET pub/app.js"}},
		{name: "empty encoded refusal", present: true, canonical: "gzip", status: 406, keys: []string{"GET pub/app.js"}},
		{name: "tie configured br", present: true, header: "gzip,br", canonical: "identity", want: "pub/app.js.br", keys: []string{"GET pub/app.js", "GET pub/app.js.br"}},
		{name: "quality beats config", present: true, header: "gzip;q=1,br;q=0.5,identity;q=0", canonical: "identity", want: "pub/app.js.gz", keys: []string{"GET pub/app.js", "GET pub/app.js.gz"}},
		{name: "identity default wins", present: true, header: "br;q=0.5", canonical: "identity", want: "pub/app.js", keys: []string{"GET pub/app.js"}},
		{name: "wildcard", present: true, header: "*", canonical: "identity", want: "pub/app.js.br", keys: []string{"GET pub/app.js", "GET pub/app.js.br"}},
		{name: "wildcard zero identity override", present: true, header: "*;q=0,identity;q=1", canonical: "identity", want: "pub/app.js", keys: []string{"GET pub/app.js"}},
		{name: "all refused", present: true, header: "*;q=0", canonical: "identity", status: 406, keys: []string{"GET pub/app.js"}},
		{name: "canonical coding ranks above sibling", present: true, header: "gzip;q=1,br;q=0.5,identity;q=0", canonical: "gzip", want: "pub/app.js", keys: []string{"GET pub/app.js"}},
		{name: "canonical samecoding tie wins", present: true, header: "gzip,identity;q=0", canonical: "gzip", want: "pub/app.js", keys: []string{"GET pub/app.js"}},
		{name: "canonical actual coding loses", present: true, header: "br;q=1,gzip;q=0.5,identity;q=0", canonical: "gzip", want: "pub/app.js.br", keys: []string{"GET pub/app.js", "GET pub/app.js.br"}},
		{name: "duplicate refusal wins", present: true, header: "br;q=0,br;q=1,gzip;q=1,identity;q=0", canonical: "identity", want: "pub/app.js.gz", keys: []string{"GET pub/app.js", "GET pub/app.js.gz"}},
		{name: "bad quality refused", present: true, header: "br;q=1.001,gzip,identity;q=0", canonical: "identity", want: "pub/app.js.gz", keys: []string{"GET pub/app.js", "GET pub/app.js.gz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolverConfig()
			cfg.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br", "gzip"}}
			canonical := fixture(tc.canonical)
			canonical.metadata.ContentType = "application/x-javascript"
			r, s := newResolverTest(t, cfg, map[string]resolverFixture{"pub/app.js": canonical, "pub/app.js.br": fixture("br"), "pub/app.js.gz": fixture("gzip")})
			h := http.Header(nil)
			if tc.present {
				h = http.Header{"Accept-Encoding": {tc.header}}
			}
			selected, err := r.Resolve(context.Background(), resolverRequest(t, "/app.js", "GET", h))
			assertCalls(t, s, tc.keys...)
			if tc.status != 0 {
				wantStatus(t, err, tc.status)
				assertBodies(t, s, 1)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if selected.Key != tc.want || !selected.VaryEncoding || selected.Metadata.ContentType != "application/x-javascript" || selected.LogicalPath != "/app.js" {
				t.Fatalf("selection = %+v", selected)
			}
			if len(s.bodies) == 2 {
				assertBodies(t, s, 1, 0)
			} else {
				assertBodies(t, s, 0)
			}
			closeSelection(selected)
			for _, body := range s.bodies {
				if body.closed != 1 {
					t.Fatalf("body closed %d times", body.closed)
				}
			}
		})
	}
}

func TestResolverEncodingOffAndSiblingMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, canonical, sibling string
		mode                     string
		status                   int
		kind                     storage.Kind
		want                     string
	}{
		{name: "off no siblings", mode: "off", canonical: "identity", want: "pub/app.js"},
		{name: "off declared accepted", mode: "off", canonical: "gzip", want: "pub/app.js"},
		{name: "off unsupported", mode: "off", canonical: "deflate", kind: storage.InvalidResponse},
		{name: "off stacked", mode: "off", canonical: "gzip, br", kind: storage.InvalidResponse},
		{name: "suffix inferred", mode: "precompressed", canonical: "identity", sibling: "", want: "pub/app.js.br"},
		{name: "suffix mismatch", mode: "precompressed", canonical: "identity", sibling: "gzip", kind: storage.InvalidResponse},
		{name: "suffix identity mismatch", mode: "precompressed", canonical: "identity", sibling: "identity", kind: storage.InvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resolverConfig()
			cfg.Compression.Mode = tc.mode
			r, s := newResolverTest(t, cfg, map[string]resolverFixture{"pub/app.js": fixture(tc.canonical), "pub/app.js.br": fixture(tc.sibling)})
			selected, err := r.Resolve(context.Background(), resolverRequest(t, "/app.js", "GET", http.Header{"Accept-Encoding": {"br,gzip,identity"}}))
			if tc.kind != "" {
				if !storage.IsKind(err, tc.kind) {
					t.Fatalf("error = %v", err)
				}
				for _, body := range s.bodies {
					if body.closed != 1 {
						t.Fatal("leaked body")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if selected.Key != tc.want || selected.ContentType != "text/javascript; charset=utf-8" {
				t.Fatalf("selection = %+v", selected)
			}
			if selected.Coding == "identity" && selected.Metadata.ContentEncoding != "" {
				t.Fatal("identity forwarded")
			}
			closeSelection(selected)
		})
	}
}

func TestResolverHEADParityAndAmbiguity(t *testing.T) {
	cfg := resolverConfig()
	cfg.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br", "gzip"}}
	for _, method := range []string{"GET", "HEAD"} {
		r, s := newResolverTest(t, cfg, map[string]resolverFixture{"pub/app.js": fixture(""), "pub/app.js.br": fixture("br")})
		selected := mustResolve(t, r, resolverRequest(t, "/app.js", method, http.Header{"Accept-Encoding": {"br"}, "Range": {"bytes=0-2"}}))
		if selected.Key != "pub/app.js.br" || selected.Metadata.Size != 42 || selected.Coding != "br" {
			t.Fatalf("selection = %+v", selected)
		}
		assertCalls(t, s, method+" pub/app.js", method+" pub/app.js.br")
		if method == "HEAD" {
			assertBodies(t, s)
			if selected.Body != nil {
				t.Fatal("HEAD owns body")
			}
		} else {
			closeSelection(selected)
			assertBodies(t, s, 1, 1)
		}
	}
	r, s := newResolverTest(t, resolverConfig(), map[string]resolverFixture{"pub/file": {metadata: fixture("").metadata, headErr: &storage.Error{Kind: storage.AmbiguousNotFound, Size: -1}}})
	selected := mustResolve(t, r, resolverRequest(t, "/file", "HEAD", nil))
	assertCalls(t, s, "HEAD pub/file", "GET pub/file")
	assertBodies(t, s, 1)
	if selected.Key != "pub/file" || selected.Body != nil {
		t.Fatalf("selection = %+v", selected)
	}
	r, s = newResolverTest(t, resolverConfig(), map[string]resolverFixture{"pub/file": {err: &storage.Error{Kind: storage.NotFound, Size: -1}, headErr: &storage.Error{Kind: storage.AmbiguousNotFound, Size: -1}}})
	selected = mustResolve(t, r, resolverRequest(t, "/file", "HEAD", nil))
	assertCalls(t, s, "HEAD pub/file", "GET pub/file", "HEAD pub/file/index.html")
	if selected.Status != 404 {
		t.Fatalf("selection = %+v", selected)
	}
}

func TestResolverBudgetsAndDeduplication(t *testing.T) {
	cfg := resolverConfig()
	cfg.Compression = config.Compression{Mode: "precompressed", Encodings: []string{"br", "gzip"}}
	cfg.SPA = &config.SPA{Fallback: "shell.html"}
	cfg.NotFound = "404.html"
	r, s := newResolverTest(t, cfg, nil)
	selected := mustResolve(t, r, resolverRequest(t, "/missing", "GET", http.Header{"Accept-Encoding": {"br,gzip"}, "Accept": {"text/html"}}))
	if selected.Status != 404 || len(s.calls) != 12 {
		t.Fatalf("selection = %+v calls %q", selected, s.calls)
	}
	seen := map[string]bool{}
	for _, call := range s.calls {
		if seen[call] {
			t.Fatalf("repeated call %q", call)
		}
		seen[call] = true
	}
	cfg.NotFound = "shell.html"
	r, s = newResolverTest(t, cfg, nil)
	mustResolve(t, r, resolverRequest(t, "/missing", "GET", http.Header{"Accept-Encoding": {"br,gzip"}, "Accept": {"text/html"}}))
	if len(s.calls) != 9 {
		t.Fatalf("duplicate logical fallback calls = %q", s.calls)
	}
	// A physical sibling can also be a later canonical fallback; it is never
	// fetched twice once its absence has already been confirmed.
	cfg.SPA = &config.SPA{Fallback: "missing.br"}
	cfg.NotFound = "missing.br"
	r, s = newResolverTest(t, cfg, nil)
	mustResolve(t, r, resolverRequest(t, "/missing", "GET", http.Header{"Accept-Encoding": {"br,gzip"}, "Accept": {"text/html"}}))
	for _, call := range s.calls {
		if strings.Count(strings.Join(s.calls, "\n"), call+"\n") > 1 {
			t.Fatalf("repeated key in %q", s.calls)
		}
	}
}

func TestResolverMissingAndUnacceptableRemainDistinct(t *testing.T) {
	cfg := resolverConfig()
	cfg.Compression.Mode = "precompressed"
	r, s := newResolverTest(t, cfg, map[string]resolverFixture{"pub/app.js.br": fixture("br")})
	selected := mustResolve(t, r, resolverRequest(t, "/app.js", "GET", nil))
	if selected.Status != 404 {
		t.Fatalf("absent header unexpectedly found sibling: %+v", selected)
	}
	assertCalls(t, s, "GET pub/app.js", "GET pub/app.js/index.html")
	r, _ = newResolverTest(t, cfg, nil)
	selected = mustResolve(t, r, resolverRequest(t, "/app.js", "GET", http.Header{"Accept-Encoding": {"*;q=0"}}))
	if selected.Status != 404 {
		t.Fatalf("absent object returned %d", selected.Status)
	}
	r, s = newResolverTest(t, cfg, map[string]resolverFixture{"pub/app.js": fixture("gzip")})
	_, err := r.Resolve(context.Background(), resolverRequest(t, "/app.js", "GET", http.Header{"Accept-Encoding": {"br,identity;q=0"}}))
	wantStatus(t, err, 406)
	assertCalls(t, s, "GET pub/app.js", "GET pub/app.js.br")
	assertBodies(t, s, 1)
}

func TestResolverDeniedSiblingClosesCanonical(t *testing.T) {
	cfg := resolverConfig()
	cfg.Compression.Mode = "precompressed"
	r, s := newResolverTest(t, cfg, map[string]resolverFixture{"pub/app.js": fixture(""), "pub/app.js.br": {err: &storage.Error{Kind: storage.Denied, Size: -1}}})
	selected, err := r.Resolve(context.Background(), resolverRequest(t, "/app.js", "GET", http.Header{"Accept-Encoding": {"br,gzip"}}))
	if selected != nil || !storage.IsKind(err, storage.Denied) {
		t.Fatalf("selection %v error %v", selected, err)
	}
	assertCalls(t, s, "GET pub/app.js", "GET pub/app.js.br")
	assertBodies(t, s, 1)
}

func TestResolverByteLimitsAndCancellation(t *testing.T) {
	cfg := resolverConfig()
	cfg.Compression.Mode = "precompressed"
	r, s := newResolverTest(t, cfg, nil)
	path := "/" + strings.Repeat("a", MaxKeyBytes-len(cfg.S3.Prefix)-1)
	_, err := r.Resolve(context.Background(), resolverRequest(t, path, "GET", http.Header{"Accept-Encoding": {"br"}}))
	wantStatus(t, err, 414)
	assertCalls(t, s, "GET "+cfg.S3.Prefix+path[1:])
	cfg = resolverConfig()
	cfg.NotFound = ".internal/404%2ehtml"
	r, s = newResolverTest(t, cfg, map[string]resolverFixture{"pub/.internal/404%2ehtml": fixture("")})
	selected := mustResolve(t, r, resolverRequest(t, "/missing", "GET", nil))
	if selected.Key != "pub/.internal/404%2ehtml" {
		t.Fatalf("literal configured key = %q", selected.Key)
	}
	closeSelection(selected)
	r, s = newResolverTest(t, resolverConfig(), map[string]resolverFixture{"pub/file": fixture("")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Resolve(ctx, resolverRequest(t, "/file", "GET", nil))
	if !errors.Is(err, context.Canceled) || len(s.calls) != 0 {
		t.Fatalf("error %v calls %q", err, s.calls)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	s.cancel = cancel
	_, err = r.Resolve(ctx, resolverRequest(t, "/file", "GET", nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	assertBodies(t, s, 1)
}

func TestResolverUnknownLengthAndInvalidPartialOrigin(t *testing.T) {
	for _, tc := range []struct {
		size         int64
		contentRange string
		invalid      bool
	}{{-1, "", false}, {-2, "", true}, {3, "bytes 0-2/42", true}} {
		entry := fixture("")
		entry.metadata.Size = tc.size
		entry.metadata.ContentRange = tc.contentRange
		r, s := newResolverTest(t, resolverConfig(), map[string]resolverFixture{"pub/file": entry})
		selected, err := r.Resolve(context.Background(), resolverRequest(t, "/file", "GET", nil))
		if tc.invalid {
			if !storage.IsKind(err, storage.InvalidResponse) {
				t.Fatalf("error = %v", err)
			}
			assertBodies(t, s, 1)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if selected.RangeCapable || selected.Metadata.Size != -1 {
			t.Fatalf("selection = %+v", selected)
		}
		closeSelection(selected)
	}
}

func TestResolverMIMERequiresExtension(t *testing.T) {
	for _, path := range []string{"/html", "/css", "/json", "/folder.js/html"} {
		r, s := newResolverTest(t, resolverConfig(), map[string]resolverFixture{"pub" + path: fixture("")})
		selected := mustResolve(t, r, resolverRequest(t, path, "GET", nil))
		if selected.ContentType != "application/octet-stream" {
			t.Fatalf("%s type = %q", path, selected.ContentType)
		}
		closeSelection(selected)
		assertBodies(t, s, 1)
	}
}
