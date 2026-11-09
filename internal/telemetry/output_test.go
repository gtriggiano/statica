package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gateWriter blocks every Write until gate closes, then forwards to out.
type gateWriter struct {
	gate    chan struct{}
	entered chan struct{} // receives once per blocked Write, when buffered
	out     lockedBuffer
}

func newGateWriter() *gateWriter {
	return &gateWriter{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
}

func (w *gateWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.gate
	return w.out.Write(p)
}

func eventually(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func line(event string, size int) []byte {
	l := fmt.Appendf(nil, "ts=2026-11-07T00:00:00.000Z level=info event=%s pad=", event)
	for len(l) < size-1 {
		l = append(l, 'x')
	}
	return append(l, '\n')
}

// TestOutputNeverBlocksOnABlockedWriter is the blocked-output resource test:
// with a destination that never returns, writes keep their enqueue cost,
// excess events are dropped and counted, queued memory stays within its
// bounds, and Close returns at its deadline.
func TestOutputNeverBlocksOnABlockedWriter(t *testing.T) {
	w := newGateWriter()
	o := NewOutput(w, OutputOptions{MaxEvents: 64, MaxBytes: 32 << 10, DropReportInterval: time.Hour})
	t.Cleanup(func() {
		close(w.gate)
		<-o.done
	})
	l := line("blocked", 1024)
	o.Write(l)
	<-w.entered
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	const n = 50000 // 50 MiB offered
	start := time.Now()
	var slowest time.Duration
	for range n {
		began := time.Now()
		if written, err := o.Write(l); written != len(l) || err != nil {
			t.Fatalf("Write = %d, %v", written, err)
		}
		slowest = max(slowest, time.Since(began))
	}
	elapsed := time.Since(start)
	runtime.GC()
	runtime.ReadMemStats(&after)
	// 31 lines fit the byte budget besides the one being written.
	if got, want := o.Dropped(), uint64(n-31); got != want {
		t.Fatalf("dropped %d, want %d", got, want)
	}
	if queued := o.queued.Load(); queued > 32<<10 || len(o.queue) != 31 {
		t.Fatalf("queued %d bytes in %d events", queued, len(o.queue))
	}
	if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > 4<<20 {
		t.Fatalf("heap grew by %d bytes", growth)
	}
	if elapsed > 2*time.Second || slowest > 250*time.Millisecond {
		t.Fatalf("writes took %v, slowest %v", elapsed, slowest)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	began := time.Now()
	if err := o.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v", err)
	}
	if waited := time.Since(began); waited < 90*time.Millisecond || waited > time.Second {
		t.Fatalf("Close returned after %v", waited)
	}
	if o.Write(l); o.Dropped() != n-31+1 {
		t.Fatal("a write after Close must be dropped and counted")
	}
}

func TestOutputEventCapacity(t *testing.T) {
	w := newGateWriter()
	o := NewOutput(w, OutputOptions{MaxEvents: 4, DropReportInterval: time.Hour})
	o.Write(line("first", 64))
	<-w.entered
	for i := range 10 {
		o.Write(line(fmt.Sprint("e", i), 64))
	}
	if o.Dropped() != 6 {
		t.Fatalf("dropped %d, want 6", o.Dropped())
	}
	close(w.gate)
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range decodeEvents(t, w.out.String()) {
		got = append(got, e.values["event"])
	}
	// The report follows the first write that succeeds after the drops.
	if want := "first log_events_dropped e0 e1 e2 e3"; strings.Join(got, " ") != want {
		t.Fatalf("events %v, want %s", got, want)
	}
}

func TestOutputReportsDropsOnceRecoveredAndRateBounded(t *testing.T) {
	const interval = 100 * time.Millisecond
	w := newGateWriter()
	o := NewOutput(w, OutputOptions{MaxEvents: 2, MaxBytes: 1 << 10, DropReportInterval: interval})
	o.Write(line("first", 64))
	<-w.entered
	for i := range 4 {
		o.Write(line(fmt.Sprint("e", i), 64))
	}
	time.Sleep(2 * interval)
	if o.Dropped() != 2 || w.out.String() != "" {
		t.Fatalf("dropped %d, output %q while blocked", o.Dropped(), w.out.String())
	}
	close(w.gate)
	reports := func() []event {
		var r []event
		for _, e := range decodeEvents(t, w.out.String()) {
			if e.values["event"] == "log_events_dropped" {
				r = append(r, e)
			}
		}
		return r
	}
	eventually(t, "the first drop report", func() bool { return len(reports()) == 1 })
	// An event over the byte budget is dropped at once; the next report
	// waits for the interval although the writer keeps writing.
	o.Write(line("oversize", 2<<10))
	for i := range 5 {
		o.Write(line(fmt.Sprint("after", i), 64))
		time.Sleep(interval / 20)
	}
	eventually(t, "the second drop report", func() bool { return len(reports()) == 2 })
	r := reports()
	if r[0].values["level"] != "warn" || r[0].values["dropped"] != "2" || r[0].values["dropped_total"] != "2" ||
		r[1].values["dropped"] != "1" || r[1].values["dropped_total"] != "3" {
		t.Fatalf("reports %v", r)
	}
	first, _ := time.Parse(time.RFC3339Nano, r[0].values["ts"])
	second, _ := time.Parse(time.RFC3339Nano, r[1].values["ts"])
	if gap := second.Sub(first); gap < interval-time.Millisecond {
		t.Fatalf("reports %v apart, want at least %v", gap, interval)
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reports()) != 2 {
		t.Fatal("Close must not repeat a report without new drops")
	}
}

func TestOutputCloseDrainsInOrder(t *testing.T) {
	var out lockedBuffer
	o := NewOutput(&out, OutputOptions{})
	var wg sync.WaitGroup
	for i := range 100 {
		o.Write(line(fmt.Sprint("e", i), 80))
	}
	for g := range 8 {
		wg.Go(func() {
			for i := range 100 {
				o.Write(line(fmt.Sprint("g", g, "_", i), 80))
			}
		})
	}
	wg.Wait()
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := decodeEvents(t, out.String())
	if len(events) != 900 || o.Dropped() != 0 {
		t.Fatalf("%d events, %d dropped", len(events), o.Dropped())
	}
	for i := range 100 {
		if events[i].values["event"] != fmt.Sprint("e", i) {
			t.Fatalf("event %d is %q", i, events[i].values["event"])
		}
	}
	if err := o.Close(context.Background()); err != nil {
		t.Fatal("Close must be idempotent")
	}
}

type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("broken pipe") }

func TestOutputCountsFailedWritesAndHonoursLevel(t *testing.T) {
	o := NewOutput(failingWriter{}, OutputOptions{})
	for range 10 {
		o.Write(line("lost", 64))
	}
	if err := o.Close(context.Background()); err != nil || o.Dropped() != 10 {
		t.Fatalf("Close = %v, dropped %d", err, o.Dropped())
	}
	// Above warn, drop reports are not written.
	w := newGateWriter()
	o = NewOutput(w, OutputOptions{MaxEvents: 1, Level: slog.LevelError})
	o.Write(line("first", 64))
	<-w.entered
	o.Write(line("queued", 64))
	o.Write(line("dropped", 64))
	close(w.gate)
	if err := o.Close(context.Background()); err != nil || o.Dropped() != 1 || strings.Contains(w.out.String(), "log_events_dropped") {
		t.Fatalf("Close = %v, dropped %d, output %q", err, o.Dropped(), w.out.String())
	}
}

// TestOutputByteBudgetUnderContention offers lines that fit the byte budget
// while concurrent writers keep offering lines that never fit: every line
// that fits is kept, none dropped because an oversize write was briefly
// counted against the budget, and the drops are exactly the oversize lines.
func TestOutputByteBudgetUnderContention(t *testing.T) {
	const size, fit, writers = 100, 31, 8
	w := newGateWriter()
	o := NewOutput(w, OutputOptions{MaxEvents: 4096, MaxBytes: (fit + 1) * size, DropReportInterval: time.Hour})
	t.Cleanup(func() {
		close(w.gate)
		_ = o.Close(context.Background())
	})
	o.Write(line("first", size))
	<-w.entered
	stop := make(chan struct{})
	var oversize atomic.Uint64
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			l := line("oversize", 2*(fit+1)*size)
			for {
				select {
				case <-stop:
					return
				default:
				}
				o.Write(l)
				oversize.Add(1)
			}
		})
	}
	eventually(t, "the oversize writers", func() bool { return oversize.Load() > 1000 })
	l := line("fits", size)
	for range fit {
		o.Write(l)
		runtime.Gosched()
	}
	close(stop)
	wg.Wait()
	if len(o.queue) != fit || o.Dropped() != oversize.Load() || o.queued.Load() != (fit+1)*size {
		t.Fatalf("%d of %d fitting lines queued, %d dropped for %d oversize, %d bytes", len(o.queue), fit, o.Dropped(), oversize.Load(), o.queued.Load())
	}
}

// reportFailer fails the first drop report written to it.
type reportFailer struct {
	lockedBuffer
	mu     sync.Mutex
	failed bool
}

func (w *reportFailer) Write(p []byte) (int, error) {
	w.mu.Lock()
	fail := !w.failed && strings.Contains(string(p), "log_events_dropped")
	w.failed = w.failed || fail
	w.mu.Unlock()
	if fail {
		return 0, errors.New("broken pipe")
	}
	return w.lockedBuffer.Write(p)
}

func TestOutputRetriesAFailedDropReport(t *testing.T) {
	w := &reportFailer{}
	o := NewOutput(w, OutputOptions{MaxBytes: 1 << 10, DropReportInterval: 20 * time.Millisecond})
	o.Write(line("oversize", 2<<10))
	o.Write(line("kept", 64))
	eventually(t, "the retried drop report", func() bool { return strings.Contains(w.String(), "log_events_dropped") })
	if err := o.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reports []event
	for _, e := range decodeEvents(t, w.String()) {
		if e.values["event"] == "log_events_dropped" {
			reports = append(reports, e)
		}
	}
	if len(reports) != 1 || reports[0].values["dropped"] != "1" || reports[0].values["dropped_total"] != "1" || !w.failed {
		t.Fatalf("reports %v, first failed %t", reports, w.failed)
	}
}
