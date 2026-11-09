package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/gtriggiano/statica/internal/config"
)

const (
	// MaxPathBytes bounds the logged path, after query redaction.
	MaxPathBytes = 8 << 10
	// Redacted replaces the value of each redacted query parameter.
	Redacted = "[REDACTED]"

	maxAuthorityBytes = 256
	maxMethodBytes    = 32
)

// NewLogger builds the process logger from validated observability
// configuration: logfmt events at the configured level, written to w
// through a bounded Output. The caller closes the Output on shutdown and
// reads its drop count for metrics.
func NewLogger(w io.Writer, cfg config.Observability) (*slog.Logger, *Output, error) {
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, nil, err
	}
	output := NewOutput(w, OutputOptions{Level: level})
	return slog.New(NewHandler(output, &HandlerOptions{Level: level})), output, nil
}

func parseLevel(text string) (slog.Level, error) {
	switch text {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid log level")
}

// AccessLogger turns each finalized Record into one http_request event.
// Install its Sink as the httpserver record sink.
//
// Levels: info for served and expected missing objects, redirects and other
// complete responses; warn for rejected requests (malformed input, unknown
// hosts, local overload), for incomplete transfers that are not the origin's
// fault, and whenever the client disconnected, timed out or failed a write
// or the server was shutting down, even after an origin failure; error for
// origin failures, including a denial D05 masks as a public 404, for
// transfers the origin interrupted, and for handler panics.
//
// Fields, in order, each omitted when empty unless stated: request_id,
// request_id_source, request_id_reason, host (the admitted normalized
// hostname) or authority (the bounded raw authority of a request that never
// passed host admission), method, path (the original escaped path and query
// before index or SPA resolution, query values redacted, omitted when the
// target is unparseable), query_redaction_failed and path_truncated (only
// when true), status (always), outcome, resolution, cache, cache_reason,
// cache_coalesced (only when true), origin_key (only for object-backed
// responses), duration_ms (always), ttfb_ms (when a status was committed),
// bytes_written (always), error_class, termination_reason, cache_diagnostic
// and, with log_client_ip, client_ip, peer_ip, client_ip_source and
// client_ip_reason. Request headers, redirect destinations and provider
// errors are never part of a Record, hence never logged.
type AccessLogger struct {
	handler  slog.Handler
	enabled  bool
	clientIP bool
	redact   []string
}

// NewAccessLogger writes access events to h. access_log: false disables
// them without affecting other events.
func NewAccessLogger(h slog.Handler, cfg config.Observability) *AccessLogger {
	return &AccessLogger{handler: h, enabled: cfg.AccessLog && h != nil, clientIP: cfg.LogClientIP,
		redact: slices.Clone(cfg.RedactQueryParams)}
}

var attrPool = sync.Pool{New: func() any { a := make([]slog.Attr, 0, 32); return &a }}

// Sink logs r. It is safe for concurrent use.
func (a *AccessLogger) Sink(r *Record) {
	if !a.enabled || r == nil {
		return
	}
	level := accessLevel(r)
	ctx := context.Background()
	if !a.handler.Enabled(ctx, level) {
		return
	}
	pooled := attrPool.Get().(*[]slog.Attr)
	attrs := (*pooled)[:0]
	text := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	flag := func(key string, value bool) {
		if value {
			attrs = append(attrs, slog.Bool(key, true))
		}
	}
	text("request_id", r.RequestID)
	text("request_id_source", r.RequestIDSource)
	text("request_id_reason", r.RequestIDReason)
	text("host", r.Host)
	if r.Host == "" {
		text("authority", bound(r.Authority, maxAuthorityBytes))
	}
	text("method", bound(r.Method, maxMethodBytes))
	path, failed, truncated := logPath(r.EscapedPath, r.RawQuery, r.ForceQuery, a.redact)
	text("path", path)
	flag("query_redaction_failed", failed)
	flag("path_truncated", truncated)
	attrs = append(attrs, slog.Int("status", r.Status))
	text("outcome", r.Outcome)
	text("resolution", r.Resolution)
	text("cache", r.CacheOutcome)
	text("cache_reason", r.CacheReason)
	flag("cache_coalesced", r.CacheCoalesced)
	text("origin_key", r.OriginKey)
	attrs = append(attrs, slog.Float64("duration_ms", float64(r.Duration().Microseconds())/1000))
	if ttfb, ok := r.TTFB(); ok {
		attrs = append(attrs, slog.Float64("ttfb_ms", float64(ttfb.Microseconds())/1000))
	}
	attrs = append(attrs, slog.Int64("bytes_written", r.BytesWritten))
	text("error_class", errorClass(r))
	text("termination_reason", r.TerminationReason)
	text("cache_diagnostic", r.CacheDiagnostic)
	if a.clientIP {
		if r.ClientIP.IsValid() {
			text("client_ip", r.ClientIP.String())
		}
		if r.PeerIP.IsValid() {
			text("peer_ip", r.PeerIP.String())
		}
		text("client_ip_source", r.ClientIPSource)
		text("client_ip_reason", r.AttributionReason)
	}
	record := slog.NewRecord(r.Finished, level, "http_request", 0)
	record.AddAttrs(attrs...)
	clear(attrs)
	*pooled = attrs[:0]
	attrPool.Put(pooled)
	_ = a.handler.Handle(ctx, record)
}

func accessLevel(r *Record) slog.Level {
	switch r.TerminationReason {
	case "disconnect", "client_write_timeout", "client_write_failure", "shutdown":
		return slog.LevelWarn
	}
	switch {
	case r.Reason == "handler_panic", r.Outcome == OutcomeOriginFailure:
		return slog.LevelError
	case r.Outcome == OutcomeIncomplete:
		switch r.TerminationReason {
		case "origin_read_timeout", "origin_read_failure", "checksum_mismatch", "length_mismatch", "origin_failure":
			return slog.LevelError
		}
		return slog.LevelWarn
	case r.Rejected, r.TerminationReason != "":
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// errorClass is the rejection reason, the origin error class, or
// handler_panic.
func errorClass(r *Record) string {
	switch {
	case r.Rejected:
		return r.Reason
	case r.ErrorClass != "":
		return r.ErrorClass
	case r.Reason == "handler_panic":
		return r.Reason
	}
	return ""
}

// bound cuts s to at most n bytes at a UTF-8 boundary.
func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// logPath returns the logged path: escapedPath and the query with the values
// of the names parameters redacted, cut to MaxPathBytes. When the query
// cannot be decoded safely it is omitted, and failed is true.
func logPath(escapedPath, rawQuery string, forceQuery bool, names []string) (path string, failed, truncated bool) {
	if escapedPath == "" {
		return "", false, false
	}
	path = escapedPath
	if rawQuery != "" || forceQuery {
		if query, ok := redactQuery(rawQuery, names); ok {
			path = escapedPath + "?" + query
		} else {
			failed = true
		}
	}
	if len(path) > MaxPathBytes {
		return bound(path, MaxPathBytes), failed, true
	}
	return path, failed, false
}

// redactQuery replaces with Redacted the value of every '&'-separated
// component whose name, after one query-style percent and plus decode,
// equals one of names ignoring ASCII case. Everything else is kept byte for
// byte. It reports false when the query or any of its names or values cannot
// be decoded into valid text without controls, also when names is empty.
// Like net/url.ParseQuery and internal/redirect, only '&' separates
// parameters: ';' is ordinary data, so a=1;token=s is one parameter named a.
func redactQuery(raw string, names []string) (string, bool) {
	if !validText(raw) {
		return "", false
	}
	var out strings.Builder
	changed := false
	for start := 0; start <= len(raw); {
		end := strings.IndexByte(raw[start:], '&')
		if end < 0 {
			end = len(raw)
		} else {
			end += start
		}
		component := raw[start:end]
		name, value, hasValue := strings.Cut(component, "=")
		decodedName, err := url.QueryUnescape(name)
		if err != nil || !validText(decodedName) {
			return "", false
		}
		if decodedValue, err := url.QueryUnescape(value); err != nil || !validText(decodedValue) {
			return "", false
		}
		if hasValue && matchesName(decodedName, names) {
			if !changed {
				out.Grow(len(raw) + len(Redacted))
				out.WriteString(raw[:start])
				changed = true
			}
			out.WriteString(name)
			out.WriteString("=" + Redacted)
		} else if changed {
			out.WriteString(component)
		}
		if end == len(raw) {
			break
		}
		if changed {
			out.WriteByte('&')
		}
		start = end + 1
	}
	if !changed {
		return raw, true
	}
	return out.String(), true
}

func matchesName(name string, names []string) bool {
	for _, n := range names {
		if asciiEqualFold(name, n) {
			return true
		}
	}
	return false
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

func validText(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < ' ' || c == 0x7f {
			return false
		} else if c >= utf8.RuneSelf {
			if !utf8.ValidString(s[i:]) {
				return false
			}
			return strings.IndexFunc(s[i:], unicode.IsControl) < 0
		}
	}
	return true
}
