package cache

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

// ErrShutdown matches the error of a request whose flight the Cache lifetime
// cancelled. That error is also a storage.Canceled.
var ErrShutdown = errors.New("cache: shutdown")

var (
	errFillTimeout = errors.New("cache: fill timeout")
	errAbandoned   = errors.New("cache: every waiter left")
	errFillDone    = errors.New("cache: fill finished")
)

// shutdownError is a storage.Canceled caused by the end of the Cache lifetime.
type shutdownError struct{}

func (shutdownError) Error() string { return "origin: canceled: cache shutdown" }
func (shutdownError) Unwrap() []error {
	return []error{ErrShutdown, &storage.Error{Kind: storage.Canceled, Size: -1}}
}

// flight is the single fill or revalidation of one key: one goroutine running
// execute and the requests waiting for it.
//
// Its origin context belongs to no request. A bounded phase cancels it with a
// cause on fill_timeout, measured from creation and never extended, on
// lifetime cancellation, or when the last waiter leaves. The phase ends once
// the result is complete, before any store update, and before an origin
// stream is handed to a waiter, which from then on owns the stream within its
// own request context alone: fill timers never cut a long download. Ending
// the phase judges completion against the deadline on the real clock, so a
// result completed at or after it is a timeout even when the timer has not
// fired yet: the outcome does not depend on timer scheduling.
type flight struct {
	key      string
	policy   config.CachePolicy
	expected Version // the key's version when the flight started; Absent for none
	stale    *Handle // the stale body being revalidated or refetched, or nil; released by drop

	ctx      context.Context
	cancel   context.CancelCauseFunc
	started  time.Time
	deadline time.Time
	timer    *time.Timer
	unhook   func() bool // stops lifetime cancellation
	mu       sync.Mutex  // guards ended
	ended    bool

	done chan struct{} // closed once res is published
	res  result

	// Guarded by Cache.mu.
	waiters  int  // current waiters, the initiator included
	finished bool // res is published
	taken    bool // a waiter took res.object
}

// result is what a flight publishes. Its handle and object belong to the
// flight until drop; each waiter takes its own share.
type result struct {
	fill         FillResult
	revalidation RevalidationResult // empty without a conditional read
	outcome      Outcome            // of a waiter served handle
	handle       *Handle            // indexed, detached, refreshed or the stale bytes confirmed by a 304
	meta         storage.Metadata   // presented with handle or object
	requested    time.Time          // the RFC 9111 request time meta pairs with
	responded    time.Time          // the RFC 9111 response time meta pairs with
	object       *storage.Object    // an origin stream for the first waiter to take it
	reason       Reason             // why object streams
	err          error              // typed; each waiter may still serve stale
	notFound     bool               // the origin answered NotFound, whether or not the flight could apply it
}

// start registers and launches a flight whose first waiter is the caller.
// Its origin reads, fills and revalidations alike, carry
// storage.PurposeCacheFill. It must be called with c.mu held.
func (c *Cache) start(key string, p config.CachePolicy, expected Version, stale *Handle) *flight {
	ctx, cancel := context.WithCancelCause(storage.WithPurpose(context.WithoutCancel(c.lifetime), storage.PurposeCacheFill))
	now := time.Now()
	f := &flight{key: key, policy: p, expected: expected, stale: stale, ctx: ctx, cancel: cancel,
		started: now, deadline: now.Add(c.cfg.FillTimeout), done: make(chan struct{}), waiters: 1}
	f.timer = time.AfterFunc(c.cfg.FillTimeout, func() { f.abort(errFillTimeout) })
	f.unhook = context.AfterFunc(c.lifetime, func() { f.abort(ErrShutdown) })
	c.flights[key] = f
	c.running++
	c.waiting++
	go c.run(f)
	return f
}

// abort cancels the flight with cause unless its phase has ended.
func (f *flight) abort(cause error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended {
		f.cancel(cause)
	}
}

// end ends the phase: afterwards nothing but the flight's owner cancels its
// context. It returns the typed cause when the phase was cancelled first,
// including by a deadline its timer has not yet reported: a fill timeout is a
// storage.Timeout, a shutdown a storage.Canceled matching ErrShutdown, and a
// departure of every waiter a plain storage.Canceled.
func (f *flight) end() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.ended {
		if !time.Now().Before(f.deadline) {
			f.cancel(errFillTimeout)
		}
		f.ended = true
		f.timer.Stop()
		f.unhook()
	}
	switch {
	case f.ctx.Err() == nil:
		return nil
	case errors.Is(context.Cause(f.ctx), errFillTimeout):
		return &storage.Error{Kind: storage.Timeout, Size: -1}
	case errors.Is(context.Cause(f.ctx), ErrShutdown):
		return shutdownError{}
	}
	return &storage.Error{Kind: storage.Canceled, Size: -1}
}

// run executes f and publishes its result. Whoever sees the last waiter gone
// once the result is published, run or leave, drops what f still owns. The
// fill observer runs last, outside every lock, before f stops counting as
// running.
func (c *Cache) run(f *flight) {
	res := c.execute(f)
	if res.object == nil {
		f.cancel(errFillDone) // a handed-off stream keeps the context until it closes
	}
	c.mu.Lock()
	f.res, f.finished = res, true
	if c.flights[f.key] == f {
		delete(c.flights, f.key)
	}
	c.fills[res.fill]++
	if res.revalidation != "" {
		c.revalidations[res.revalidation]++
	}
	close(f.done)
	idle := f.waiters == 0
	c.mu.Unlock()
	if idle {
		f.drop()
	}
	if c.onFill != nil {
		c.onFill(FillEvent{Result: res.fill, Revalidation: res.revalidation, Duration: time.Since(f.started)})
	}
	c.mu.Lock()
	c.running--
	c.mu.Unlock()
}

// drop releases what f still owns once no waiter remains: its result handle,
// its stale handle and a stream nobody took.
func (f *flight) drop() {
	if f.res.handle != nil {
		f.res.handle.Release()
	}
	if f.stale != nil {
		f.stale.Release()
	}
	if f.res.object != nil && !f.taken {
		_ = f.res.object.Body.Close()
		f.cancel(errFillDone)
	}
}

// handOff gives f's stream to the waiter that took it, bound to that waiter's
// request: the end of ctx closes it, as does Close. The flight issued the
// origin GET, a storage.PurposeCacheFill operation, but the bytes now serve
// one request, so a storage.PurposeSetter body is retagged PurposeRequest
// before anything reads it. The Object keeps its identity, so a checksum
// verified at EOF still shows in its Metadata.
func (f *flight) handOff(ctx context.Context) *storage.Object {
	obj := f.res.object
	if p, ok := obj.Body.(storage.PurposeSetter); ok {
		p.SetPurpose(storage.PurposeRequest)
	}
	b := &handedBody{body: obj.Body, done: func() { f.cancel(errFillDone) }}
	b.stop = context.AfterFunc(ctx, func() { _ = b.close() })
	obj.Body = b
	return obj
}

type handedBody struct {
	body io.ReadCloser
	once sync.Once
	stop func() bool
	done func()
}

func (b *handedBody) Read(p []byte) (int, error) { return b.body.Read(p) }

// Close closes the origin body once and returns its error to the call that
// closed it, which may instead be the end of the taker's request.
func (b *handedBody) Close() error {
	b.stop()
	return b.close()
}

func (b *handedBody) close() (err error) {
	b.once.Do(func() {
		err = b.body.Close()
		b.done()
	})
	return err
}

// execute performs f's origin read and store update: a revalidation when the
// stale body has a usable validator, otherwise an unconditional fill.
func (c *Cache) execute(f *flight) result {
	if f.stale != nil {
		if etag := Validator(f.stale.Snapshot(), c.conditional); etag != "" {
			return c.revalidate(f, etag)
		}
	}
	return c.fetch(f)
}

func (c *Cache) fetch(f *flight) result {
	requested := c.now()
	obj, err := c.origin.Get(f.ctx, f.key, storage.ReadOptions{})
	if err != nil {
		if storage.IsKind(err, storage.NotModified) { // impossible without a condition
			err = &storage.Error{Kind: storage.InvalidResponse, Size: -1}
		}
		return c.failed(f, err, c.now())
	}
	return c.receive(f, obj, requested, c.now())
}

// revalidate asks the origin whether the stale body changed. A 304 refreshes
// its metadata in place without a body download, or serves the confirmed
// bytes with the merged metadata when that cannot be stored; a 304 with
// another entity-tag costs one unconditional refetch within the same flight;
// a 200 is a changed object, buffered or streamed like any other.
func (c *Cache) revalidate(f *flight, etag string) result {
	requested := c.now()
	obj, err := c.origin.Get(f.ctx, f.key, storage.ReadOptions{IfNoneMatch: etag})
	responded := c.now()
	var nm *storage.Error
	switch {
	case err == nil:
		res := c.receive(f, obj, requested, responded)
		res.revalidation = RevalidationModified
		return res
	case !errors.As(err, &nm) || nm.Kind != storage.NotModified:
		res := c.failed(f, err, responded)
		res.revalidation = RevalidationError
		if res.notFound {
			res.revalidation = RevalidationMissing
		}
		return res
	}
	var fields storage.Metadata
	if nm.Metadata != nil {
		fields = *nm.Metadata
	}
	merged, err := httpcache.MergeNotModified(f.stale.Snapshot().Metadata, fields)
	if err != nil { // httpcache.ErrETagChanged
		res := c.fetch(f)
		res.revalidation = RevalidationETagChanged
		if res.notFound {
			res.revalidation = RevalidationMissing
		}
		return res
	}
	if err := f.end(); err != nil {
		res := c.failed(f, err, responded)
		res.revalidation = RevalidationNotModified
		return res
	}
	res := result{revalidation: RevalidationNotModified, outcome: OutcomeRevalidated, meta: merged, requested: requested, responded: responded}
	next := Snapshot{Kind: KindBody, Metadata: merged, RequestTime: requested, ResponseTime: responded}
	if Admissible(f.policy, httpcache.Request{}, next) != AdmissionEligible {
		c.store.Remove(f.key, f.expected)
	} else if h, r := c.store.Refresh(f.stale, next); r == ResultRefreshed {
		res.fill, res.handle = FillRefreshed, h
		return res
	}
	res.fill, res.handle = FillUnstored, f.stale.Retain()
	return res
}

// receive buffers an object eligible for memory and promotes it, or hands it
// off as a stream when its size is unknown or above max_object_bytes, its
// metadata may not be stored, or the fill budget has no room for it. A stale
// body the flight refetched is removed only when the origin now forbids
// storing the object; a size or capacity limit keeps it indexed with its
// expiry, since only a complete successful fetch replaces an entry.
func (c *Cache) receive(f *flight, obj *storage.Object, requested, responded time.Time) result {
	snap := Snapshot{Kind: KindBody, Metadata: obj.Metadata, RequestTime: requested, ResponseTime: responded}
	size := obj.Metadata.Size
	var r *Reservation
	reason := ReasonNone
	switch {
	case size < 0:
		reason = ReasonUnknownSize
	case size > c.store.limits.MaxObjectBytes:
		reason = ReasonTooLarge
	case Admissible(f.policy, httpcache.Request{}, snap) != AdmissionEligible:
		reason = ReasonUncacheable
	default:
		var res Result
		if r, res = c.store.Reserve(size); res != ResultAdmitted {
			reason = ReasonFillBudget
		}
	}
	if reason != ReasonNone {
		if err := f.end(); err != nil {
			_ = obj.Body.Close()
			return c.failed(f, err, responded)
		}
		if f.stale != nil && reason == ReasonUncacheable {
			c.store.Remove(f.key, f.expected)
		}
		return result{fill: FillStreamed, object: obj, reason: reason, meta: obj.Metadata, requested: requested, responded: responded}
	}
	body, err := readExactly(obj.Body, size)
	if err == nil && obj.Metadata.Checksum == storage.ChecksumPending { // EOF must have verified it
		err = &storage.Error{Kind: storage.ChecksumMismatch, Size: -1}
	}
	if err == nil {
		err = f.end()
	}
	if err != nil {
		r.Release()
		return c.failed(f, err, responded)
	}
	snap.Metadata, snap.Body = obj.Metadata, body // after EOF: the checksum status is final
	expected := f.expected
	if v, kind, ok := c.store.Peek(f.key); ok && kind == KindMetadata && v != expected {
		expected = v // a complete body supersedes metadata a concurrent HEAD stored
	}
	h, res := c.store.Promote(f.key, expected, r, snap)
	out := result{fill: FillAdmitted, outcome: OutcomeMiss, handle: h, meta: snap.Metadata, requested: requested, responded: responded}
	switch res {
	case ResultAdmitted, ResultReplaced:
	case ResultInvalid:
		return result{fill: FillFailed, err: &storage.Error{Kind: storage.InvalidResponse, Size: -1}}
	default:
		out.fill = FillDetached
	}
	return out
}

// readExactly reads exactly size bytes, confirms the body ends there and
// closes it. A short or long body is an invalid response. A partially read
// body is never retried.
func readExactly(body io.ReadCloser, size int64) ([]byte, error) {
	defer body.Close()
	buf := make([]byte, size)
	if _, err := io.ReadFull(body, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
		}
		return nil, err
	}
	var probe [1]byte
	switch n, err := io.ReadFull(body, probe[:]); {
	case n > 0:
		return nil, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	case !errors.Is(err, io.EOF):
		return nil, err
	}
	return buf, nil
}

// failed ends the phase and publishes err typed, or the cancellation cause
// when the phase was cancelled first. A confirmed absence or a denial
// invalidates the entry the flight started from; any other failure leaves it
// indexed with its expiry for each waiter's stale_if_error decision. The
// result records an origin NotFound even when the cause replaced it.
func (c *Cache) failed(f *flight, err error, responded time.Time) result {
	notFound := storage.IsKind(err, storage.NotFound)
	if cause := f.end(); cause != nil {
		err = cause
	} else {
		err = typed(err)
	}
	fill := FillFailed
	switch {
	case storage.IsKind(err, storage.NotFound):
		fill = FillNotFound
		c.absent(f.key, f.policy, f.expected, responded)
	case storage.IsKind(err, storage.Denied):
		fill = FillDenied
		if f.expected != Absent {
			c.store.Remove(f.key, f.expected)
		}
	case storage.IsKind(err, storage.Timeout):
		fill = FillTimeout
	case storage.IsKind(err, storage.Canceled):
		fill = FillCanceled
	}
	return result{fill: fill, err: err, notFound: notFound}
}

// typed keeps an error carrying a storage.Error and classifies anything else
// a Reader returns without exposing its text.
func typed(err error) error {
	var e *storage.Error
	switch {
	case errors.As(err, &e):
		return err
	case errors.Is(err, io.ErrUnexpectedEOF):
		return &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	case errors.Is(err, context.DeadlineExceeded):
		return &storage.Error{Kind: storage.Timeout, Size: -1}
	case errors.Is(err, context.Canceled):
		return &storage.Error{Kind: storage.Canceled, Size: -1}
	}
	return &storage.Error{Kind: storage.Unavailable, Size: -1}
}
