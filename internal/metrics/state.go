package metrics

import (
	"errors"

	"github.com/gtriggiano/statica/internal/admin"
	"github.com/gtriggiano/statica/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus"
)

// RegisterReadiness registers the readiness and origin health series of r,
// read from its snapshot at scrape time, so staleness is judged when scraped.
func (m *Metrics) RegisterReadiness(r *admin.Readiness) error {
	if r == nil {
		return errors.New("metrics: a readiness model is required")
	}
	return m.registry.Register(readinessCollector{r})
}

// RegisterLogOutput registers statica_log_events_dropped_total over o.
func (m *Metrics) RegisterLogOutput(o *telemetry.Output) error {
	if o == nil {
		return errors.New("metrics: a log output is required")
	}
	return m.registry.Register(prometheus.NewCounterFunc(prometheus.CounterOpts{Namespace: namespace, Name: "log_events_dropped_total",
		Help: "Log lines dropped by the bounded output: queue or byte budget full, destination write failed, or written after Close."}, func() float64 { return float64(o.Dropped()) }))
}

var (
	healthChecksDesc = newDesc("origin_health_checks_total",
		"Background origin health check outcomes, raw before 403 masking: success, not_found, ambiguous_not_found, denied, throttled, unavailable, configuration, timeout, failed.",
		"result")
	healthLastSuccessDesc = newDesc("origin_health_last_success_timestamp_seconds", "Unix time of the last successful origin health check; 0 before the first.")
	readyDesc             = newDesc("ready", "1 when the instance is ready: serving and, with origin health enabled, its origin healthy and fresh.")
	healthEnabledDesc     = newDesc("origin_health_enabled", "1 when the background origin health check is enabled.")
)

type readinessCollector struct{ r *admin.Readiness }

func (readinessCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- healthChecksDesc
	ch <- healthLastSuccessDesc
	ch <- readyDesc
	ch <- healthEnabledDesc
}

func (c readinessCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.r.Snapshot()
	for result, n := range s.Checks {
		ch <- prometheus.MustNewConstMetric(healthChecksDesc, prometheus.CounterValue, float64(n), string(result))
	}
	last := 0.0
	if !s.LastSuccess.IsZero() {
		last = float64(s.LastSuccess.UnixNano()) / 1e9
	}
	ch <- prometheus.MustNewConstMetric(healthLastSuccessDesc, prometheus.GaugeValue, last)
	ch <- prometheus.MustNewConstMetric(readyDesc, prometheus.GaugeValue, boolValue(s.Ready))
	ch <- prometheus.MustNewConstMetric(healthEnabledDesc, prometheus.GaugeValue, boolValue(s.OriginHealthEnabled))
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
