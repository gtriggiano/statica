package httpserver

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gtriggiano/statica/internal/config"
)

// ErrShutdown is the safe cancellation cause for active requests stopped by
// Close or an exhausted drain budget. Handlers can distinguish process teardown
// from an individual client's disconnect without inspecting transport errors.
var ErrShutdown = errors.New("HTTP server shutdown")

// Server serves an injected application handler on one caller-bound listener.
// It is constructed with all its dependencies before any socket is bound, so
// the application lifecycle (internal/app) binds the public and admin
// listeners synchronously and rolls back either bind without exposing a
// partially initialized application.
type Server struct {
	http            *http.Server
	maxConnections  int
	shutdownTimeout time.Duration
	cancel          context.CancelCauseFunc
	done            <-chan struct{}
	mu              sync.Mutex
	served          bool
	closed          bool
}

// ServerOption configures a Server.
type ServerOption func(*serverOptions)

type serverOptions struct{ errorLog *slog.Logger }

// WithErrorLogger reports net/http's own error log for this server to logger,
// reduced to fixed text: a handler panic becomes one warn http_server_error
// event with error_class=handler_panic, and every other line is discarded.
// The lines carry client addresses and untrusted values (TLS handshake
// errors, for example), so no part of a line is ever copied. Without this
// option, or with a nil logger, the error log is discarded.
func WithErrorLogger(logger *slog.Logger) ServerOption {
	return func(o *serverOptions) { o.errorLog = logger }
}

// NewServer prepares TLS material without binding a socket. cfg must contain
// parsed configuration defaults. Nil handlers are rejected so the process-wide
// DefaultServeMux is never exposed accidentally.
//
// The server enforces header, keep-alive, connection capacity, handshake and
// drain limits. The D13 response-preparation, write-inactivity and optional
// total-request deadlines are enforced per request by the handlers it serves
// (this package's handler for generated responses, the content handler for
// objects), never by connection deadlines: ReadTimeout and WriteTimeout
// deliberately remain zero, as their absolute deadlines would truncate
// progressing long downloads. WithErrorLogger reports net/http's handler
// panics; Done reports when request contexts have been cancelled.
func NewServer(cfg config.Server, handler http.Handler, options ...ServerOption) (*Server, error) {
	if handler == nil {
		return nil, errors.New("httpserver: a handler is required")
	}
	if cfg.ReadHeaderTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.ShutdownTimeout <= 0 || cfg.MaxHeaderBytes <= 0 || cfg.MaxConnections <= 0 {
		return nil, errors.New("httpserver: positive header, idle, shutdown and connection limits are required")
	}
	var tlsConfig *tls.Config
	if cfg.TLS != nil {
		var minVersion uint16
		switch cfg.TLS.MinVersion {
		case "1.2":
			minVersion = tls.VersionTLS12
		case "1.3":
			minVersion = tls.VersionTLS13
		default:
			return nil, errors.New("httpserver: TLS minimum version must be 1.2 or 1.3")
		}
		pair, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, errors.New("httpserver: cannot load a matching TLS certificate and private key")
		}
		for _, der := range pair.Certificate {
			if _, err := x509.ParseCertificate(der); err != nil {
				return nil, errors.New("httpserver: TLS chain contains invalid certificate material")
			}
		}
		tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{pair}, MinVersion: minVersion,
			NextProtos: []string{"h2", "http/1.1"},
		}
	}
	var o serverOptions
	for _, option := range options {
		if option != nil {
			option(&o)
		}
	}
	// net/http errors can include untrusted TLS/client details, so they never
	// reach the global logger.
	errorLog := log.New(io.Discard, "", 0)
	if o.errorLog != nil {
		errorLog = log.New(httpErrorLog{o.errorLog}, "", 0)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(tlsConfig != nil)
	s := &Server{maxConnections: cfg.MaxConnections, shutdownTimeout: cfg.ShutdownTimeout, cancel: cancel, done: ctx.Done()}
	s.http = &http.Server{
		Handler: handler, TLSConfig: tlsConfig, Protocols: protocols,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout, IdleTimeout: cfg.IdleTimeout,
		MaxHeaderBytes: cfg.MaxHeaderBytes,
		BaseContext:    func(net.Listener) context.Context { return ctx },
		ErrorLog:       errorLog,
	}
	return s, nil
}

// httpErrorLog adapts net/http's error log: only the fact of a handler panic,
// over HTTP/1.1 or HTTP/2, becomes an event, without any of the line's text.
type httpErrorLog struct{ log *slog.Logger }

func (l httpErrorLog) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("http: panic serving ")) || bytes.HasPrefix(p, []byte("http2: panic serving ")) {
		l.log.Warn("http_server_error", "error_class", "handler_panic")
	}
	return len(p), nil
}

// Serve owns and closes listener, including on failure. It can be called once.
// It returns http.ErrServerClosed after Shutdown or Close. TLS configuration
// never falls back to serving cleartext content.
func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("httpserver: a bound listener is required")
	}
	defer listener.Close()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return http.ErrServerClosed
	}
	if s.served {
		s.mu.Unlock()
		return errors.New("httpserver: Serve may only be called once")
	}
	s.served = true
	s.mu.Unlock()
	limited := newConnectionListener(listener, s.maxConnections)
	var err error
	if s.http.TLSConfig == nil {
		err = s.http.Serve(limited)
	} else {
		err = s.http.ServeTLS(limited, "", "")
	}
	if !errors.Is(err, http.ErrServerClosed) {
		// Unexpected listener failure must not leave existing handlers or
		// connections orphaned after the serving loop exits.
		_ = s.Close()
	}
	return err
}

// Shutdown stops accepting work and drains active requests within the earlier
// of ctx's deadline and the configured shutdown budget. On expiry it cancels
// active request contexts and force-closes their connections, returning the
// context error. The server cannot be restarted after shutdown.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, s.shutdownTimeout)
	defer cancel()
	err := s.http.Shutdown(ctx)
	s.cancel(ErrShutdown)
	if err != nil {
		_ = s.http.Close()
	}
	return err
}

// Done is closed once the server has cancelled every request context with
// cause ErrShutdown: when Shutdown returns, whether it drained or its budget
// expired, or when Close begins, including the Close of an unexpected
// listener failure. Resources that active requests may still wait on can be
// released after it, without failing a draining request early.
func (s *Server) Done() <-chan struct{} { return s.done }

// Close immediately cancels active requests and closes their connections.
func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel(ErrShutdown)
	return s.http.Close()
}
