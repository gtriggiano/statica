package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-logfmt/logfmt"
)

// lockedBuffer is a concurrency-safe destination.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// event is one decoded logfmt record: its keys in order and their values.
type event struct {
	keys   []string
	values map[string]string
}

// decodeEvents decodes text with the go-logfmt decoder, requiring one
// nonempty record per line, unique keys, and the common event shape.
func decodeEvents(t testing.TB, text string) []event {
	t.Helper()
	dec := logfmt.NewDecoderSize(strings.NewReader(text), 4<<20)
	var events []event
	for dec.ScanRecord() {
		e := event{values: map[string]string{}}
		for dec.ScanKeyval() {
			k := string(dec.Key())
			if _, dup := e.values[k]; dup {
				t.Fatalf("duplicate key %q in %q", k, text)
			}
			e.keys = append(e.keys, k)
			e.values[k] = string(dec.Value())
		}
		checkShape(t, e)
		events = append(events, e)
	}
	if err := dec.Err(); err != nil {
		t.Fatalf("decode: %v in %q", err, text)
	}
	if lines := strings.Count(text, "\n"); lines != len(events) || text != "" && !strings.HasSuffix(text, "\n") {
		t.Fatalf("%d lines for %d events: %q", lines, len(events), text)
	}
	return events
}

func checkShape(t testing.TB, e event) {
	t.Helper()
	if len(e.keys) < 3 || e.keys[0] != "ts" || e.keys[1] != "level" || e.keys[2] != "event" {
		t.Fatalf("event must start with ts, level, event: %v", e.keys)
	}
	if _, ok := e.values["time"]; ok {
		t.Fatalf("event has a time key: %v", e.keys)
	}
	ts := e.values["ts"]
	parsed, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || !strings.HasSuffix(ts, "Z") || parsed.Location() != time.UTC || len(ts) != len("2006-01-02T15:04:05.000Z") {
		t.Fatalf("ts %q is not UTC RFC 3339 with milliseconds: %v", ts, err)
	}
}

func TestHandlerRoundTripsHostileValues(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	longUnicode := strings.Repeat("☃", MaxValueBytes/3+1) // three bytes each: the bound falls inside a rune
	for _, tc := range []struct{ name, value, want string }{
		{"newline", "a\nb\nlevel=error event=forged", "a\nb\nlevel=error event=forged"},
		{"cr_tab", "a\rb\tc", "a\rb\tc"},
		{"quote", `say "hi"`, `say "hi"`},
		{"equals", "a=b", "a=b"},
		{"backslash", `c:\path\`, `c:\path\`},
		{"controls", "\x00\x01\x1b[31m\x7f", "\x00\x01\x1b[31m\x7f"},
		{"invalid_utf8", "ok\xff\xfeok", "ok\uFFFD\uFFFDok"},
		{"c1_control", "a\u0085b", "a\uFFFDb"},
		{"separators", "a\u2028b\u2029c", "a\uFFFDb\uFFFDc"},
		{"null", "null", "null"},
		{"empty", "", ""},
		{"spaces", "  padded  ", "  padded  "},
		{"unicode", "café ☃", "café ☃"},
		{"long", long, long[:MaxValueBytes]},
		{"long_unicode", longUnicode, longUnicode[:MaxValueBytes/3*3]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out lockedBuffer
			slog.New(NewHandler(&out, nil)).Info("event "+tc.value, "value", tc.value, "after", "kept")
			events := decodeEvents(t, out.String())
			if len(events) != 1 {
				t.Fatalf("%d events", len(events))
			}
			e := events[0]
			if got := e.values["value"]; got != tc.want {
				t.Fatalf("value %q, want %q", got[:min(len(got), 64)], tc.want[:min(len(tc.want), 64)])
			}
			if !utf8.ValidString(e.values["value"]) || e.values["after"] != "kept" || !strings.HasPrefix(e.values["event"], "event ") {
				t.Fatalf("event %v", e.values)
			}
		})
	}
}

func TestHandlerKeysAreUniqueAndSafe(t *testing.T) {
	var out lockedBuffer
	logger := slog.New(NewHandler(&out, nil)).With("dup", "logger", "ts", "forged")
	logger.Info("keys", "dup", "record", "time", "forged", "level", "forged", "event", "forged",
		"bad key=\"x", "cleaned", "ünï", "ascii only", "\n", "dropped", "", "empty key", "z", "last")
	e := decodeEvents(t, out.String())[0]
	want := []string{"ts", "level", "event", "dup", "badkeyx", "n", "z"}
	if strings.Join(e.keys, " ") != strings.Join(want, " ") {
		t.Fatalf("keys %v, want %v", e.keys, want)
	}
	if e.values["dup"] != "logger" || e.values["level"] != "info" || e.values["event"] != "keys" || e.values["badkeyx"] != "cleaned" {
		t.Fatalf("values %v", e.values)
	}
}

type valuer struct{}

func (valuer) LogValue() slog.Value { return slog.StringValue("resolved") }

type panicky struct{}

func (panicky) String() string { panic("boom") }

func TestHandlerFlattensGroupsInStableOrder(t *testing.T) {
	var out lockedBuffer
	logger := slog.New(NewHandler(&out, nil)).With("component", "test", slog.Group("build", "version", "1.0")).WithGroup("req").WithGroup("")
	at := time.Date(2026, 1, 2, 3, 4, 5, 6e6, time.FixedZone("plus1", 3600))
	for range 2 {
		logger.Info("diag", "n", 3, slog.Group("cache", "hit", true, slog.Group("fill", "bytes", int64(-2))),
			slog.Group("", "inline", "x"), slog.Group("empty"), "d", 1500*time.Millisecond, "f", 0.25,
			"big", 1e21, "u", uint64(7), "t", at, "err", errors.New("boom"), "valuer", valuer{},
			"struct", struct{ A int }{1}, "panics", panicky{}, "nil", nil)
	}
	logger.WithGroup("unused").Info("bare")
	events := decodeEvents(t, out.String())
	if len(events) != 3 {
		t.Fatalf("%d events", len(events))
	}
	want := []string{"ts", "level", "event", "component", "build.version", "req.n", "req.cache.hit", "req.cache.fill.bytes",
		"req.inline", "req.d", "req.f", "req.big", "req.u", "req.t", "req.err", "req.valuer", "req.struct", "req.panics", "req.nil"}
	values := map[string]string{"component": "test", "build.version": "1.0", "req.n": "3", "req.cache.hit": "true",
		"req.cache.fill.bytes": "-2", "req.inline": "x", "req.d": "1.5s", "req.f": "0.25", "req.big": "1000000000000000000000",
		"req.u": "7", "req.t": "2026-01-02T02:04:05.006Z", "req.err": "boom", "req.valuer": "resolved", "req.struct": "{1}",
		"req.panics": "!PANIC", "req.nil": "<nil>"}
	for _, e := range events[:2] {
		if strings.Join(e.keys, " ") != strings.Join(want, " ") {
			t.Fatalf("keys %v\nwant %v", e.keys, want)
		}
		for k, v := range values {
			if e.values[k] != v {
				t.Fatalf("%s = %q, want %q", k, e.values[k], v)
			}
		}
	}
	if keys := strings.Join(events[2].keys, " "); keys != "ts level event component build.version" {
		t.Fatalf("an empty group must not emit keys: %v", keys)
	}
}

func TestHandlerLevelFiltering(t *testing.T) {
	var out lockedBuffer
	level := new(slog.LevelVar)
	level.Set(slog.LevelWarn)
	h := NewHandler(&out, &HandlerOptions{Level: level})
	logger := slog.New(h)
	logger.Debug("debug")
	logger.Info("info")
	logger.Warn("warn")
	logger.Error("error")
	logger.Log(context.Background(), slog.LevelError+2, "custom")
	level.Set(slog.LevelDebug)
	logger.Debug("debug again")
	var got []string
	for _, e := range decodeEvents(t, out.String()) {
		got = append(got, e.values["level"]+":"+e.values["event"])
	}
	if want := "warn:warn error:error error+2:custom debug:debug again"; strings.Join(got, " ") != want {
		t.Fatalf("events %v, want %s", got, want)
	}
	if NewHandler(&out, nil).Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("the default level must be info")
	}
	for text, want := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := parseLevel(text); err != nil || got != want {
			t.Fatalf("parseLevel(%q) = %v, %v", text, got, err)
		}
	}
	for _, text := range []string{"", "INFO", "trace", "warning"} {
		if _, err := parseLevel(text); err == nil {
			t.Fatalf("parseLevel(%q) accepted", text)
		}
	}
}
