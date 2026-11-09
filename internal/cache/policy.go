package cache

import (
	"context"
	"errors"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

// The Store never interprets freshness; these pure functions do, for callers
// deciding what a snapshot may satisfy. Callers pass the object's effective
// config.CachePolicy (config.Cache.PolicyFor of the resolved object path), the
// request directives and the clock. Internal freshness decides reuse only: the
// response layer projects downstream headers on its own, so capping the TTL
// by an origin lifetime never changes the policy advertised to clients.

// Admission is the finite outcome of the admission check: AdmissionEligible
// or the reason a snapshot bypasses the cache.
type Admission string

const (
	AdmissionEligible       Admission = "eligible"
	AdmissionDisabled       Admission = "disabled"         // the effective policy is disabled
	AdmissionRequestNoStore Admission = "request_no_store" // the request carries no-store
	AdmissionInvalid        Admission = "invalid"          // unknown snapshot kind
	AdmissionUnstamped      Admission = "unstamped"        // zero ResponseTime, or zero RequestTime on a positive snapshot
	AdmissionMalformed      Admission = "malformed"        // httpcache.Malformed origin metadata
	AdmissionNoStore        Admission = "no_store"         // origin no-store
	AdmissionPrivate        Admission = "private"          // origin private, qualified or not
	AdmissionNegativeTTL    Admission = "negative_ttl"     // a negative entry under a zero negative_ttl
)

// Admissible decides whether s may be stored for req under p. Origin no-cache
// is admissible: every reuse revalidates. A zero TTL is admissible for the
// same reason. A snapshot missing the times its age accounting needs is
// refused rather than stored as never fresh, so a fill that forgets to stamp
// them fails visibly. Admissibility never depends on residency or capacity.
func Admissible(p config.CachePolicy, req httpcache.Request, s Snapshot) Admission {
	a, _ := classify(p, req, s)
	return a
}

// classify is Admissible that also returns the parsed origin directives.
func classify(p config.CachePolicy, req httpcache.Request, s Snapshot) (Admission, httpcache.Directives) {
	switch {
	case !p.Enabled:
		return AdmissionDisabled, httpcache.Directives{}
	case req.NoStore:
		return AdmissionRequestNoStore, httpcache.Directives{}
	case s.Kind != KindBody && s.Kind != KindMetadata && s.Kind != KindNegative:
		return AdmissionInvalid, httpcache.Directives{}
	case s.ResponseTime.IsZero() || s.Kind != KindNegative && s.RequestTime.IsZero():
		return AdmissionUnstamped, httpcache.Directives{}
	case httpcache.Malformed(s.Metadata):
		return AdmissionMalformed, httpcache.Directives{}
	}
	d, _ := httpcache.Parse(s.Metadata.CacheControl)
	switch {
	case d.NoStore:
		return AdmissionNoStore, d
	case d.Private:
		return AdmissionPrivate, d
	case s.Kind == KindNegative && p.NegativeTTL <= 0:
		return AdmissionNegativeTTL, d
	}
	return AdmissionEligible, d
}

// State is the finite outcome of evaluating a stored snapshot for reuse.
type State string

const (
	StateFresh  State = "fresh"  // reusable as is
	StateStale  State = "stale"  // revalidate or refetch before use
	StateBypass State = "bypass" // neither reuse nor admit: disabled policy or request no-store
)

// Freshness evaluates one snapshot at one instant. Age is the RFC 9111
// current age of a positive snapshot, or the time since a negative one was
// received. Lifetime is the effective freshness lifetime. Remaining is
// Lifetime minus Age while fresh and zero otherwise: a hit reports what is
// left, never a renewed lifetime.
type Freshness struct {
	State                    State
	Age, Lifetime, Remaining time.Duration
}

// Expired reports whether the snapshot exhausted a positive lifetime: its age
// has reached it. A snapshot with a zero lifetime (origin no-cache, a zero
// TTL) revalidates on every reuse by policy and never expires; neither does a
// snapshot still within its lifetime whose revalidation a request forces, nor
// a bypass.
func (f Freshness) Expired() bool {
	return f.State == StateStale && f.Lifetime > 0 && f.Age >= f.Lifetime
}

// Evaluate decides whether stored snapshot s may satisfy req at now under p.
// It is fresh iff its lifetime exceeds its age and req does not force
// revalidation: request no-cache or max-age=0 always does, and max-age=N does
// once the age in whole seconds, as an Age header would state it, exceeds N. Body and metadata-only snapshots live for the TTL,
// capped by an origin lifetime but never extended by one; origin no-cache or a
// zero TTL makes the lifetime zero. A negative snapshot lives for
// negative_ttl from its response time. A snapshot that is not admissible,
// which a correct caller never stores, is stale with zero lifetime.
func Evaluate(p config.CachePolicy, req httpcache.Request, s Snapshot, now time.Time) Freshness {
	f, _, _ := evaluate(p, req, s, now)
	return f
}

// evaluate is Evaluate that also returns the admission outcome and the parsed
// origin directives.
func evaluate(p config.CachePolicy, req httpcache.Request, s Snapshot, now time.Time) (Freshness, Admission, httpcache.Directives) {
	a, d := classify(p, req, s)
	switch a {
	case AdmissionEligible:
	case AdmissionDisabled, AdmissionRequestNoStore:
		return Freshness{State: StateBypass}, a, d
	default:
		return Freshness{State: StateStale}, a, d
	}
	f := Freshness{State: StateStale}
	if s.Kind == KindNegative {
		f.Age, f.Lifetime = max(now.Sub(s.ResponseTime), 0), p.NegativeTTL
	} else {
		f.Age, f.Lifetime = httpcache.CurrentAge(s.Metadata, s.RequestTime, s.ResponseTime, now), lifetime(p, d, s)
	}
	if f.Lifetime > f.Age && !forced(req, f.Age) {
		f.State, f.Remaining = StateFresh, f.Lifetime-f.Age
	}
	return f, a, d
}

// lifetime is a positive snapshot's effective freshness lifetime.
func lifetime(p config.CachePolicy, d httpcache.Directives, s Snapshot) time.Duration {
	if d.NoCache || p.TTL <= 0 {
		return 0
	}
	if origin, ok := httpcache.Lifetime(d, s.Metadata, s.ResponseTime); ok {
		return min(p.TTL, origin)
	}
	return p.TTL
}

// forced reports whether req refuses a stored response of the given age
// without revalidation.
func forced(req httpcache.Request, age time.Duration) bool {
	return req.NoCache || req.MaxAge.Present && (req.MaxAge.Seconds == 0 || int64(age/time.Second) > req.MaxAge.Seconds)
}

// StaleIfError reports whether s, whose revalidation or refetch failed with
// cause at now, may still be served under p. Only a positive snapshot
// qualifies, and only when p.StaleIfError is positive, s is stale by its own
// lifetime and its staleness (age minus lifetime) is at most p.StaleIfError.
// Origin must-revalidate, proxy-revalidate, no-cache and s-maxage (which
// implies proxy-revalidate, RFC 9111 §5.2.2.10) forbid it, as does a request
// that forces revalidation or carries no-store. Origin stale-if-error
// directives (RFC 5861) are ignored: statica's configuration alone governs
// stale service. Serving stale never renews the snapshot's lifetime.
func StaleIfError(p config.CachePolicy, req httpcache.Request, s Snapshot, now time.Time, cause error) bool {
	if p.StaleIfError <= 0 || s.Kind != KindBody && s.Kind != KindMetadata || !staleCause(cause) {
		return false
	}
	f, a, d := evaluate(p, req, s, now)
	if a != AdmissionEligible || f.Age < f.Lifetime || forced(req, f.Age) ||
		d.MustRevalidate || d.ProxyRevalidate || d.NoCache || d.SMaxAge.Present {
		return false
	}
	return f.Age-f.Lifetime <= p.StaleIfError
}

// staleCause reports whether cause is an origin outage that stale_if_error
// covers: a timeout, exhausted throttling, a provider 5xx or an exhausted
// transport. A storage.Error decides by its kind alone, so a denial, a
// confirmed or ambiguous absence, a local validation failure or a cancellation
// never qualifies, even when joined with a deadline.
func staleCause(cause error) bool {
	var e *storage.Error
	if errors.As(cause, &e) {
		return e.Kind == storage.Timeout || e.Kind == storage.Throttled || e.Kind == storage.Unavailable
	}
	return errors.Is(cause, context.DeadlineExceeded)
}

// Validator returns the If-None-Match value for revalidating positive
// snapshot s: its ETag when that is a valid entity-tag and provider
// conditional reads are enabled. A weak tag qualifies because If-None-Match
// uses weak comparison. Empty means revalidation must fetch a new snapshot.
func Validator(s Snapshot, conditionalReads bool) string {
	if !conditionalReads || s.Kind != KindBody && s.Kind != KindMetadata || !httpcache.ValidETag(s.Metadata.ETag) {
		return ""
	}
	return s.Metadata.ETag
}
