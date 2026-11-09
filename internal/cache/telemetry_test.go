package cache

import (
	"context"
	"io"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

func expirations(body, metadata, negative uint64) map[Kind]uint64 {
	return map[Kind]uint64{KindBody: body, KindMetadata: metadata, KindNegative: negative}
}

func TestMarkExpiredCountsEachVersionOnce(t *testing.T) {
	s := newTestStore(t, roomy)
	want := func(body, metadata, negative uint64) {
		t.Helper()
		check(t, s)
		if got := s.Stats().Expirations; !reflect.DeepEqual(got, expirations(body, metadata, negative)) {
			t.Fatalf("expirations %v, want body %d metadata %d negative %d", got, body, metadata, negative)
		}
	}
	want(0, 0, 0)
	admit(t, s, "b", textSnap("hello"), ResultAdmitted)
	h := acquire(t, s, "b")
	other := acquire(t, s, "b")
	if !h.MarkExpired() || h.MarkExpired() || other.MarkExpired() {
		t.Fatal("an entry version was counted other than once")
	}
	want(1, 0, 0)
	// A refreshed entry is a new version that may expire again.
	next := h.Snapshot()
	next.ResponseTime = time.Unix(1, 0)
	nh, res := s.Refresh(h, next)
	if res != ResultRefreshed || !nh.MarkExpired() {
		t.Fatalf("Refresh = %s, or its new version was not counted", res)
	}
	want(2, 0, 0)
	release(t, s, h, other, nh)

	// A retired entry a lookup still pins counts; so does each kind.
	admit(t, s, "m", Snapshot{Kind: KindMetadata, Metadata: storage.Metadata{Size: 3}}, ResultAdmitted)
	admit(t, s, "n", Snapshot{Kind: KindNegative}, ResultAdmitted)
	hm, hn := acquire(t, s, "m"), acquire(t, s, "n")
	remove(t, s, "m")
	if !hm.MarkExpired() || !hn.MarkExpired() {
		t.Fatal("retired metadata or indexed negative entry not counted")
	}
	want(2, 1, 1)
	release(t, s, hm, hn)

	// A detached entry is never counted.
	d, res := s.Promote("b", Absent, reserve(t, s, 5), textSnap("other"))
	if res != ResultConflict || d.MarkExpired() {
		t.Fatalf("Promote = %s, or a detached entry was counted", res)
	}
	want(2, 1, 1)
	release(t, s, d)
}

func TestExpirationsByKind(t *testing.T) {
	h := newHarness(t)
	want := func(body, metadata, negative uint64) {
		t.Helper()
		if got := h.store.Stats().Expirations; !reflect.DeepEqual(got, expirations(body, metadata, negative)) {
			t.Fatalf("expirations %v, want body %d metadata %d negative %d", got, body, metadata, negative)
		}
	}
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.origin.setGet(func(ctx context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if opts.IfNoneMatch == `"v1"` {
			return nil, notModifiedErr(objMeta(`"v1"`, "max-age=60", 5))
		}
		return h.origin.object(ctx, storage.Metadata{Size: 2, ETag: `"v1"`, ContentRange: "bytes 0-1/5"}, "he"), nil
	})
	// Requests forcing revalidation of a fresh entry expire nothing.
	for _, req := range []httpcache.Request{{}, {NoCache: true}, {MaxAge: httpcache.Delta{Present: true}}} {
		l, err := h.get("site/a", req)
		if err != nil {
			t.Fatal(err)
		}
		l.Release()
	}
	want(0, 0, 0)
	// The first lookup to find the entry past its lifetime counts it, whatever
	// the lookup; later lookups of the same version do not.
	h.clock.Advance(2 * time.Minute)
	l, err := h.c.Range(context.Background(), "site/a", httpcache.Request{}, storage.ReadOptions{Range: "bytes=0-1"})
	if err != nil || l.Outcome != OutcomeBypass || l.Reason != ReasonRange {
		t.Fatalf("Range of an expired body = %s/%s, %v", l.Outcome, l.Reason, err)
	}
	l.Release()
	want(1, 0, 0)
	l = h.mustGet("site/a", httpcache.Request{NoCache: true}, OutcomeRevalidated)
	l.Release()
	want(1, 0, 0)
	h.clock.Advance(2 * time.Minute) // the refreshed version expires in turn
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeRevalidated)
	l.Release()
	want(2, 0, 0)

	h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return objMeta(`"m"`, "max-age=60", 7), nil })
	for range 2 {
		l, err = h.c.Head(context.Background(), "site/m", httpcache.Request{})
		if err != nil {
			t.Fatal(err)
		}
		l.Release()
	}
	h.clock.Advance(2 * time.Minute)
	if l, err = h.c.Head(context.Background(), "site/m", httpcache.Request{}); err != nil || l.Outcome != OutcomeMiss {
		t.Fatalf("HEAD of expired metadata = %s, %v", l.Outcome, err)
	}
	l.Release()
	want(2, 1, 0)

	h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
		return nil, originErr(storage.NotFound)
	})
	for range 2 {
		if l, err = h.get("site/n", httpcache.Request{}); !storage.IsKind(err, storage.NotFound) {
			t.Fatal(err)
		}
	}
	want(2, 1, 0)
	h.clock.Advance(6 * time.Second) // past negative_ttl
	if l, err = h.get("site/n", httpcache.Request{}); !storage.IsKind(err, storage.NotFound) || l.Outcome != OutcomeMiss {
		t.Fatalf("GET of an expired negative = %s, %v", l.Outcome, err)
	}
	want(2, 1, 1)
	h.settle()
}

func TestCoalescedLookups(t *testing.T) {
	const n = 6
	h := newHarness(t)
	r := NewReader(h.c)
	gate := make(chan struct{})
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return h.origin.object(ctx, objMeta(`"v1"`, "max-age=60", 5), "hello"), nil
	})
	records := make(chan Record, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			ctx := WithRequest(context.Background(), httpcache.Request{})
			obj, err := r.Get(ctx, "site/a", storage.ReadOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			read(t, obj)
			rec, _ := LastLookup(ctx, "site/a")
			records <- rec
		})
	}
	h.waiters(n)
	close(gate)
	wg.Wait()
	close(records)
	coalesced := 0
	for rec := range records {
		if rec.Outcome != OutcomeMiss {
			t.Fatalf("record %+v: the outcome is the flight's", rec)
		}
		if rec.Coalesced {
			coalesced++
		}
	}
	if coalesced != n-1 || h.c.Stats().Coalesced != n-1 || h.origin.getCount() != 1 {
		t.Fatalf("%d coalesced records, stats %+v, %d origin reads", coalesced, h.c.Stats(), h.origin.getCount())
	}
	// Hits, HEADs and direct reads never join a flight.
	ctx := WithRequest(context.Background(), httpcache.Request{})
	obj, err := r.Get(ctx, "site/a", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	read(t, obj)
	if _, err := r.Head(ctx, "site/a"); err != nil {
		t.Fatal(err)
	}
	for _, rec := range Lookups(ctx) {
		if rec.Coalesced {
			t.Fatalf("record %+v", rec)
		}
	}
	h.settle()
}

// TestOriginPurpose checks the purpose each origin read carries: a flight's
// reads, fills and revalidations alike, are cache_fill whatever the request's
// own purpose; direct, bypass, range and HEAD reads keep the request's.
func TestOriginPurpose(t *testing.T) {
	if got := storage.PurposeFrom(context.Background()); got != storage.PurposeRequest {
		t.Fatalf("default purpose %q", got)
	}
	h := newHarness(t, withConfig(func(c *config.Cache) {
		off := false
		c.Rules = []config.CacheRule{{Match: config.Match{Prefix: "/private/"}, Enabled: &off}}
	}))
	var mu sync.Mutex
	var seen []string
	note := func(ctx context.Context, what string) {
		mu.Lock()
		seen = append(seen, what+" "+string(storage.PurposeFrom(ctx)))
		mu.Unlock()
	}
	take := func() []string {
		mu.Lock()
		defer mu.Unlock()
		s := seen
		seen = nil
		return s
	}
	etag := `"v1"`
	h.origin.setGet(func(ctx context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		switch {
		case opts.IfNoneMatch != "":
			note(ctx, "conditional")
			if opts.IfNoneMatch == etag {
				return nil, notModifiedErr(objMeta(etag, "max-age=60", 5))
			}
			return nil, notModifiedErr(objMeta(`"v2"`, "max-age=60", 5)) // the tag changed: refetch
		case opts.Range != "":
			note(ctx, "range")
			return h.origin.object(ctx, storage.Metadata{Size: 2, ETag: `"v1"`, ContentRange: "bytes 0-1/5"}, "he"), nil
		}
		note(ctx, "get")
		return h.origin.object(ctx, objMeta(`"v1"`, "max-age=60", 5), "hello"), nil
	})
	h.origin.setHead(func(ctx context.Context, _ string) (storage.Metadata, error) {
		note(ctx, "head")
		return objMeta(`"v1"`, "max-age=60", 5), nil
	})
	health := storage.WithPurpose(context.Background(), storage.PurposeHealth)
	for _, step := range []struct {
		name string
		run  func() (*Lookup, error)
		want []string
	}{
		{"miss fills", func() (*Lookup, error) { return h.c.Get(health, "site/a", httpcache.Request{}) }, []string{"get cache_fill"}},
		{"revalidation", func() (*Lookup, error) { return h.c.Get(health, "site/a", httpcache.Request{NoCache: true}) }, []string{"conditional cache_fill"}},
		{"etag changed refetch", func() (*Lookup, error) {
			etag = `"other"`
			return h.c.Get(context.Background(), "site/a", httpcache.Request{NoCache: true})
		}, []string{"conditional cache_fill", "get cache_fill"}},
		{"request no-store", func() (*Lookup, error) {
			return h.c.Get(context.Background(), "site/a", httpcache.Request{NoStore: true})
		}, []string{"get request"}},
		{"disabled policy", func() (*Lookup, error) { return h.c.Get(health, "site/private/a", httpcache.Request{}) }, []string{"get health"}},
		{"range", func() (*Lookup, error) {
			return h.c.Range(context.Background(), "site/cold", httpcache.Request{}, storage.ReadOptions{Range: "bytes=0-1"})
		}, []string{"range request"}},
		{"head", func() (*Lookup, error) { return h.c.Head(context.Background(), "site/h", httpcache.Request{}) }, []string{"head request"}},
		{"head bypass", func() (*Lookup, error) { return h.c.Head(health, "site/h", httpcache.Request{NoStore: true}) }, []string{"head health"}},
	} {
		l, err := step.run()
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if obj := l.TakeObject(); obj != nil {
			read(t, obj)
		}
		l.Release()
		if got := take(); !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s: origin saw %q, want %q", step.name, got, step.want)
		}
	}
	h.settle()
}

func TestFreshnessExpired(t *testing.T) {
	for _, tc := range []struct {
		f    Freshness
		want bool
	}{
		{Freshness{State: StateStale, Age: time.Minute, Lifetime: time.Minute}, true},
		{Freshness{State: StateStale, Age: 2 * time.Minute, Lifetime: time.Minute}, true},
		{Freshness{State: StateStale, Age: 30 * time.Second, Lifetime: time.Minute}, false}, // forced revalidation
		{Freshness{State: StateStale, Age: time.Minute}, false},                             // zero lifetime: always revalidate
		{Freshness{State: StateStale}, false},
		{Freshness{State: StateFresh, Age: time.Second, Lifetime: time.Minute, Remaining: 59 * time.Second}, false},
		{Freshness{State: StateBypass}, false},
	} {
		if got := tc.f.Expired(); got != tc.want {
			t.Errorf("%+v.Expired() = %t, want %t", tc.f, got, tc.want)
		}
	}
}

// TestZeroLifetimeNeverExpires checks that entries revalidating on every
// reuse by policy, an origin no-cache and a zero rule TTL, are not counted as
// expirations however old they get.
func TestZeroLifetimeNeverExpires(t *testing.T) {
	zero := time.Duration(0)
	h := newHarness(t, withConfig(func(c *config.Cache) {
		c.Rules = []config.CacheRule{{Match: config.Match{Prefix: "/zero/"}, TTL: &zero}}
	}))
	cacheControl := map[string]string{"site/nc": "no-cache", "site/zero/a": "max-age=600"}
	for key, cc := range cacheControl {
		h.prime(key, objMeta(`"v1"`, cc, 5), "hello")
	}
	h.origin.setGet(func(_ context.Context, key string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if opts.IfNoneMatch != `"v1"` {
			t.Errorf("unconditional read of %s", key)
		}
		return nil, notModifiedErr(objMeta(`"v1"`, cacheControl[key], 5))
	})
	for range 3 {
		h.clock.Advance(time.Hour)
		for key := range cacheControl {
			l := h.mustGet(key, httpcache.Request{}, OutcomeRevalidated)
			l.Release()
		}
	}
	if got := h.store.Stats().Expirations; !reflect.DeepEqual(got, expirations(0, 0, 0)) {
		t.Fatalf("expirations %v", got)
	}
	if st := h.c.Stats(); st.Revalidations[RevalidationNotModified] != 6 {
		t.Fatalf("stats %+v", st)
	}
	h.settle()
}

// TestLateNotFoundIsMissing checks that a revalidation names the origin's
// answer even when the flight could not apply it: a NotFound arriving after
// fill_timeout is RevalidationMissing with FillTimeout, and the entry stays.
func TestLateNotFoundIsMissing(t *testing.T) {
	t.Parallel() // each harness is self-contained; the fill timeout is wall time
	h := newHarness(t, withConfig(func(c *config.Cache) { c.FillTimeout = shortFill }))
	v := h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(70 * time.Second)
	h.origin.setGet(conditional(t, func(ctx context.Context) (*storage.Object, error) {
		<-ctx.Done() // answers after the fill deadline
		return nil, originErr(storage.NotFound)
	}))
	l, err := h.get("site/a", httpcache.Request{})
	if !storage.IsKind(err, storage.Timeout) || l.Outcome != OutcomeMiss {
		t.Fatalf("got %s, %v", l.Outcome, err)
	}
	l.Release()
	h.settle()
	st := h.c.Stats()
	if got, kind := h.peek("site/a"); got != v || kind != KindBody || st.Revalidations[RevalidationMissing] != 1 || st.Revalidations[RevalidationError] != 0 || st.Fills[FillTimeout] != 1 {
		t.Fatalf("entry %s (unchanged %t), stats %+v", kind, got == v, st)
	}
}

func TestETagChangedRefetchNotFoundIsMissing(t *testing.T) {
	h := newHarness(t)
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(2 * time.Minute)
	h.origin.setGet(func(_ context.Context, _ string, n int, _ storage.ReadOptions) (*storage.Object, error) {
		if n == 2 {
			return nil, notModifiedErr(storage.Metadata{ETag: `"v2"`})
		}
		return nil, originErr(storage.NotFound)
	})
	l, err := h.get("site/a", httpcache.Request{})
	if !storage.IsKind(err, storage.NotFound) || l.Outcome != OutcomeMiss {
		t.Fatalf("got %s, %v", l.Outcome, err)
	}
	l.Release()
	h.settle()
	gets, _ := h.origin.calls()
	st := h.c.Stats()
	if _, kind := h.peek("site/a"); kind != KindNegative || len(gets) != 3 || st.Revalidations[RevalidationMissing] != 1 || st.Revalidations[RevalidationETagChanged] != 0 || st.Fills[FillNotFound] != 1 {
		t.Fatalf("indexed %q after %d reads, stats %+v", kind, len(gets), st)
	}
}

// taggedBody records the purposes a body is retagged with, and whether a
// retag came after a read.
type taggedBody struct {
	io.ReadCloser
	mu       sync.Mutex
	purposes []storage.Purpose
	read     bool
	late     bool
}

func (b *taggedBody) SetPurpose(p storage.Purpose) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purposes = append(b.purposes, p)
	b.late = b.late || b.read
}

func (b *taggedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.read = true
	b.mu.Unlock()
	return b.ReadCloser.Read(p)
}

// TestHandOffRetagsStreamBytes checks the purpose rule: the flight's origin
// GET is a cache_fill operation, the bytes of a stream it hands to a request
// are retagged request before anything reads them, and a buffered fill keeps
// its bytes cache_fill.
func TestHandOffRetagsStreamBytes(t *testing.T) {
	big := string(make([]byte, 4096))
	unknown := objMeta(`"v1"`, "max-age=60", 5)
	unknown.Size = -1
	for _, tc := range []struct {
		name string
		meta storage.Metadata
		data string
		want []storage.Purpose
	}{
		{"buffered", objMeta(`"v1"`, "max-age=60", 5), "hello", nil},
		{"too_large", objMeta(`"v1"`, "max-age=60", len(big)), big, []storage.Purpose{storage.PurposeRequest}},
		{"uncacheable", objMeta(`"v1"`, "no-store", 5), "hello", []storage.Purpose{storage.PurposeRequest}},
		{"unknown_size", unknown, "hello", []storage.Purpose{storage.PurposeRequest}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var body *taggedBody
			var operation storage.Purpose
			h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
				operation = storage.PurposeFrom(ctx)
				obj := h.origin.object(ctx, tc.meta, tc.data)
				body = &taggedBody{ReadCloser: obj.Body}
				obj.Body = body
				return obj, nil
			})
			l, err := h.get("site/a", httpcache.Request{})
			if err != nil || content(t, l) != tc.data {
				t.Fatalf("got %s, %v", l.Outcome, err)
			}
			l.Release()
			h.settle()
			body.mu.Lock()
			defer body.mu.Unlock()
			if operation != storage.PurposeCacheFill || !slices.Equal(body.purposes, tc.want) || body.late {
				t.Fatalf("operation %q, body retagged %q (late %t), want %q", operation, body.purposes, body.late, tc.want)
			}
		})
	}
}
