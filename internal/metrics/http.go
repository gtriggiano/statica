package metrics

import (
	"net/http"
	"strconv"

	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

// coalesced is the cache_requests_total outcome of a request that joined a
// fill another request started, whatever outcome the fill produced for it.
const coalesced = "coalesced"

// unknownHost labels an admitted record that carries no host, a boundary
// defect. Unlike the "unknown" fallback of the other labels it must not be a
// value a real request can produce: "unknown" is itself a valid hostname, and
// without an allowlist a request for it is labelled host="unknown". The
// boundary's normalized hosts only use [a-z0-9.-] or IP literals, so the
// underscore keeps this series distinct.
const unknownHost = "_unknown"

// exchangeKey identifies the series of one admitted request shape; exchangeSeries
// holds them, so a warm request resolves its common series with one lookup.
type exchangeKey struct {
	host, method string
	status       int
	outcome      string
}

type exchangeSeries struct {
	requests prometheus.Counter
	duration prometheus.Observer
	bytes    prometheus.Counter
}

// hostLabel keys a series labelled by host and one other finite label.
type hostLabel struct{ host, label string }

// RecordSink updates the request metrics from one finalized request record;
// install it, alongside any other consumer, with httpserver.WithRecordSink.
// A rejected record only counts under http_rejected_requests_total, by its
// fixed reason. An admitted record updates the host-labelled HTTP series,
// stream_failures_total when its outcome is incomplete, and the request-level
// cache series when it reached the cache. httpserver sets Host on every
// admitted record; one without counts under host="_unknown" (see
// unknownHost), so a boundary defect stays visible in a single finite series. It reads no target, key,
// address or request ID, and does not retain r.
func (m *Metrics) RecordSink(r *telemetry.Record) {
	if r.Rejected {
		m.rejections.get(orUnknown(r.Reason)).Inc()
		return
	}
	host := r.Host
	if host == "" {
		host = unknownHost
	}
	s := m.exchanges.get(exchangeKey{host: host, method: method(r.Method), status: r.Status, outcome: orUnknown(r.Outcome)})
	s.requests.Inc()
	s.duration.Observe(r.Duration().Seconds())
	s.bytes.Add(float64(max(r.BytesWritten, 0)))
	if ttfb, ok := r.TTFB(); ok {
		m.ttfb.get(host).Observe(ttfb.Seconds())
	}
	if r.Outcome == telemetry.OutcomeIncomplete {
		// The termination reason is set for every content and generated
		// response failure; a handler panic or an abort without one is
		// classified by the boundary reason.
		reason := r.TerminationReason
		if reason == "" {
			reason = orUnknown(r.Reason)
		}
		m.failures.get(hostLabel{host, reason}).Inc()
	}
	if r.CacheOutcome != "" {
		outcome := r.CacheOutcome
		if r.CacheCoalesced {
			outcome = coalesced
		}
		m.cacheRequests.get(hostLabel{host, outcome}).Inc()
		m.cacheBytes.get(host).Add(float64(max(r.CacheBytes, 0)))
	}
}

// HTTPObserver maintains statica_http_requests_in_flight; install it with
// httpserver.WithObserver. It counts start and finish events, which come in
// pairs for every acquired request slot, rather than setting the Inflight
// snapshot, which concurrent requests may deliver out of order.
func (m *Metrics) HTTPObserver(o httpserver.Observation) {
	switch o.Event {
	case httpserver.ObservationStart:
		m.inflight.Inc()
	case httpserver.ObservationFinish:
		m.inflight.Dec()
	}
}

// method normalizes a request method to GET, HEAD, OPTIONS or OTHER.
func method(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return m
	}
	return "OTHER"
}

func statusLabel(status int) string { return strconv.Itoa(status) }

// orUnknown guards finite labels against an unexpected empty value, which
// Prometheus would not distinguish from an absent label.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
