package httpserver

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

type responseWriter struct {
	http.ResponseWriter
	status          int
	committed       time.Time // when status was committed
	written         int64     // body bytes the underlying writer accepted
	head            bool
	requestID       string
	static          map[string]string
	cors            http.Header
	generated       generatedBudget
	generatedReason string
}

type generatedBudget struct {
	preparation, total time.Time
	writeIdle          time.Duration
}

func (w *responseWriter) responseBudget() generatedBudget  { return w.generated }
func (w *responseWriter) setGeneratedReason(reason string) { w.generatedReason = reason }

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *responseWriter) policyHeaders() {
	h := w.Header()
	for name, value := range w.static {
		h.Set(name, value)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Request-ID", w.requestID)
	// Grants are computed by the boundary, never by the content handler.
	for _, name := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers"} {
		h.Del(name)
		for _, value := range w.cors.Values(name) {
			h.Add(name, value)
		}
	}
	for _, value := range w.cors.Values("Vary") {
		mergeVary(h, value)
	}
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.policyHeaders()
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status = status
		w.committed = time.Now()
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.head {
		return len(p), nil
	}
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

// FlushError and Unwrap preserve ResponseController operations, including
// request/stream-scoped deadline support supplied by net/http.
func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

type flushWriter struct{ *responseWriter }

func (w flushWriter) Flush() { _ = w.FlushError() }

type hijackWriter struct{ *responseWriter }

func (w hijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

type flushHijackWriter struct{ *responseWriter }

func (w flushHijackWriter) Flush() { _ = w.FlushError() }
func (w flushHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}

type pushWriter struct {
	*responseWriter
	http.Pusher
}
type flushPushWriter struct {
	flushWriter
	http.Pusher
}
type hijackPushWriter struct {
	hijackWriter
	http.Pusher
}
type flushHijackPushWriter struct {
	flushHijackWriter
	http.Pusher
}

func preserveCapabilities(w *responseWriter) http.ResponseWriter {
	_, flush := w.ResponseWriter.(http.Flusher)
	_, hijack := w.ResponseWriter.(http.Hijacker)
	push, canPush := w.ResponseWriter.(http.Pusher)
	switch {
	case flush && hijack && canPush:
		return flushHijackPushWriter{flushHijackWriter{w}, push}
	case flush && canPush:
		return flushPushWriter{flushWriter{w}, push}
	case hijack && canPush:
		return hijackPushWriter{hijackWriter{w}, push}
	case canPush:
		return pushWriter{w, push}
	case flush && hijack:
		return flushHijackWriter{w}
	case flush:
		return flushWriter{w}
	case hijack:
		return hijackWriter{w}
	default:
		return w
	}
}

func generatedResponse(w http.ResponseWriter, r *http.Request, status int) {
	var budget generatedBudget
	if bounded, ok := w.(interface{ responseBudget() generatedBudget }); ok {
		budget = bounded.responseBudget()
	}
	now := time.Now()
	if budget.writeIdle > 0 && (now.After(budget.preparation) || !budget.total.IsZero() && now.After(budget.total)) {
		reason := "preparation_timeout"
		if !budget.total.IsZero() && now.After(budget.total) {
			reason = "total_deadline"
		}
		if observed, ok := w.(interface{ setGeneratedReason(string) }); ok {
			observed.setGeneratedReason(reason)
		}
		status = http.StatusGatewayTimeout
		w.Header().Del("Location")
		// Expired preparation/total limits still allow a final bounded 504.
		budget.total = time.Time{}
	}
	controller := http.NewResponseController(w)
	abort := func(err error) {
		reason := "client_write_failure"
		var timeout interface{ Timeout() bool }
		switch {
		case !budget.total.IsZero() && !time.Now().Before(budget.total):
			reason = "total_deadline"
		case errors.As(err, &timeout) && timeout.Timeout():
			reason = "client_write_timeout"
		case errors.Is(context.Cause(r.Context()), ErrShutdown):
			reason = "shutdown"
		case r.Context().Err() != nil:
			reason = "disconnect"
		}
		if observed, ok := w.(interface{ setGeneratedReason(string) }); ok {
			observed.setGeneratedReason(reason)
		}
		panic(http.ErrAbortHandler)
	}
	deadline := func() {
		if budget.writeIdle <= 0 {
			return
		}
		d := time.Now().Add(budget.writeIdle)
		if !budget.total.IsZero() && budget.total.Before(d) {
			d = budget.total
		}
		if err := controller.SetWriteDeadline(d); err != nil && !errors.Is(err, http.ErrNotSupported) {
			abort(err)
		}
	}
	flush := func() {
		if err := controller.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			abort(err)
		}
	}
	clear := func() {
		if budget.writeIdle > 0 {
			if err := controller.SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
				abort(err)
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Expires")
	if status == http.StatusNoContent {
		deadline()
		w.WriteHeader(status)
		flush()
		clear()
		return
	}
	body := http.StatusText(status) + "\n"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	deadline()
	w.WriteHeader(status)
	flush()
	if r.Method != http.MethodHead {
		deadline()
		n, err := w.Write([]byte(body))
		if err == nil && n != len(body) {
			err = io.ErrShortWrite
		}
		if err != nil {
			abort(err)
		}
		flush()
	}
	clear()
}

func hasRequestBody(r *http.Request) bool {
	// Server requests can have a non-nil Body even without payload (notably
	// HTTP/2). Framing, rather than Body's concrete type, establishes a body.
	return r.ContentLength != 0 || len(r.TransferEncoding) != 0
}

func stopRequestBody(w http.ResponseWriter, r *http.Request) {
	if !hasRequestBody(r) {
		return
	}
	if r.ProtoMajor < 2 {
		r.Close = true
		w.Header().Set("Connection", "close")
	}
	// net/http otherwise may wait for delayed bytes while closing an unread
	// request body. Do not read payload bytes just to return a rejection.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now())
}
