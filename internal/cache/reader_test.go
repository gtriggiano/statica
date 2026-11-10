package cache

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

// read reads and closes obj's body.
func read(t *testing.T, obj *storage.Object) string {
	t.Helper()
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return string(b)
}

func TestReaderGetServesPinnedSnapshots(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	h.origin.serve(objMeta(`"v1"`, "max-age=60", 5), "hello")
	ctx := WithRequest(context.Background(), httpcache.Request{})
	for i, want := range []Outcome{OutcomeMiss, OutcomeHit} {
		obj, err := r.Get(ctx, "site/a", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := obj.Body.(*Body); !ok || obj.Metadata.ETag != `"v1"` || read(t, obj) != "hello" {
			t.Fatalf("read %d: %T %+v", i, obj.Body, obj.Metadata)
		}
		if rec, _ := LastLookup(ctx, "site/a"); rec.Outcome != want {
			t.Fatalf("read %d recorded %+v", i, rec)
		}
		if err := obj.Body.Close(); err != nil {
			t.Fatal("Close is not idempotent:", err)
		}
	}
	if n := h.origin.getCount(); n != 1 {
		t.Fatalf("%d origin reads", n)
	}
	h.settle()
}

func TestReaderPresentsCurrentAge(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	m := objMeta(`"v1"`, "max-age=300", 5)
	m.RawAge, m.Age = "120", 120*time.Second
	h.origin.serve(m, "hello")
	for _, step := range []struct {
		advance time.Duration
		age     int64
	}{{0, 120}, {30*time.Second + 999*time.Millisecond, 150}, {time.Second, 151}} {
		h.clock.Advance(step.advance)
		obj, err := r.Get(context.Background(), "site/a", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		read(t, obj)
		if obj.Metadata.RawAge != strconv.FormatInt(step.age, 10) || obj.Metadata.Age != time.Duration(step.age)*time.Second || httpcache.Malformed(obj.Metadata) {
			t.Fatalf("presented %+v, want Age %d", obj.Metadata, step.age)
		}
		if obj.Metadata.CacheControl != "max-age=300" {
			t.Fatalf("a hit renewed the lifetime: %q", obj.Metadata.CacheControl)
		}
		if head, err := r.Head(context.Background(), "site/a"); err != nil || head.RawAge != obj.Metadata.RawAge || head.Age != obj.Metadata.Age {
			t.Fatalf("HEAD presented %+v, %v", head, err)
		}
	}
	h.settle()
}

func TestReaderPresentedAgeSaturates(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Cache) { c.TTL = 1 << 62 }))
	r := NewReader(h.c)
	m := objMeta(`"v1"`, "", 5)
	m.RawAge = "99999999999999"
	h.origin.serve(m, "hello")
	obj, err := r.Get(context.Background(), "site/a", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	read(t, obj)
	if obj.Metadata.RawAge != "2147483648" || obj.Metadata.Age != httpcache.MaxDelta*time.Second || httpcache.Malformed(obj.Metadata) {
		t.Fatalf("presented %+v", obj.Metadata)
	}
	h.settle()
}

func TestReaderStreamsKeepOriginMetadata(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	big := strings.Repeat("x", 4096)
	m := objMeta(`"big"`, "max-age=60", len(big))
	m.RawAge = "7"
	h.origin.serve(m, big)
	ctx := WithRequest(context.Background(), httpcache.Request{})
	obj, err := r.Get(ctx, "site/big", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := obj.Body.(*Body); ok || obj.Metadata != m || read(t, obj) != big {
		t.Fatalf("stream %T %+v", obj.Body, obj.Metadata)
	}
	if rec, _ := LastLookup(ctx, ""); rec.Outcome != OutcomeBypass || rec.Reason != ReasonTooLarge {
		t.Fatalf("recorded %+v", rec)
	}
	h.settle()
}

func TestReaderNegativeHitConfirmsAbsence(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
		return nil, originErr(storage.NotFound)
	})
	ctx := WithRequest(context.Background(), httpcache.Request{})
	for range 2 {
		if obj, err := r.Get(ctx, "site/missing", storage.ReadOptions{}); obj != nil || !storage.IsKind(err, storage.NotFound) {
			t.Fatalf("got %v, %v", obj, err)
		}
	}
	if _, err := r.Head(ctx, "site/missing"); !storage.IsKind(err, storage.NotFound) {
		t.Fatalf("HEAD after a negative entry: %v", err)
	}
	got := Lookups(ctx)
	if len(got) != 3 || got[0].Outcome != OutcomeMiss || got[1].Outcome != OutcomeNegativeHit || got[2].Outcome != OutcomeNegativeHit || h.origin.getCount() != 1 {
		t.Fatalf("records %+v, %d origin reads", got, h.origin.getCount())
	}
	h.settle()
}

func TestReaderRejectsReadOptions(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	for _, opts := range []storage.ReadOptions{
		{Range: "bytes=0-1"},
		{IfMatch: `"v"`},
		{IfNoneMatch: `"v"`},
	} {
		if obj, err := r.Get(context.Background(), "site/a", opts); obj != nil || !storage.IsKind(err, storage.Unsupported) || h.origin.getCount() != 0 {
			t.Fatalf("%+v: got %v, %v", opts, obj, err)
		}
	}
}

func TestReaderHeadReadsMetadataOnly(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	m := objMeta(`"v1"`, "max-age=60", 5)
	m.RawAge = "3"
	h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return m, nil })
	ctx := WithRequest(context.Background(), httpcache.Request{})
	got, err := r.Head(ctx, "site/a")
	if err != nil || got != m {
		t.Fatalf("cold HEAD %+v, %v", got, err)
	}
	h.clock.Advance(10 * time.Second)
	if got, err = r.Head(ctx, "site/a"); err != nil || got.RawAge != "13" {
		t.Fatalf("warm HEAD %+v, %v", got, err)
	}
	if gets, heads := h.origin.calls(); len(gets) != 0 || heads != 1 {
		t.Fatalf("origin calls: %d gets, %d heads", len(gets), heads)
	}
	if rs := Lookups(ctx); len(rs) != 2 || rs[0].Outcome != OutcomeMiss || rs[1].Outcome != OutcomeMetadataHit {
		t.Fatalf("records %+v", rs)
	}
	h.settle()
}

func TestReaderCarriesRequestDirectives(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.origin.setGet(func(ctx context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if opts.IfNoneMatch == `"v1"` {
			return nil, notModifiedErr(objMeta(`"v1"`, "max-age=60", 5))
		}
		return h.origin.object(ctx, objMeta(`"v1"`, "max-age=60", 5), "hello"), nil
	})
	for _, tc := range []struct {
		req    httpcache.Request
		want   Outcome
		reason Reason
	}{
		{httpcache.Request{}, OutcomeHit, ReasonNone},
		{httpcache.Request{NoStore: true}, OutcomeBypass, ReasonRequestNoStore},
		{httpcache.Request{NoCache: true}, OutcomeRevalidated, ReasonNone},
	} {
		ctx := WithRequest(context.Background(), tc.req)
		obj, err := r.Get(ctx, "site/a", storage.ReadOptions{})
		if err != nil || read(t, obj) != "hello" {
			t.Fatal(err)
		}
		if rec, ok := LastLookup(ctx, "site/a"); !ok || rec.Outcome != tc.want || rec.Reason != tc.reason {
			t.Fatalf("%+v recorded %+v", tc.req, rec)
		}
	}
	// Without a scope, lookups carry no directives and record nothing.
	obj, err := r.Get(context.Background(), "site/a", storage.ReadOptions{})
	if err != nil || read(t, obj) != "hello" {
		t.Fatal(err)
	}
	if rs, ok := LastLookup(context.Background(), ""); ok || Lookups(context.Background()) != nil {
		t.Fatalf("recorded without a scope: %+v", rs)
	}
	h.settle()
}

func TestBodyCloseRacesReads(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	data := strings.Repeat("0123456789", 100)
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", len(data)), data)
	for range 50 {
		obj, err := r.Get(context.Background(), "site/a", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		b := obj.Body.(*Body)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := make([]byte, 7)
				for {
					_, err := b.Read(p)
					if storage.IsKind(err, storage.Canceled) {
						return
					}
					if err != nil && err != io.EOF {
						t.Errorf("read: %v", err)
						return
					}
				}
			}()
		}
		wg.Add(2)
		for range 2 {
			go func() { defer wg.Done(); _ = b.Close() }()
		}
		wg.Wait()
		if _, err := b.Read(make([]byte, 1)); !storage.IsKind(err, storage.Canceled) {
			t.Fatalf("read after close: %v", err)
		}
	}
	h.settle()
}

func TestBodyBytes(t *testing.T) {
	h := newHarness(t)
	r := NewReader(h.c)
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 6), "abcdef")
	obj, err := r.Get(context.Background(), "site/a", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := obj.Body.(*Body)
	if _, err := b.Read(make([]byte, 4)); err != nil || string(b.Bytes()) != "abcdef" {
		t.Fatalf("Bytes after a read: %q, %v", b.Bytes(), err)
	}
	_ = obj.Body.Close()
	if b.Bytes() != nil {
		t.Fatal("Bytes after Close")
	}
	h.settle()
}

func TestFromConfig(t *testing.T) {
	parse := func(yaml string) *config.Config {
		cfg, err := config.Parse([]byte("s3:\n  bucket: b\n  region: eu-west-1\n  prefix: pub\n"+yaml), func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	origin := &fakeOrigin{t: t}
	if c, err := FromConfig(context.Background(), parse("cache:\n  enabled: false\n"), origin); c != nil || err != nil {
		t.Fatalf("disabled cache: %v, %v", c, err)
	}
	if _, err := FromConfig(context.Background(), nil, origin); err == nil {
		t.Fatal("nil configuration accepted")
	}
	cfg := parse("")
	cfg.S3.Features.ConditionalReads = false
	clock := &fakeClock{t: lookupT0}
	var events []FillEvent
	var mu sync.Mutex
	c, err := FromConfig(context.Background(), cfg, origin, func(o *Options) {
		o.Now = clock.Now
		o.OnFill = func(e FillEvent) { mu.Lock(); events = append(events, e); mu.Unlock() }
	})
	if err != nil || c == nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if c.prefix != "pub/" || c.conditional || c.store.limits != LimitsFrom(cfg.Cache) || c.cfg.MaxConcurrentFills != cfg.Cache.MaxConcurrentFills {
		t.Fatalf("cache %+v", c)
	}
	origin.serve(objMeta(`"v1"`, "", 5), "hello")
	l, err := c.Get(context.Background(), "pub/a", httpcache.Request{})
	if err != nil || l.Outcome != OutcomeMiss {
		t.Fatal(err)
	}
	l.Release()
	waitFor(t, "the fill event", func() bool { mu.Lock(); defer mu.Unlock(); return len(events) == 1 })
	cfg.Cache.MaxObjectBytes = cfg.Cache.MaxBytes + 1
	if _, err := FromConfig(context.Background(), cfg, origin); err == nil {
		t.Fatal("invalid limits accepted")
	}
}
