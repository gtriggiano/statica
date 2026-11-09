// Package admin serves statica's private administration listener (liveness,
// readiness and metrics) and runs the optional origin health worker that
// readiness consults.
//
// Probes and scrapes only inspect in-memory state; they never call the
// origin. Liveness is local: it holds while the process runs, including
// during drain and origin outages. Readiness combines the process phase with
// the raw origin health results the Worker records, and evaluates staleness
// whenever it is read, so a stopped or blocked worker cannot keep an instance
// ready.
package admin

import (
	"maps"
	"sync"
	"time"

	"github.com/gtriggiano/statica/internal/config"
)

// Phase is the process lifecycle phase. Phases only advance: initializing,
// then serving, then draining, which is final.
type Phase string

const (
	PhaseInitializing Phase = "initializing" // constructing dependencies or binding listeners
	PhaseServing      Phase = "serving"      // initialized, with both listeners bound
	PhaseDraining     Phase = "draining"     // shutdown has begun
)

// Reason is the finite cause of not being ready, ReasonNone when ready.
type Reason string

const (
	ReasonNone            Reason = ""
	ReasonInitializing    Reason = "initializing"     // the phase is initializing
	ReasonDraining        Reason = "draining"         // the phase is draining
	ReasonOriginUnknown   Reason = "origin_unknown"   // origin health is enabled and no check has succeeded yet
	ReasonOriginUnhealthy Reason = "origin_unhealthy" // failure_threshold consecutive failures, not yet recovered
	ReasonOriginStale     Reason = "origin_stale"     // the last success is older than max_staleness
)

// Result is the finite raw outcome of one origin health check, classified
// before D05 masking: only a Head that returns metadata succeeds, and every
// error, including confirmed absence, is a failure.
type Result string

const (
	ResultSuccess           Result = "success"
	ResultNotFound          Result = "not_found"
	ResultAmbiguousNotFound Result = "ambiguous_not_found"
	ResultDenied            Result = "denied" // a raw 403, even when the public response masks it as 404
	ResultThrottled         Result = "throttled"
	ResultUnavailable       Result = "unavailable"
	ResultConfiguration     Result = "configuration"
	ResultTimeout           Result = "timeout" // the check's own deadline or an origin timeout
	ResultFailed            Result = "failed"  // any other error
)

var results = [...]Result{ResultSuccess, ResultNotFound, ResultAmbiguousNotFound, ResultDenied, ResultThrottled, ResultUnavailable, ResultConfiguration, ResultTimeout, ResultFailed}

// originState is the threshold state of enabled origin health. Staleness is
// not a state: it is judged against the clock whenever readiness is read.
type originState string

const (
	originUnknown   originState = "unknown" // no success yet
	originHealthy   originState = "healthy"
	originUnhealthy originState = "unhealthy"
)

// Snapshot is a point-in-time copy of the readiness state, for metrics.
// Checks holds every Result label, all zero while origin health is disabled.
// LastSuccess is when the last successful check completed, zero before the
// first.
type Snapshot struct {
	Phase               Phase
	Ready               bool
	Reason              Reason
	OriginHealthEnabled bool
	Checks              map[Result]uint64
	LastSuccess         time.Time
}

// Readiness is the process readiness model, safe for concurrent use. Ready
// means the phase is PhaseServing and, when origin health is enabled, the
// origin is healthy and its last success is no older than max_staleness.
//
// Enabled origin health starts unknown, so readiness stays false until a first
// success, which alone makes the origin healthy. Thereafter failure_threshold
// consecutive failures make it unhealthy and success_threshold consecutive
// successes recover it.
type Readiness struct {
	enabled bool
	health  config.OriginHealth
	now     func() time.Time

	mu                  sync.Mutex
	phase               Phase
	origin              originState
	successes, failures int // consecutive check results
	lastSuccess         time.Time
	checks              map[Result]uint64
}

// NewReadiness returns a model in PhaseInitializing. h is the validated
// admin.origin_health section; nil or disabled leaves readiness to the phase
// alone. Enabled origin health becomes ready only through a started Worker.
func NewReadiness(h *config.OriginHealth) *Readiness {
	r := &Readiness{now: time.Now, phase: PhaseInitializing, origin: originUnknown, checks: make(map[Result]uint64, len(results))}
	if h != nil && h.Enabled {
		r.enabled, r.health = true, *h
	}
	for _, res := range results {
		r.checks[res] = 0
	}
	return r
}

// SetServing marks initialization complete and both listeners bound. It has
// no effect once draining.
func (r *Readiness) SetServing() { r.advance(PhaseServing) }

// SetDraining marks shutdown as begun. Draining is final.
func (r *Readiness) SetDraining() { r.advance(PhaseDraining) }

func (r *Readiness) advance(p Phase) {
	r.mu.Lock()
	if r.phase != PhaseDraining {
		r.phase = p
	}
	r.mu.Unlock()
}

// Ready reports whether the process is ready and, if not, why.
func (r *Readiness) Ready() (bool, Reason) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status(now)
}

// Snapshot returns a consistent copy of the state, judged at the current time.
func (r *Readiness) Snapshot() Snapshot {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	ready, reason := r.status(now)
	return Snapshot{Phase: r.phase, Ready: ready, Reason: reason, OriginHealthEnabled: r.enabled, Checks: maps.Clone(r.checks), LastSuccess: r.lastSuccess}
}

// status must be called with mu held. Phase reasons take precedence over
// origin reasons, and a reported unhealthy origin over a stale one.
func (r *Readiness) status(now time.Time) (bool, Reason) {
	switch {
	case r.phase == PhaseInitializing:
		return false, ReasonInitializing
	case r.phase == PhaseDraining:
		return false, ReasonDraining
	case !r.enabled:
		return true, ReasonNone
	case r.origin == originUnknown:
		return false, ReasonOriginUnknown
	case r.origin == originUnhealthy:
		return false, ReasonOriginUnhealthy
	case now.Sub(r.lastSuccess) > r.health.MaxStaleness:
		return false, ReasonOriginStale
	}
	return true, ReasonNone
}

// record applies the raw result of one completed check at time at and returns
// the origin state before and after it.
func (r *Readiness) record(res Result, at time.Time) (from, to originState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks[res]++
	from = r.origin
	if res == ResultSuccess {
		r.lastSuccess, r.failures = at, 0
		r.successes++
		if r.origin == originUnknown || r.origin == originUnhealthy && r.successes >= r.health.SuccessThreshold {
			r.origin = originHealthy
		}
	} else {
		r.successes = 0
		r.failures++
		if r.origin == originHealthy && r.failures >= r.health.FailureThreshold {
			r.origin = originUnhealthy
		}
	}
	return from, r.origin
}
