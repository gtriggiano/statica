// Package content prepares immutable object responses and streams selected
// representations with bounded memory. Admission and path validation live in
// httpserver; provider I/O and active origin-read timers live in storage.
package content

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/site"
	"github.com/gtriggiano/statica/internal/storage"
	"github.com/gtriggiano/statica/internal/telemetry"
)

type Outcome string

const (
	OutcomeSuccess       Outcome = "success"
	OutcomeOriginFailure Outcome = "origin_failure"
	OutcomeIncomplete    Outcome = "incomplete"
)

type TerminationReason string

const (
	ReasonNone               TerminationReason = ""
	ReasonPreparationTimeout TerminationReason = "preparation_timeout"
	ReasonOriginReadTimeout  TerminationReason = "origin_read_timeout"
	ReasonClientWriteTimeout TerminationReason = "client_write_timeout"
	ReasonClientWriteFailure TerminationReason = "client_write_failure"
	ReasonTotalDeadline      TerminationReason = "total_deadline"
	ReasonDisconnect         TerminationReason = "disconnect"
	ReasonShutdown           TerminationReason = "shutdown"
	ReasonReadFailure        TerminationReason = "origin_read_failure"
	ReasonChecksumMismatch   TerminationReason = "checksum_mismatch"
	ReasonLengthMismatch     TerminationReason = "length_mismatch"
	ReasonOriginFailure      TerminationReason = "origin_failure"
)

// Observation contains only selected object identity and bounded diagnostics.
// OriginKey is populated when object headers are committed, including HEAD/304
// and interrupted transfers, never for an unsuccessful origin candidate.
//
// With WithCache, CacheOutcome and CacheReason classify the lookup that
// decided the response, for the access log's cache field and
// cache_requests_total. A HEAD answered from memory is
// cache.OutcomeMetadataHit, never cache.OutcomeHit. CacheCoalesced reports
// that this lookup joined a cache fill another request started; CacheOutcome
// is then what that fill produced for this request. When resolution
// selected an object, that is its resolver lookup: for a successful GET,
// HEAD, 304, 412, range or 416, for a SPA shell or custom 404, and for the
// index lookup behind a trailing-slash redirect. A guarded origin range read
// that follows a cold selection does not replace it. Otherwise, as for a
// built-in 404 or an origin failure, it is the request's last lookup, so a
// built-in 404 whose last fallback candidate was a negative entry reports
// cache.OutcomeNegativeHit. They are empty without a cache or a lookup.
// CacheBytes counts the body bytes, whole or sliced, written from a snapshot
// the request reused from the internal cache (a hit, revalidation or stale
// service), for cache_response_bytes_total. It is zero when the request's own
// fill supplied the bytes (a miss), and for HEAD, 304, 412, generated bodies
// and origin streams. CacheDiagnostic reports malformed origin cache metadata.
type Observation struct {
	OriginKey         string
	Resolution        site.Resolution
	Status            int
	Bytes             int64
	Outcome           Outcome
	TerminationReason TerminationReason
	CacheDiagnostic   string
	CacheOutcome      cache.Outcome
	CacheReason       cache.Reason
	CacheCoalesced    bool
	CacheBytes        int64
}

type HandlerOption func(*handler)

// Error classes recorded on the request's telemetry.Record. Every origin
// failure carries one: origin_<kind> for a storage.Error of that kind;
// origin_inconsistent when the object kept changing between its selection
// and its guarded range read; origin_timeout whenever the preparation or
// total deadline ended the request; origin_canceled when a shutdown or a
// cancellation ended it without a storage error; origin_error for any other error; and
// missing_metadata when the handler runs without the HTTP boundary. A
// client disconnect before the response headers is not an origin failure.
const (
	classInconsistent    = "origin_inconsistent"
	classTimeout         = "origin_timeout"
	classCanceled        = "origin_canceled"
	classError           = "origin_error"
	classMissingMetadata = "missing_metadata"
)

// failOrigin marks o as an origin failure ending with reason, and gives the
// request's record class unless an earlier failure of the request set one.
func failOrigin(r *http.Request, o *Observation, reason TerminationReason, class string) {
	o.Outcome, o.TerminationReason = OutcomeOriginFailure, reason
	if record := telemetry.RecordFrom(r.Context()); record != nil && record.ErrorClass == "" {
		record.ErrorClass = class
	}
}

// failureClass classifies an origin failure ended by a deadline as a
// timeout, whatever error the interrupted read returned; any other failure
// by err, else by its reason.
func failureClass(err error, reason TerminationReason) string {
	var origin *storage.Error
	switch {
	case reason == ReasonPreparationTimeout || reason == ReasonTotalDeadline:
		return classTimeout
	case errors.As(err, &origin):
		return "origin_" + string(origin.Kind)
	case reason == ReasonShutdown || reason == ReasonDisconnect:
		return classCanceled
	}
	return classError
}

// fill copies the observation into the request's telemetry record.
func (o *Observation) fill(r *telemetry.Record) {
	r.OriginKey, r.Resolution = o.OriginKey, string(o.Resolution)
	r.Outcome, r.TerminationReason = string(o.Outcome), string(o.TerminationReason)
	r.CacheOutcome, r.CacheReason, r.CacheCoalesced = string(o.CacheOutcome), string(o.CacheReason), o.CacheCoalesced
	r.CacheBytes, r.CacheDiagnostic = o.CacheBytes, o.CacheDiagnostic
}

// WithObserver installs a prompt, concurrency-safe synchronous callback.
func WithObserver(observer func(Observation)) HandlerOption {
	return func(h *handler) { h.observe = observer }
}

// WithCache serves objects through c, which must be built over the
// handler's origin reader: resolution reads through cache.NewReader(c)
// instead, and each request's Cache-Control directives travel in its context.
// A selected body held in memory, including one this request has just
// filled, is a complete snapshot: it answers single ranges by slicing it,
// whatever the provider range and conditional-read capabilities, while
// streamed objects keep the uncached range behaviour. Accept-Ranges still
// follows the provider capabilities alone, as without a cache, so that it
// never depends on residency. A nil c leaves the handler uncached.
func WithCache(c *cache.Cache) HandlerOption {
	return func(h *handler) { h.memory = c }
}

type handler struct {
	origin                        storage.Reader
	reader                        storage.Reader
	resolver                      *site.Resolver
	memory                        *cache.Cache
	cache                         config.Cache
	cacheControl                  config.CacheControl
	features                      config.Features
	accessDenied                  string
	preparation, writeIdle, total time.Duration
	observe                       func(Observation)
}

// NewHandler constructs the content boundary. Wrap it with httpserver.NewHandler
// before exposing it: that boundary supplies validated target and admission time.
// Configuration is copied, including pointer-valued cache policy rules.
// When the request context carries a telemetry.Record, the handler fills
// its object fields from the request's Observation, before the observer
// runs, and the ErrorClass of every origin failure.
func NewHandler(cfg *config.Config, reader storage.Reader, options ...HandlerOption) (http.Handler, error) {
	if cfg == nil || reader == nil {
		return nil, fmt.Errorf("content requires configuration and origin reader")
	}
	if cfg.Server.ResponseHeaderTimeout <= 0 || cfg.Server.WriteIdleTimeout <= 0 || cfg.Server.RequestTimeout < 0 {
		return nil, fmt.Errorf("content requires valid timeout budgets")
	}
	h := &handler{origin: reader, reader: reader, cache: cfg.Cache, cacheControl: cfg.CacheControl,
		features: cfg.S3.Features, accessDenied: cfg.Errors.AccessDenied,
		preparation: cfg.Server.ResponseHeaderTimeout, writeIdle: cfg.Server.WriteIdleTimeout, total: cfg.Server.RequestTimeout}
	h.cache.Rules = append([]config.CacheRule(nil), cfg.Cache.Rules...)
	for i := range h.cache.Rules {
		r := &h.cache.Rules[i]
		if r.Enabled != nil {
			value := *r.Enabled
			r.Enabled = &value
		}
		if r.TTL != nil {
			value := *r.TTL
			r.TTL = &value
		}
		if r.NegativeTTL != nil {
			value := *r.NegativeTTL
			r.NegativeTTL = &value
		}
		if r.StaleIfError != nil {
			value := *r.StaleIfError
			r.StaleIfError = &value
		}
	}
	h.cacheControl.Rules = append([]config.CacheControlRule(nil), cfg.CacheControl.Rules...)
	for _, option := range options {
		if option != nil {
			option(h)
		}
	}
	if h.memory != nil {
		h.reader = cache.NewReader(h.memory)
	}
	resolver, err := site.NewResolver(cfg, h.reader)
	if err != nil {
		return nil, err
	}
	h.resolver = resolver
	return h, nil
}

var errPreparation = errors.New("response preparation timeout")

// A phase timer cancels origin preparation, but is disarmed atomically at final
// headers. Unlike context.WithTimeout for preparation, it cannot later cancel a
// successfully prepared long-running body.
type preparationPhase struct {
	mu       sync.Mutex
	finished bool
	timer    *time.Timer
	ctx      context.Context
	cancel   context.CancelCauseFunc
	deadline time.Time
}

func startPreparation(parent context.Context, deadline time.Time) *preparationPhase {
	ctx, cancel := context.WithCancelCause(parent)
	p := &preparationPhase{ctx: ctx, cancel: cancel, deadline: deadline}
	p.timer = time.AfterFunc(time.Until(deadline), func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.finished {
			cancel(errPreparation)
		}
	})
	return p
}

func (p *preparationPhase) commit() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.finished && !time.Now().Before(p.deadline) {
		p.cancel(errPreparation)
	}
	p.finished = true
	p.timer.Stop()
	return p.ctx.Err()
}

func (p *preparationPhase) close() {
	p.mu.Lock()
	p.finished = true
	p.timer.Stop()
	p.mu.Unlock()
	p.cancel(context.Canceled)
}

// Every body has one ownership scope. Cancellation closes it to release a
// blocked read even when a Reader implementation relies on Close for unblocking.
type ownedBody struct {
	body io.ReadCloser
	once sync.Once
	stop func() bool
}

func ownBody(ctx context.Context, body io.ReadCloser) *ownedBody {
	b := &ownedBody{body: body}
	// A body resident in memory never blocks a read, so it needs no
	// asynchronous Close: it stays pinned until released, which lets transfer
	// write its bytes in place.
	if _, resident := body.(*cache.Body); body != nil && !resident {
		b.stop = context.AfterFunc(ctx, b.close)
	}
	return b
}
func (b *ownedBody) close() {
	if b != nil && b.body != nil {
		b.once.Do(func() { _ = b.body.Close() })
	}
}
func (b *ownedBody) release() {
	if b != nil {
		if b.stop != nil {
			b.stop()
		}
		b.close()
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	observation := Observation{Outcome: OutcomeSuccess}
	record := telemetry.RecordFrom(r.Context())
	defer func() {
		if record != nil {
			observation.fill(record)
		}
		if h.observe != nil {
			h.observe(observation)
		}
	}()
	metadata, ok := httpserver.MetadataFromContext(r.Context())
	if !ok || metadata.Object == nil {
		failOrigin(r, &observation, ReasonNone, classMissingMetadata)
		h.generated(w, r, http.StatusInternalServerError, &observation, nil)
		return
	}
	start := metadata.AdmittedAt
	if start.IsZero() {
		start = time.Now()
	}
	ctx := r.Context()
	var totalDeadline time.Time
	if inherited, ok := ctx.Deadline(); ok {
		totalDeadline = inherited
	}
	if h.total > 0 {
		configured := start.Add(h.total)
		if totalDeadline.IsZero() || configured.Before(totalDeadline) {
			totalDeadline = configured
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, totalDeadline)
		defer cancel()
	}
	if h.memory != nil {
		ctx = cache.WithRequest(ctx, httpcache.RequestDirectives(r.Header))
	}
	phase := startPreparation(ctx, start.Add(h.preparation))
	defer phase.close()
	ctx = phase.ctx
	req := site.Request{Target: metadata.Target, Method: r.Method, Header: r.Header}
	for revision := 0; revision < 2; revision++ {
		selection, err := h.resolver.Resolve(ctx, req)
		if h.memory != nil {
			key := ""
			if err == nil && selection != nil {
				key = selection.Key
			}
			record, _ := cache.LastLookup(ctx, key)
			observation.CacheOutcome, observation.CacheReason, observation.CacheCoalesced = record.Outcome, record.Reason, record.Coalesced
		}
		if err != nil {
			h.originError(w, r, ctx, err, &observation, phase)
			return
		}
		if selection == nil {
			h.originError(w, r, ctx, &storage.Error{Kind: storage.InvalidResponse}, &observation, phase)
			return
		}
		body := ownBody(ctx, selection.Body)
		// Each iteration releases its snapshot before a guarded retry. The final
		// body's deferred release also runs on ErrAbortHandler.
		if selection.Status == http.StatusPermanentRedirect {
			body.release()
			observation.Resolution = selection.Resolution
			vary(w.Header(), selection)
			w.Header().Set("Location", selection.Location)
			h.generated(w, r, selection.Status, &observation, phase)
			return
		}
		if selection.Key == "" {
			body.release()
			observation.Resolution = selection.Resolution
			vary(w.Header(), selection)
			h.generated(w, r, selection.Status, &observation, phase)
			return
		}
		status := selection.Status
		if status == http.StatusOK {
			if conditional := precondition(r.Header, selection.Metadata); conditional != 0 {
				status = conditional
			}
		}
		// A body in memory is a complete snapshot and is sliced locally: its
		// metadata and bytes are one pinned revision, so no guard is needed.
		// Like origin ranges, it still requires a strong entity-tag.
		// Accept-Ranges is advertised from provider capabilities alone.
		_, local := selection.Body.(*cache.Body)
		local = local && selection.Metadata.Size >= 0 && httpcache.StrongETag(selection.Metadata.ETag)
		advertise := h.features.RangeReads && h.features.ConditionalReads && selection.RangeCapable && httpcache.StrongETag(selection.Metadata.ETag)
		var span byteRange
		sliced := false
		if status == http.StatusOK && r.Method != http.MethodHead && (local || advertise) && requestIfRangeMatches(r.Header, selection.Metadata) {
			br, valid, satisfiable := parseRange(strings.Join(r.Header.Values("Range"), ","), selection.Metadata.Size)
			if valid && !satisfiable {
				body.release()
				observation.Resolution = selection.Resolution
				vary(w.Header(), selection)
				w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(selection.Metadata.Size, 10))
				h.generated(w, r, http.StatusRequestedRangeNotSatisfiable, &observation, phase)
				return
			}
			if valid && local {
				span, sliced = br, true
				selection.Metadata.Size, selection.Metadata.ContentRange = br.length(), br.contentRange()
				status = http.StatusPartialContent
			} else if valid {
				body.release()
				object, rangeErr := h.origin.Get(ctx, selection.Key, storage.ReadOptions{Range: br.request(), IfMatch: selection.Metadata.ETag})
				if rangeErr != nil {
					if object != nil && object.Body != nil {
						_ = object.Body.Close()
					}
					if storage.IsKind(rangeErr, storage.Precondition) {
						if revision == 0 {
							continue
						}
						failOrigin(r, &observation, ReasonOriginFailure, classInconsistent)
						h.generated(w, r, http.StatusServiceUnavailable, &observation, phase)
						return
					}
					h.originError(w, r, ctx, rangeErr, &observation, phase)
					return
				}
				if object == nil || object.Body == nil || object.Metadata.Size != br.length() || object.Metadata.ContentRange != br.contentRange() || object.Metadata.ETag != selection.Metadata.ETag {
					if object != nil && object.Body != nil {
						_ = object.Body.Close()
					}
					h.originError(w, r, ctx, &storage.Error{Kind: storage.InvalidResponse}, &observation, phase)
					return
				}
				coding := strings.ToLower(strings.TrimSpace(object.Metadata.ContentEncoding))
				if coding == "" || coding == "identity" {
					coding = "identity"
					if selection.ObjectPath != selection.LogicalPath && object.Metadata.ContentEncoding == "" {
						coding = selection.Coding
					}
				}
				if coding != selection.Coding || !requestIfRangeMatches(r.Header, object.Metadata) {
					_ = object.Body.Close()
					if revision == 0 {
						continue
					}
					failOrigin(r, &observation, ReasonOriginFailure, classInconsistent)
					h.generated(w, r, http.StatusServiceUnavailable, &observation, phase)
					return
				}
				selection.Body = object.Body
				// If-Match guards representation bytes, not metadata-only updates.
				// All response metadata therefore comes from the actual ranged GET.
				selection.Metadata = object.Metadata
				if coding == "identity" {
					selection.Metadata.ContentEncoding = ""
				} else {
					selection.Metadata.ContentEncoding = coding
				}
				if selection.ObjectPath == selection.LogicalPath {
					selection.ContentType = contentType(object.Metadata.ContentType, selection.LogicalPath)
				}
				body = ownBody(ctx, object.Body)
				status = http.StatusPartialContent
				if conditional := precondition(r.Header, selection.Metadata); conditional != 0 {
					status = conditional
				}
			}
		}
		if r.Method != http.MethodHead && status != http.StatusNotModified && status != http.StatusPreconditionFailed && body.body == nil {
			body.release()
			h.originError(w, r, ctx, &storage.Error{Kind: storage.InvalidResponse}, &observation, phase)
			return
		}
		defer body.release()
		observation.CacheDiagnostic = h.objectHeaders(w.Header(), selection, metadata.Target.Path, time.Now())
		// Ranges apply only to the object's full and partial representations.
		if advertise && (status == http.StatusOK || status == http.StatusPartialContent) {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		if status == http.StatusPartialContent {
			w.Header().Set("Content-Range", selection.Metadata.ContentRange)
		}
		if status == http.StatusNotModified {
			w.Header().Del("Content-Length")
			w.Header().Del("Content-Type")
			w.Header().Del("Content-Encoding")
		}
		if status == http.StatusPreconditionFailed {
			// Preconditions have no object body and must not claim its length.
			w.Header().Set("Content-Length", "0")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Del("Expires")
		}
		if err := phase.commit(); err != nil {
			// Object headers prepared above belong to an uncommitted snapshot.
			// Discard them before generating the deadline response.
			for _, name := range []string{"Content-Length", "Content-Type", "Content-Encoding", "Content-Disposition", "Content-Language", "ETag", "Last-Modified", "Cache-Control", "Expires", "Age", "Date", "Accept-Ranges", "Content-Range", "Vary"} {
				w.Header().Del(name)
			}
			h.originError(w, r, ctx, err, &observation, nil)
			return
		}
		observation.OriginKey = selection.Key
		observation.Resolution = selection.Resolution
		observation.Status = status
		if err := h.commit(w, status, totalDeadline); err != nil {
			h.abort(ctx, &observation, err, true)
		}
		if r.Method == http.MethodHead || status == http.StatusNotModified || status == http.StatusPreconditionFailed {
			return
		}
		if resident, ok := body.body.(*cache.Body); ok {
			data := resident.Bytes()
			if sliced {
				data = data[span.start : span.end+1]
			}
			err = h.transferResident(ctx, w, data, selection.Metadata.Size, totalDeadline, &observation)
			if observation.CacheOutcome == cache.OutcomeHit || observation.CacheOutcome == cache.OutcomeRevalidated || observation.CacheOutcome == cache.OutcomeStale {
				observation.CacheBytes = observation.Bytes
			}
		} else {
			err = h.transfer(ctx, w, body.body, selection.Metadata.Size, totalDeadline, &observation)
		}
		if err != nil {
			h.abort(ctx, &observation, err, false)
		}
		return
	}
}

func (h *handler) originError(w http.ResponseWriter, r *http.Request, ctx context.Context, err error, o *Observation, p *preparationPhase) {
	status := http.StatusBadGateway
	reason := ReasonOriginFailure
	failed := true
	var input *site.Error
	switch {
	case errors.Is(context.Cause(ctx), errPreparation):
		status = http.StatusGatewayTimeout
		reason = ReasonPreparationTimeout
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		reason = ReasonTotalDeadline
	case storage.IsKind(err, storage.Timeout) || errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		reason = ReasonPreparationTimeout
	case errors.Is(context.Cause(ctx), httpserver.ErrShutdown) || errors.Is(err, cache.ErrShutdown):
		status = http.StatusServiceUnavailable
		reason = ReasonShutdown
	case errors.Is(ctx.Err(), context.Canceled) || storage.IsKind(err, storage.Canceled):
		if r.Context().Err() != nil {
			disconnected(o)
		}
		// Not expected: only the client's own cancellation reaches here.
		// Kept as a 502 origin failure rather than a silent abort.
		reason = ReasonDisconnect
	case storage.IsKind(err, storage.Denied):
		if h.accessDenied != "bad_gateway" {
			status = http.StatusNotFound
		}
	case storage.IsKind(err, storage.NotFound):
		status = http.StatusNotFound
		failed = false
	case storage.IsKind(err, storage.Throttled):
		status = http.StatusServiceUnavailable
	case errors.As(err, &input):
		status = input.Status
		failed = false
		if status == http.StatusNotAcceptable {
			mergeVary(w.Header(), "Accept-Encoding")
			o.Resolution = input.Resolution
		}
	}
	if failed {
		failOrigin(r, o, reason, failureClass(err, reason))
	} else {
		o.Outcome, o.TerminationReason = OutcomeSuccess, ReasonNone
	}
	h.generated(w, r, status, o, p)
}

func (h *handler) generated(w http.ResponseWriter, r *http.Request, status int, o *Observation, p *preparationPhase) {
	if p != nil {
		if err := p.commit(); err != nil {
			// The prepared response is replaced, and so is its Vary.
			w.Header().Del("Vary")
			status = http.StatusGatewayTimeout
			reason := ReasonDisconnect
			if errors.Is(context.Cause(p.ctx), errPreparation) {
				reason = ReasonPreparationTimeout
			} else if errors.Is(p.ctx.Err(), context.DeadlineExceeded) {
				reason = ReasonTotalDeadline
			} else if errors.Is(context.Cause(p.ctx), httpserver.ErrShutdown) {
				status = http.StatusServiceUnavailable
				reason = ReasonShutdown
			} else if r.Context().Err() != nil {
				disconnected(o)
			}
			// Otherwise unreachable: the phase is canceled only by its
			// deadline, a shutdown or the client going away.
			failOrigin(r, o, reason, failureClass(nil, reason))
		}
	}
	if status < 200 {
		status = http.StatusBadGateway
	}
	if status != http.StatusPermanentRedirect {
		w.Header().Del("Location")
	}
	if status != http.StatusRequestedRangeNotSatisfiable {
		w.Header().Del("Content-Range")
	}
	body := http.StatusText(status) + "\n"
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Expires")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	o.Status = status
	// A pre-header deadline should still deliver a bounded 504 when writable;
	// its expired preparation/total deadline must not poison this last write.
	if err := h.commit(w, status, time.Time{}); err != nil {
		h.abort(r.Context(), o, err, true)
	}
	if r.Method != http.MethodHead {
		if err := h.write(w, []byte(body), time.Time{}, o); err != nil {
			h.abort(r.Context(), o, err, true)
		}
	}
}

// disconnected ends a request whose client went away before any response
// headers: nothing is sent, and the request is incomplete with no status.
func disconnected(o *Observation) {
	o.Status, o.Outcome, o.TerminationReason = 0, OutcomeIncomplete, ReasonDisconnect
	panic(http.ErrAbortHandler)
}

func (h *handler) writeDeadline(total time.Time) time.Time {
	deadline := time.Now().Add(h.writeIdle)
	if !total.IsZero() && total.Before(deadline) {
		deadline = total
	}
	return deadline
}

func (h *handler) commit(w http.ResponseWriter, status int, total time.Time) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(h.writeDeadline(total)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	w.WriteHeader(status)
	err := controller.Flush()
	if errors.Is(err, http.ErrNotSupported) {
		err = nil
	}
	if err != nil {
		return err
	}
	if err = controller.SetWriteDeadline(time.Time{}); errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (h *handler) write(w http.ResponseWriter, p []byte, total time.Time, o *Observation) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(h.writeDeadline(total)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	n, err := w.Write(p)
	o.Bytes += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = controller.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	if err != nil {
		return &writeError{err}
	}
	if err = controller.SetWriteDeadline(time.Time{}); errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

type writeError struct{ error }

func (e *writeError) Unwrap() error { return e.error }

var errLength = errors.New("origin length mismatch")

// transferChunk bounds each body write.
const transferChunk = 32 * 1024

func (h *handler) transfer(ctx context.Context, w http.ResponseWriter, body io.Reader, length int64, total time.Time, o *Observation) error {
	buffer := make([]byte, transferChunk)
	var read int64
	empty := 0
	for {
		// Once every announced byte is written, the client has the whole
		// response and the last read only confirms the end of the origin
		// body: a client that leaves meanwhile does not make it incomplete.
		complete := length >= 0 && read == length
		if err := ctx.Err(); err != nil && !(complete && clientGone(ctx)) {
			return err
		}
		n, err := body.Read(buffer)
		if n < 0 || n > len(buffer) {
			return &storage.Error{Kind: storage.InvalidResponse}
		}
		if n > 0 {
			empty = 0
			read += int64(n)
			if length >= 0 && read > length {
				return errLength
			}
			if writeErr := h.write(w, buffer[:n], total, o); writeErr != nil {
				return writeErr
			}
		} else if err == nil {
			empty++
			if empty >= 100 {
				return io.ErrNoProgress
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// The client's departure closes the origin body, which fails
				// that last read as canceled. With n > 0 it returned above.
				if complete && clientGone(ctx) && (storage.IsKind(err, storage.Canceled) || errors.Is(err, context.Canceled)) {
					return nil
				}
				return err
			}
			if length >= 0 && read != length {
				return errLength
			}
			return nil
		}
	}
}

// clientGone reports whether ctx ended because its client went away:
// canceled, and not by a shutdown. A deadline ends it as DeadlineExceeded.
func clientGone(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled) && !errors.Is(context.Cause(ctx), httpserver.ErrShutdown)
}

// transferResident writes bytes already in memory in place, in the chunks
// transfer would write them, with the same write deadlines, accounting and
// errors, but without a buffer or a copy. Once the last byte is written it
// returns at once, whatever ends ctx then: unlike transfer, no end of an
// origin body remains to confirm.
func (h *handler) transferResident(ctx context.Context, w http.ResponseWriter, data []byte, length int64, total time.Time, o *Observation) error {
	if int64(len(data)) != length {
		return errLength
	}
	for {
		if len(data) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(len(data), transferChunk)
		if err := h.write(w, data[:n], total, o); err != nil {
			return err
		}
		data = data[n:]
	}
}

func (h *handler) abort(ctx context.Context, o *Observation, err error, writing bool) {
	o.Outcome = OutcomeIncomplete
	var write *writeError
	var timeout interface{ Timeout() bool }
	deadline, hasDeadline := ctx.Deadline()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || hasDeadline && !time.Now().Before(deadline):
		o.TerminationReason = ReasonTotalDeadline
	case (writing || errors.As(err, &write)) && errors.As(err, &timeout) && timeout.Timeout():
		o.TerminationReason = ReasonClientWriteTimeout
	case errors.Is(context.Cause(ctx), httpserver.ErrShutdown):
		o.TerminationReason = ReasonShutdown
	case errors.Is(ctx.Err(), context.Canceled):
		o.TerminationReason = ReasonDisconnect
	case writing || errors.As(err, &write):
		o.TerminationReason = ReasonClientWriteFailure
	case storage.IsKind(err, storage.Timeout):
		o.TerminationReason = ReasonOriginReadTimeout
	case storage.IsKind(err, storage.ChecksumMismatch):
		o.TerminationReason = ReasonChecksumMismatch
	case errors.Is(err, errLength):
		o.TerminationReason = ReasonLengthMismatch
	default:
		o.TerminationReason = ReasonReadFailure
	}
	panic(http.ErrAbortHandler)
}
