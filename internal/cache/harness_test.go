package cache

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

var lookupT0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type getFunc func(ctx context.Context, key string, n int, opts storage.ReadOptions) (*storage.Object, error)

// fakeOrigin records every read and delegates it to functions a test may
// swap between reads. n counts Get calls from 1.
type fakeOrigin struct {
	t      *testing.T
	mu     sync.Mutex
	get    getFunc
	head   func(ctx context.Context, key string) (storage.Metadata, error)
	gets   []storage.ReadOptions
	heads  int
	bodies []*fakeBody
}

func (o *fakeOrigin) setGet(f getFunc) {
	o.mu.Lock()
	o.get = f
	o.mu.Unlock()
}

func (o *fakeOrigin) setHead(f func(ctx context.Context, key string) (storage.Metadata, error)) {
	o.mu.Lock()
	o.head = f
	o.mu.Unlock()
}

func (o *fakeOrigin) Get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	o.mu.Lock()
	o.gets = append(o.gets, opts)
	n, get := len(o.gets), o.get
	o.mu.Unlock()
	if get == nil {
		o.t.Errorf("unexpected origin Get of %q", key)
		return nil, &storage.Error{Kind: storage.Unavailable, Size: -1}
	}
	return get(ctx, key, n, opts)
}

func (o *fakeOrigin) Head(ctx context.Context, key string) (storage.Metadata, error) {
	o.mu.Lock()
	o.heads++
	head := o.head
	o.mu.Unlock()
	if head == nil {
		o.t.Errorf("unexpected origin Head of %q", key)
		return storage.Metadata{}, &storage.Error{Kind: storage.Unavailable, Size: -1}
	}
	return head(ctx, key)
}

func (o *fakeOrigin) calls() (gets []storage.ReadOptions, heads int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]storage.ReadOptions(nil), o.gets...), o.heads
}

func (o *fakeOrigin) getCount() int {
	gets, _ := o.calls()
	return len(gets)
}

// open counts bodies not yet closed.
func (o *fakeOrigin) open() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, b := range o.bodies {
		if !b.closed.Load() {
			n++
		}
	}
	return n
}

// object returns an Object whose body serves data and, like the S3 adapter's,
// fails once the context of the Get that opened it ends.
func (o *fakeOrigin) object(ctx context.Context, m storage.Metadata, data string, opts ...func(*fakeBody)) *storage.Object {
	b := &fakeBody{ctx: ctx, data: []byte(data), closedCh: make(chan struct{})}
	for _, opt := range opts {
		opt(b)
	}
	o.mu.Lock()
	o.bodies = append(o.bodies, b)
	o.mu.Unlock()
	return &storage.Object{Metadata: m, Body: b}
}

// serve makes every Get return a fresh object with m and data.
func (o *fakeOrigin) serve(m storage.Metadata, data string) {
	o.setGet(func(ctx context.Context, _ string, _ int, _ storage.ReadOptions) (*storage.Object, error) {
		return o.object(ctx, m, data), nil
	})
}

type fakeBody struct {
	ctx      context.Context
	data     []byte
	chunk    int           // bytes per Read; 0: as many as fit
	gate     chan struct{} // each Read first waits for it to close
	delay    time.Duration // each Read first waits this long
	end      error         // returned instead of io.EOF after data
	closeErr error         // returned by Close
	pos      int
	terminal error
	once     sync.Once
	closedCh chan struct{}
	closed   atomic.Bool
	canceled atomic.Bool // a Read observed the Get context ending
}

func slow(chunk int, delay time.Duration) func(*fakeBody) {
	return func(b *fakeBody) { b.chunk, b.delay = chunk, delay }
}

func gated(gate chan struct{}) func(*fakeBody) { return func(b *fakeBody) { b.gate = gate } }

func ending(err error) func(*fakeBody) { return func(b *fakeBody) { b.end = err } }

func (b *fakeBody) Read(p []byte) (int, error) {
	if b.terminal != nil {
		return 0, b.terminal
	}
	if err := b.wait(); err != nil {
		b.terminal = err
		return 0, err
	}
	if b.pos == len(b.data) {
		b.terminal = io.EOF
		if b.end != nil {
			b.terminal = b.end
		}
		return 0, b.terminal
	}
	n := len(p)
	if b.chunk > 0 {
		n = min(n, b.chunk)
	}
	n = copy(p[:n], b.data[b.pos:])
	b.pos += n
	return n, nil
}

func (b *fakeBody) wait() error {
	gate := b.gate
	var delay <-chan time.Time
	if b.delay > 0 {
		delay = time.After(b.delay)
	}
	for gate != nil || delay != nil {
		select {
		case <-gate:
			gate = nil
		case <-delay:
			delay = nil
		case <-b.ctx.Done():
		case <-b.closedCh:
		}
		if b.ctx.Err() != nil || b.closed.Load() {
			break
		}
	}
	if b.ctx.Err() != nil {
		b.canceled.Store(true)
		return &storage.Error{Kind: storage.Canceled, Size: -1}
	}
	if b.closed.Load() {
		return &storage.Error{Kind: storage.Canceled, Size: -1}
	}
	return nil
}

func (b *fakeBody) Close() error {
	b.once.Do(func() {
		b.closed.Store(true)
		close(b.closedCh)
	})
	return b.closeErr
}

func objMeta(etag, cacheControl string, size int) storage.Metadata {
	return storage.Metadata{Size: int64(size), ETag: etag, ContentType: "text/plain", CacheControl: cacheControl, Checksum: storage.ChecksumUnavailable}
}

func originErr(k storage.Kind) error { return &storage.Error{Kind: k, Size: -1} }

func notModifiedErr(m storage.Metadata) error {
	m.Size = -1
	return &storage.Error{Kind: storage.NotModified, Size: -1, Metadata: &m}
}

// waitGate blocks until gate closes or ctx ends, as an origin call would.
func waitGate(ctx context.Context, gate chan struct{}) error {
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return originErr(storage.Canceled)
	}
}

// shortFill is a FillTimeout a test waits out in full, long enough that a fill
// which should succeed (such as the prime before the timed read) cannot
// plausibly exceed it on a loaded machine.
const shortFill = 250 * time.Millisecond

func testCacheConfig() config.Cache {
	return config.Cache{Enabled: true, MaxBytes: 1 << 20, MaxEntries: 64, MaxObjectBytes: 1 << 10, MaxFillBytes: 4 << 10,
		MaxConcurrentFills: 4, MaxWaitersPerKey: 16, FillTimeout: 5 * time.Second, TTL: 5 * time.Minute, NegativeTTL: 5 * time.Second}
}

type harness struct {
	t      *testing.T
	c      *Cache
	store  *Store
	origin *fakeOrigin
	clock  *fakeClock
	stop   context.CancelFunc // ends the Cache lifetime
}

func newHarness(t *testing.T, mutate ...func(*config.Cache, *Options)) *harness {
	t.Helper()
	cfg := testCacheConfig()
	o := Options{Prefix: "site/", ConditionalReads: true}
	for _, m := range mutate {
		m(&cfg, &o)
	}
	h := &harness{t: t, store: newTestStore(t, LimitsFrom(cfg)), origin: &fakeOrigin{t: t}, clock: &fakeClock{t: lookupT0}}
	lifetime, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	h.stop = stop
	o.Store, o.Origin, o.Config, o.Now, o.Lifetime = h.store, h.origin, cfg, h.clock.Now, lifetime
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	h.c = c
	return h
}

func withConfig(f func(*config.Cache)) func(*config.Cache, *Options) {
	return func(c *config.Cache, _ *Options) { f(c) }
}

type got struct {
	l   *Lookup
	err error
	in  time.Duration
}

// goGet runs Get in its own goroutine.
func (h *harness) goGet(ctx context.Context, key string, req httpcache.Request) <-chan got {
	ch := make(chan got, 1)
	go func() {
		start := time.Now()
		l, err := h.c.Get(ctx, key, req)
		ch <- got{l, err, time.Since(start)}
	}()
	return ch
}

func (h *harness) get(key string, req httpcache.Request) (*Lookup, error) {
	h.t.Helper()
	l, err := h.c.Get(context.Background(), key, req)
	if l == nil {
		h.t.Fatal("Get returned a nil Lookup")
	}
	return l, err
}

// mustGet expects a Lookup served from memory with outcome want.
func (h *harness) mustGet(key string, req httpcache.Request, want Outcome) *Lookup {
	h.t.Helper()
	l, err := h.get(key, req)
	if err != nil || l.Outcome != want {
		h.t.Fatalf("Get(%q) = %s, %v; want %s", key, l.Outcome, err, want)
	}
	return l
}

// prime stores key's body from the origin at the current clock.
func (h *harness) prime(key string, m storage.Metadata, data string) Version {
	h.t.Helper()
	h.origin.serve(m, data)
	l := h.mustGet(key, httpcache.Request{}, OutcomeMiss)
	l.Release()
	h.origin.setGet(nil)
	v, kind, ok := h.store.Peek(key)
	if !ok || kind != KindBody {
		h.t.Fatalf("prime(%q) left %s, %t", key, kind, ok)
	}
	return v
}

func (h *harness) peek(key string) (Version, Kind) {
	v, k, _ := h.store.Peek(key)
	return v, k
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *harness) waiters(n int) {
	h.t.Helper()
	waitFor(h.t, "waiters", func() bool { return h.c.Stats().Waiters == n })
}

// pinned lists the indexed keys some handle still pins.
func pinned(s *Store) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, k := range s.index.Keys() {
		if e, _ := s.index.Peek(k); e.refs != 0 {
			keys = append(keys, k)
		}
	}
	return keys
}

// settle waits for every flight to unwind, then asserts that nothing leaked:
// no flight, waiter, reservation, detached or retired entry, pinned indexed
// entry or open body. Every Lookup must have been released.
func (h *harness) settle() {
	h.t.Helper()
	waitFor(h.t, "flights to unwind", func() bool {
		st := h.c.Stats()
		return st.InFlight == 0 && st.Waiters == 0 && h.origin.open() == 0
	})
	h.c.mu.Lock()
	flights := len(h.c.flights)
	h.c.mu.Unlock()
	check(h.t, h.store)
	s := h.store.Stats()
	if flights != 0 || s.FillBytes != 0 || s.Reservations != 0 || s.DetachedEntries != 0 || s.RetiredEntries != 0 || s.RetiredBytes != 0 {
		h.t.Fatalf("leak: %d flights, store %+v", flights, s)
	}
	if keys := pinned(h.store); len(keys) != 0 {
		h.t.Fatalf("leak: indexed entries still pinned: %q", keys)
	}
}

// content returns what l serves: its snapshot's bytes, or its object read to
// the end and closed.
func content(t *testing.T, l *Lookup) string {
	t.Helper()
	if h := l.Handle(); h != nil {
		return string(h.Snapshot().Body)
	}
	o := l.TakeObject()
	if o == nil {
		t.Fatal("Lookup serves nothing")
	}
	defer o.Body.Close()
	b, err := io.ReadAll(o.Body)
	if err != nil {
		t.Fatalf("reading the object: %v", err)
	}
	return string(b)
}

func releaseAll(ls ...*Lookup) {
	for _, l := range ls {
		l.Release()
	}
}
