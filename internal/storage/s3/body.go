package s3

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gtriggiano/statica/internal/storage"
)

// body arms an inactivity timer only during a pending Read. Paused consumers
// consume no origin inactivity budget. Cancellation is private to this object.
// With an observer, every chunk read from the origin is reported under the
// body's current purpose, which SetPurpose may change at any time.
type body struct {
	source   io.ReadCloser
	ctx      context.Context
	cancel   context.CancelCauseFunc
	idle     time.Duration
	meta     *storage.Metadata
	readMu   sync.Mutex
	once     sync.Once
	read     int64
	terminal error
	observer Observer
	purpose  atomic.Pointer[storage.Purpose] // set whenever observer is
}

var _ storage.PurposeSetter = (*body)(nil)

// SetPurpose attributes the bytes read from now on to p.
func (b *body) SetPurpose(p storage.Purpose) { b.purpose.Store(purposeRef(p)) }

func (b *body) Close() error {
	b.finish()
	return nil
}
func (b *body) finish() {
	b.finishCause(context.Canceled)
}

var errReadIdle = errors.New("origin read inactivity")

func (b *body) finishCause(cause error) {
	b.once.Do(func() { b.cancel(cause); _ = b.source.Close() })
}
func (b *body) Read(p []byte) (int, error) {
	b.readMu.Lock()
	defer b.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if b.terminal != nil {
		return 0, b.terminal
	}
	if err := b.ctx.Err(); err != nil {
		b.finish()
		b.terminal = classify(b.ctx, err, false)
		return 0, b.terminal
	}
	fired := make(chan struct{})
	timer := time.AfterFunc(b.idle, func() { b.finishCause(errReadIdle); close(fired) })
	n, err := b.source.Read(p)
	if n > 0 && b.observer != nil {
		b.observer.ObserveBodyRead(*b.purpose.Load(), n)
	}
	if !timer.Stop() {
		<-fired
		if !errors.Is(context.Cause(b.ctx), errReadIdle) {
			b.terminal = classify(b.ctx, b.ctx.Err(), false)
			return n, b.terminal
		}
		b.terminal = &storage.Error{Kind: storage.Timeout, Size: -1}
		return n, b.terminal
	}
	b.read += int64(n)
	if b.meta.Size >= 0 && b.read > b.meta.Size {
		b.finish()
		b.terminal = &storage.Error{Kind: storage.InvalidResponse, Size: -1}
		return 0, b.terminal
	}
	if err == io.EOF {
		if b.meta.Size >= 0 && b.read != b.meta.Size {
			b.finish()
			b.terminal = &storage.Error{Kind: storage.InvalidResponse, Size: -1}
			return n, b.terminal
		}
		if b.meta.Checksum == storage.ChecksumPending {
			b.meta.Checksum = storage.ChecksumVerified
		}
		b.finish()
		b.terminal = io.EOF
		return n, io.EOF
	}
	if err != nil {
		mapped := classify(b.ctx, err, false)
		b.finish()
		b.terminal = mapped
		return n, b.terminal
	}
	return n, nil
}
