package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gtriggiano/statica/internal/config"
)

func TestHandlerCORS(t *testing.T) {
	cfg := handlerConfig(t)
	cfg.CORS = &config.CORS{AllowedOrigins: []string{"https://client.example", "http://localhost:3000", "https://[::1]:8443", "https://café.example"}}
	calls := 0
	h := makeHandler(t, cfg, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Vary", "Accept-Encoding")
		w.WriteHeader(200)
	})
	for _, tc := range []struct {
		name, method, origin, requestedMethod, headers string
		status                                         int
		grant                                          bool
	}{
		{"get", "GET", "https://client.example", "", "", 200, true},
		{"head", "HEAD", "https://client.example", "", "", 200, true},
		{"default port equivalent", "GET", "https://CLIENT.example:443", "", "", 200, true},
		{"no origin", "GET", "", "", "", 200, false},
		{"disallowed ordinary", "GET", "https://evil.example", "", "", 200, false},
		{"null ordinary", "GET", "null", "", "", 200, false},
		{"preflight", "OPTIONS", "https://client.example", "GET", "Range, If-None-Match", 204, true},
		{"preflight HEAD", "OPTIONS", "https://client.example", "HEAD", "If-Match,If-Modified-Since,If-Unmodified-Since,If-Range", 204, true},
		{"IPv6", "OPTIONS", "https://[::1]:8443", "GET", "", 204, true},
		{"IDNA config", "OPTIONS", "https://xn--caf-dma.example", "GET", "", 204, true},
		{"denied preflight", "OPTIONS", "https://evil.example", "GET", "", 403, false},
		{"null preflight", "OPTIONS", "null", "GET", "", 403, false},
		{"wildcard", "OPTIONS", "*", "GET", "", 403, false},
		{"path in origin", "OPTIONS", "https://client.example/", "GET", "", 403, false},
		{"fragment origin", "OPTIONS", "https://client.example#", "GET", "", 403, false},
		{"credentials origin", "OPTIONS", "https://user@client.example", "GET", "", 403, false},
		{"bad preflight method", "OPTIONS", "https://client.example", "POST", "", 403, false},
		{"lowercase method", "OPTIONS", "https://client.example", "get", "", 403, false},
		{"missing method", "OPTIONS", "https://client.example", "", "", 403, false},
		{"auth header", "OPTIONS", "https://client.example", "GET", "Authorization", 403, false},
		{"unknown header", "OPTIONS", "https://client.example", "GET", "Range, X-Evil", 403, false},
		{"empty field", "OPTIONS", "https://client.example", "GET", "Range,", 403, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			r := boundaryRequest(tc.method, "/")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.requestedMethod != "" {
				r.Header.Set("Access-Control-Request-Method", tc.requestedMethod)
			}
			if tc.headers != "" {
				r.Header.Set("Access-Control-Request-Headers", tc.headers)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || (w.Header().Get("Access-Control-Allow-Origin") != "") != tc.grant {
				t.Fatalf("status=%d headers=%+v", w.Code, w.Header())
			}
			if tc.grant && w.Header().Get("Access-Control-Allow-Origin") != tc.origin {
				t.Error("origin was not echoed exactly")
			}
			if w.Header().Get("Access-Control-Allow-Credentials") != "" {
				t.Error("credential grant")
			}
			vary := strings.Join(w.Header().Values("Vary"), ",")
			if !strings.Contains(vary, "Origin") {
				t.Errorf("Vary=%q", vary)
			}
			if tc.method == "OPTIONS" {
				if calls != before {
					t.Error("preflight reached content")
				}
				if !strings.Contains(vary, "Access-Control-Request-Method") || !strings.Contains(vary, "Access-Control-Request-Headers") || w.Header().Get("Cache-Control") != "no-store" {
					t.Errorf("preflight headers: %+v", w.Header())
				}
				if tc.grant && w.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD" {
					t.Error("preflight methods")
				}
			} else if calls != before+1 || !strings.Contains(vary, "Accept-Encoding") {
				t.Error("ordinary CORS or Vary handling")
			}
		})
	}
	for _, header := range []string{"Origin", "Access-Control-Request-Method"} {
		r := boundaryRequest("OPTIONS", "/")
		r.Header.Set("Origin", "https://client.example")
		r.Header.Set("Access-Control-Request-Method", "GET")
		r.Header.Add(header, r.Header.Get(header))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Errorf("duplicate %s accepted: %d", header, w.Code)
		}
	}
	r := boundaryRequest("POST", "/")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD, OPTIONS" {
		t.Errorf("CORS Allow: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestHandlerParsedIPv6CORSConfiguration(t *testing.T) {
	cfg, err := config.Parse([]byte("s3:\n  bucket: test-bucket\ncors:\n  allowed_origins: ['https://[::1]']\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := makeHandler(t, cfg, func(http.ResponseWriter, *http.Request) { t.Error("preflight reached content") })
	r := boundaryRequest("OPTIONS", "/")
	r.Header.Set("Origin", "https://[::1]")
	r.Header.Set("Access-Control-Request-Method", "GET")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "https://[::1]" {
		t.Fatalf("status=%d headers=%+v", w.Code, w.Header())
	}
}

func TestHandlerInvalidTargetHasNoHostObservation(t *testing.T) {
	var events []Observation
	h := makeHandler(t, handlerConfig(t), func(http.ResponseWriter, *http.Request) { t.Error("invalid target reached content") }, WithObserver(func(o Observation) { events = append(events, o) }))
	r := boundaryRequest("GET", "/a%2fb")
	r.Host = "attacker-controlled.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("status=%d", w.Code)
	}
	for _, o := range events {
		if o.Host != "" {
			t.Errorf("invalid input produced host observation: %+v", o)
		}
	}
}
