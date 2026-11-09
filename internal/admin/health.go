package admin

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/gtriggiano/statica/internal/storage"
)

// Worker is the optional origin health worker. It runs at most one goroutine,
// which issues one uncached Head of prefix + key at a time, so checks never
// overlap, and records each raw result in its Readiness. Disabled origin
// health never starts it and causes no origin traffic.
//
// The first check starts when the worker starts. Each later check starts
// interval less a uniform random jitter of up to 10% after the previous one
// ended. Subtracting the jitter spreads instances that share an origin while
// keeping two successes of a healthy origin at most interval + timeout apart,
// which the validated max_staleness exceeds.
//
// Each check runs under its own timeout deadline and storage.PurposeHealth.
// The worker never retries: the caller provides a dedicated reader built with
// s3.max_attempts 1, so one check is one origin attempt.
type Worker struct {
	readiness         *Readiness
	reader            storage.Reader
	key               string
	interval, timeout time.Duration
	log               *slog.Logger

	// unknownResult is the last failure logged before a first success, so an
	// origin that never becomes healthy is explained once per change of
	// result rather than every interval. Only the check goroutine uses it.
	unknownResult Result

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	stopped bool
}

// NewWorker returns the worker feeding r, configured by the origin health
// section r was built from. With origin health disabled it needs no reader
// and Start does nothing. Otherwise reader is required: a dedicated
// single-attempt reader that bypasses the cache. prefix is the publication
// prefix of the key. A nil logger discards the state transition events.
func NewWorker(r *Readiness, reader storage.Reader, prefix string, logger *slog.Logger) (*Worker, error) {
	if r == nil {
		return nil, errors.New("admin: a readiness model is required")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	w := &Worker{readiness: r, reader: reader, log: logger}
	if !r.enabled {
		return w, nil
	}
	h := r.health
	if reader == nil {
		return nil, errors.New("admin: origin health requires a reader")
	}
	if h.Interval <= 0 || h.Timeout <= 0 || h.MaxStaleness <= 0 || h.FailureThreshold <= 0 || h.SuccessThreshold <= 0 {
		return nil, errors.New("admin: origin health requires positive durations and thresholds")
	}
	w.key, w.interval, w.timeout = prefix+h.Key, h.Interval, h.Timeout
	return w, nil
}

// Start launches the worker goroutine. It has no effect when origin health is
// disabled, when already started, or after Stop.
func (w *Worker) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.readiness.enabled || w.cancel != nil || w.stopped {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	go w.run(ctx, w.done)
}

// Stop cancels any in-flight check, whose result is then not recorded, and
// waits for the goroutine to exit or ctx to end. It returns nil once the
// goroutine has exited, even if ctx ended too, and ctx's error otherwise. The
// error is advisory: a stopped worker records nothing more, and its goroutine
// exits as soon as a reader that ignored cancellation returns. Stop is
// idempotent, and the worker cannot be restarted.
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.stopped = true
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (w *Worker) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		w.check(ctx)
		timer.Reset(w.delay())
	}
}

// delay is interval less a uniform jitter in [0, interval/10).
func (w *Worker) delay() time.Duration {
	if n := int64(w.interval / 10); n > 0 {
		return w.interval - time.Duration(rand.Int64N(n))
	}
	return w.interval
}

// check performs one probe and records its raw result, unless ctx, the
// worker's lifetime, ended first: a check cut short by Stop says nothing
// about the origin. It logs every state transition and, before a first
// success, each failure whose result differs from the last one logged.
func (w *Worker) check(ctx context.Context) {
	checkCtx, cancel := context.WithTimeout(storage.WithPurpose(ctx, storage.PurposeHealth), w.timeout)
	_, err := w.reader.Head(checkCtx, w.key)
	result := classify(checkCtx, err)
	cancel()
	if ctx.Err() != nil {
		return
	}
	switch from, to := w.readiness.record(result, w.readiness.now()); {
	case from != to:
		level := slog.LevelInfo
		if to == originUnhealthy {
			level = slog.LevelError
		}
		w.log.Log(ctx, level, "origin_health_changed", "state", string(to), "previous", string(from), "result", string(result))
	case to == originUnknown && result != w.unknownResult:
		w.unknownResult = result
		w.log.Log(ctx, slog.LevelWarn, "origin_health_check_failed", "state", string(to), "result", string(result))
	}
}

// classify maps one check's raw outcome to its Result. ctx is the check's own
// context. A kind with its own label wins, even when the deadline lapsed as
// it arrived; otherwise an expired deadline is a timeout. Unknown kinds are
// never echoed into a label.
func classify(ctx context.Context, err error) Result {
	if err == nil {
		return ResultSuccess
	}
	var e *storage.Error
	if errors.As(err, &e) {
		switch e.Kind {
		case storage.NotFound:
			return ResultNotFound
		case storage.AmbiguousNotFound:
			return ResultAmbiguousNotFound
		case storage.Denied:
			return ResultDenied
		case storage.Throttled:
			return ResultThrottled
		case storage.Unavailable:
			return ResultUnavailable
		case storage.Configuration:
			return ResultConfiguration
		case storage.Timeout:
			return ResultTimeout
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return ResultTimeout
	}
	return ResultFailed
}
