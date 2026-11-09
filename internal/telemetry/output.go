package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logfmt/logfmt"
)

// OutputOptions configures an Output. Zero values select the defaults.
type OutputOptions struct {
	// MaxEvents is the queue capacity in events; default 4096.
	MaxEvents int
	// MaxBytes is the queue capacity in bytes, counting events until they
	// are written; default 4 MiB. A larger single event is always dropped.
	MaxBytes int64
	// DropReportInterval is the minimum spacing of log_events_dropped
	// warnings; default 10s.
	DropReportInterval time.Duration
	// Level decides whether the warn-level drop reports are written; nil
	// means info.
	Level slog.Leveler
}

const maxBatchBytes = 64 << 10

// Output is a bounded, non-blocking line writer: Write copies the line into
// a fixed-capacity queue that one goroutine drains into the destination,
// batching queued lines into a single destination write. Write never waits
// for the destination; when the queue is full the line is dropped and
// counted. Lines whose destination write fails are counted as dropped too.
// After the writer makes progress, at most once per DropReportInterval, it
// writes a log_events_dropped warning carrying the drops since the previous
// report (dropped) and in total (dropped_total).
type Output struct {
	w        io.Writer
	queue    chan *[]byte
	maxBytes int64
	interval time.Duration
	warn     bool

	queued  atomic.Int64 // bytes accepted and not yet written
	dropped atomic.Uint64

	mu     sync.RWMutex // guards closed against in-flight enqueues
	closed bool
	once   sync.Once
	stop   chan struct{}
	done   chan struct{}

	// Writer goroutine state.
	batch    []byte
	reported uint64
	lastSent time.Time
}

// NewOutput starts the writer goroutine for w; Close stops it.
func NewOutput(w io.Writer, opts OutputOptions) *Output {
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 4096
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 4 << 20
	}
	if opts.DropReportInterval <= 0 {
		opts.DropReportInterval = 10 * time.Second
	}
	level := slog.LevelInfo
	if opts.Level != nil {
		level = opts.Level.Level()
	}
	o := &Output{w: w, queue: make(chan *[]byte, opts.MaxEvents), maxBytes: opts.MaxBytes,
		interval: opts.DropReportInterval, warn: level <= slog.LevelWarn,
		stop: make(chan struct{}), done: make(chan struct{})}
	go o.run()
	return o
}

var linePool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

// Write enqueues a copy of p as one line, or drops it. It never blocks on
// the destination and always reports success, so that a logging call
// cannot fail a request.
func (o *Output) Write(p []byte) (int, error) {
	n := int64(len(p))
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		o.dropped.Add(1)
		return len(p), nil
	}
	// Reserve the bytes only if they fit, so that a concurrent write over
	// the budget cannot make one that fits fail.
	for {
		queued := o.queued.Load()
		if queued+n > o.maxBytes {
			o.dropped.Add(1)
			return len(p), nil
		}
		if o.queued.CompareAndSwap(queued, queued+n) {
			break
		}
	}
	line := linePool.Get().(*[]byte)
	*line = append((*line)[:0], p...)
	select {
	case o.queue <- line:
	default:
		o.queued.Add(-n)
		o.dropped.Add(1)
		putLine(line)
	}
	return len(p), nil
}

// Dropped returns the number of lines dropped since the Output started.
func (o *Output) Dropped() uint64 { return o.dropped.Load() }

// Close stops accepting lines, later ones being dropped and counted, and
// waits until the queue is written, together with a final drop report, or
// until ctx is done. A destination blocked beyond ctx keeps the writer
// goroutine until it returns.
func (o *Output) Close(ctx context.Context) error {
	o.once.Do(func() {
		o.mu.Lock()
		o.closed = true
		o.mu.Unlock()
		close(o.stop)
	})
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func putLine(line *[]byte) {
	if cap(*line) <= maxBatchBytes {
		linePool.Put(line)
	}
}

func (o *Output) run() {
	defer close(o.done)
	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()
	for {
		healthy := true
		select {
		case line := <-o.queue:
			healthy = o.flush(line)
		case <-ticker.C:
		case <-o.stop:
			for {
				select {
				case line := <-o.queue:
					o.flush(line)
				default:
					o.report(true)
					return
				}
			}
		}
		if healthy {
			o.report(false)
		}
	}
}

// flush writes first and the lines queued behind it, up to a batch bound,
// with one destination write, and reports whether that write succeeded.
func (o *Output) flush(first *[]byte) bool {
	o.batch = append(o.batch[:0], *first...)
	lines := 1
	putLine(first)
	for len(o.batch) < maxBatchBytes {
		select {
		case line := <-o.queue:
			o.batch = append(o.batch, *line...)
			lines++
			putLine(line)
			continue
		default:
		}
		break
	}
	_, err := o.w.Write(o.batch)
	o.queued.Add(-int64(len(o.batch)))
	if cap(o.batch) > 4*maxBatchBytes {
		o.batch = nil
	}
	if err != nil {
		o.dropped.Add(uint64(lines))
		return false
	}
	return true
}

// report writes a log_events_dropped warning when lines were dropped since
// the previous report and, unless final, the interval has elapsed. A report
// whose write fails is retried later with the drops it should have carried.
func (o *Output) report(final bool) {
	total := o.dropped.Load()
	if !o.warn || total == o.reported {
		return
	}
	now := time.Now()
	if !final && !o.lastSent.IsZero() && now.Sub(o.lastSent) < o.interval {
		return
	}
	var line bytes.Buffer
	enc := logfmt.NewEncoder(&line)
	_ = enc.EncodeKeyvals("ts", now.UTC().Format(timestampLayout), "level", "warn", "event", "log_events_dropped",
		"dropped", total-o.reported, "dropped_total", total)
	_ = enc.EndRecord()
	if _, err := o.w.Write(line.Bytes()); err == nil {
		o.reported, o.lastSent = total, now
	}
}
