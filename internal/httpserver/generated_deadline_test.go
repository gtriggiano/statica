package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

type deadlineWriter struct {
	header                  http.Header
	status, writes, flushes int
	deadlines               []time.Time
	flushError              error
}

func (w *deadlineWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *deadlineWriter) WriteHeader(status int)      { w.status = status }
func (w *deadlineWriter) Write(p []byte) (int, error) { w.writes++; return len(p), nil }
func (w *deadlineWriter) FlushError() error           { w.flushes++; return w.flushError }
func (w *deadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestGeneratedResponsesBoundHeadersBodyAndClearDeadline(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			underlying := &deadlineWriter{}
			response := &responseWriter{ResponseWriter: underlying, head: method == "HEAD", generated: generatedBudget{preparation: time.Now().Add(time.Second), writeIdle: time.Second, total: time.Now().Add(100 * time.Millisecond)}}
			start := time.Now()
			generatedResponse(preserveCapabilities(response), httptest.NewRequest(method, "http://example.org/", nil), 404)
			if response.status != 404 || underlying.flushes < 1 || len(underlying.deadlines) < 2 || !underlying.deadlines[len(underlying.deadlines)-1].IsZero() {
				t.Fatalf("status=%d deadlines=%v flush=%d", response.status, underlying.deadlines, underlying.flushes)
			}
			for _, deadline := range underlying.deadlines[:len(underlying.deadlines)-1] {
				if deadline.Before(start) || deadline.After(start.Add(150*time.Millisecond)) {
					t.Fatalf("not bounded by earliest total deadline: %v", deadline)
				}
			}
			if method == "HEAD" && underlying.writes != 0 {
				t.Fatal("HEAD wrote a body")
			}
		})
	}
}

func TestGeneratedExpiredPreparationProducesBounded504(t *testing.T) {
	underlying := &deadlineWriter{}
	response := &responseWriter{ResponseWriter: underlying, generated: generatedBudget{preparation: time.Now().Add(-time.Second), writeIdle: time.Second}}
	response.Header().Set("Location", "/redirect")
	generatedResponse(preserveCapabilities(response), httptest.NewRequest("GET", "http://example.org/", nil), 308)
	if response.status != 504 || response.generatedReason != "preparation_timeout" || response.Header().Get("Location") != "" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d reason=%s headers=%v", response.status, response.generatedReason, response.Header())
	}
}

func TestGeneratedWriteFailureAbortsReleasesSlotAndKeepsActualStatus(t *testing.T) {
	c := handlerConfig(t)
	c.Server.MaxConcurrentRequests = 1
	var observations []Observation
	h, err := NewHandler(c, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { generatedResponse(w, r, 404) }), WithObserver(func(o Observation) { observations = append(observations, o) }))
	if err != nil {
		t.Fatal(err)
	}
	underlying := &deadlineWriter{flushError: os.ErrDeadlineExceeded}
	func() {
		defer func() {
			if panicValue := recover(); panicValue != http.ErrAbortHandler {
				t.Fatalf("panic=%v", panicValue)
			}
		}()
		h.ServeHTTP(underlying, httptest.NewRequest("GET", "http://example.org/", nil))
		t.Fatal("write failure did not abort")
	}()
	if underlying.writes != 0 || underlying.status != 404 {
		t.Fatalf("error appended body or changed committed status: %+v", underlying)
	}
	last := observations[len(observations)-1]
	if last.Event != ObservationFinish || last.Status != 404 || last.Reason != "client_write_timeout" || last.Inflight != 0 {
		t.Fatalf("finish=%+v", last)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://example.org/", nil))
	if w.Code != 404 {
		t.Fatalf("slot leaked: %d", w.Code)
	}
	if !errors.Is(underlying.flushError, os.ErrDeadlineExceeded) {
		t.Fatal("test did not use timeout")
	}
}
