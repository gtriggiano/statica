// Package app runs the statica serving process. Run constructs every
// component from a validated configuration before binding any socket, binds
// the public and admin listeners, serves until a shutdown trigger, and then
// drains within the single server.shutdown_timeout budget.
//
// # Startup
//
// Construction order: logger and log output, metrics, the origin client (and,
// with origin health enabled, a dedicated single-attempt health client), the
// memory cache, the content handler, the public handler and server,
// readiness, the health worker and the admin server. Credential and region
// discovery is bounded by server.response_header_timeout, the budget
// check-origin also uses for its origin access. A construction failure logs
// startup_failed with its stage, releases what was built and binds nothing.
//
// Both listeners are then bound synchronously, public first. If either bind
// fails, the other listener is closed and Run fails. Only once both are
// bound does readiness become serving and the health worker start, so
// readiness is never claimed on elapsed time. The startup event follows,
// with the actual bound addresses.
//
// A signal received before both listeners are bound aborts the startup: it
// cancels construction, nothing stays bound, startup_aborted reason=signal
// is logged and the logs are flushed. A context cancelled before the
// listeners are bound aborts it the same way, with reason=context. A genuine
// construction or bind failure takes precedence over a racing signal: it is
// logged as startup_failed, with signal_pending=true when a signal was
// received too, and fails Run.
//
// # Shutdown
//
// A shutdown signal, an unexpected listener failure or the cancellation of
// Run's context starts one shutdown, measured on a fresh context with the
// total budget B = server.shutdown_timeout and its tail T = min(B/4, 1s):
//
//  1. Readiness becomes draining (/readyz 503, /livez still 200) and
//     shutdown_started is logged with its reason: signal, listener_failure
//     or context.
//  2. The public server stops accepting and drains active requests until
//     B - T, while the health worker stops, bounded by the same deadline.
//     At that point the requests still active are counted and cancelled
//     with httpserver.ErrShutdown, whatever their own duration limits: long
//     downloads may be cut by design. A shutdown that is itself delayed past
//     that point, even past B, still counts as a budget expiry, though its
//     count of cancelled requests is then best effort.
//  3. The cache lifetime ends once the public server has cancelled its
//     request contexts (httpserver.Server.Done), never before, so a
//     draining request is never failed by a cancelled fill.
//  4. Until B - T/2, Run waits for cancelled requests to unwind, so that
//     their access events are logged.
//  5. Until B - T/4, the admin server, which stayed up so that
//     orchestrators see the drain, shuts down; the origin clients close.
//  6. shutdown_complete is logged with the outcome, drained or forced (warn,
//     with forced_by and cancelled_requests), and the log output is flushed
//     in the remaining T/4 or more.
//
// Every step is bounded by a deadline at or before B, so Run returns within
// the budget plus scheduling delays even when request handlers or the origin
// ignore cancellation. Any signal received while shutting down forces an
// immediate close: both servers close, every remaining step is cancelled,
// and the log output gets at most 250ms to flush what fits. A listener that
// fails while shutting down is logged as listener_failed, as before; the
// exit status still follows the cause of the shutdown.
//
// # Exit status
//
// Run returns nil, exit status 0, for a shutdown initiated by a signal or by
// its context, even when the drain was forced at budget expiry or by a
// second signal (the forced outcome is logged at warn), and for a startup
// aborted by a signal without any construction or bind failure. It returns
// an error, exit status 1, for runtime failures, even when a signal was
// pending: component or origin client construction, a bind failure, an
// unexpected listener failure that started the shutdown, or a context
// cancelled before the listeners are bound. Configuration errors, exit
// status 2, are the caller's to detect before Run: config.Load and
// (*config.Config).ValidateTLS.
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gtriggiano/statica/internal/admin"
	"github.com/gtriggiano/statica/internal/cache"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/content"
	"github.com/gtriggiano/statica/internal/httpserver"
	"github.com/gtriggiano/statica/internal/metrics"
	"github.com/gtriggiano/statica/internal/storage"
	origins3 "github.com/gtriggiano/statica/internal/storage/s3"
	"github.com/gtriggiano/statica/internal/telemetry"
)

// Shutdown and startup abort reasons logged by shutdown_started,
// shutdown_complete and startup_aborted.
const (
	ReasonSignal          = "signal"
	ReasonListenerFailure = "listener_failure"
	ReasonContext         = "context"
)

const (
	maxTail          = time.Second
	forcedFlushGrace = 250 * time.Millisecond
	unwindPoll       = 5 * time.Millisecond
)

// errAborted ends a startup that a signal stopped; Run reports success.
var errAborted = errors.New("startup aborted by a signal")

// Options supplies the process dependencies of Run.
type Options struct {
	// Logs receives the logfmt event stream, normally stdout. Nil discards.
	Logs io.Writer
	// Build identifies the binary in the startup event and
	// statica_build_info. Empty fields use local development defaults.
	Build metrics.BuildInfo
	// Signals delivers shutdown signals: one received during startup aborts
	// it, the first once serving starts a graceful shutdown, and any signal
	// received while shutting down forces an immediate close. Nil leaves
	// shutdown to the context and listener failures.
	Signals <-chan os.Signal
	// OriginOptions are appended to the options of every origin client,
	// after the metrics observer; tests supply static credentials with them.
	OriginOptions []origins3.Option
	// Listen binds a listener; it is called for the public listener, then
	// for the admin listener. Nil uses net.Listen.
	Listen func(network, address string) (net.Listener, error)
}

// Run serves cfg, a loaded and validated configuration, until shutdown. See
// the package documentation for the startup and shutdown sequences and the
// exit status of each result.
func Run(ctx context.Context, cfg *config.Config, opts Options) error {
	if cfg == nil {
		return errors.New("a configuration is required")
	}
	watch := watchStartup(ctx, opts.Signals)
	defer watch.finish()
	a, err := build(ctx, cfg, opts, watch)
	if err == nil {
		err = a.run(ctx)
	}
	if errors.Is(err, errAborted) {
		return nil
	}
	return err
}

// startupWatch records a signal received before both listeners are bound,
// which aborts the startup. While it watches, such a signal cancels its
// construction context; run checks it before binding and once both
// listeners are bound, and finish hands the signals over to the run loop.
type startupWatch struct {
	ctx       context.Context // construction context, cancelled by a signal
	cancel    context.CancelFunc
	signals   <-chan os.Signal
	signalled atomic.Bool
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
}

// watchStartup starts watching signals, with a construction context derived
// from ctx.
func watchStartup(ctx context.Context, signals <-chan os.Signal) *startupWatch {
	w := &startupWatch{signals: signals, stop: make(chan struct{}), done: make(chan struct{})}
	w.ctx, w.cancel = context.WithCancel(ctx)
	go func() {
		defer close(w.done)
		select {
		case <-signals:
			w.signalled.Store(true)
			w.cancel()
		case <-w.stop:
		}
	}()
	return w
}

// aborted reports whether a signal arrived, taking one still queued.
func (w *startupWatch) aborted() bool {
	if !w.signalled.Load() {
		select {
		case <-w.signals:
			w.signalled.Store(true)
			w.cancel()
		default:
		}
	}
	return w.signalled.Load()
}

// finish stops watching, still taking a queued signal, and reports whether
// the startup was aborted. Later signals belong to the run loop.
func (w *startupWatch) finish() bool {
	w.once.Do(func() {
		close(w.stop)
		<-w.done
		w.aborted()
		w.cancel()
	})
	return w.signalled.Load()
}

type app struct {
	cfg     *config.Config
	opts    Options
	parent  context.Context // Run's context
	watch   *startupWatch
	build   metrics.BuildInfo
	logger  *slog.Logger
	output  *telemetry.Output
	origin  *origins3.Client
	health  *origins3.Client // nil without origin health
	public  *httpserver.Server
	ready   *admin.Readiness
	worker  *admin.Worker
	admin   *admin.Server
	release []func() // undoes construction, in reverse order

	cancelLifetime context.CancelFunc
	// active counts admitted public requests whose record has not been
	// delivered yet: a forced drain reports how many it cancelled and waits
	// for their access events before flushing.
	active atomic.Int64
	// beforeDrainWait is a test hook that delays the shutdown just before
	// it waits for the drain; nil in production.
	beforeDrainWait func()
}

func build(ctx context.Context, cfg *config.Config, opts Options, watch *startupWatch) (*app, error) {
	logs := opts.Logs
	if logs == nil {
		logs = io.Discard
	}
	logger, output, err := telemetry.NewLogger(logs, cfg.Observability)
	if err != nil {
		return nil, errors.New("cannot initialize logging")
	}
	b := opts.Build
	if b.Version == "" {
		b.Version = "dev"
	}
	if b.Revision == "" {
		b.Revision = "unknown"
	}
	if b.GoVersion == "" {
		b.GoVersion = runtime.Version()
	}
	a := &app{cfg: cfg, opts: opts, parent: ctx, watch: watch, build: b, logger: logger, output: output}
	fail := func(stage, message string) (*app, error) {
		return nil, a.abort(stage, errors.New(message))
	}

	m, err := metrics.New(cfg, b)
	if err != nil {
		return fail("metrics", "cannot initialize metrics")
	}
	// Discovery of credentials and region shares check-origin's budget, and
	// a startup signal cancels it.
	startup, cancel := context.WithTimeout(watch.ctx, cfg.Server.ResponseHeaderTimeout)
	defer cancel()
	originOptions := append([]origins3.Option{origins3.WithObserver(m.OriginObserver())}, opts.OriginOptions...)
	if a.origin, err = origins3.New(startup, cfg.S3, originOptions...); err != nil {
		return nil, a.abortOrigin("origin_client", startup, err)
	}
	origin := a.origin
	a.release = append(a.release, func() { _ = origin.Close() })
	var healthReader storage.Reader // an untyped nil when disabled
	if h := cfg.Admin.OriginHealth; h != nil && h.Enabled {
		// One check is one origin attempt, never retried, and uncached.
		healthConfig := cfg.S3
		healthConfig.MaxAttempts = 1
		if a.health, err = origins3.New(startup, healthConfig, originOptions...); err != nil {
			return nil, a.abortOrigin("health_client", startup, err)
		}
		health := a.health
		a.release = append(a.release, func() { _ = health.Close() })
		healthReader = health
	}

	lifetime, cancelLifetime := context.WithCancel(context.Background())
	a.cancelLifetime = cancelLifetime
	a.release = append(a.release, cancelLifetime)
	memory, err := cache.FromConfig(lifetime, cfg, a.origin, func(o *cache.Options) { o.OnFill = m.OnFill })
	if err != nil {
		return fail("cache", "cannot initialize the cache")
	}
	if err := m.RegisterCache(memory, cfg.Cache); err != nil {
		return fail("metrics", "cannot initialize metrics")
	}
	objects, err := content.NewHandler(cfg, a.origin, content.WithCache(memory))
	if err != nil {
		return fail("content", "cannot initialize the content handler")
	}
	access := telemetry.NewAccessLogger(logger.Handler(), cfg.Observability)
	sink := func(r *telemetry.Record) {
		access.Sink(r)
		m.RecordSink(r)
		if !r.Admitted.IsZero() {
			a.active.Add(-1) // after the start observation of the same request
		}
	}
	observe := func(o httpserver.Observation) {
		if o.Event == httpserver.ObservationStart {
			a.active.Add(1)
		}
		m.HTTPObserver(o)
	}
	handler, err := httpserver.NewHandler(cfg, objects, httpserver.WithRecordSink(sink), httpserver.WithObserver(observe))
	if err != nil {
		return fail("public_handler", "cannot initialize the public handler")
	}
	if a.public, err = httpserver.NewServer(cfg.Server, handler, httpserver.WithErrorLogger(logger)); err != nil {
		return fail("public_server", "cannot initialize the public server")
	}
	public := a.public
	a.release = append(a.release, func() { _ = public.Close() })

	a.ready = admin.NewReadiness(cfg.Admin.OriginHealth)
	if err := m.RegisterReadiness(a.ready); err != nil {
		return fail("metrics", "cannot initialize metrics")
	}
	if err := m.RegisterLogOutput(output); err != nil {
		return fail("metrics", "cannot initialize metrics")
	}
	if a.worker, err = admin.NewWorker(a.ready, healthReader, cfg.S3.Prefix, logger); err != nil {
		return fail("health_worker", "cannot initialize the origin health worker")
	}
	if a.admin, err = admin.NewServerWithLogger(a.ready, m.Handler(), logger); err != nil {
		return fail("admin_server", "cannot initialize the admin server")
	}
	adminServer := a.admin
	a.release = append(a.release, func() { _ = adminServer.Close() })
	return a, nil
}

// abortOrigin aborts after a failed origin client construction at stage,
// whose context was ctx. A cancellation caused by a startup signal or by
// Run's context is that abort itself, whatever error kind it surfaced as:
// configuration loading reports one as a configuration error, and only the
// signal watch or Run's context ever cancel ctx. Any other error, timeouts
// included, is a failure, reported in fixed text: neither SDK errors nor
// endpoints may reach the command output.
func (a *app) abortOrigin(stage string, ctx context.Context, err error) error {
	canceled := storage.IsKind(err, storage.Canceled) || errors.Is(ctx.Err(), context.Canceled)
	switch {
	case canceled && (a.watch.finish() || a.parent.Err() != nil):
		return a.abort(stage, nil)
	case canceled:
		return a.abort(stage, errors.New("origin client initialization canceled"))
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || storage.IsKind(err, storage.Timeout):
		return a.abort(stage, errors.New("origin client initialization timed out"))
	}
	return a.abort(stage, errors.New("cannot initialize the origin client"))
}

// abort ends a startup that did not reach serving. A non-nil err is the
// failure of stage, which takes precedence over a pending signal. A nil err
// is a clean abort: by a signal (errAborted), otherwise by Run's cancelled
// context. Everything built so far is released, nothing having been served,
// and the logs are flushed.
func (a *app) abort(stage string, err error) error {
	signalled := a.watch.finish()
	switch {
	case err != nil:
		attrs := []any{"stage", stage}
		if signalled {
			attrs = append(attrs, "signal_pending", true)
		}
		a.logger.Error("startup_failed", attrs...)
	case signalled:
		a.logger.Info("startup_aborted", "reason", ReasonSignal)
		err = errAborted
	default:
		a.logger.Warn("startup_aborted", "reason", ReasonContext)
		err = errors.New("startup canceled")
	}
	for i := len(a.release) - 1; i >= 0; i-- {
		a.release[i]()
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Server.ShutdownTimeout)
	defer cancel()
	_ = a.output.Close(ctx)
	return err
}

func (a *app) run(ctx context.Context) error {
	if a.watch.aborted() || ctx.Err() != nil {
		return a.abort("startup", nil)
	}
	listen := a.opts.Listen
	if listen == nil {
		listen = net.Listen
	}
	publicListener, err := listen("tcp", a.cfg.Server.Listen)
	if err != nil {
		return a.abort("bind_public", errors.New("cannot bind the public listener"))
	}
	adminListener, err := listen("tcp", a.cfg.Admin.Listen)
	if err != nil {
		_ = publicListener.Close()
		return a.abort("bind_admin", errors.New("cannot bind the admin listener"))
	}
	if a.watch.finish() { // a signal arrived while binding
		_ = publicListener.Close()
		_ = adminListener.Close()
		return a.abort("startup", nil)
	}
	a.ready.SetServing()
	a.worker.Start()
	a.logStartup(publicListener.Addr(), adminListener.Addr())

	// Serve returns only after Shutdown or Close, or on a listener failure,
	// which is logged whenever it happens, during shutdown too.
	exited := make(chan string, 2)
	serve := func(name string, server interface{ Serve(net.Listener) error }, l net.Listener) {
		if err := server.Serve(l); !errors.Is(err, http.ErrServerClosed) {
			a.logger.Error("listener_failed", "listener", name)
		}
		exited <- name
	}
	go serve("public", a.public, publicListener)
	go serve("admin", a.admin, adminListener)
	pending := 2
	var reason string
	var failure error
	publicFailed := false
	select {
	case <-a.opts.Signals:
		reason = ReasonSignal
	case <-ctx.Done():
		reason = ReasonContext
	case name := <-exited:
		pending--
		reason = ReasonListenerFailure
		failure = errors.New(name + " listener failed")
		publicFailed = name == "public"
	}
	a.shutdown(reason, publicFailed, exited, pending)
	return failure
}

// shutdown drains and stops the servers. publicFailed reports a shutdown
// caused by a public listener failure, which has already closed the public
// connections and cancelled their requests: nothing is left to drain.
func (a *app) shutdown(reason string, publicFailed bool, exited <-chan string, pending int) {
	start := time.Now()
	// Best effort: cancelled requests may have unwound already.
	cancelledByFailure := a.active.Load()
	budget := a.cfg.Server.ShutdownTimeout
	tail := min(budget/4, maxTail)
	drainDeadline := start.Add(budget - tail)
	overall, cancelOverall := context.WithDeadline(context.Background(), start.Add(budget))
	defer cancelOverall()
	flush, cancelFlush := context.WithDeadline(context.Background(), start.Add(budget))
	defer cancelFlush()

	a.ready.SetDraining()
	a.logger.Info("shutdown_started", "reason", reason, "shutdown_timeout", budget)

	// Shutdown, its expiry and Close all cancel request contexts before Done
	// closes, so fills end strictly after the requests waiting on them.
	lifetimeEnded := make(chan struct{})
	go func() {
		<-a.public.Done()
		a.cancelLifetime()
		close(lifetimeEnded)
	}()

	var forcedBySignal atomic.Bool
	var cancelledAtForce atomic.Int64
	stopWatching, watching := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case <-a.opts.Signals:
		case <-stopWatching:
			return
		}
		cancelledAtForce.Store(a.active.Load())
		forcedBySignal.Store(true)
		// Closing TLS connections can block on close_notify writes to
		// stalled clients, so nothing waits for it.
		go func() { _ = a.public.Close() }()
		_ = a.admin.Close()
		cancelOverall()
		grace := time.AfterFunc(forcedFlushGrace, cancelFlush)
		<-flush.Done()
		grace.Stop()
	}()
	defer func() {
		close(stopWatching)
		cancelFlush()
		<-watching
	}()

	// Public intake stops first; the health worker stops meanwhile.
	publicCtx, cancelPublic := context.WithCancel(overall)
	defer cancelPublic()
	drained := make(chan error, 1)
	go func() { drained <- a.public.Shutdown(publicCtx) }()
	drain, cancelDrain := context.WithDeadline(overall, drainDeadline)
	defer cancelDrain()
	workerStopped := make(chan struct{})
	go func() {
		_ = a.worker.Stop(drain) // advisory: a stopped worker records nothing more
		close(workerStopped)
	}()

	// The drain deadline is enforced here rather than through the context,
	// so the requests still active are counted before they are cancelled.
	expiry := time.NewTimer(time.Until(drainDeadline))
	if a.beforeDrainWait != nil {
		a.beforeDrainWait()
	}
	var drainErr error
	finished, expired := false, false
	cancelled := int64(0)
	select {
	case drainErr = <-drained:
		finished = true
	case <-expiry.C:
	case <-overall.Done():
	}
	expiry.Stop()
	switch {
	case forcedBySignal.Load():
		if !finished {
			drainErr = overall.Err() // the force closes the server
		}
	case finished && drainErr == nil:
	case finished:
		// Shutdown ended on its own deadline, so this goroutine was delayed
		// past the budget: a budget expiry whose count is best effort, as
		// cancelled requests may have unwound already.
		expired = errors.Is(drainErr, context.DeadlineExceeded)
		cancelled = a.active.Load()
	default:
		// The drain deadline passed or, when the shutdown itself was
		// delayed, the whole budget: either way a budget expiry. Shutdown
		// then force-closes connections; the wait for it ends with the
		// budget, like the force path.
		expired = true
		cancelled = a.active.Load()
		cancelPublic()
		select {
		case drainErr = <-drained:
		case <-overall.Done():
			drainErr = overall.Err()
		}
	}
	<-workerStopped // bounded by the drain deadline
	select {
	case <-lifetimeEnded:
	case <-overall.Done():
	}

	unwind, cancelUnwind := context.WithDeadline(overall, drainDeadline.Add(tail/2))
	a.waitUnwound(unwind)
	cancelUnwind()
	// The admin server's deadline leaves at least T/4 for the final event
	// and the flush.
	adminCtx, cancelAdmin := context.WithDeadline(overall, drainDeadline.Add(3*tail/4))
	defer cancelAdmin()
	_ = a.admin.Shutdown(adminCtx)
	_ = a.origin.Close()
	if a.health != nil {
		_ = a.health.Close()
	}
	for ; pending > 0; pending-- {
		select {
		case <-exited:
		case <-adminCtx.Done():
			pending = 0
		}
	}
	select {
	case <-a.public.Done():
	default:
		// A Shutdown stuck closing connections has not cancelled the
		// request contexts yet: Close does that first, and then the cache
		// lifetime ends.
		go func() { _ = a.public.Close() }()
	}

	attrs := []any{"reason", reason}
	level := slog.LevelInfo
	switch {
	case forcedBySignal.Load():
		level = slog.LevelWarn
		attrs = append(attrs, "outcome", "forced", "forced_by", "signal", "cancelled_requests", max(cancelled, cancelledAtForce.Load()))
	case publicFailed:
		level = slog.LevelWarn
		attrs = append(attrs, "outcome", "forced", "forced_by", "listener_failure", "cancelled_requests", max(cancelled, cancelledByFailure))
	case drainErr != nil:
		level = slog.LevelWarn
		forcedBy := "error"
		if expired {
			forcedBy = "budget"
		}
		attrs = append(attrs, "outcome", "forced", "forced_by", forcedBy, "cancelled_requests", cancelled)
	default:
		attrs = append(attrs, "outcome", "drained")
	}
	attrs = append(attrs, "duration_ms", float64(time.Since(start).Microseconds())/1000)
	a.logger.Log(context.Background(), level, "shutdown_complete", attrs...)
	_ = a.output.Close(flush)
}

// waitUnwound waits until every admitted request delivered its record, or
// ctx ends.
func (a *app) waitUnwound(ctx context.Context) {
	if a.active.Load() <= 0 {
		return
	}
	ticker := time.NewTicker(unwindPoll)
	defer ticker.Stop()
	for a.active.Load() > 0 {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// logStartup reports the resolved, non-secret operating mode: no bucket,
// prefix, endpoint, host names, credentials, key material or other
// configuration values beyond modes, flags and budgets.
func (a *app) logStartup(public, adminAddr net.Addr) {
	c := a.cfg
	onOff := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	attrs := []any{
		"version", a.build.Version, "revision", a.build.Revision, "go_version", a.build.GoVersion,
		"public_listen", public.String(), "admin_listen", adminAddr.String(),
		"tls", onOff(c.Server.TLS != nil),
	}
	if c.Server.TLS != nil {
		attrs = append(attrs, "tls_min_version", c.Server.TLS.MinVersion)
	}
	if len(c.Hosts) > 0 {
		attrs = append(attrs, "hosts", "allowlist", "hosts_count", len(c.Hosts))
	} else {
		attrs = append(attrs, "hosts", "any")
	}
	endpoint := "aws"
	if c.S3.Endpoint != "" {
		endpoint = "custom"
	}
	s := c.Server
	attrs = append(attrs,
		"trusted_proxies", len(s.TrustedProxies),
		"read_header_timeout", s.ReadHeaderTimeout, "response_header_timeout", s.ResponseHeaderTimeout,
		"write_idle_timeout", s.WriteIdleTimeout, "idle_timeout", s.IdleTimeout,
		"request_timeout", s.RequestTimeout, "shutdown_timeout", s.ShutdownTimeout,
		"max_connections", s.MaxConnections, "max_concurrent_requests", s.MaxConcurrentRequests,
		"max_header_bytes", s.MaxHeaderBytes,
		"s3_endpoint", endpoint, "s3_region", a.origin.Region(),
		"s3_path_style", c.S3.PathStyle, "s3_allow_http", c.S3.AllowHTTP,
		"s3_insecure_skip_verify", c.S3.InsecureSkipVerify,
		"s3_conditional_reads", c.S3.Features.ConditionalReads, "s3_range_reads", c.S3.Features.RangeReads,
		"s3_checksums", c.S3.Features.Checksums, "s3_max_attempts", c.S3.MaxAttempts,
		"s3_response_header_timeout", c.S3.ResponseHeaderTimeout, "s3_read_idle_timeout", c.S3.ReadIdleTimeout,
		"cache", onOff(c.Cache.Enabled),
	)
	if k := c.Cache; k.Enabled {
		attrs = append(attrs,
			"cache_max_bytes", int64(k.MaxBytes), "cache_max_entries", k.MaxEntries,
			"cache_max_object_bytes", int64(k.MaxObjectBytes), "cache_max_fill_bytes", int64(k.MaxFillBytes),
			"cache_max_concurrent_fills", k.MaxConcurrentFills, "cache_max_waiters_per_key", k.MaxWaitersPerKey,
			"cache_fill_timeout", k.FillTimeout, "cache_ttl", k.TTL, "cache_ttl_mode", k.TTLMode, "cache_negative_ttl", k.NegativeTTL,
			"cache_stale_if_error", k.StaleIfError, "cache_request_directives", k.RequestDirectives, "cache_rules", len(k.Rules),
		)
	}
	attrs = append(attrs, "compression", c.Compression.Mode)
	if len(c.Compression.Encodings) > 0 {
		attrs = append(attrs, "compression_encodings", strings.Join(c.Compression.Encodings, ","))
	}
	health := c.Admin.OriginHealth
	attrs = append(attrs, "spa", onOff(c.SPA != nil), "origin_health", onOff(health != nil && health.Enabled))
	if health != nil && health.Enabled {
		attrs = append(attrs, "origin_health_interval", health.Interval, "origin_health_timeout", health.Timeout)
	}
	attrs = append(attrs, "access_log", c.Observability.AccessLog, "log_client_ip", c.Observability.LogClientIP,
		"log_level", c.Observability.LogLevel)
	a.logger.Info("startup", attrs...)
	if c.S3.InsecureSkipVerify {
		a.logger.Warn("origin_tls_verification_disabled")
	}
}
