package httpserver

import (
	"net"
	"sync"
)

// connectionListener reserves capacity before Accept, leaving excess clients in
// the operating system's bounded backlog. There is no application connection
// queue or goroutine per waiting client. TLS wraps this listener, so handshakes
// and idle keep-alives consume the same capacity as active HTTP connections.
type connectionListener struct {
	net.Listener
	mu       sync.Mutex
	ready    *sync.Cond
	limit    int
	used     int
	closed   bool
	closeErr error
}

func newConnectionListener(inner net.Listener, limit int) *connectionListener {
	l := &connectionListener{Listener: inner, limit: limit}
	l.ready = sync.NewCond(&l.mu)
	return l
}

func (l *connectionListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	for !l.closed && l.used == l.limit {
		l.ready.Wait()
	}
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	l.used++
	l.mu.Unlock()
	c, err := l.Listener.Accept()
	if err != nil {
		l.release()
		return nil, err
	}
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		_ = c.Close()
		l.release()
		return nil, net.ErrClosed
	}
	return &connection{Conn: c, release: l.release}, nil
}

func (l *connectionListener) release() {
	l.mu.Lock()
	l.used--
	l.ready.Signal()
	l.mu.Unlock()
}

func (l *connectionListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.ready.Broadcast()
		l.closeErr = l.Listener.Close()
	}
	return l.closeErr
}

type connection struct {
	net.Conn
	once    sync.Once
	release func()
	err     error
}

func (c *connection) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.release()
	})
	return c.err
}
