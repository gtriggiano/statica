package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

func TestNewValidatesOptions(t *testing.T) {
	h := newHarness(t)
	valid := Options{Store: h.store, Origin: h.origin, Config: testCacheConfig(), Lifetime: context.Background()}
	if _, err := New(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Options){
		"store":    func(o *Options) { o.Store = nil },
		"origin":   func(o *Options) { o.Origin = nil },
		"lifetime": func(o *Options) { o.Lifetime = nil },
		"fills":    func(o *Options) { o.Config.MaxConcurrentFills = 0 },
		"waiters":  func(o *Options) { o.Config.MaxWaitersPerKey = 0 },
		"timeout":  func(o *Options) { o.Config.FillTimeout = 0 },
	} {
		o := valid
		mutate(&o)
		if c, err := New(o); err == nil || c != nil {
			t.Errorf("%s: New accepted invalid options", name)
		}
	}
	ttl := time.Hour
	o := valid
	o.Config.Rules = []config.CacheRule{{Match: config.Match{Prefix: "/assets/"}, TTL: &ttl}}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ttl = time.Second
	o.Config.Rules[0].Match.Prefix = "/other/"
	if p := c.policy("assets/app.js"); p.TTL != time.Hour {
		t.Fatalf("rules not copied: %+v", p)
	}
}

func TestGetMissFillsThenHits(t *testing.T) {
	h := newHarness(t)
	h.origin.serve(objMeta(`"v1"`, "", 5), "hello")
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeMiss)
	snap := l.Handle().Snapshot()
	if content(t, l) != "hello" || l.Metadata.ETag != `"v1"` || !snap.RequestTime.Equal(lookupT0) || !snap.ResponseTime.Equal(lookupT0) {
		t.Fatalf("miss served %+v", snap)
	}
	if _, kind := h.peek("site/a"); kind != KindBody {
		t.Fatalf("indexed %s", kind)
	}
	l.Release()
	l.Release()
	if l.Handle() != nil || l.TakeObject() != nil {
		t.Fatal("a released Lookup still owns something")
	}
	hit := h.mustGet("site/a", httpcache.Request{}, OutcomeHit)
	if content(t, hit) != "hello" || h.origin.getCount() != 1 {
		t.Fatalf("hit read the origin: %d gets", h.origin.getCount())
	}
	hit.Release()
	if st := h.c.Stats(); st.Fills[FillAdmitted] != 1 || len(st.Fills) != len(fillResults) || len(st.Bypasses) != len(reasons) || len(st.Revalidations) != len(revalidationResults) {
		t.Fatalf("stats %+v", st)
	}
	h.settle()
}

func TestHeadColdStoresMetadataOnly(t *testing.T) {
	h := newHarness(t)
	h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return objMeta(`"v1"`, "", 500), nil })
	l, err := h.c.Head(context.Background(), "site/a", httpcache.Request{})
	if err != nil || l.Outcome != OutcomeMiss || l.Handle() != nil || l.Metadata.ETag != `"v1"` || l.Metadata.Size != 500 {
		t.Fatalf("cold HEAD = %+v, %v", l, err)
	}
	l.Release()
	if _, kind := h.peek("site/a"); kind != KindMetadata {
		t.Fatalf("indexed %s", kind)
	}
	hit, err := h.c.Head(context.Background(), "site/a", httpcache.Request{})
	if err != nil || hit.Outcome != OutcomeMetadataHit || hit.Handle() == nil || hit.Metadata.Size != 500 {
		t.Fatalf("warm HEAD = %+v, %v", hit, err)
	}
	hit.Release()
	if gets, heads := h.origin.calls(); len(gets) != 0 || heads != 1 {
		t.Fatalf("origin gets %d heads %d", len(gets), heads)
	}
	h.settle()
}

func TestHeadOnFreshBodyIsAMetadataHit(t *testing.T) {
	h := newHarness(t)
	h.prime("site/a", objMeta(`"v1"`, "", 5), "hello")
	l, err := h.c.Head(context.Background(), "site/a", httpcache.Request{})
	if err != nil || l.Outcome != OutcomeMetadataHit || l.Handle() == nil || l.Metadata.ETag != `"v1"` {
		t.Fatalf("HEAD = %+v, %v", l, err)
	}
	l.Release()
	if _, heads := h.origin.calls(); heads != 0 {
		t.Fatal("HEAD on a fresh body read the origin")
	}
	h.settle()
}

func TestGetSupersedesMetadataEntry(t *testing.T) {
	h := newHarness(t)
	h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return objMeta(`"v1"`, "", 5), nil })
	l, _ := h.c.Head(context.Background(), "site/a", httpcache.Request{})
	l.Release()
	h.origin.serve(objMeta(`"v1"`, "", 5), "hello")
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeMiss)
	if content(t, l) != "hello" {
		t.Fatal("wrong body")
	}
	l.Release()
	gets, _ := h.origin.calls()
	if _, kind := h.peek("site/a"); kind != KindBody || gets[0].IfNoneMatch != "" {
		t.Fatalf("indexed %s after %+v", kind, gets)
	}
	h.settle()
}

func TestConcurrentMissesShareOneFill(t *testing.T) {
	const n = 8
	h := newHarness(t)
	gate := make(chan struct{})
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return h.origin.object(ctx, objMeta(`"v1"`, "", 5), "hello"), nil
	})
	var chans []<-chan got
	for range n {
		chans = append(chans, h.goGet(context.Background(), "site/a", httpcache.Request{}))
	}
	h.waiters(n)
	close(gate)
	handles := map[*Handle]bool{}
	var bytes *byte
	var ls []*Lookup
	for _, ch := range chans {
		g := <-ch
		ls = append(ls, g.l)
		if g.err != nil || g.l.Outcome != OutcomeMiss || content(t, g.l) != "hello" {
			t.Fatalf("waiter got %s, %v", g.l.Outcome, g.err)
		}
		handles[g.l.Handle()] = true
		if b := &g.l.Handle().Snapshot().Body[0]; bytes == nil {
			bytes = b
		} else if b != bytes {
			t.Fatal("waiters got different copies")
		}
	}
	if len(handles) != n || h.origin.getCount() != 1 || h.c.Stats().Coalesced != n-1 {
		t.Fatalf("%d handles, %d origin gets, stats %+v", len(handles), h.origin.getCount(), h.c.Stats())
	}
	releaseAll(ls...)
	h.settle()
}

func TestWaitersFullBypassesWithOwnRead(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Cache) { c.MaxWaitersPerKey = 2 }))
	gate := make(chan struct{})
	h.origin.setGet(func(ctx context.Context, _ string, n int, _ storage.ReadOptions) (*storage.Object, error) {
		if n > 1 {
			return h.origin.object(ctx, objMeta(`"direct"`, "", 6), "direct"), nil
		}
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return h.origin.object(ctx, objMeta(`"fill"`, "", 4), "fill"), nil
	})
	first := h.goGet(context.Background(), "site/a", httpcache.Request{})
	h.waiters(1)
	second := h.goGet(context.Background(), "site/a", httpcache.Request{})
	h.waiters(2)
	l, err := h.get("site/a", httpcache.Request{})
	if err != nil || l.Outcome != OutcomeBypass || l.Reason != ReasonWaitersFull || content(t, l) != "direct" {
		t.Fatalf("third waiter got %s/%s, %v", l.Outcome, l.Reason, err)
	}
	l.Release()
	close(gate)
	for _, ch := range []<-chan got{first, second} {
		g := <-ch
		if g.err != nil || content(t, g.l) != "fill" {
			t.Fatalf("waiter got %v", g.err)
		}
		g.l.Release()
	}
	if st := h.c.Stats(); st.Bypasses[ReasonWaitersFull] != 1 || h.origin.getCount() != 2 {
		t.Fatalf("stats %+v", st)
	}
	h.settle()
}

func TestFillsFullBypassesWithOwnRead(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Cache) { c.MaxConcurrentFills = 1 }))
	gate := make(chan struct{})
	h.origin.setGet(func(ctx context.Context, key string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		if key == "site/b" {
			return h.origin.object(ctx, objMeta(`"b"`, "", 1), "b"), nil
		}
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return h.origin.object(ctx, objMeta(`"a"`, "", 1), "a"), nil
	})
	first := h.goGet(context.Background(), "site/a", httpcache.Request{})
	h.waiters(1)
	l, err := h.get("site/b", httpcache.Request{})
	if err != nil || l.Outcome != OutcomeBypass || l.Reason != ReasonFillsFull || content(t, l) != "b" {
		t.Fatalf("second key got %s/%s, %v", l.Outcome, l.Reason, err)
	}
	l.Release()
	if _, kind := h.peek("site/b"); kind != "" {
		t.Fatal("a bypass was admitted")
	}
	close(gate)
	g := <-first
	if g.err != nil || content(t, g.l) != "a" {
		t.Fatal(g.err)
	}
	g.l.Release()
	if h.c.Stats().Bypasses[ReasonFillsFull] != 1 {
		t.Fatalf("stats %+v", h.c.Stats())
	}
	h.settle()
}

type ctxKey struct{}

// endingCtx reports cancellation from Err without ever closing Done. It
// simulates a request ending between its waiter's wake-up on a published
// flight and the waiter's own context check.
type endingCtx struct {
	context.Context
	ended atomic.Bool
}

func (c *endingCtx) Err() error {
	if c.ended.Load() {
		return context.Canceled
	}
	return nil
}

func TestFirstWaiterLeavingKeepsFillForOthers(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	fill := make(chan context.Context, 1)
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		fill <- ctx
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return h.origin.object(ctx, objMeta(`"v1"`, "", 5), "hello"), nil
	})
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKey{}, "request"))
	first := h.goGet(ctx, "site/a", httpcache.Request{})
	fillCtx := <-fill
	if fillCtx.Value(ctxKey{}) != nil {
		t.Fatal("the fill context derives from a request")
	}
	others := []<-chan got{h.goGet(context.Background(), "site/a", httpcache.Request{}), h.goGet(context.Background(), "site/a", httpcache.Request{})}
	h.waiters(3)
	cancel()
	if g := <-first; !storage.IsKind(g.err, storage.Canceled) || errors.Is(g.err, ErrShutdown) || g.l.Outcome != OutcomeMiss || g.l.Handle() != nil {
		t.Fatalf("cancelled first waiter got %s, %v", g.l.Outcome, g.err)
	}
	h.waiters(2)
	if fillCtx.Err() != nil {
		t.Fatal("the first waiter cancelled the fill")
	}
	close(gate)
	for _, ch := range others {
		g := <-ch
		if g.err != nil || content(t, g.l) != "hello" {
			t.Fatal(g.err)
		}
		g.l.Release()
	}
	if _, kind := h.peek("site/a"); kind != KindBody {
		t.Fatal("fill not admitted")
	}
	h.settle()
}

func TestLastWaiterLeavingCancelsFill(t *testing.T) {
	h := newHarness(t)
	bodies := make(chan *fakeBody, 1)
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		o := h.origin.object(ctx, objMeta(`"v1"`, "", 5), "hello", gated(make(chan struct{})))
		bodies <- o.Body.(*fakeBody)
		return o, nil
	})
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	first := h.goGet(ctx1, "site/a", httpcache.Request{})
	h.waiters(1)
	second := h.goGet(ctx2, "site/a", httpcache.Request{})
	h.waiters(2)
	body := <-bodies
	waitFor(t, "the fill reservation", func() bool { return h.store.Stats().FillBytes == 5 })
	cancel1()
	cancel2()
	for _, ch := range []<-chan got{first, second} {
		if g := <-ch; !storage.IsKind(g.err, storage.Canceled) {
			t.Fatalf("waiter got %v", g.err)
		}
	}
	h.settle()
	if !body.canceled.Load() {
		t.Fatal("the origin did not observe the fill being cancelled")
	}
	if _, kind := h.peek("site/a"); kind != "" || h.c.Stats().Fills[FillCanceled] != 1 {
		t.Fatalf("abandoned fill stored %q, stats %+v", kind, h.c.Stats())
	}
}

func TestWaiterDeadlineLeavesFillRunning(t *testing.T) {
	h := newHarness(t)
	gate := make(chan struct{})
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		if err := waitGate(ctx, gate); err != nil {
			return nil, err
		}
		return h.origin.object(ctx, objMeta(`"v1"`, "", 5), "hello"), nil
	})
	first := h.goGet(context.Background(), "site/a", httpcache.Request{})
	h.waiters(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	l, err := h.c.Get(ctx, "site/a", httpcache.Request{})
	if !storage.IsKind(err, storage.Timeout) || l.Outcome != OutcomeMiss {
		t.Fatalf("short deadline got %s, %v", l.Outcome, err)
	}
	l.Release()
	close(gate)
	g := <-first
	if g.err != nil || content(t, g.l) != "hello" {
		t.Fatal(g.err)
	}
	g.l.Release()
	h.settle()
}

func TestFillTimeoutIsNotExtendedByLateWaiters(t *testing.T) {
	const timeout = 500 * time.Millisecond
	h := newHarness(t, withConfig(func(c *config.Cache) { c.FillTimeout = timeout }))
	called := make(chan struct{})
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		close(called)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	first := h.goGet(context.Background(), "site/a", httpcache.Request{})
	// The origin read blocks until the fill timer cancels it, so once it has
	// been called the flight is joinable for the whole remaining timeout.
	<-called
	time.Sleep(timeout / 2)
	late := h.goGet(context.Background(), "site/a", httpcache.Request{})
	g1, g2 := <-first, <-late
	for _, g := range []got{g1, g2} {
		if !storage.IsKind(g.err, storage.Timeout) || g.l.Outcome != OutcomeMiss {
			t.Fatalf("waiter got %s, %v", g.l.Outcome, g.err)
		}
	}
	// The late joiner waits about timeout/2; an extended timeout would keep it
	// a whole timeout. The 200ms between both bounds absorbs a slow runner.
	if g1.in < timeout || g2.in >= timeout*9/10 {
		t.Fatalf("first waited %v, late joiner %v, fill timeout %v", g1.in, g2.in, timeout)
	}
	if st := h.c.Stats(); st.Coalesced != 1 || st.Fills[FillTimeout] != 1 {
		t.Fatalf("stats %+v", st)
	}
	h.settle()
}

func TestFillTimeoutServesStale(t *testing.T) {
	t.Parallel() // each harness is self-contained; the fill timeout is wall time
	h := newHarness(t, withConfig(func(c *config.Cache) { c.FillTimeout = shortFill; c.StaleIfError = time.Minute }))
	v := h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(70 * time.Second)
	h.origin.setGet(func(ctx context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if opts.IfNoneMatch != `"v1"` {
			t.Errorf("revalidation sent %q", opts.IfNoneMatch)
		}
		<-ctx.Done()
		return nil, originErr(storage.Canceled)
	})
	l := h.mustGet("site/a", httpcache.Request{}, OutcomeStale)
	if content(t, l) != "hello" || l.Metadata.ETag != `"v1"` {
		t.Fatal("stale entry not served")
	}
	l.Release()
	if got, _ := h.peek("site/a"); got != v || h.c.Stats().Fills[FillTimeout] != 1 {
		t.Fatalf("stale entry changed: version %d→%d, stats %+v", v, got, h.c.Stats())
	}
	h.settle()
}

func TestStreamHandoff(t *testing.T) {
	t.Parallel()
	// The fill deadline runs from the flight's creation, and joining three
	// waiters happens inside it: the timeout leaves that setup ample headroom
	// on a loaded machine. The body is withheld on a schedule anchored to the
	// flight's first origin call, so it still outlasts the fill deadline.
	const timeout = 500 * time.Millisecond
	big := strings.Repeat("x", 4096)
	for _, tc := range []struct {
		name   string
		meta   storage.Metadata
		data   string
		reason Reason
		budget bool // other fills hold the whole fill budget
	}{
		{"too-large", objMeta(`"fill"`, "", len(big)), big, ReasonTooLarge, false},
		{"unknown-size", objMeta(`"fill"`, "", -1), big, ReasonUnknownSize, false},
		{"no-store", objMeta(`"fill"`, "no-store", 100), big[:100], ReasonUncacheable, false},
		{"private", objMeta(`"fill"`, "private", 100), big[:100], ReasonUncacheable, false},
		{"malformed", objMeta(`"fill"`, "max-age=", 100), big[:100], ReasonUncacheable, false},
		{"fill-budget", objMeta(`"fill"`, "", 100), big[:100], ReasonFillBudget, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // each harness is self-contained; the fill and body hold are wall time
			h := newHarness(t, withConfig(func(c *config.Cache) { c.FillTimeout = timeout }))
			var held []*Reservation
			if tc.budget {
				for range 4 {
					held = append(held, reserve(t, h.store, 1<<10))
				}
			}
			gate := make(chan struct{})
			var deadline time.Time // past the fill deadline, set before the flight publishes
			h.origin.setGet(func(ctx context.Context, _ string, n int, _ storage.ReadOptions) (*storage.Object, error) {
				if n > 1 {
					m := tc.meta
					m.ETag = `"direct"`
					return h.origin.object(ctx, m, tc.data), nil
				}
				// The flight was created just before this call, so its fill
				// deadline is no later than startedAt+timeout.
				startedAt := time.Now()
				deadline = startedAt.Add(timeout)
				if err := waitGate(ctx, gate); err != nil {
					return nil, err
				}
				// The body delivers nothing before the fill deadline has
				// passed: only a stream the fill timer no longer owns survives.
				held := make(chan struct{})
				time.AfterFunc(time.Until(startedAt.Add(timeout*3/2)), func() { close(held) })
				return h.origin.object(ctx, tc.meta, tc.data, gated(held)), nil
			})
			var chans []<-chan got
			for range 3 {
				chans = append(chans, h.goGet(context.Background(), "site/a", httpcache.Request{}))
			}
			h.waiters(3)
			close(gate)
			takers := 0
			for _, ch := range chans {
				g := <-ch
				if g.err != nil || g.l.Outcome != OutcomeBypass || g.l.Reason != tc.reason || g.l.Handle() != nil {
					t.Fatalf("waiter got %s/%s, %v", g.l.Outcome, g.l.Reason, g.err)
				}
				if g.l.Metadata.ETag == `"fill"` {
					takers++
				}
				if got := content(t, g.l); got != tc.data {
					t.Fatalf("served %d bytes, want %d", len(got), len(tc.data))
				}
				if g.l.Metadata.ETag == `"fill"` && time.Now().Before(deadline) {
					t.Fatal("the stream ended before the fill deadline")
				}
				g.l.Release()
			}
			if st := h.c.Stats(); takers != 1 || h.origin.getCount() != 3 || st.Bypasses[tc.reason] != 3 || st.Fills[FillStreamed] != 1 {
				t.Fatalf("%d takers, %d origin gets, stats %+v", takers, h.origin.getCount(), st)
			}
			if _, kind := h.peek("site/a"); kind != "" {
				t.Fatalf("a streamed object was stored as %s", kind)
			}
			for _, r := range held {
				r.Release()
			}
			h.settle()
		})
	}
}

func TestHandedStreamBelongsToTakerRequest(t *testing.T) {
	h := newHarness(t)
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		return h.origin.object(ctx, objMeta(`"v1"`, "", -1), "data", gated(make(chan struct{}))), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	l, err := h.c.Get(ctx, "site/a", httpcache.Request{})
	if err != nil || l.Reason != ReasonUnknownSize {
		t.Fatalf("got %s, %v", l.Reason, err)
	}
	o := l.TakeObject()
	l.Release() // nothing left to release
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(o.Body)
		read <- err
	}()
	cancel()
	if err := <-read; err == nil {
		t.Fatal("the stream outlived its taker's request")
	}
	_ = o.Body.Close()
	h.settle()
}

func TestUntakenStreamIsClosed(t *testing.T) {
	t.Run("lookup released", func(t *testing.T) {
		h := newHarness(t)
		h.origin.serve(objMeta(`"v1"`, "", -1), "data")
		l, err := h.get("site/a", httpcache.Request{})
		if err != nil || l.Outcome != OutcomeBypass {
			t.Fatal(err)
		}
		l.Release()
		h.settle()
	})
	t.Run("waiter ended after publish", func(t *testing.T) {
		h := newHarness(t)
		ctx := &endingCtx{Context: context.Background()}
		h.origin.setGet(func(fill context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
			ctx.ended.Store(true) // the request ends as the flight publishes, before it collects
			return h.origin.object(fill, objMeta(`"v1"`, "", -1), "data"), nil
		})
		l, err := h.c.Get(ctx, "site/a", httpcache.Request{})
		if !storage.IsKind(err, storage.Canceled) || l.TakeObject() != nil {
			t.Fatalf("got %s, %v", l.Outcome, err)
		}
		h.settle()
		if h.c.Stats().Fills[FillStreamed] != 1 {
			t.Fatalf("stats %+v", h.c.Stats())
		}
	})
	t.Run("every waiter left", func(t *testing.T) {
		h := newHarness(t)
		proceed := make(chan struct{})
		h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
			<-proceed // ignores the cancellation, as a slow origin may
			return h.origin.object(ctx, objMeta(`"v1"`, "", -1), "data"), nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		ch := h.goGet(ctx, "site/a", httpcache.Request{})
		h.waiters(1)
		cancel()
		if g := <-ch; !storage.IsKind(g.err, storage.Canceled) {
			t.Fatal(g.err)
		}
		close(proceed)
		h.settle()
		if h.c.Stats().Fills[FillCanceled] != 1 {
			t.Fatalf("stats %+v", h.c.Stats())
		}
	})
}

func TestFailedReadsAdmitNothing(t *testing.T) {
	pending := objMeta(`"v1"`, "", 5)
	pending.Checksum = storage.ChecksumPending
	for _, tc := range []struct {
		name string
		meta storage.Metadata
		data string
		opts []func(*fakeBody)
		want storage.Kind
	}{
		{"incomplete", objMeta(`"v1"`, "", 5), "abc", nil, storage.InvalidResponse},
		{"too-long", objMeta(`"v1"`, "", 3), "abcdef", nil, storage.InvalidResponse},
		{"read-error", objMeta(`"v1"`, "", 5), "abc", []func(*fakeBody){ending(originErr(storage.Unavailable))}, storage.Unavailable},
		{"checksum-mismatch", pending, "hello", []func(*fakeBody){ending(originErr(storage.ChecksumMismatch))}, storage.ChecksumMismatch},
		{"checksum-unverified", pending, "hello", nil, storage.ChecksumMismatch},
		{"untyped-short-read", objMeta(`"v1"`, "", 5), "abc", []func(*fakeBody){ending(io.ErrUnexpectedEOF)}, storage.InvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
				return h.origin.object(ctx, tc.meta, tc.data, tc.opts...), nil
			})
			l, err := h.get("site/a", httpcache.Request{})
			if !storage.IsKind(err, tc.want) || l.Outcome != OutcomeMiss || l.Handle() != nil {
				t.Fatalf("got %s, %v; want %s", l.Outcome, err, tc.want)
			}
			l.Release()
			if _, kind := h.peek("site/a"); kind != "" || h.c.Stats().Fills[FillFailed] != 1 {
				t.Fatalf("failed read stored %q, stats %+v", kind, h.c.Stats())
			}
			h.settle()
		})
	}
}

func TestUntypedOriginErrorsAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want storage.Kind
	}{
		{"provider-text", errors.New("private provider text"), storage.Unavailable},
		{"deadline", context.DeadlineExceeded, storage.Timeout},
		{"canceled", context.Canceled, storage.Canceled},
		{"short-response", io.ErrUnexpectedEOF, storage.InvalidResponse},
		{"unconditional-not-modified", originErr(storage.NotModified), storage.InvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) { return nil, tc.err })
			l, err := h.get("site/a", httpcache.Request{})
			if !storage.IsKind(err, tc.want) || l.Outcome != OutcomeMiss || strings.Contains(err.Error(), "private") {
				t.Fatalf("got %s, %v; want %s", l.Outcome, err, tc.want)
			}
			h.settle()
		})
	}
}

func TestNotFoundAdmitsNegative(t *testing.T) {
	h := newHarness(t)
	h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
		return nil, originErr(storage.NotFound)
	})
	l, err := h.get("site/a", httpcache.Request{})
	if !storage.IsKind(err, storage.NotFound) || l.Outcome != OutcomeMiss {
		t.Fatalf("got %s, %v", l.Outcome, err)
	}
	if _, kind := h.peek("site/a"); kind != KindNegative {
		t.Fatalf("indexed %q", kind)
	}
	l, err = h.get("site/a", httpcache.Request{})
	if !storage.IsKind(err, storage.NotFound) || l.Outcome != OutcomeNegativeHit || h.origin.getCount() != 1 {
		t.Fatalf("got %s, %v after %d gets", l.Outcome, err, h.origin.getCount())
	}
	l, err = h.c.Head(context.Background(), "site/a", httpcache.Request{})
	if !storage.IsKind(err, storage.NotFound) || l.Outcome != OutcomeNegativeHit {
		t.Fatalf("HEAD got %s, %v", l.Outcome, err)
	}
	h.clock.Advance(6 * time.Second) // past negative_ttl
	h.origin.serve(objMeta(`"v1"`, "", 5), "hello")
	l = h.mustGet("site/a", httpcache.Request{}, OutcomeMiss)
	l.Release()
	if _, kind := h.peek("site/a"); kind != KindBody {
		t.Fatalf("indexed %q", kind)
	}
	h.settle()
}

func TestNegativeEntriesNeedConfirmedAbsenceAndTTL(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        storage.Kind
		negativeTTL time.Duration
	}{
		{"zero-negative-ttl", storage.NotFound, 0},
		{"ambiguous", storage.AmbiguousNotFound, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withConfig(func(c *config.Cache) { c.NegativeTTL = tc.negativeTTL }))
			h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
				return nil, originErr(tc.kind)
			})
			for i := 1; i <= 2; i++ {
				l, err := h.get("site/a", httpcache.Request{})
				if !storage.IsKind(err, tc.kind) || l.Outcome != OutcomeMiss || h.origin.getCount() != i {
					t.Fatalf("read %d: %s, %v", i, l.Outcome, err)
				}
				if _, kind := h.peek("site/a"); kind != "" {
					t.Fatalf("indexed %q", kind)
				}
			}
			h.settle()
		})
	}
}

func TestZeroNegativeTTLRemovesDeletedEntry(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Cache) { c.NegativeTTL = 0 }))
	h.prime("site/a", objMeta(`"v1"`, "max-age=60", 5), "hello")
	h.clock.Advance(2 * time.Minute)
	h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
		return nil, originErr(storage.NotFound)
	})
	if _, err := h.get("site/a", httpcache.Request{}); !storage.IsKind(err, storage.NotFound) {
		t.Fatal(err)
	}
	if _, kind := h.peek("site/a"); kind != "" {
		t.Fatalf("indexed %q", kind)
	}
	h.settle()
}

func TestRequestDirectives(t *testing.T) {
	t.Run("no-store", func(t *testing.T) {
		h := newHarness(t)
		h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
			if ctx.Value(ctxKey{}) == nil {
				t.Error("a bypass did not read within the request context")
			}
			return h.origin.object(ctx, objMeta(`"v1"`, "", 5), "hello"), nil
		})
		ctx := context.WithValue(context.Background(), ctxKey{}, "request")
		l, err := h.c.Get(ctx, "site/a", httpcache.Request{NoStore: true})
		if err != nil || l.Outcome != OutcomeBypass || l.Reason != ReasonRequestNoStore || content(t, l) != "hello" {
			t.Fatalf("got %s/%s, %v", l.Outcome, l.Reason, err)
		}
		l.Release()
		h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return objMeta(`"v1"`, "", 5), nil })
		l, err = h.c.Head(ctx, "site/a", httpcache.Request{NoStore: true})
		if err != nil || l.Outcome != OutcomeBypass || l.Reason != ReasonRequestNoStore {
			t.Fatalf("HEAD got %s/%s, %v", l.Outcome, l.Reason, err)
		}
		if _, kind := h.peek("site/a"); kind != "" || h.c.Stats().Bypasses[ReasonRequestNoStore] != 2 {
			t.Fatalf("no-store stored %q", kind)
		}
		h.settle()
	})
	t.Run("no-cache-revalidates-fresh-entry", func(t *testing.T) {
		h := newHarness(t)
		h.prime("site/a", objMeta(`"v1"`, "", 5), "hello")
		h.origin.setGet(func(_ context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
			if opts.IfNoneMatch != `"v1"` {
				t.Errorf("If-None-Match %q", opts.IfNoneMatch)
			}
			return nil, notModifiedErr(objMeta(`"v1"`, "", 5))
		})
		l := h.mustGet("site/a", httpcache.Request{NoCache: true}, OutcomeRevalidated)
		l.Release()
		l = h.mustGet("site/a", httpcache.Request{}, OutcomeHit)
		l.Release()
		h.settle()
	})
	t.Run("disabled-rule", func(t *testing.T) {
		off := false
		h := newHarness(t, withConfig(func(c *config.Cache) {
			c.Rules = []config.CacheRule{{Match: config.Match{Prefix: "/private/"}, Enabled: &off}}
		}))
		h.origin.serve(objMeta(`"v1"`, "", 5), "hello")
		h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return objMeta(`"v1"`, "", 5), nil })
		l, err := h.get("site/private/a", httpcache.Request{})
		if err != nil || l.Reason != ReasonDisabled {
			t.Fatalf("got %s, %v", l.Reason, err)
		}
		l.Release()
		l, err = h.c.Head(context.Background(), "site/private/a", httpcache.Request{})
		if err != nil || l.Reason != ReasonDisabled {
			t.Fatalf("HEAD got %s, %v", l.Reason, err)
		}
		if _, kind := h.peek("site/private/a"); kind != "" {
			t.Fatal("a disabled object was stored")
		}
		l = h.mustGet("site/public", httpcache.Request{}, OutcomeMiss) // the prefix is not part of the rule path
		l.Release()
		h.settle()
	})
}

func TestTTLModeFixedSkipsTheOriginUntilTheTTL(t *testing.T) {
	fixed := config.TTLModeFixed
	h := newHarness(t, withConfig(func(c *config.Cache) {
		c.Rules = []config.CacheRule{{Match: config.Match{Prefix: "/draft/"}, TTLMode: &fixed}}
	}))
	m := objMeta(`"v1"`, "no-cache", 5)
	h.prime("site/draft/a", m, "hello")
	h.prime("site/b", m, "hello")
	h.origin.setGet(func(_ context.Context, key string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if opts.IfNoneMatch != `"v1"` {
			t.Errorf("%s: If-None-Match %q", key, opts.IfNoneMatch)
		}
		return nil, notModifiedErr(m)
	})
	h.clock.Advance(4 * time.Minute)
	before := h.origin.getCount()
	l := h.mustGet("site/draft/a", httpcache.Request{}, OutcomeHit)
	l.Release()
	hl, err := h.c.Head(context.Background(), "site/draft/a", httpcache.Request{})
	if err != nil || hl.Outcome != OutcomeMetadataHit {
		t.Fatalf("HEAD = %s, %v", hl.Outcome, err)
	}
	hl.Release()
	if gets, heads := h.origin.calls(); len(gets) != before || heads != 0 {
		t.Fatalf("a fresh entry under ttl_mode fixed read the origin: %d gets, %d heads", len(gets)-before, heads)
	}
	// Outside the rule, ttl_mode ceiling keeps the origin's no-cache.
	l = h.mustGet("site/b", httpcache.Request{}, OutcomeRevalidated)
	l.Release()
	// The TTL counts from the response: it is spent five minutes after the
	// prime, and the revalidation starts it again.
	h.clock.Advance(time.Minute)
	l = h.mustGet("site/draft/a", httpcache.Request{}, OutcomeRevalidated)
	l.Release()
	h.clock.Advance(4 * time.Minute)
	l = h.mustGet("site/draft/a", httpcache.Request{}, OutcomeHit)
	l.Release()
	if expired := h.c.StoreStats().Expirations[KindBody]; expired != 1 {
		t.Errorf("%d body expirations, want 1", expired)
	}
	h.settle()
}

func TestOriginTTLModesLetTheOriginExtendTheTTL(t *testing.T) {
	for _, tc := range []struct {
		mode, cacheControl string
		hitUntil           time.Duration // the age up to which a GET is a hit
	}{
		{config.TTLModeFloor, "no-cache", 5 * time.Minute},       // the TTL is a floor
		{config.TTLModeFloor, "max-age=3600", time.Hour},         // the origin extends it
		{config.TTLModeFallback, "max-age=3600", time.Hour},      // the origin decides
		{config.TTLModeFallback, "max-age=60", time.Minute},      // in both directions
		{config.TTLModeCeiling, "max-age=3600", 5 * time.Minute}, // the default caps the origin
	} {
		t.Run(tc.mode+" "+tc.cacheControl, func(t *testing.T) {
			mode := tc.mode
			h := newHarness(t, withConfig(func(c *config.Cache) { c.TTLMode = mode }))
			m := objMeta(`"v1"`, tc.cacheControl, 5)
			h.prime("site/a", m, "hello")
			h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
				return nil, notModifiedErr(m)
			})
			h.clock.Advance(tc.hitUntil - time.Second)
			l := h.mustGet("site/a", httpcache.Request{}, OutcomeHit)
			l.Release()
			h.clock.Advance(time.Second)
			l = h.mustGet("site/a", httpcache.Request{}, OutcomeRevalidated)
			l.Release()
			h.settle()
		})
	}
}

func TestRequestDirectivesPolicy(t *testing.T) {
	ignore, revalidate := config.RequestDirectivesIgnore, config.RequestDirectivesRevalidateOnly
	h := newHarness(t, withConfig(func(c *config.Cache) {
		c.Rules = []config.CacheRule{
			{Match: config.Match{Prefix: "/ignore/"}, RequestDirectives: &ignore},
			{Match: config.Match{Prefix: "/revalidate/"}, RequestDirectives: &revalidate},
		}
	}))
	m := objMeta(`"v1"`, "", 5)
	h.prime("site/ignore/a", m, "hello")
	h.prime("site/revalidate/a", m, "hello")
	h.origin.setGet(func(_ context.Context, _ string, _ int, opts storage.ReadOptions) (*storage.Object, error) {
		if opts.IfNoneMatch != `"v1"` {
			t.Errorf("If-None-Match %q", opts.IfNoneMatch)
		}
		return nil, notModifiedErr(m)
	})
	all := httpcache.Request{NoStore: true, NoCache: true}
	before := h.origin.getCount()
	for _, req := range []httpcache.Request{all, {NoStore: true}, {NoCache: true}, maxAge(0)} {
		l := h.mustGet("site/ignore/a", req, OutcomeHit)
		l.Release()
		hl, err := h.c.Head(context.Background(), "site/ignore/a", req)
		if err != nil || hl.Outcome != OutcomeMetadataHit {
			t.Fatalf("%+v: HEAD = %s, %v", req, hl.Outcome, err)
		}
		hl.Release()
	}
	if gets, heads := h.origin.calls(); len(gets) != before || heads != 0 {
		t.Fatalf("ignored directives read the origin: %d gets, %d heads", len(gets)-before, heads)
	}
	// revalidate_only: no-store alone no longer bypasses, the others still revalidate.
	l := h.mustGet("site/revalidate/a", httpcache.Request{NoStore: true}, OutcomeHit)
	l.Release()
	l = h.mustGet("site/revalidate/a", all, OutcomeRevalidated)
	l.Release()
	l = h.mustGet("site/revalidate/a", maxAge(0), OutcomeRevalidated)
	l.Release()
	hl, err := h.c.Head(context.Background(), "site/revalidate/a", httpcache.Request{NoStore: true})
	if err != nil || hl.Outcome != OutcomeMetadataHit {
		t.Fatalf("revalidate_only: HEAD with no-store = %s, %v", hl.Outcome, err)
	}
	hl.Release()
	h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return m, nil })
	hl, err = h.c.Head(context.Background(), "site/revalidate/a", httpcache.Request{NoCache: true})
	if err != nil || hl.Outcome != OutcomeRevalidated {
		t.Fatalf("revalidate_only: HEAD with no-cache = %s, %v", hl.Outcome, err)
	}
	hl.Release()
	if st := h.c.Stats(); st.Bypasses[ReasonRequestNoStore] != 0 || h.origin.getCount() != before+2 {
		t.Fatalf("bypasses %v, %d origin gets", st.Bypasses, h.origin.getCount()-before)
	}
	if _, heads := h.origin.calls(); heads != 1 {
		t.Fatalf("%d origin heads, want 1", heads)
	}
	// A stale entry under ignore is still revalidated.
	h.clock.Advance(5 * time.Minute)
	l = h.mustGet("site/ignore/a", httpcache.Request{}, OutcomeRevalidated)
	l.Release()
	h.settle()
}

// A directive that request_directives drops no longer refuses stale service,
// while statica's own revalidation still does.
func TestDroppedDirectivesAllowStaleIfError(t *testing.T) {
	outage := originErr(storage.Unavailable)
	for _, tc := range []struct {
		directives string
		revalidate bool
		stale      bool
	}{
		{config.RequestDirectivesHonor, false, false},
		{config.RequestDirectivesRevalidateOnly, false, false},
		{config.RequestDirectivesIgnore, false, true},
		{config.RequestDirectivesIgnore, true, false},
	} {
		t.Run(fmt.Sprintf("%s revalidate=%t", tc.directives, tc.revalidate), func(t *testing.T) {
			directives := tc.directives
			h := newHarness(t, withConfig(func(c *config.Cache) { c.StaleIfError = time.Minute; c.RequestDirectives = directives }))
			h.prime("site/a", objMeta(`"v1"`, "", 5), "hello")
			h.clock.Advance(5*time.Minute + time.Second)
			h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) { return nil, outage })
			h.origin.setHead(func(context.Context, string) (storage.Metadata, error) { return storage.Metadata{}, outage })
			req := httpcache.Request{NoCache: true}
			for name, lookup := range map[string]func() (*Lookup, error){
				"GET":  func() (*Lookup, error) { return h.c.get(context.Background(), "site/a", req, tc.revalidate) },
				"HEAD": func() (*Lookup, error) { return h.c.head(context.Background(), "site/a", req, tc.revalidate) },
			} {
				l, err := lookup()
				switch {
				case tc.stale && (err != nil || l.Outcome != OutcomeStale):
					t.Errorf("%s = %s, %v; want stale", name, l.Outcome, err)
				case !tc.stale && (!storage.IsKind(err, storage.Unavailable) || l.Outcome != OutcomeMiss):
					t.Errorf("%s = %s, %v; want the origin error", name, l.Outcome, err)
				}
				l.Release()
			}
			h.settle()
		})
	}
}

func TestIgnoredDirectivesKeepNegativeHits(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Cache) { c.RequestDirectives = config.RequestDirectivesIgnore }))
	h.origin.setGet(func(context.Context, string, int, storage.ReadOptions) (*storage.Object, error) {
		return nil, originErr(storage.NotFound)
	})
	l, err := h.get("site/a", httpcache.Request{})
	if !storage.IsKind(err, storage.NotFound) || l.Outcome != OutcomeMiss {
		t.Fatalf("got %s, %v", l.Outcome, err)
	}
	l.Release()
	h.origin.setGet(nil) // any further origin read fails the test
	for _, req := range []httpcache.Request{{NoCache: true}, maxAge(0), {NoStore: true}} {
		l, err := h.get("site/a", req)
		if !storage.IsKind(err, storage.NotFound) || l.Outcome != OutcomeNegativeHit {
			t.Errorf("%+v: GET = %s, %v", req, l.Outcome, err)
		}
		l.Release()
		hl, err := h.c.Head(context.Background(), "site/a", req)
		if !storage.IsKind(err, storage.NotFound) || hl.Outcome != OutcomeNegativeHit {
			t.Errorf("%+v: HEAD = %s, %v", req, hl.Outcome, err)
		}
		hl.Release()
	}
	h.settle()
}

func TestLifetimeCancelsFlights(t *testing.T) {
	h := newHarness(t)
	h.origin.setGet(func(ctx context.Context, _ string, n int, _ storage.ReadOptions) (*storage.Object, error) {
		if n > 2 {
			return h.origin.object(ctx, objMeta(`"v1"`, "", 5), "hello"), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	chans := []<-chan got{
		h.goGet(context.Background(), "site/a", httpcache.Request{}),
		h.goGet(context.Background(), "site/a", httpcache.Request{}),
		h.goGet(context.Background(), "site/b", httpcache.Request{}),
	}
	h.waiters(3)
	waitFor(t, "both origin reads", func() bool { return h.origin.getCount() == 2 })
	h.stop()
	for _, ch := range chans {
		g := <-ch
		if !storage.IsKind(g.err, storage.Canceled) || !errors.Is(g.err, ErrShutdown) || g.l.Outcome != OutcomeMiss {
			t.Fatalf("waiter got %s, %v", g.l.Outcome, g.err)
		}
		if g.err.Error() != "origin: canceled: cache shutdown" {
			t.Fatalf("error text %q", g.err.Error())
		}
	}
	l, err := h.get("site/c", httpcache.Request{})
	if err != nil || l.Reason != ReasonShutdown || content(t, l) != "hello" {
		t.Fatalf("after shutdown got %s, %v", l.Reason, err)
	}
	l.Release()
	if h.c.Stats().Fills[FillCanceled] != 2 {
		t.Fatalf("stats %+v", h.c.Stats())
	}
	h.settle()
}

func TestHandedStreamCloseReportsOriginError(t *testing.T) {
	closeErr := errors.New("close failed")
	h := newHarness(t)
	h.origin.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		return h.origin.object(ctx, objMeta(`"v1"`, "", -1), "data", func(b *fakeBody) { b.closeErr = closeErr }), nil
	})
	l, err := h.get("site/a", httpcache.Request{})
	if err != nil {
		t.Fatal(err)
	}
	o := l.TakeObject()
	if err := o.Body.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("first Close = %v", err)
	}
	if err := o.Body.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	h.settle()
}
