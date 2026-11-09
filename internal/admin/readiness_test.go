package admin

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

// fakeReader counts origin calls. Get is never part of a health check.
type fakeReader struct {
	heads, gets atomic.Int64
	head        func(context.Context, string) (storage.Metadata, error)
}

func (f *fakeReader) Head(ctx context.Context, key string) (storage.Metadata, error) {
	f.heads.Add(1)
	if f.head == nil {
		return storage.Metadata{Size: 1}, nil
	}
	return f.head(ctx, key)
}

func (f *fakeReader) Get(context.Context, string, storage.ReadOptions) (*storage.Object, error) {
	f.gets.Add(1)
	return nil, &storage.Error{Kind: storage.Unsupported, Size: -1}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// newClock replaces r's clock; call it before starting a worker.
func newClock(r *Readiness) *fakeClock {
	c := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	r.now = c.now
	return c
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func healthConfig() *config.OriginHealth {
	return &config.OriginHealth{Enabled: true, Key: "index.html", Interval: time.Hour, Timeout: time.Minute, FailureThreshold: 3, SuccessThreshold: 2, MaxStaleness: 2 * time.Hour}
}

func newWorker(t *testing.T, r *Readiness, reader storage.Reader) *Worker {
	t.Helper()
	w, err := NewWorker(r, reader, "site/", nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func wantReady(t *testing.T, r *Readiness, ready bool, reason Reason) {
	t.Helper()
	if gotReady, gotReason := r.Ready(); gotReady != ready || gotReason != reason {
		t.Fatalf("Ready() = %t, %q; want %t, %q", gotReady, gotReason, ready, reason)
	}
}

func TestReadinessPhasesWithoutOriginHealth(t *testing.T) {
	for name, h := range map[string]*config.OriginHealth{"absent": nil, "disabled": {Enabled: false, Interval: time.Millisecond}} {
		t.Run(name, func(t *testing.T) {
			r := NewReadiness(h)
			wantReady(t, r, false, ReasonInitializing)
			r.SetServing()
			wantReady(t, r, true, ReasonNone)
			r.SetServing()
			wantReady(t, r, true, ReasonNone)
			r.SetDraining()
			wantReady(t, r, false, ReasonDraining)
			r.SetServing()
			wantReady(t, r, false, ReasonDraining)
			if s := r.Snapshot(); s.Phase != PhaseDraining || s.OriginHealthEnabled || !s.LastSuccess.IsZero() {
				t.Fatalf("snapshot = %+v", s)
			}
		})
	}
	r := NewReadiness(nil)
	r.SetDraining()
	wantReady(t, r, false, ReasonDraining)
}

func TestReadinessThresholds(t *testing.T) {
	r := NewReadiness(healthConfig()) // failure_threshold 3, success_threshold 2
	newClock(r)
	var logs bytes.Buffer
	var next error
	f := &fakeReader{head: func(context.Context, string) (storage.Metadata, error) { return storage.Metadata{Size: 1}, next }}
	w, err := NewWorker(r, f, "site/", slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r.SetServing()
	wantReady(t, r, false, ReasonOriginUnknown)
	fail := &storage.Error{Kind: storage.Unavailable, Size: -1}
	steps := []struct {
		err    error
		ready  bool
		reason Reason
	}{
		{fail, false, ReasonOriginUnknown}, // failures before a first success keep it unknown
		{nil, true, ReasonNone},            // a first success alone is enough
		{fail, true, ReasonNone},
		{fail, true, ReasonNone},
		{nil, true, ReasonNone}, // the failures were not consecutive
		{fail, true, ReasonNone},
		{fail, true, ReasonNone},
		{fail, false, ReasonOriginUnhealthy},
		{nil, false, ReasonOriginUnhealthy},
		{fail, false, ReasonOriginUnhealthy}, // the successes were not consecutive
		{nil, false, ReasonOriginUnhealthy},
		{nil, true, ReasonNone},
	}
	for i, s := range steps {
		next = s.err
		w.check(context.Background())
		if ready, reason := r.Ready(); ready != s.ready || reason != s.reason {
			t.Fatalf("step %d: Ready() = %t, %q; want %t, %q", i, ready, reason, s.ready, s.reason)
		}
	}
	if c := r.Snapshot().Checks; c[ResultSuccess] != 5 || c[ResultUnavailable] != 7 {
		t.Fatalf("checks = %v", c)
	}
	wantEvents(t, logs.String(),
		"level=WARN msg=origin_health_check_failed state=unknown result=unavailable",
		"level=INFO msg=origin_health_changed state=healthy previous=unknown result=success",
		"level=ERROR msg=origin_health_changed state=unhealthy previous=healthy result=unavailable",
		"level=INFO msg=origin_health_changed state=healthy previous=unhealthy result=success")
}

// wantEvents compares text-handler lines, ignoring their time field.
func wantEvents(t *testing.T, logs string, want ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) != len(want) {
		t.Fatalf("events:\n%s", logs)
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, " "+want[i]) {
			t.Fatalf("event %d = %q; want %q", i, line, want[i])
		}
	}
}

func TestReadinessStalenessIsJudgedAtReadTime(t *testing.T) {
	r := NewReadiness(healthConfig()) // max_staleness 2h
	c := newClock(r)
	var next error
	w := newWorker(t, r, &fakeReader{head: func(context.Context, string) (storage.Metadata, error) { return storage.Metadata{}, next }})
	r.SetServing()
	first := c.now()
	w.check(context.Background())
	wantReady(t, r, true, ReasonNone)
	c.advance(2 * time.Hour)
	wantReady(t, r, true, ReasonNone) // exactly max_staleness old is not older
	c.advance(time.Nanosecond)
	wantReady(t, r, false, ReasonOriginStale)
	if s := r.Snapshot(); s.Ready || s.Reason != ReasonOriginStale || !s.LastSuccess.Equal(first) {
		t.Fatalf("snapshot = %+v", s)
	}
	next = &storage.Error{Kind: storage.Throttled, Size: -1}
	for range 3 {
		w.check(context.Background())
	}
	wantReady(t, r, false, ReasonOriginUnhealthy) // a reported unhealthy origin outranks staleness
	next = nil
	w.check(context.Background())
	w.check(context.Background())
	wantReady(t, r, true, ReasonNone)
	if s := r.Snapshot(); !s.LastSuccess.Equal(c.now()) {
		t.Fatalf("last success = %v; want %v", s.LastSuccess, c.now())
	}
}

func TestSnapshotLabelsAreFinite(t *testing.T) {
	for _, h := range []*config.OriginHealth{nil, healthConfig()} {
		s := NewReadiness(h).Snapshot()
		if got := slices.Sorted(maps.Keys(s.Checks)); !slices.Equal(got, slices.Sorted(slices.Values(results[:]))) {
			t.Fatalf("labels = %v", got)
		}
		for res, n := range s.Checks {
			if n != 0 {
				t.Fatalf("%s = %d before any check", res, n)
			}
		}
		if s.OriginHealthEnabled != (h != nil) || s.Ready || s.Reason != ReasonInitializing || s.Phase != PhaseInitializing || !s.LastSuccess.IsZero() {
			t.Fatalf("snapshot = %+v", s)
		}
	}
	r := NewReadiness(healthConfig())
	s := r.Snapshot()
	s.Checks[ResultSuccess] = 7
	if r.Snapshot().Checks[ResultSuccess] != 0 {
		t.Fatal("snapshot aliases the live counters")
	}
}
