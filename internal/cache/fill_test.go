package cache

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

func staleWindow(c *config.Cache) { c.StaleIfError = time.Minute }

// conditional answers the revalidation of v1 and fails any other read.
func conditional(t *testing.T, answer func(ctx context.Context) (*storage.Object, error)) getFunc {
	return func(ctx context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if opts.IfNoneMatch != `"v1"` {
			t.Errorf("If-None-Match %q", opts.IfNoneMatch)
		}
		return answer(ctx)
	}
}

func TestRevalidationNotModifiedRefreshesInPlace(t *testing.T) {
	h := newHarness(t)
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	old := h.mustGet("site/a", httpcache.Request{}, OutcomeHit) // pins the bytes being refreshed
	h.clock.Advance(2 * time.Minute)
	now := h.clock.Now()
	h.origin.setGet(conditional(t, func(context.Context) (*storage.Object, error) {
		return nil, notModifiedErr(storage.Metadata{ETag: `"v1"`, CacheControl: "max-age=600"})
	}))
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeRevalidated)
	snap := l.Handle().Snapshot()
	if content(t, l) != "hello" || &snap.Body[0] != &old.Handle().Snapshot().Body[0] {
		t.Fatal("revalidation did not keep the stored bytes")
	}
	if snap.Metadata.CacheControl != "max-age=600" || l.Metadata.CacheControl != "max-age=600" || !snap.RequestTime.Equal(now) || !snap.ResponseTime.Equal(now) {
		t.Fatalf("refreshed snapshot %+v", snap)
	}
	releaseAll(l, old)
	hit := h.mustGet("site/a", httpcache.Request{}, OutcomeHit)
	hit.Release()
	if st := h.c.Stats(); st.Fills[FillRefreshed] != 1 || st.Revalidations[RevalidationNotModified] != 1 || h.origin.getCount() != 2 {
		t.Fatalf("stats %+v", st)
	}
	h.settle()
}

func TestLateNotModifiedIsAFillTimeout(t *testing.T) {
	t.Parallel() // each harness is self-contained; the fill timeout is wall time
	for _, window := range []time.Duration{0, time.Minute} {
		h := newHarness(t, withConfig(func(c *config.Cache) { c.FillTimeout = shortFill; c.StaleIfError = window }))
		v := h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
		h.clock.Advance(70 * time.Second)
		h.origin.setGet(conditional(t, func(ctx context.Context) (*storage.Object, error) {
			<-ctx.Done() // answers after the fill deadline
			return nil, notModifiedErr(storage.Metadata{ETag: `"v1"`})
		}))
		l, err := h.get("site/a", httpcache.Request{})
		switch {
		case window == 0 && (!storage.IsKind(err, storage.Timeout) || l.Outcome != OutcomeMiss):
			t.Fatalf("got %s, %v", l.Outcome, err)
		case window > 0 && (err != nil || l.Outcome != OutcomeStale):
			t.Fatalf("with stale_if_error got %s, %v", l.Outcome, err)
		}
		l.Release()
		if got, _ := h.peek("site/a"); got != v || h.c.Stats().Fills[FillTimeout] != 1 {
			t.Fatalf("a late 304 refreshed the entry; stats %+v", h.c.Stats())
		}
		h.settle()
	}
}

func TestRevalidationETagChangedRefetchesOnce(t *testing.T) {
	h := newHarness(t)
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(2 * time.Minute)
	h.origin.setGet(func(ctx context.Context, _ string, n int, _ storage.ReadOptions) (*storage.Object, error) {
		if n == 2 {
			return nil, notModifiedErr(storage.Metadata{ETag: `"v2"`})
		}
		return h.origin.object(ctx, objMeta(`"v2"`, "max-age=60", 5), "world"), nil
	})
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeMiss)
	if content(t, l) != "world" || l.Metadata.ETag != `"v2"` {
		t.Fatal("refetch not served")
	}
	l.Release()
	gets, _ := h.origin.calls()
	if !slices.Equal(gets, []storage.ReadOptions{{}, {IfNoneMatch: `"v1"`}, {}}) || h.c.Stats().Revalidations[RevalidationETagChanged] != 1 {
		t.Fatalf("origin reads %+v, stats %+v", gets, h.c.Stats())
	}
	h.settle()
}

func TestRevalidationOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(h *harness, ctx context.Context) (*storage.Object, error)
		err    storage.Kind
		kind   Kind // indexed afterwards; empty for none
		result RevalidationResult
	}{
		{"modified", func(h *harness, ctx context.Context) (*storage.Object, error) {
			return h.origin.object(ctx, objMeta(`"v2"`, "max-age=60", 5), "world"), nil
		}, "", KindBody, RevalidationModified},
		{"modified-streams", func(h *harness, ctx context.Context) (*storage.Object, error) {
			return h.origin.object(ctx, objMeta(`"v2"`, "no-store", 5), "world"), nil
		}, "", "", RevalidationModified},
		{"not-found", func(*harness, context.Context) (*storage.Object, error) { return nil, originErr(storage.NotFound) }, storage.NotFound, KindNegative, RevalidationMissing},
		{"denied", func(*harness, context.Context) (*storage.Object, error) { return nil, originErr(storage.Denied) }, storage.Denied, "", RevalidationError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withConfig(staleWindow))
			v := h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
			h.clock.Advance(70 * time.Second) // inside stale_if_error, which these never use
			h.origin.setGet(conditional(t, func(ctx context.Context) (*storage.Object, error) { return tc.answer(h, ctx) }))
			l, err := h.get("site/a", httpcache.Request{})
			switch {
			case tc.err != "" && (!storage.IsKind(err, tc.err) || l.Outcome != OutcomeMiss):
				t.Fatalf("got %s, %v; want %s", l.Outcome, err, tc.err)
			case tc.err == "" && (err != nil || content(t, l) != "world"):
				t.Fatalf("got %s, %v", l.Outcome, err)
			}
			l.Release()
			if got, kind := h.peek("site/a"); kind != tc.kind || got == v {
				t.Fatalf("indexed %q (version changed: %t), want %q", kind, got != v, tc.kind)
			}
			if h.c.Stats().Revalidations[tc.result] != 1 {
				t.Fatalf("stats %+v", h.c.Stats())
			}
			h.settle()
		})
	}
}

func TestRevalidationOutageServesStaleWithinWindow(t *testing.T) {
	for _, k := range []storage.Kind{storage.Unavailable, storage.Timeout, storage.Throttled} {
		t.Run(string(k), func(t *testing.T) {
			h := newHarness(t, withConfig(staleWindow))
			v := h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
			h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
				return nil, originErr(k)
			})
			h.clock.Advance(70 * time.Second)
			for range 2 { // serving stale never renews the entry
				l := h.mustGet("site/a", httpcache.Request{}, OutcomeStale)
				if content(t, l) != "hello" {
					t.Fatal("wrong stale body")
				}
				l.Release()
				if got, _ := h.peek("site/a"); got != v {
					t.Fatal("stale service changed the entry")
				}
			}
			h.clock.Advance(time.Minute) // staleness 70s > stale_if_error
			l, err := h.get("site/a", httpcache.Request{})
			if !storage.IsKind(err, k) || l.Outcome != OutcomeMiss || h.origin.getCount() != 4 {
				t.Fatalf("outside the window got %s, %v", l.Outcome, err)
			}
			h.settle()
		})
	}
}

func TestStaleIfErrorIsDecidedPerWaiter(t *testing.T) {
	h := newHarness(t, withConfig(staleWindow))
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(70 * time.Second)
	gate := make(chan struct{})
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return nil, originErr(storage.Unavailable)
	})
	plain := h.goGet(context.Background(), "site/a", httpcache.Request{})
	h.waiters(1)
	forced := h.goGet(context.Background(), "site/a", httpcache.Request{NoCache: true})
	h.waiters(2)
	close(gate)
	g := <-plain
	if g.err != nil || g.l.Outcome != OutcomeStale || content(t, g.l) != "hello" {
		t.Fatalf("plain waiter got %s, %v", g.l.Outcome, g.err)
	}
	g.l.Release()
	if g := <-forced; !storage.IsKind(g.err, storage.Unavailable) || g.l.Outcome != OutcomeMiss {
		t.Fatalf("no-cache waiter got %s, %v", g.l.Outcome, g.err)
	}
	if h.origin.getCount() != 2 {
		t.Fatal("the waiters did not share one revalidation")
	}
	h.settle()
}

func TestConditionalReadsDisabledRefetches(t *testing.T) {
	h := newHarness(t, func(_ *config.Cache, o *Options) { o.ConditionalReads = false })
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(2 * time.Minute)
	h.origin.serve(objMeta(`"v1"`, "max-age=60", 5), "hello")
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeMiss)
	l.Release()
	gets, _ := h.origin.calls()
	if !slices.Equal(gets, []storage.ReadOptions{{}, {}}) || h.c.Stats().Revalidations[RevalidationNotModified] != 0 {
		t.Fatalf("origin reads %+v", gets)
	}
	h.settle()
}

func TestRevalidationMergedNoStoreRemovesEntry(t *testing.T) {
	h := newHarness(t)
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(2 * time.Minute)
	h.origin.setGet(conditional(t, func(context.Context) (*storage.Object, error) {
		return nil, notModifiedErr(storage.Metadata{ETag: `"v1"`, CacheControl: "no-store"})
	}))
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeRevalidated)
	if content(t, l) != "hello" || l.Metadata.CacheControl != "no-store" || l.Handle().Snapshot().Metadata.CacheControl != "max-age=60" {
		t.Fatalf("served %+v", l.Metadata)
	}
	l.Release()
	if _, kind := h.peek("site/a"); kind != "" || h.c.Stats().Fills[FillUnstored] != 1 {
		t.Fatalf("indexed %q, stats %+v", kind, h.c.Stats())
	}
	h.settle()
}

func TestFillRacesWithOtherWriters(t *testing.T) {
	stamped := func(s Snapshot) Snapshot {
		s.RequestTime, s.ResponseTime = lookupT0, lookupT0
		return s
	}
	for _, tc := range []struct {
		name     string
		write    func(h *harness)
		promoted bool // the fill is indexed rather than served detached
	}{
		{"head-metadata", func(h *harness) {
			h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return objMeta(`"v1"`, "", 5), nil })
			l, err := h.c.Head(context.Background(), "site/a", httpcache.Request{})
			if err != nil || l.Outcome != OutcomeMiss {
				h.t.Errorf("HEAD got %s, %v", l.Outcome, err)
			}
			l.Release()
		}, true},
		{"body", func(h *harness) {
			h.store.Admit("site/a", Absent, stamped(textSnap("other")))
		}, false},
		{"negative", func(h *harness) {
			h.store.Admit("site/a", Absent, stamped(Snapshot{Kind: KindNegative}))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			gate := make(chan struct{})
			h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
				if err := waitGate(ctx, gate); err != nil {
					return nil, err
				}
				return h.origin.object(ctx, objMeta(`"v1"`, "", 5), "hello"), nil
			})
			ch := h.goGet(context.Background(), "site/a", httpcache.Request{})
			h.waiters(1)
			tc.write(h)
			written, _ := h.peek("site/a")
			close(gate)
			g := <-ch
			if g.err != nil || g.l.Outcome != OutcomeMiss || content(t, g.l) != "hello" {
				t.Fatalf("got %s, %v", g.l.Outcome, g.err)
			}
			got, kind := h.peek("site/a")
			detached := h.store.Stats().DetachedEntries
			switch {
			case tc.promoted && (kind != KindBody || detached != 0 || h.c.Stats().Fills[FillAdmitted] != 1):
				t.Fatalf("indexed %q, %d detached", kind, detached)
			case !tc.promoted && (got != written || detached != 1 || h.c.Stats().Fills[FillDetached] != 1):
				t.Fatalf("the concurrent write was overwritten, %d detached", detached)
			}
			g.l.Release()
			h.settle()
		})
	}
}

func TestHeadRevalidation(t *testing.T) {
	stale := func(t *testing.T, mutate ...func(*config.Cache, *Options)) *harness {
		h := newHarness(t, mutate...)
		h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
		h.clock.Advance(70 * time.Second)
		return h
	}
	head := func(h *harness) (*Lookup, error) {
		h.t.Helper()
		l, err := h.c.Head(context.Background(), "site/a", httpcache.Request{})
		if l == nil {
			h.t.Fatal("nil Lookup")
		}
		return l, err
	}
	answer := func(m storage.Metadata, err error) func(context.Context, string) (storage.Metadata, error) {
		return func(context.Context, string) (storage.Metadata, error) { return m, err }
	}
	t.Run("same-strong-etag-refreshes", func(t *testing.T) {
		h := stale(t)
		h.origin.setHead(answer(objMeta(`"v1"`, "max-age=600", 5), nil))
		l, err := head(h)
		if err != nil || l.Outcome != OutcomeRevalidated || l.Handle() == nil || !l.Handle().Snapshot().ResponseTime.Equal(h.clock.Now()) {
			t.Fatalf("got %s, %v", l.Outcome, err)
		}
		l.Release()
		hit := h.mustGet("site/a", httpcache.Request{}, OutcomeHit)
		hit.Release()
		h.settle()
	})
	t.Run("weak-or-other-etag-stores-metadata", func(t *testing.T) {
		for _, etag := range []string{`"v2"`, `W/"v1"`} {
			h := stale(t)
			h.origin.setHead(answer(objMeta(etag, "", 5), nil))
			l, err := head(h)
			if err != nil || l.Outcome != OutcomeMiss || l.Metadata.ETag != etag {
				t.Fatalf("got %s, %v", l.Outcome, err)
			}
			l.Release()
			if _, kind := h.peek("site/a"); kind != KindMetadata {
				t.Fatalf("%s: indexed %q", etag, kind)
			}
			h.settle()
		}
	})
	t.Run("uncacheable-removes", func(t *testing.T) {
		h := stale(t)
		h.origin.setHead(answer(objMeta(`"v1"`, "private", 5), nil))
		l, err := head(h)
		if err != nil || l.Outcome != OutcomeBypass || l.Reason != ReasonUncacheable || l.Metadata.CacheControl != "private" {
			t.Fatalf("got %s/%s, %v", l.Outcome, l.Reason, err)
		}
		if _, kind := h.peek("site/a"); kind != "" {
			t.Fatalf("indexed %q", kind)
		}
		h.settle()
	})
	t.Run("denied-removes-without-stale", func(t *testing.T) {
		h := stale(t, withConfig(staleWindow))
		h.origin.setHead(answer(storage.Metadata{}, originErr(storage.Denied)))
		if l, err := head(h); !storage.IsKind(err, storage.Denied) || l.Outcome != OutcomeMiss {
			t.Fatalf("got %s, %v", l.Outcome, err)
		}
		if _, kind := h.peek("site/a"); kind != "" {
			t.Fatalf("indexed %q", kind)
		}
		h.settle()
	})
	t.Run("ambiguous-changes-nothing", func(t *testing.T) {
		h := stale(t, withConfig(staleWindow))
		v, _ := h.peek("site/a")
		h.origin.setHead(answer(storage.Metadata{}, originErr(storage.AmbiguousNotFound)))
		if l, err := head(h); !storage.IsKind(err, storage.AmbiguousNotFound) || l.Outcome != OutcomeMiss {
			t.Fatalf("got %s, %v", l.Outcome, err)
		}
		if got, kind := h.peek("site/a"); got != v || kind != KindBody {
			t.Fatal("an ambiguous absence changed the entry")
		}
		h.settle()
	})
	t.Run("confirmed-absence-stores-negative", func(t *testing.T) {
		h := stale(t)
		h.origin.setHead(answer(storage.Metadata{}, originErr(storage.NotFound)))
		if _, err := head(h); !storage.IsKind(err, storage.NotFound) {
			t.Fatal(err)
		}
		if _, kind := h.peek("site/a"); kind != KindNegative {
			t.Fatalf("indexed %q", kind)
		}
		h.settle()
	})
	t.Run("outage-serves-stale", func(t *testing.T) {
		h := stale(t, withConfig(staleWindow))
		h.origin.setHead(answer(storage.Metadata{}, originErr(storage.Unavailable)))
		l, err := head(h)
		if err != nil || l.Outcome != OutcomeStale || l.Handle() == nil || l.Metadata.ETag != `"v1"` {
			t.Fatalf("got %s, %v", l.Outcome, err)
		}
		l.Release()
		l, err = h.c.Head(context.Background(), "site/a", httpcache.Request{NoCache: true})
		if !storage.IsKind(err, storage.Unavailable) || l.Handle() != nil {
			t.Fatalf("no-cache HEAD got %s, %v", l.Outcome, err)
		}
		h.settle()
	})
}

// TestStressRandomBehaviours drives random keys, directives, origin answers,
// cancellations and clock steps through tight limits, checking every served
// snapshot against its metadata and, at the end, that nothing leaked.
func TestStressRandomBehaviours(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Cache) {
		*c = config.Cache{Enabled: true, MaxBytes: 12 << 10, MaxEntries: 8, MaxObjectBytes: 1 << 10, MaxFillBytes: 3 << 10,
			MaxConcurrentFills: 3, MaxWaitersPerKey: 4, FillTimeout: 30 * time.Millisecond, TTL: 2 * time.Second, NegativeTTL: time.Second, StaleIfError: 5 * time.Second}
	}))
	fillByte := func(etag string) byte { return etag[1] }
	payload := func(etag string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = fillByte(etag)
		}
		return string(b)
	}
	etags := []string{`"a"`, `"b"`, `"c"`}
	errs := []storage.Kind{storage.NotFound, storage.Denied, storage.Unavailable, storage.Timeout, storage.Throttled, storage.AmbiguousNotFound}
	pause := func(ctx context.Context) error {
		if rand.IntN(3) > 0 {
			return nil
		}
		select {
		case <-time.After(time.Duration(rand.IntN(5)) * time.Millisecond):
			return nil
		case <-ctx.Done():
			return originErr(storage.Canceled)
		}
	}
	h.origin.setGet(func(ctx context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if err := pause(ctx); err != nil {
			return nil, err
		}
		if rand.IntN(5) == 0 {
			return nil, originErr(errs[rand.IntN(len(errs))])
		}
		etag := etags[rand.IntN(len(etags))]
		if opts.IfNoneMatch != "" && rand.IntN(2) == 0 {
			if rand.IntN(4) > 0 {
				etag = opts.IfNoneMatch
			}
			return nil, notModifiedErr(storage.Metadata{ETag: etag, CacheControl: []string{"", "max-age=1", "no-store"}[rand.IntN(3)]})
		}
		size := []int{0, 10, 300, 1000, 1500}[rand.IntN(5)]
		m := objMeta(etag, []string{"", "", "max-age=1", "no-store", "no-cache"}[rand.IntN(5)], size)
		var opt []func(*fakeBody)
		switch rand.IntN(8) {
		case 0:
			m.Size = -1
		case 1:
			opt = append(opt, slow(256, time.Duration(rand.IntN(4))*time.Millisecond))
		case 2:
			m.Checksum = storage.ChecksumPending
			opt = append(opt, ending(originErr(storage.ChecksumMismatch)))
		case 3:
			m.Size++ // truncated
		}
		return h.origin.object(ctx, m, payload(etag, size), opt...), nil
	})
	h.origin.setHead(func(ctx context.Context, _ string) (storage.Metadata, error) {
		if err := pause(ctx); err != nil {
			return storage.Metadata{}, err
		}
		if rand.IntN(4) == 0 {
			return storage.Metadata{}, originErr(errs[rand.IntN(len(errs))])
		}
		return objMeta(etags[rand.IntN(len(etags))], []string{"", "max-age=1", "private"}[rand.IntN(3)], []int{10, 1000, 5000}[rand.IntN(3)]), nil
	})
	verify := func(l *Lookup) error {
		if hd := l.Handle(); hd != nil {
			s := hd.Snapshot()
			if s.Kind == KindBody && (int64(len(s.Body)) != s.Metadata.Size || s.Body != nil && len(s.Body) > 0 && s.Body[0] != fillByte(s.Metadata.ETag)) {
				return fmt.Errorf("snapshot bytes disagree with metadata %+v", s.Metadata)
			}
		}
		if o := l.TakeObject(); o != nil {
			defer o.Body.Close()
			if rand.IntN(3) == 0 {
				return nil // abandoned mid-stream
			}
			b, err := io.ReadAll(o.Body)
			if err == nil && len(b) > 0 && b[0] != fillByte(o.Metadata.ETag) {
				return fmt.Errorf("stream bytes disagree with metadata %+v", o.Metadata)
			}
		}
		return nil
	}
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 150 {
				key := fmt.Sprintf("site/k%d", rand.IntN(5))
				var req httpcache.Request
				switch rand.IntN(10) {
				case 0:
					req.NoCache = true
				case 1:
					req.NoStore = true
				case 2:
					req.MaxAge = httpcache.Delta{Present: true}
				}
				ctx, cancel := context.Background(), context.CancelFunc(func() {})
				switch rand.IntN(4) {
				case 0:
					ctx, cancel = context.WithTimeout(ctx, time.Duration(1+rand.IntN(20))*time.Millisecond)
				case 1:
					ctx, cancel = context.WithCancel(ctx)
					time.AfterFunc(time.Duration(rand.IntN(5))*time.Millisecond, cancel)
				}
				var l *Lookup
				if rand.IntN(5) == 0 {
					l, _ = h.c.Head(ctx, key, req)
				} else {
					l, _ = h.c.Get(ctx, key, req)
				}
				if l == nil {
					t.Error("nil Lookup")
				} else if err := verify(l); err != nil {
					t.Error(err)
				}
				if l != nil {
					l.Release()
				}
				cancel()
				if rand.IntN(10) == 0 {
					h.clock.Advance(time.Duration(rand.IntN(1500)) * time.Millisecond)
				}
				if (w+i)%50 == 0 {
					if err := checkInvariants(h.store); err != nil {
						t.Error(err)
					}
				}
			}
		}()
	}
	wg.Wait()
	h.settle()
	st := h.c.Stats()
	var fills uint64
	for _, n := range st.Fills {
		fills += n
	}
	if fills == 0 || st.Coalesced == 0 {
		t.Fatalf("the stress run exercised too little: %+v", st)
	}
	t.Logf("stats %+v; store %+v", st, h.store.Stats())
}

func TestStreamedRefetchKeepsStaleUnlessForbidden(t *testing.T) {
	big := string(make([]byte, 2048))
	for _, tc := range []struct {
		name   string
		meta   storage.Metadata
		data   string
		reason Reason
		budget bool // other fills hold the whole fill budget
		kept   bool
	}{
		{"too-large", objMeta(`"v2"`, "", len(big)), big, ReasonTooLarge, false, true},
		{"unknown-size", objMeta(`"v2"`, "", -1), "world", ReasonUnknownSize, false, true},
		{"fill-budget", objMeta(`"v2"`, "", 5), "world", ReasonFillBudget, true, true},
		{"no-store", objMeta(`"v2"`, "no-store", 5), "world", ReasonUncacheable, false, false},
		{"private", objMeta(`"v2"`, "private", 5), "world", ReasonUncacheable, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withConfig(staleWindow))
			v := h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
			h.clock.Advance(70 * time.Second)
			var held []*Reservation
			if tc.budget {
				for range 4 {
					held = append(held, reserve(t, h.store, 1<<10))
				}
			}
			h.origin.setGet(conditional(t, func(ctx context.Context) (*storage.Object, error) {
				return h.origin.object(ctx, tc.meta, tc.data), nil
			}))
			l, err := h.get("site/a", httpcache.Request{})
			if err != nil || l.Outcome != OutcomeBypass || l.Reason != tc.reason || content(t, l) != tc.data {
				t.Fatalf("got %s/%s, %v", l.Outcome, l.Reason, err)
			}
			l.Release()
			for _, r := range held {
				r.Release()
			}
			got, kind := h.peek("site/a")
			if !tc.kept {
				if kind != "" {
					t.Fatalf("a body the origin now forbids storing is still indexed as %s", kind)
				}
				h.settle()
				return
			}
			if got != v {
				t.Fatal("a capacity or size limit replaced the stale entry")
			}
			h.origin.setGet(conditional(t, func(context.Context) (*storage.Object, error) { return nil, originErr(storage.Unavailable) }))
			l = h.mustGet("site/a", httpcache.Request{}, OutcomeStale) // the kept entry still covers an outage
			if content(t, l) != "hello" {
				t.Fatal("wrong stale body")
			}
			l.Release()
			h.settle()
		})
	}
}

func TestLookupTimesPairWithMetadata(t *testing.T) {
	h := newHarness(t, withConfig(staleWindow))
	times := func(l *Lookup, want time.Time) {
		t.Helper()
		if !l.RequestTime.Equal(want) || !l.ResponseTime.Equal(want) {
			t.Fatalf("%s lookup times %v/%v, want %v", l.Outcome, l.RequestTime, l.ResponseTime, want)
		}
	}
	h.origin.serve(objMeta(`"v1"`, "max-age=60", 5), "hello")
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeMiss)
	times(l, lookupT0)
	l.Release()
	h.clock.Advance(10 * time.Second)
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeHit)
	times(l, lookupT0)
	l.Release()

	h.clock.Advance(time.Minute) // stale by 10s, inside stale_if_error
	h.origin.setGet(conditional(t, func(context.Context) (*storage.Object, error) { return nil, originErr(storage.Unavailable) }))
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeStale)
	times(l, lookupT0)
	l.Release()

	t1 := h.clock.Now()
	h.origin.setGet(conditional(t, func(context.Context) (*storage.Object, error) {
		return nil, notModifiedErr(storage.Metadata{ETag: `"v1"`, CacheControl: "max-age=60"})
	}))
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeRevalidated)
	times(l, t1)
	l.Release()

	h.clock.Advance(2 * time.Minute)
	t2 := h.clock.Now()
	h.origin.setGet(conditional(t, func(context.Context) (*storage.Object, error) {
		return nil, notModifiedErr(storage.Metadata{ETag: `"v1"`, CacheControl: "no-store"})
	}))
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeRevalidated)
	times(l, t2) // the merged 304 metadata pairs with the 304 exchange
	if snap := l.Handle().Snapshot(); l.Handle().Snapshot().ResponseTime.Equal(t2) || snap.Metadata.CacheControl == l.Metadata.CacheControl {
		t.Fatalf("an unstored 304 changed the snapshot %+v", snap)
	}
	l.Release()

	h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return objMeta(`"b"`, "", 5), nil })
	l, err := h.c.Head(context.Background(), "site/b", httpcache.Request{})
	if err != nil {
		t.Fatal(err)
	}
	times(l, t2)
	h.origin.serve(objMeta(`"c"`, "", -1), "stream")
	for _, req := range []httpcache.Request{{}, {NoStore: true}} {
		l, err = h.get("site/c", req)
		if err != nil || l.Outcome != OutcomeBypass {
			t.Fatal(err)
		}
		times(l, t2)
		l.Release()
	}
	h.settle()
}

func TestOnFillObservesEveryFlight(t *testing.T) {
	var mu sync.Mutex
	var events []FillEvent
	h := newHarness(t, func(_ *config.Cache, o *Options) {
		o.OnFill = func(e FillEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}
	})
	const wait = 20 * time.Millisecond
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		time.Sleep(wait)
		return h.origin.object(ctx, objMeta(`"v1"`, "max-age=60", 5), "hello"), nil
	})
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeMiss)
	l.Release()
	h.clock.Advance(2 * time.Minute)
	h.origin.setGet(conditional(t, func(context.Context) (*storage.Object, error) {
		return nil, notModifiedErr(storage.Metadata{ETag: `"v1"`})
	}))
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeRevalidated)
	l.Release()
	h.settle() // InFlight 0: every observer call has returned
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0].Result != FillAdmitted || events[0].Revalidation != "" || events[0].Duration < wait ||
		events[1].Result != FillRefreshed || events[1].Revalidation != RevalidationNotModified || events[1].Duration <= 0 {
		t.Fatalf("events %+v", events)
	}
}

func TestHandedStreamSurvivesShutdown(t *testing.T) {
	h := newHarness(t)
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		return h.origin.object(ctx, objMeta(`"v1"`, "", -1), "abcdefgh", slow(2, 10*time.Millisecond)), nil
	})
	l, err := h.get("site/a", httpcache.Request{})
	if err != nil || l.Reason != ReasonUnknownSize {
		t.Fatalf("got %s, %v", l.Reason, err)
	}
	h.stop() // the lifetime ends after the handoff
	if got := content(t, l); got != "abcdefgh" {
		t.Fatalf("read %q", got)
	}
	l.Release()
	h.settle()
}

func TestHandoffRacingFillTimeout(t *testing.T) {
	streamed, timedOut := 0, 0
	for i := range 50 {
		h := newHarness(t, withConfig(func(c *config.Cache) { c.FillTimeout = 2 * time.Millisecond }))
		delay := time.Duration(rand.IntN(3000)) * time.Microsecond
		h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				if i%2 == 0 { // half the origins ignore the cancellation and answer late
					return nil, originErr(storage.Canceled)
				}
				<-time.After(delay)
			}
			return h.origin.object(ctx, objMeta(`"v1"`, "", -1), "abcdefgh", slow(2, time.Millisecond)), nil
		})
		l, err := h.get("site/a", httpcache.Request{})
		switch {
		case err == nil:
			if got := content(t, l); got != "abcdefgh" { // content fails on a cut stream
				t.Fatalf("iteration %d read %q", i, got)
			}
			streamed++
		case storage.IsKind(err, storage.Timeout):
			timedOut++
		default:
			t.Fatalf("iteration %d: %v", i, err)
		}
		l.Release()
		h.settle()
	}
	t.Logf("%d streamed, %d timed out", streamed, timedOut)
}
