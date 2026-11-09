package admin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

func stopWorker(t *testing.T, w *Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestNewWorkerRequirements(t *testing.T) {
	if _, err := NewWorker(nil, &fakeReader{}, "", nil); err == nil {
		t.Fatal("accepted a nil readiness model")
	}
	if _, err := NewWorker(NewReadiness(healthConfig()), nil, "", nil); err == nil {
		t.Fatal("enabled origin health accepted a nil reader")
	}
	bad := healthConfig()
	bad.SuccessThreshold = 0
	if _, err := NewWorker(NewReadiness(bad), &fakeReader{}, "", nil); err == nil {
		t.Fatal("accepted a zero threshold")
	}
	w, err := NewWorker(NewReadiness(nil), nil, "", nil)
	if err != nil {
		t.Fatalf("disabled origin health requires a reader: %v", err)
	}
	w.Start()
	stopWorker(t, w)
}

func TestWorkerDisabledMakesNoOriginCalls(t *testing.T) {
	h := &config.OriginHealth{Enabled: false, Key: "index.html", Interval: time.Millisecond, Timeout: time.Microsecond, FailureThreshold: 1, SuccessThreshold: 1, MaxStaleness: 3 * time.Millisecond}
	r := NewReadiness(h)
	f := &fakeReader{}
	w := newWorker(t, r, f)
	w.Start()
	time.Sleep(20 * time.Millisecond) // twenty intervals
	stopWorker(t, w)
	if n := f.heads.Load() + f.gets.Load(); n != 0 {
		t.Fatalf("disabled origin health made %d origin calls", n)
	}
	r.SetServing()
	wantReady(t, r, true, ReasonNone)
}

func TestClassifyRawOutcomes(t *testing.T) {
	kind := func(k storage.Kind) error { return &storage.Error{Kind: k, Size: -1} }
	expired, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	bg := context.Background()
	for i, c := range []struct {
		ctx  context.Context
		err  error
		want Result
	}{
		{bg, nil, ResultSuccess},
		{bg, kind(storage.NotFound), ResultNotFound},
		{bg, kind(storage.AmbiguousNotFound), ResultAmbiguousNotFound},
		{bg, kind(storage.Denied), ResultDenied},
		{bg, fmt.Errorf("wrapped: %w", kind(storage.Denied)), ResultDenied},
		{bg, kind(storage.Throttled), ResultThrottled},
		{bg, kind(storage.Unavailable), ResultUnavailable},
		{bg, kind(storage.Configuration), ResultConfiguration},
		{bg, kind(storage.Timeout), ResultTimeout},
		{bg, context.DeadlineExceeded, ResultTimeout},
		{expired, kind(storage.Canceled), ResultTimeout},
		{expired, errors.New("raw SDK error"), ResultTimeout},
		{expired, kind(storage.Denied), ResultDenied}, // a concrete outcome arriving as the deadline lapses
		{expired, kind(storage.NotFound), ResultNotFound},
		{expired, kind(storage.Unavailable), ResultUnavailable},
		{expired, nil, ResultSuccess},
		{bg, kind(storage.Canceled), ResultFailed},
		{bg, kind(storage.InvalidResponse), ResultFailed},
		{bg, kind("provider text with a secret"), ResultFailed},
		{bg, errors.New("raw SDK error"), ResultFailed},
	} {
		if got := classify(c.ctx, c.err); got != c.want {
			t.Errorf("case %d (%v): %q; want %q", i, c.err, got, c.want)
		}
	}
	for _, k := range []storage.Kind{storage.NotFound, storage.AmbiguousNotFound, storage.Denied, storage.Configuration, storage.Unavailable, storage.NotModified, storage.Precondition, storage.InvalidRange, storage.Throttled, storage.Timeout, storage.Canceled, storage.InvalidResponse, storage.ChecksumMismatch, storage.Unsupported, storage.InvalidKey} {
		if got := classify(bg, kind(k)); got == ResultSuccess || !slices.Contains(results[:], got) {
			t.Errorf("kind %s classified as %q", k, got)
		}
	}
}

// A health key or credential that never works must be explained without
// logging every interval.
func TestWorkerLogsFailuresBeforeFirstSuccess(t *testing.T) {
	r := NewReadiness(healthConfig()) // failure_threshold 3
	newClock(r)
	var logs bytes.Buffer
	var next error
	w, err := NewWorker(r, &fakeReader{head: func(context.Context, string) (storage.Metadata, error) { return storage.Metadata{}, next }}, "site/", slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	denied, missing := &storage.Error{Kind: storage.Denied, Size: -1}, &storage.Error{Kind: storage.NotFound, Size: -1}
	for _, err := range []error{denied, denied, denied, missing, missing, denied, nil, denied, denied} {
		next = err
		w.check(context.Background())
	}
	wantEvents(t, logs.String(),
		"level=WARN msg=origin_health_check_failed state=unknown result=denied",
		"level=WARN msg=origin_health_check_failed state=unknown result=not_found",
		"level=WARN msg=origin_health_check_failed state=unknown result=denied",
		"level=INFO msg=origin_health_changed state=healthy previous=unknown result=success")
}

// D05 masks a raw 403 as a public 404, but origin health must see it.
func TestDeniedCountsAsFailure(t *testing.T) {
	r := NewReadiness(healthConfig())
	newClock(r)
	var next error
	w := newWorker(t, r, &fakeReader{head: func(context.Context, string) (storage.Metadata, error) { return storage.Metadata{}, next }})
	r.SetServing()
	w.check(context.Background())
	next = &storage.Error{Kind: storage.Denied, Size: -1}
	for range 3 {
		w.check(context.Background())
	}
	wantReady(t, r, false, ReasonOriginUnhealthy)
	if c := r.Snapshot().Checks; c[ResultDenied] != 3 || c[ResultSuccess] != 1 || c[ResultNotFound] != 0 {
		t.Fatalf("checks = %v", c)
	}
}

func TestWorkerCheckTimeoutPurposeAndKey(t *testing.T) {
	h := healthConfig()
	h.Key, h.Timeout = "health/probe.txt", 50*time.Millisecond
	r := NewReadiness(h)
	newClock(r)
	type seen struct {
		key      string
		purpose  storage.Purpose
		start    time.Time
		deadline time.Time
	}
	var calls []seen
	f := &fakeReader{head: func(ctx context.Context, key string) (storage.Metadata, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("check has no deadline")
		}
		calls = append(calls, seen{key, storage.PurposeFrom(ctx), time.Now(), deadline})
		if len(calls) == 2 { // honours its deadline
			<-ctx.Done()
			return storage.Metadata{}, &storage.Error{Kind: storage.Canceled, Size: -1}
		}
		return storage.Metadata{Size: 1}, nil
	}}
	w := newWorker(t, r, f)
	for range 3 {
		start := time.Now()
		w.check(context.Background())
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("check took %v with a %v timeout", elapsed, h.Timeout)
		}
	}
	for i, c := range calls {
		if c.key != "site/health/probe.txt" || c.purpose != storage.PurposeHealth {
			t.Fatalf("call %d: key %q purpose %q", i, c.key, c.purpose)
		}
		if budget := c.deadline.Sub(c.start); budget <= 0 || budget > h.Timeout {
			t.Fatalf("call %d: deadline %v after its start", i, budget)
		}
		if i > 0 && !c.deadline.After(calls[i-1].deadline) {
			t.Fatalf("call %d reused an earlier deadline", i)
		}
	}
	if c := r.Snapshot().Checks; c[ResultSuccess] != 2 || c[ResultTimeout] != 1 {
		t.Fatalf("checks = %v", c)
	}
}

func TestWorkerNeverOverlaps(t *testing.T) {
	h := healthConfig()
	h.Interval, h.Timeout = 2*time.Millisecond, time.Millisecond
	r := NewReadiness(h)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var active, peak, calls atomic.Int32
	f := &fakeReader{head: func(context.Context, string) (storage.Metadata, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		if calls.Add(1) == 1 {
			<-release // ignores its deadline: the check outlives many intervals
		}
		return storage.Metadata{Size: 1}, nil
	}}
	w := newWorker(t, r, f)
	w.Start()
	t.Cleanup(func() { stopWorker(t, w) })
	<-entered
	time.Sleep(30 * time.Millisecond) // fifteen intervals
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d checks started while the first was in flight", n-1)
	}
	close(release)
	waitFor(t, "later checks", func() bool { return calls.Load() >= 4 })
	stopWorker(t, w)
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d checks overlapped", p)
	}
}

func TestWorkerJitterIsBounded(t *testing.T) {
	w := &Worker{interval: 30 * time.Second}
	for range 1000 {
		if d := w.delay(); d <= 27*time.Second || d > 30*time.Second {
			t.Fatalf("delay %v outside (27s, 30s]", d)
		}
	}
	if d := (&Worker{interval: 5}).delay(); d != 5 {
		t.Fatalf("tiny interval delay = %v", d)
	}
}

func TestWorkerStaleWithoutReports(t *testing.T) {
	t.Run("blocked", func(t *testing.T) {
		h := healthConfig()
		h.Interval, h.Timeout = time.Millisecond, time.Millisecond/2
		r := NewReadiness(h)
		c := newClock(r)
		blocked, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		f := &fakeReader{head: func(context.Context, string) (storage.Metadata, error) {
			if calls.Add(1) == 2 {
				close(blocked)
				<-release // ignores its deadline
			}
			return storage.Metadata{Size: 1}, nil
		}}
		w := newWorker(t, r, f)
		r.SetServing()
		w.Start()
		t.Cleanup(func() { close(release); stopWorker(t, w) })
		<-blocked
		wantReady(t, r, true, ReasonNone)
		c.advance(h.MaxStaleness + time.Nanosecond)
		wantReady(t, r, false, ReasonOriginStale)
		if s := r.Snapshot(); s.Checks[ResultSuccess] != 1 || s.Reason != ReasonOriginStale {
			t.Fatalf("snapshot = %+v", s)
		}
	})
	t.Run("stopped", func(t *testing.T) {
		r := NewReadiness(healthConfig())
		c := newClock(r)
		w := newWorker(t, r, &fakeReader{})
		r.SetServing()
		w.Start()
		waitFor(t, "a first success", func() bool { return r.Snapshot().Checks[ResultSuccess] == 1 })
		stopWorker(t, w)
		wantReady(t, r, true, ReasonNone)
		c.advance(healthConfig().MaxStaleness + time.Nanosecond)
		wantReady(t, r, false, ReasonOriginStale)
	})
}

func TestWorkerStopCancelsInFlightCheck(t *testing.T) {
	r := NewReadiness(healthConfig()) // a long timeout: only Stop can end the check
	entered := make(chan struct{})
	ended := make(chan error, 1)
	f := &fakeReader{head: func(ctx context.Context, _ string) (storage.Metadata, error) {
		close(entered)
		<-ctx.Done()
		ended <- ctx.Err()
		return storage.Metadata{}, &storage.Error{Kind: storage.Canceled, Size: -1}
	}}
	w := newWorker(t, r, f)
	w.Start()
	w.Start()
	<-entered
	start := time.Now()
	stopWorker(t, w)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stop took %v", elapsed)
	}
	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight check ended with %v", err)
	}
	select {
	case <-w.done:
	default:
		t.Fatal("worker goroutine still running after Stop")
	}
	for res, n := range r.Snapshot().Checks {
		if n != 0 {
			t.Fatalf("a check cut short by Stop was recorded as %s", res)
		}
	}
	stopWorker(t, w)
	w.Start()
	time.Sleep(10 * time.Millisecond)
	if n := f.heads.Load(); n != 1 {
		t.Fatalf("worker restarted after Stop: %d checks", n)
	}
	// An exited worker reports success even to an ended context.
	over, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		if err := w.Stop(over); err != nil {
			t.Fatalf("Stop after exit = %v", err)
		}
	}
}

func TestWorkerStopIsBoundedByContext(t *testing.T) {
	r := NewReadiness(healthConfig())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w := newWorker(t, r, &fakeReader{head: func(context.Context, string) (storage.Metadata, error) {
		once.Do(func() { close(entered) })
		<-release // ignores cancellation
		return storage.Metadata{}, nil
	}})
	w.Start()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := w.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop = %v; want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Stop took %v past its 50ms bound", elapsed)
	}
	close(release)
	stopWorker(t, w)
	if c := r.Snapshot().Checks; c[ResultSuccess] != 0 {
		t.Fatalf("a check outliving Stop was recorded: %v", c)
	}
}
