// Package telemetry carries the per-request telemetry record and turns
// records and diagnostics into structured logfmt events.
//
// # Request records
//
// When a record sink is installed, the HTTP boundary (httpserver) creates one
// Record per request, stores it in the request context with WithRecord,
// fills the boundary fields, and delivers it once, finalized, to the sink.
// The content layer fills the object fields of the record it finds with
// RecordFrom. Access events (AccessLogger) and request metrics
// (metrics.RecordSink) are derived from the same record, so they cannot
// disagree.
//
// # Output
//
// Handler encodes each event synchronously into one logfmt line and hands it
// to Output, a fixed-capacity queue drained by a single writer goroutine.
// Enqueueing never blocks: when the queue is full, by events or by bytes, the
// event is dropped and counted (Output.Dropped), and once the writer makes
// progress again it writes a rate-bounded log_events_dropped warning. This is
// a deliberate trade-off. The observability contract forbids an unbounded
// asynchronous log queue, and serving requests matters more than a complete
// log: a stalled stdout costs log lines, never request latency beyond the
// enqueue. Output.Close drains the queue within a deadline for shutdown.
package telemetry

import (
	"context"
	"net/netip"
	"time"
)

// Outcome values shared with the content layer's observation.
const (
	OutcomeSuccess       = "success"
	OutcomeOriginFailure = "origin_failure"
	OutcomeIncomplete    = "incomplete"
)

// Record is the mutable telemetry of one request. It is owned by the request
// goroutine: httpserver and content write it while serving, and httpserver
// delivers it to its sink after the response finished. A sink may read it
// during the call and must not retain it. Shared cache fills never see a
// Record, so one visitor's request ID cannot reach another's record.
//
// Method, Authority and the target fields are raw request input and must be
// bounded and escaped before output; AccessLogger does. Addresses and request
// IDs are attribution data and must never become metric labels.
type Record struct {
	// Filled by httpserver.

	Method string
	// Host is the normalized hostname, set once host admission passed.
	// Authority is the raw request authority, set only when it did not.
	Host, Authority string
	// EscapedPath, RawQuery and ForceQuery are the original target, before
	// index or SPA resolution. EscapedPath is empty when the target could
	// not be parsed.
	EscapedPath, RawQuery string
	ForceQuery            bool
	RequestID             string
	RequestIDSource       string // proxy or generated
	RequestIDReason       string // fixed reason a trusted proxy's ID was not used
	ClientIP, PeerIP      netip.Addr
	ClientIPSource        string
	AttributionReason     string // fixed reason a forwarded client IP was not used
	// Started is when the boundary received the request, Admitted when it
	// acquired a request slot (zero on overload), FirstByte when the final
	// status was committed (zero if never) and Finished when the response
	// ended.
	Started, Admitted, FirstByte, Finished time.Time
	Status                                 int   // committed status; 0 if none
	BytesWritten                           int64 // body bytes actually written, partial transfers included
	Rejected                               bool  // rejected before ordinary serving; Reason says why
	// Reason is the boundary classification: a rejection reason, redirect,
	// cors_preflight, handler_panic, or response_aborted for an aborted
	// response that had no other classification.
	Reason string

	// Filled by content for object-backed requests. httpserver completes
	// Outcome and TerminationReason for the responses it generates itself:
	// Outcome is success or, when aborted, incomplete, except that a
	// rejected request never has one, even when its rejection response
	// failed; TerminationReason then still says why it failed.

	OriginKey         string // exact selected key, only once object headers were committed
	Resolution        string
	Outcome           string
	TerminationReason string // finite D13 reason
	// ErrorClass is the finite class every origin_failure carries: an
	// origin_<storage kind> such as origin_denied, origin_inconsistent,
	// origin_timeout, origin_canceled or origin_error.
	ErrorClass      string
	CacheOutcome    string
	CacheReason     string
	CacheCoalesced  bool
	CacheBytes      int64
	CacheDiagnostic string
}

// Duration is the full response or abort duration.
func (r *Record) Duration() time.Duration { return r.Finished.Sub(r.Started) }

// TTFB is the time until the response was committed, if it was.
func (r *Record) TTFB() (time.Duration, bool) {
	if r.FirstByte.IsZero() {
		return 0, false
	}
	return r.FirstByte.Sub(r.Started), true
}

type recordKey struct{}

// WithRecord returns a context carrying r.
func WithRecord(ctx context.Context, r *Record) context.Context {
	return context.WithValue(ctx, recordKey{}, r)
}

// RecordFrom returns the record ctx carries, or nil.
func RecordFrom(ctx context.Context) *Record {
	r, _ := ctx.Value(recordKey{}).(*Record)
	return r
}
