package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// limits are the admin server's own budgets, independent of the public
// server's. Admin requests are small and answered from memory, so absolute
// read and write deadlines, small headers and a small connection cap keep the
// listener bounded and available under public load. The short idle timeout
// keeps keep-alive scrapers from holding the slots that probes need.
type limits struct {
	readHeader, read, write, idle, shutdown time.Duration
	headerBytes, connections                int
}

var defaultLimits = limits{
	readHeader: 5 * time.Second, read: 10 * time.Second, write: 10 * time.Second,
	idle: 10 * time.Second, shutdown: 5 * time.Second, headerBytes: 8 << 10, connections: 16,
}

// Server serves the admin endpoints on one caller-bound listener, in
// cleartext HTTP/1.1: the admin listener is loopback or private by contract
// and unauthenticated. It exposes exactly /livez, /readyz and /metrics, for
// GET and HEAD, and no debug or profiling route.
type Server struct {
	http        *http.Server
	connections int
	shutdown    time.Duration
}

// NewServer prepares the admin server without binding a socket and discards
// its error events. metrics serves /metrics; while it is nil, /metrics is 404.
func NewServer(r *Readiness, metrics http.Handler) (*Server, error) {
	return newServer(r, metrics, nil, defaultLimits)
}

// NewServerWithLogger is NewServer reporting each handler panic, such as one
// in metrics, to logger as a fixed-text admin_http_error event. A nil logger
// discards.
func NewServerWithLogger(r *Readiness, metrics http.Handler, logger *slog.Logger) (*Server, error) {
	return newServer(r, metrics, logger, defaultLimits)
}

func newServer(r *Readiness, metrics http.Handler, logger *slog.Logger, l limits) (*Server, error) {
	if r == nil {
		return nil, errors.New("admin: a readiness model is required")
	}
	if l.connections <= 0 {
		return nil, errors.New("admin: a positive connection limit is required")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	return &Server{connections: l.connections, shutdown: l.shutdown, http: &http.Server{
		Handler: handler(r, metrics), Protocols: protocols,
		ReadHeaderTimeout: l.readHeader, ReadTimeout: l.read, WriteTimeout: l.write, IdleTimeout: l.idle,
		MaxHeaderBytes: l.headerBytes,
		ErrorLog:       log.New(httpErrorLog{logger}, "", 0),
	}}, nil
}

// httpErrorLog adapts net/http's error log. Its lines can carry client
// addresses and untrusted values, so only the fact of a handler panic becomes
// an event, without any of the line's text; every other line is discarded.
type httpErrorLog struct{ log *slog.Logger }

func (l httpErrorLog) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("http: panic serving ")) {
		l.log.Warn("admin_http_error", "error_class", "handler_panic")
	}
	return len(p), nil
}

// Serve owns and closes listener, including on failure. It returns
// http.ErrServerClosed after Shutdown or Close; any other error closes the
// active connections too.
func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("admin: a bound listener is required")
	}
	err := s.http.Serve(newLimitListener(listener, s.connections))
	if !errors.Is(err, http.ErrServerClosed) {
		_ = s.http.Close()
	}
	return err
}

// Shutdown stops accepting connections and drains active requests within the
// earlier of ctx's deadline and the admin shutdown budget. On expiry it closes
// the remaining connections and returns the context error. The server cannot
// be restarted.
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.shutdown)
	defer cancel()
	err := s.http.Shutdown(ctx)
	if err != nil {
		_ = s.http.Close()
	}
	return err
}

// Close immediately closes the listener and every connection.
func (s *Server) Close() error { return s.http.Close() }

// handler routes exact escaped paths only, so encoded spellings are not
// aliases. Every response carries Cache-Control: no-store. Probes and scrapes
// only read in-memory state.
func handler(r *Readiness, metrics http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		path := req.URL.EscapedPath()
		if path != "/livez" && path != "/readyz" && (path != "/metrics" || metrics == nil) {
			text(w, http.StatusNotFound, "not found\n")
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			text(w, http.StatusMethodNotAllowed, "method not allowed\n")
			return
		}
		switch path {
		case "/livez":
			text(w, http.StatusOK, "ok\n")
		case "/readyz":
			if ready, reason := r.Ready(); ready {
				text(w, http.StatusOK, "ready\n")
			} else {
				text(w, http.StatusServiceUnavailable, "not ready: "+string(reason)+"\n")
			}
		default:
			metrics.ServeHTTP(w, req)
		}
	})
}

// text writes a short plain-text response; net/http drops the body of a HEAD.
func text(w http.ResponseWriter, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// limitListener reserves one of cap(slots) connection slots before Accept, so
// excess clients wait in the operating system's bounded backlog rather than in
// an application queue. A connection releases its slot once, on Close.
type limitListener struct {
	net.Listener
	slots  chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newLimitListener(inner net.Listener, n int) *limitListener {
	return &limitListener{Listener: inner, slots: make(chan struct{}, n), closed: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.slots }}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
