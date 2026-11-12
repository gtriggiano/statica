package cache

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

// The request layer serves origin reads through a Store. Misses and
// revalidations of one key share one bounded flight (fill.go); everything a
// flight cannot buffer streams. Ownership is explicit at every step: a Lookup
// owns what it returns, a flight owns its result until its last waiter has
// taken its own share or left, and nothing waits outside a context.

// Outcome is the finite classification of one Get or Head, set on error
// paths too. Only OutcomeHit serves a body from memory without origin I/O: a
// HEAD answered from memory is OutcomeMetadataHit, so HEADs never inflate a
// body hit ratio.
type Outcome string

const (
	OutcomeHit         Outcome = "hit"          // a fresh body entry served from memory to a GET
	OutcomeMetadataHit Outcome = "metadata_hit" // a HEAD answered from a fresh body or metadata entry
	OutcomeNegativeHit Outcome = "negative_hit" // a fresh negative entry: NotFound without origin I/O
	OutcomeMiss        Outcome = "miss"         // an origin read for an absent or unusable entry, or its failure
	OutcomeRevalidated Outcome = "revalidated"  // stored bytes the origin confirmed unchanged
	OutcomeStale       Outcome = "stale"        // an expired entry served after an origin outage (stale_if_error)
	OutcomeBypass      Outcome = "bypass"       // an origin read the cache neither reused nor stored; see Reason
)

// Reason is the finite reason of an OutcomeBypass.
type Reason string

const (
	ReasonNone           Reason = ""
	ReasonDisabled       Reason = "disabled"         // the effective policy is disabled
	ReasonRequestNoStore Reason = "request_no_store" // the request carries no-store
	ReasonShutdown       Reason = "shutdown"         // the lifetime ended: no new flights
	ReasonFillsFull      Reason = "fills_full"       // max_concurrent_fills flights are running
	ReasonWaitersFull    Reason = "waiters_full"     // the key's flight has max_waiters_per_key waiters
	ReasonUnknownSize    Reason = "unknown_size"     // the object streams: its length is unknown
	ReasonTooLarge       Reason = "too_large"        // the object streams: it exceeds max_object_bytes
	ReasonUncacheable    Reason = "uncacheable"      // the origin metadata may not be stored (no-store, private, malformed)
	ReasonFillBudget     Reason = "fill_budget"      // the object streams: other fills hold max_fill_bytes
)

var reasons = [...]Reason{ReasonDisabled, ReasonRequestNoStore, ReasonShutdown, ReasonFillsFull, ReasonWaitersFull, ReasonUnknownSize, ReasonTooLarge, ReasonUncacheable, ReasonFillBudget}

// FillResult is the finite outcome of one flight.
type FillResult string

const (
	FillAdmitted  FillResult = "admitted"  // a complete body was indexed
	FillDetached  FillResult = "detached"  // a complete body was served without being indexed: conflict or no room
	FillRefreshed FillResult = "refreshed" // a 304 refreshed the stored metadata in place
	FillUnstored  FillResult = "unstored"  // a 304 confirmed bytes served with merged metadata that was not stored
	FillStreamed  FillResult = "streamed"  // the origin stream was handed to one waiter
	FillNotFound  FillResult = "not_found" // confirmed absence
	FillDenied    FillResult = "denied"    // origin access denied
	FillTimeout   FillResult = "timeout"   // fill_timeout or an origin timeout
	FillCanceled  FillResult = "canceled"  // every waiter left, or shutdown
	FillFailed    FillResult = "failed"    // any other origin or body failure
)

var fillResults = [...]FillResult{FillAdmitted, FillDetached, FillRefreshed, FillUnstored, FillStreamed, FillNotFound, FillDenied, FillTimeout, FillCanceled, FillFailed}

// RevalidationResult is the finite answer to one conditional origin read made
// by a flight. It names the origin's answer, independently of whether the
// flight could apply it; the flight's FillResult tells what came of it, e.g.
// a 304 or a NotFound that arrives after fill_timeout is RevalidationNotModified
// or RevalidationMissing with FillTimeout. It maps onto the design's
// revalidation results as follows:
//   - unchanged: RevalidationNotModified;
//   - replaced: RevalidationModified, a 200, whatever its download then
//     yields, and RevalidationETagChanged, a 304 with another entity-tag,
//     whatever its refetch yields other than a NotFound;
//   - missing: RevalidationMissing, a NotFound answering the conditional
//     read or the refetch after a changed entity-tag;
//   - failed: RevalidationError, any other error.
//
// A HEAD that refreshes a stale body in place is an uncoalesced metadata read,
// not a flight: its request reports OutcomeRevalidated and is not counted here.
type RevalidationResult string

const (
	RevalidationNotModified RevalidationResult = "not_modified" // 304 with the stored entity-tag
	RevalidationETagChanged RevalidationResult = "etag_changed" // 304 with another entity-tag: refetched
	RevalidationModified    RevalidationResult = "modified"     // 200: a changed object
	RevalidationMissing     RevalidationResult = "missing"      // confirmed NotFound
	RevalidationError       RevalidationResult = "error"        // any other error; FillResult tells which
)

var revalidationResults = [...]RevalidationResult{RevalidationNotModified, RevalidationETagChanged, RevalidationModified, RevalidationMissing, RevalidationError}

// FillStats is a point-in-time copy of the request layer's counters, which
// never carry a key. InFlight counts running flights, including abandoned
// ones still unwinding; Waiters counts requests waiting on one. Every map
// holds every label.
type FillStats struct {
	InFlight, Waiters int
	Coalesced         uint64 // requests that joined an existing flight
	Bypasses          map[Reason]uint64
	Fills             map[FillResult]uint64
	Revalidations     map[RevalidationResult]uint64
}

// FillEvent describes one finished flight. Duration runs from its creation to
// the publication of its result.
type FillEvent struct {
	Result       FillResult
	Revalidation RevalidationResult // empty without a conditional read
	Duration     time.Duration
}

// Options configure a Cache. Store, Origin and Lifetime are required.
type Options struct {
	Store  *Store
	Origin storage.Reader
	// Config is validated; its rules select each object's policy, and
	// MaxConcurrentFills, MaxWaitersPerKey and FillTimeout bound flights.
	Config config.Cache
	// Prefix is the normalized publication prefix every key carries. Rules
	// match "/" + key without it.
	Prefix string
	// ConditionalReads enables If-None-Match revalidation.
	ConditionalReads bool
	// Now is the freshness clock, time.Now when nil. Fill deadlines always
	// use the real clock.
	Now func() time.Time
	// Lifetime ends at process shutdown. It cancels flights that have not yet
	// committed their result, whose waiters then fail with an error matching
	// both ErrShutdown and storage.Canceled, and makes later requests read
	// the origin directly. Cancel it no earlier than request contexts are
	// cancelled at shutdown, so draining requests are not failed early.
	Lifetime context.Context
	// OnFill, when set, observes every flight once, synchronously from the
	// flight's goroutine after its result is published and outside every
	// lock. It must be fast and safe for concurrent use.
	OnFill func(FillEvent)
}

// Cache is the request-level memory cache over one origin, safe for
// concurrent use. It never retries an origin read and never holds its mutex
// while blocking.
type Cache struct {
	store       *Store
	origin      storage.Reader
	cfg         config.Cache
	prefix      string
	conditional bool
	now         func() time.Time
	lifetime    context.Context
	onFill      func(FillEvent)

	mu            sync.Mutex
	flights       map[string]*flight // joinable flights, at most one per key and MaxConcurrentFills in all
	running       int                // flight goroutines, joinable or abandoned
	waiting       int
	coalesced     uint64
	bypasses      map[Reason]uint64
	fills         map[FillResult]uint64
	revalidations map[RevalidationResult]uint64
}

// New returns a Cache. It copies the configuration, including its rules.
func New(o Options) (*Cache, error) {
	switch {
	case o.Store == nil || o.Origin == nil || o.Lifetime == nil:
		return nil, errors.New("cache: store, origin and lifetime are required")
	case o.Config.MaxConcurrentFills <= 0 || o.Config.MaxWaitersPerKey <= 0 || o.Config.FillTimeout <= 0:
		return nil, errors.New("cache: fill limits must be positive")
	}
	cfg := o.Config
	cfg.Rules = slices.Clone(cfg.Rules)
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		r.Enabled, r.TTL, r.NegativeTTL, r.StaleIfError = clone(r.Enabled), clone(r.TTL), clone(r.NegativeTTL), clone(r.StaleIfError)
		r.TTLMode, r.RequestDirectives = clone(r.TTLMode), clone(r.RequestDirectives)
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	c := &Cache{store: o.Store, origin: o.Origin, cfg: cfg, prefix: o.Prefix, conditional: o.ConditionalReads, now: now, lifetime: o.Lifetime, onFill: o.OnFill,
		flights: map[string]*flight{}, bypasses: map[Reason]uint64{}, fills: map[FillResult]uint64{}, revalidations: map[RevalidationResult]uint64{}}
	for _, r := range reasons {
		c.bypasses[r] = 0
	}
	for _, r := range fillResults {
		c.fills[r] = 0
	}
	for _, r := range revalidationResults {
		c.revalidations[r] = 0
	}
	return c, nil
}

func clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Stats returns a consistent copy of the counters.
func (c *Cache) Stats() FillStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return FillStats{InFlight: c.running, Waiters: c.waiting, Coalesced: c.coalesced,
		Bypasses: maps.Clone(c.bypasses), Fills: maps.Clone(c.fills), Revalidations: maps.Clone(c.revalidations)}
}

// StoreStats returns a consistent copy of the accounting of the Store the
// Cache serves through.
func (c *Cache) StoreStats() Stats { return c.store.Stats() }

func (c *Cache) policy(key string) config.CachePolicy {
	return c.cfg.PolicyFor("/" + strings.TrimPrefix(key, c.prefix))
}

func (c *Cache) bypassed(r Reason) {
	c.mu.Lock()
	c.bypasses[r]++
	c.mu.Unlock()
}

// Lookup is the result of one Get, Head or Range. It owns at most one of a
// pinned Handle (served from memory) and an origin Object (a stream) until
// Release, which is idempotent; TakeObject transfers the Object instead.
// Outcome and Reason are set on error paths too. A Lookup has a single owner.
type Lookup struct {
	Outcome Outcome
	Reason  Reason // set only with OutcomeBypass
	// Coalesced reports that a Get joined a flight another request started,
	// as FillStats.Coalesced counts it. Outcome is still what the flight
	// produced for this request, including a read of its own when another
	// waiter took the flight's stream.
	Coalesced bool
	// Metadata is what to present. It may differ from the Handle's snapshot
	// when a 304 confirmed bytes whose merged metadata was not stored. For an
	// Object it is copied when headers arrive; the Object's own Metadata
	// reports the checksum status after EOF.
	Metadata storage.Metadata
	// RequestTime and ResponseTime are the RFC 9111 request and response
	// times that pair with Metadata, e.g. for httpcache.CurrentAge. Use them
	// rather than the Handle's snapshot times, which belong to the snapshot's
	// own metadata. They are set whenever Metadata is.
	RequestTime, ResponseTime time.Time
	handle                    *Handle
	object                    *storage.Object
}

// served presents snap, pinned by h, with outcome o.
func served(o Outcome, h *Handle, snap Snapshot) *Lookup {
	return &Lookup{Outcome: o, Metadata: snap.Metadata, RequestTime: snap.RequestTime, ResponseTime: snap.ResponseTime, handle: h}
}

// Handle is the pinned snapshot served from memory, or nil. It stays valid
// until Release; Retain it to keep it longer.
func (l *Lookup) Handle() *Handle { return l.handle }

// TakeObject transfers the origin stream, or returns nil if there is none or
// it was taken. The caller must close its Body.
func (l *Lookup) TakeObject() *storage.Object {
	o := l.object
	l.object = nil
	return o
}

// Release unpins the handle and closes an untaken object.
func (l *Lookup) Release() {
	if l.handle != nil {
		l.handle.Release()
		l.handle = nil
	}
	if l.object != nil {
		_ = l.object.Body.Close()
		l.object = nil
	}
}

// lookupAttempts bounds Get's re-reads of an entry that changed while it
// decided to start a flight.
const lookupAttempts = 3

// Get reads key for a GET carrying req. A fresh body entry is a hit and a
// fresh negative one a NotFound without origin I/O. Otherwise the request
// joins the key's flight, starting it if there is none: a revalidation of a
// stale body, or a fill. It waits for the flight within ctx, and leaving
// never cancels a flight other waiters still need. A disabled policy, a
// request no-store, full flight limits or shutdown make it read the origin
// directly within ctx instead, as does an object the flight streams to
// another waiter. Typed storage errors are returned as such, together
// with a non-nil Lookup the caller releases in every case. Only the
// directives of req that the object's request_directives allow apply.
func (c *Cache) Get(ctx context.Context, key string, req httpcache.Request) (*Lookup, error) {
	return c.get(ctx, key, req, false)
}

// get is Get, revalidating even a fresh entry when revalidate is set.
func (c *Cache) get(ctx context.Context, key string, req httpcache.Request, revalidate bool) (*Lookup, error) {
	p := c.policy(key)
	req = applied(p, req, revalidate)
	switch {
	case !p.Enabled:
		return c.direct(ctx, key, storage.ReadOptions{}, ReasonDisabled)
	case req.NoStore:
		return c.direct(ctx, key, storage.ReadOptions{}, ReasonRequestNoStore)
	}
	for attempt := 1; ; attempt++ {
		observed, stale := Absent, (*Handle)(nil)
		if h, ok := c.store.Acquire(key); ok {
			snap := h.Snapshot()
			fresh := c.fresh(h, p, req, snap)
			switch observed = h.Version(); {
			case fresh && snap.Kind == KindBody:
				return served(OutcomeHit, h, snap), nil
			case fresh && snap.Kind == KindNegative:
				h.Release()
				return &Lookup{Outcome: OutcomeNegativeHit}, notFound()
			case snap.Kind == KindBody:
				stale = h
			default: // metadata never satisfies a GET; a stale negative is refetched
				h.Release()
			}
		}
		if ctx.Err() != nil {
			if stale != nil {
				stale.Release()
			}
			return &Lookup{Outcome: OutcomeMiss}, ctxError(ctx)
		}
		f, coalesced, reason, retry := c.join(key, p, observed, stale, attempt < lookupAttempts)
		switch {
		case retry:
			continue
		case f == nil:
			return c.direct(ctx, key, storage.ReadOptions{}, reason)
		}
		l, err := c.wait(ctx, f, req)
		l.Coalesced = coalesced
		return l, err
	}
}

// fresh reports whether snap, pinned by h, is fresh for req under p, and
// marks it expired when it exhausted a positive lifetime (Freshness.Expired),
// which the Store counts once per entry version.
func (c *Cache) fresh(h *Handle, p config.CachePolicy, req httpcache.Request, snap Snapshot) bool {
	f := Evaluate(p, req, snap, c.now())
	if f.Expired() {
		h.MarkExpired()
	}
	return f.State == StateFresh
}

// join registers the caller as a waiter on key's flight, or starts the flight
// with the caller as its first waiter, and takes ownership of stale: the new
// flight keeps it, every other path releases it. coalesced reports that the
// caller joined a flight another request started. With recheck it asks for a
// retry instead of starting a flight when the key's entry is no longer the
// observed one, typically because a flight has just published.
func (c *Cache) join(key string, p config.CachePolicy, observed Version, stale *Handle, recheck bool) (f *flight, coalesced bool, reason Reason, retry bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch f = c.flights[key]; {
	case f != nil && f.waiters >= c.cfg.MaxWaitersPerKey:
		f, reason = nil, ReasonWaitersFull
	case f != nil:
		f.waiters++
		c.waiting++
		c.coalesced++
		coalesced = true
	case recheck && c.peek(key) != observed:
		retry = true
	case c.lifetime.Err() != nil:
		reason = ReasonShutdown
	case c.running >= c.cfg.MaxConcurrentFills:
		reason = ReasonFillsFull
	default:
		return c.start(key, p, observed, stale), false, ReasonNone, false
	}
	if stale != nil {
		stale.Release()
	}
	return f, coalesced, reason, retry
}

func (c *Cache) peek(key string) Version {
	v, _, _ := c.store.Peek(key)
	return v
}

// wait blocks until f publishes or ctx ends. A waiter whose ctx ends leaves
// with its own typed error; only the departure of the last waiter cancels an
// unfinished flight.
func (c *Cache) wait(ctx context.Context, f *flight, req httpcache.Request) (*Lookup, error) {
	select {
	case <-f.done:
		return c.collect(ctx, f, req)
	case <-ctx.Done():
		c.leave(f)
		return &Lookup{Outcome: OutcomeMiss}, ctxError(ctx)
	}
}

// collect gives one waiter its own share of f's published result before it
// leaves: a retained handle; the stream, if it takes it first, or else a
// direct read of its own; the stale entry, if stale_if_error allows it for
// this request's directives; or the typed error.
func (c *Cache) collect(ctx context.Context, f *flight, req httpcache.Request) (*Lookup, error) {
	res := &f.res
	l, err, direct := &Lookup{Outcome: OutcomeMiss}, error(nil), false
	switch {
	case ctx.Err() != nil: // too late for the result, and the stream is left to others
		err = ctxError(ctx)
	case res.handle != nil:
		l = &Lookup{Outcome: res.outcome, Metadata: res.meta, RequestTime: res.requested, ResponseTime: res.responded, handle: res.handle.Retain()}
	case res.object != nil:
		c.mu.Lock()
		taker := !f.taken
		f.taken = true
		if taker {
			c.bypasses[res.reason]++
		}
		c.mu.Unlock()
		direct = !taker
		if taker {
			l = &Lookup{Outcome: OutcomeBypass, Reason: res.reason, Metadata: res.meta, RequestTime: res.requested, ResponseTime: res.responded, object: f.handOff(ctx)}
		}
	case f.stale != nil && StaleIfError(f.policy, req, f.stale.Snapshot(), c.now(), res.err):
		l = served(OutcomeStale, f.stale.Retain(), f.stale.Snapshot())
	default:
		err = res.err
	}
	c.leave(f)
	if direct {
		return c.direct(ctx, f.key, storage.ReadOptions{}, res.reason)
	}
	return l, err
}

// leave removes a waiter. The last one to leave a published flight releases
// what it still owns; the last one to leave an unfinished flight unpublishes
// it, so later requests start afresh, and cancels it.
func (c *Cache) leave(f *flight) {
	c.mu.Lock()
	f.waiters--
	c.waiting--
	last, finished := f.waiters == 0, f.finished
	if last && !finished && c.flights[f.key] == f {
		delete(c.flights, f.key)
	}
	c.mu.Unlock()
	switch {
	case last && finished:
		f.drop()
	case last:
		f.abort(errAbandoned)
	}
}

// direct reads key with opts from the origin within the request's ctx,
// outside the cache.
func (c *Cache) direct(ctx context.Context, key string, opts storage.ReadOptions, reason Reason) (*Lookup, error) {
	c.bypassed(reason)
	l := &Lookup{Outcome: OutcomeBypass, Reason: reason}
	requested := c.now()
	obj, err := c.origin.Get(ctx, key, opts)
	if err != nil {
		return l, err
	}
	l.Metadata, l.RequestTime, l.ResponseTime, l.object = obj.Metadata, requested, c.now(), obj
	return l, nil
}

// Head reads key's metadata for a HEAD carrying req and never fetches a
// body. A fresh body or metadata entry is an OutcomeMetadataHit, a fresh
// negative one a NotFound. Otherwise the request reads origin metadata within its own ctx,
// uncoalesced: a body with the same strong entity-tag is refreshed in place,
// anything else is replaced by a metadata entry, and failures follow the GET
// rules for confirmed absence, denial and stale_if_error. An ambiguous
// absence changes nothing. The returned Lookup has a Handle only when served
// from memory. Only the directives of req that the object's
// request_directives allow apply.
func (c *Cache) Head(ctx context.Context, key string, req httpcache.Request) (*Lookup, error) {
	return c.head(ctx, key, req, false)
}

// head is Head, revalidating even a fresh entry when revalidate is set.
func (c *Cache) head(ctx context.Context, key string, req httpcache.Request, revalidate bool) (*Lookup, error) {
	p := c.policy(key)
	req = applied(p, req, revalidate)
	if !p.Enabled || req.NoStore {
		reason := ReasonDisabled
		if p.Enabled {
			reason = ReasonRequestNoStore
		}
		c.bypassed(reason)
		requested := c.now()
		m, err := c.origin.Head(ctx, key)
		if err != nil {
			return &Lookup{Outcome: OutcomeBypass, Reason: reason}, err
		}
		return &Lookup{Outcome: OutcomeBypass, Reason: reason, Metadata: m, RequestTime: requested, ResponseTime: c.now()}, nil
	}
	h, ok := c.store.Acquire(key)
	observed, snap := Absent, Snapshot{}
	if ok {
		snap, observed = h.Snapshot(), h.Version()
		if c.fresh(h, p, req, snap) {
			if snap.Kind != KindNegative {
				return served(OutcomeMetadataHit, h, snap), nil
			}
			h.Release()
			return &Lookup{Outcome: OutcomeNegativeHit}, notFound()
		}
	}
	requested := c.now()
	m, err := c.origin.Head(ctx, key)
	responded := c.now()
	if err != nil {
		switch {
		case storage.IsKind(err, storage.NotFound):
			c.absent(key, p, observed, responded)
		case storage.IsKind(err, storage.Denied):
			if ok {
				c.store.Remove(key, observed)
			}
		case ok && ctx.Err() == nil && StaleIfError(p, req, snap, c.now(), err):
			return served(OutcomeStale, h, snap), nil
		}
		if ok {
			h.Release()
		}
		return &Lookup{Outcome: OutcomeMiss}, err
	}
	if ok {
		defer h.Release()
	}
	meta := Snapshot{Kind: KindMetadata, Metadata: m, RequestTime: requested, ResponseTime: responded}
	if Admissible(p, httpcache.Request{}, meta) != AdmissionEligible {
		if ok {
			c.store.Remove(key, observed)
		}
		c.bypassed(ReasonUncacheable)
		return &Lookup{Outcome: OutcomeBypass, Reason: ReasonUncacheable, Metadata: m, RequestTime: requested, ResponseTime: responded}, nil
	}
	if ok && snap.Kind == KindBody && httpcache.StrongETag(m.ETag) && m.ETag == snap.Metadata.ETag {
		merged, _ := httpcache.MergeNotModified(snap.Metadata, m) // equal entity-tags cannot conflict
		next := Snapshot{Kind: KindBody, Metadata: merged, RequestTime: requested, ResponseTime: responded}
		if Admissible(p, httpcache.Request{}, next) == AdmissionEligible {
			if r, res := c.store.Refresh(h, next); res == ResultRefreshed {
				return served(OutcomeRevalidated, r, next), nil
			}
		}
		return served(OutcomeMiss, nil, meta), nil
	}
	c.store.Admit(key, observed, meta)
	return served(OutcomeMiss, nil, meta), nil
}

// absent records a confirmed missing object: a negative entry when one may be
// stored, otherwise the removal of the entry expected names.
func (c *Cache) absent(key string, p config.CachePolicy, expected Version, responded time.Time) {
	neg := Snapshot{Kind: KindNegative, ResponseTime: responded}
	if Admissible(p, httpcache.Request{}, neg) == AdmissionEligible {
		c.store.Admit(key, expected, neg)
	} else if expected != Absent {
		c.store.Remove(key, expected)
	}
}

func notFound() error { return &storage.Error{Kind: storage.NotFound, Size: -1} }

// ctxError is the typed error of a request whose own context ended.
func ctxError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &storage.Error{Kind: storage.Timeout, Size: -1}
	}
	return &storage.Error{Kind: storage.Canceled, Size: -1}
}
