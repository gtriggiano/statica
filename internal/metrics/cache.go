package metrics

import (
	"errors"

	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/prometheus/client_golang/prometheus"
)

// OnFill observes one finished shared fill in
// statica_cache_fill_duration_seconds; install it as cache.Options.OnFill.
func (m *Metrics) OnFill(e cache.FillEvent) {
	m.fills.get(orUnknown(string(e.Result))).Observe(e.Duration.Seconds())
}

// RegisterCache registers the global cache series of c, read from its
// snapshots at scrape time, and initializes every fill duration series at
// zero. cfg is the validated cache configuration c was built from.
//
// c is nil exactly when the cache is disabled. Then nothing is registered:
// the only cache series is statica_cache_enabled 0, since no request reaches
// a cache and no fill runs. A nil c with an enabled configuration, or a cache
// with a disabled one, is an error.
func (m *Metrics) RegisterCache(c *cache.Cache, cfg config.Cache) error {
	if c == nil {
		if m.cacheEnabled || cfg.Enabled {
			return errors.New("metrics: no cache was registered while the configuration enables it")
		}
		return nil
	}
	if !m.cacheEnabled || !cfg.Enabled {
		return errors.New("metrics: a cache was registered while the configuration disables it")
	}
	if err := m.registry.Register(&cacheCollector{cache: c, cfg: cfg}); err != nil {
		return err
	}
	for result := range c.Stats().Fills {
		m.fills.get(string(result))
	}
	return nil
}

var (
	cacheBytesDesc        = newDesc("cache_bytes", "Charged bytes of resident and retired cache entries.")
	cacheRetiredBytesDesc = newDesc("cache_retired_bytes", "Charged bytes of removed entries still pinned by readers, a subset of cache_bytes.")
	cacheEntriesDesc      = newDesc("cache_entries", "Charged cache entries, resident and retired, metadata and negative entries included.")
	cacheFillBytesDesc    = newDesc("cache_fill_reserved_bytes", "Reserved fill capacity, detached snapshots included.")
	cacheInFlightDesc     = newDesc("cache_fills_in_flight", "Running shared fills, abandoned ones still unwinding included.")
	cacheWaitersDesc      = newDesc("cache_waiters", "Requests waiting on a shared fill.")
	cacheAdmissionsDesc   = newDesc("cache_admissions_total",
		"Cache admission outcomes. Store results are result=admitted|replaced|refreshed|too_large|no_space|invalid|conflict with reason=\"none\"; "+
			"request bypasses are result=\"bypass\" with reason=disabled|request_no_store|shutdown|fills_full|waiters_full|unknown_size|too_large|uncacheable|fill_budget|range.",
		"result", "reason")
	cacheEvictionsDesc     = newDesc("cache_evictions_total", "Capacity evictions by entry kind (body, metadata, negative).", "kind")
	cacheExpirationsDesc   = newDesc("cache_expirations_total", "Entry versions a lookup first found past their positive lifetime, by entry kind.", "kind")
	cacheRevalidationsDesc = newDesc("cache_revalidations_total",
		"Conditional origin reads made by shared fills, by the origin's answer: not_modified (unchanged), etag_changed and modified (replaced), missing (missing), error (failed).",
		"result")
	cacheLimitBytesDesc       = newDesc("cache_limit_bytes", "Configured max_bytes of resident and retired entries.")
	cacheLimitEntriesDesc     = newDesc("cache_limit_entries", "Configured max_entries.")
	cacheFillLimitBytesDesc   = newDesc("cache_fill_limit_bytes", "Configured max_fill_bytes of fill reservations.")
	cacheObjectLimitBytesDesc = newDesc("cache_object_limit_bytes", "Configured max_object_bytes of one admitted object.")
	cacheFillConcurrencyDesc  = newDesc("cache_fill_limit_concurrency", "Configured max_concurrent_fills.")
	cacheWaiterLimitDesc      = newDesc("cache_waiter_limit_per_key", "Configured max_waiters_per_key.")
)

func newDesc(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", name), help, labels, nil)
}

// cacheCollector exports the global cache state, which has no host, from
// Cache.Stats and the Store's Stats. Every counter map in those snapshots
// holds every label, so each series exists at zero from the start.
type cacheCollector struct {
	cache *cache.Cache
	cfg   config.Cache
}

func (c *cacheCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{cacheBytesDesc, cacheRetiredBytesDesc, cacheEntriesDesc, cacheFillBytesDesc, cacheInFlightDesc,
		cacheWaitersDesc, cacheAdmissionsDesc, cacheEvictionsDesc, cacheExpirationsDesc, cacheRevalidationsDesc, cacheLimitBytesDesc,
		cacheLimitEntriesDesc, cacheFillLimitBytesDesc, cacheObjectLimitBytesDesc, cacheFillConcurrencyDesc, cacheWaiterLimitDesc} {
		ch <- d
	}
}

func (c *cacheCollector) Collect(ch chan<- prometheus.Metric) {
	st, fs := c.cache.StoreStats(), c.cache.Stats()
	gauge := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	gauge(cacheBytesDesc, float64(st.ChargedBytes))
	gauge(cacheRetiredBytesDesc, float64(st.RetiredBytes))
	gauge(cacheEntriesDesc, float64(st.ResidentEntries+st.RetiredEntries))
	gauge(cacheFillBytesDesc, float64(st.FillBytes))
	gauge(cacheInFlightDesc, float64(fs.InFlight))
	gauge(cacheWaitersDesc, float64(fs.Waiters))
	for result, n := range st.Admissions {
		counter(cacheAdmissionsDesc, n, string(result), "none")
	}
	for reason, n := range fs.Bypasses {
		counter(cacheAdmissionsDesc, n, "bypass", string(reason))
	}
	for kind, n := range st.Evictions {
		counter(cacheEvictionsDesc, n, string(kind))
	}
	for kind, n := range st.Expirations {
		counter(cacheExpirationsDesc, n, string(kind))
	}
	for result, n := range fs.Revalidations {
		counter(cacheRevalidationsDesc, n, string(result))
	}
	gauge(cacheLimitBytesDesc, float64(st.Limits.MaxBytes))
	gauge(cacheLimitEntriesDesc, float64(st.Limits.MaxEntries))
	gauge(cacheFillLimitBytesDesc, float64(st.Limits.MaxFillBytes))
	gauge(cacheObjectLimitBytesDesc, float64(st.Limits.MaxObjectBytes))
	gauge(cacheFillConcurrencyDesc, float64(c.cfg.MaxConcurrentFills))
	gauge(cacheWaiterLimitDesc, float64(c.cfg.MaxWaitersPerKey))
}
