package httpserver

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/redirect"
)

type controllerWriter struct {
	header                      http.Header
	status                      int
	readDeadline, writeDeadline time.Time
	duplex                      bool
}

func (w *controllerWriter) Header() http.Header         { return w.header }
func (w *controllerWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *controllerWriter) WriteHeader(status int)      { w.status = status }
func (w *controllerWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return nil
}
func (w *controllerWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return nil
}
func (w *controllerWriter) EnableFullDuplex() error { w.duplex = true; return nil }

type optionalWriter struct {
	*controllerWriter
	flush, hijack, push bool
}

func (w *optionalWriter) Flush() { w.flush = true }
func (w *optionalWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijack = true
	return nil, nil, nil
}
func (w *optionalWriter) Push(string, *http.PushOptions) error { w.push = true; return nil }

func TestHandlerPreservesResponseControllerAndCapabilities(t *testing.T) {
	for _, optional := range []bool{false, true} {
		base := &controllerWriter{header: make(http.Header)}
		all := &optionalWriter{controllerWriter: base}
		var underlying http.ResponseWriter = base
		if optional {
			underlying = all
		}
		deadline := time.Now().Add(time.Minute)
		h := makeHandler(t, handlerConfig(t), func(w http.ResponseWriter, r *http.Request) {
			controller := http.NewResponseController(w)
			if err := controller.SetReadDeadline(deadline); err != nil {
				t.Error(err)
			}
			if err := controller.SetWriteDeadline(deadline); err != nil {
				t.Error(err)
			}
			if err := controller.EnableFullDuplex(); err != nil {
				t.Error(err)
			}
			flusher, flush := w.(http.Flusher)
			hijacker, hijack := w.(http.Hijacker)
			pusher, push := w.(http.Pusher)
			if flush != optional || hijack != optional || push != optional {
				t.Errorf("capabilities changed: flush=%v hijack=%v push=%v optional=%v", flush, hijack, push, optional)
			}
			if optional {
				flusher.Flush()
				_, _, _ = hijacker.Hijack()
				_ = pusher.Push("/asset", nil)
			} else if err := controller.Flush(); !errors.Is(err, http.ErrNotSupported) {
				t.Errorf("unsupported flush=%v", err)
			}
		})
		h.ServeHTTP(underlying, boundaryRequest("GET", "/"))
		if !base.readDeadline.Equal(deadline) || !base.writeDeadline.Equal(deadline) || !base.duplex {
			t.Errorf("controller not forwarded: %+v", base)
		}
		if optional && (!all.flush || !all.hijack || !all.push) {
			t.Errorf("capability not forwarded: %+v", all)
		}
		if base.header.Get("X-Content-Type-Options") != "nosniff" || base.status != 200 {
			t.Errorf("flush omitted boundary headers: %+v", base)
		}
	}
}

func TestHandlerRejectBodyReadDeadline(t *testing.T) {
	base := &controllerWriter{header: make(http.Header)}
	h := makeHandler(t, handlerConfig(t), func(http.ResponseWriter, *http.Request) { t.Error("body reached content") })
	r := boundaryRequest("GET", "/")
	r.ContentLength = 10
	r.Body = &unreadBody{}
	before := time.Now()
	h.ServeHTTP(base, r)
	if base.readDeadline.Before(before) || base.readDeadline.After(time.Now()) || base.status != 400 {
		t.Fatalf("deadline=%v status=%d", base.readDeadline, base.status)
	}
}

func TestHandlerBareQueryRedirect(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.Redirects = []redirect.Rule{{From: "/old", To: "/new#section", Status: 308, Query: "preserve"}}
	h := makeHandler(t, cfg, func(http.ResponseWriter, *http.Request) { t.Error("redirect reached content") })
	for _, tc := range []struct{ target, location string }{{"/old", "/new#section"}, {"/old?", "/new?#section"}, {"/old?a=1", "/new?a=1#section"}} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, boundaryRequest("GET", tc.target))
		if w.Code != 308 || w.Header().Get("Location") != tc.location {
			t.Errorf("%s: status=%d location=%q", tc.target, w.Code, w.Header().Get("Location"))
		}
	}
}
