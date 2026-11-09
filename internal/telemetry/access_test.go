package telemetry

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gtriggiano/statica/internal/config"
)

func observability(t *testing.T) config.Observability {
	t.Helper()
	cfg, err := config.Parse([]byte("s3:\n  bucket: test\n  region: eu-west-1\n"), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Observability
}

func TestRedactQuery(t *testing.T) {
	defaults := observability(t).RedactQueryParams
	for _, tc := range []struct {
		name  string
		names []string
		raw   string
		want  string
		ok    bool
	}{
		{"default list", defaults, "a=1&token=abc&b=2", "a=1&token=[REDACTED]&b=2", true},
		{"every default", defaults, "access_token=1&api_key=2&password=3&secret=4&signature=5&X-Amz-Credential=6&X-Amz-Signature=7&X-Amz-Security-Token=8",
			"access_token=[REDACTED]&api_key=[REDACTED]&password=[REDACTED]&secret=[REDACTED]&signature=[REDACTED]&X-Amz-Credential=[REDACTED]&X-Amz-Signature=[REDACTED]&X-Amz-Security-Token=[REDACTED]", true},
		{"repeated names", defaults, "token=a&x=token&token=b", "token=[REDACTED]&x=token&token=[REDACTED]", true},
		{"encoded name", defaults, "%74oken=abc&%54OKEN=def", "%74oken=[REDACTED]&%54OKEN=[REDACTED]", true},
		{"mixed case", defaults, "ToKeN=1&x-amz-signature=2", "ToKeN=[REDACTED]&x-amz-signature=[REDACTED]", true},
		{"plus and space in names", []string{"my token"}, "my+token=v&my%20token=w&mytoken=z", "my+token=[REDACTED]&my%20token=[REDACTED]&mytoken=z", true},
		{"ASCII folding only", []string{"key"}, "%E2%84%AAey=1&KEY=2", "%E2%84%AAey=1&KEY=[REDACTED]", true},
		{"empty value", defaults, "token=&a=", "token=[REDACTED]&a=", true},
		{"name without equals", defaults, "token&a=1", "token&a=1", true},
		{"value with equals", defaults, "token=a=b&c=d", "token=[REDACTED]&c=d", true},
		{"unrelated bytes kept", defaults, "q=a%20b+c&p=%2Fx%2f&e=%E2%98%83&raw=☃&semi=a;b&token=s&z=%41",
			"q=a%20b+c&p=%2Fx%2f&e=%E2%98%83&raw=☃&semi=a;b&token=[REDACTED]&z=%41", true},
		{"semicolon is data", defaults, "a=1;token=s&b=2", "a=1;token=s&b=2", true},
		{"empty components", defaults, "&&token=x&", "&&token=[REDACTED]&", true},
		{"empty query", defaults, "", "", true},
		{"custom list replaces defaults", []string{"session"}, "token=keep&SESSION=s", "token=keep&SESSION=[REDACTED]", true},
		{"disabled", []string{}, "token=keep&password=keep", "token=keep&password=keep", true},
		{"malformed value escape", defaults, "a=%zz&token=x", "", false},
		{"malformed name escape", defaults, "%g0=1", "", false},
		{"truncated escape", defaults, "token=%2", "", false},
		{"malformed while disabled", []string{}, "a=%zz", "", false},
		{"decoded control", defaults, "a=%0a", "", false},
		{"decoded invalid UTF-8", defaults, "a=%ff", "", false},
		{"raw control", defaults, "a=\x01", "", false},
		{"raw invalid UTF-8", defaults, "a=\xff", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := redactQuery(tc.raw, tc.names)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("redactQuery(%q) = %q, %t; want %q, %t", tc.raw, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestLogPathRedactsBeforeBounding(t *testing.T) {
	defaults := observability(t).RedactQueryParams
	for _, tc := range []struct {
		name, path, query string
		force             bool
		want              string
		failed, truncated bool
	}{
		{"no query", "/a%20b/", "", false, "/a%20b/", false, false},
		{"bare question mark", "/a", "", true, "/a?", false, false},
		{"redacted", "/guide/", "lang=en&token=abc", false, "/guide/?lang=en&token=[REDACTED]", false, false},
		{"unparseable target", "", "token=abc", false, "", false, false},
		{"failed redaction omits the query", "/a", "token=SECRET&x=%zz", false, "/a", true, false},
		{"exact bound", "/" + strings.Repeat("a", MaxPathBytes-1), "", false, "/" + strings.Repeat("a", MaxPathBytes-1), false, false},
		{"one byte over", "/" + strings.Repeat("a", MaxPathBytes), "", false, "/" + strings.Repeat("a", MaxPathBytes-1), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, failed, truncated := logPath(tc.path, tc.query, tc.force, defaults)
			if got != tc.want || failed != tc.failed || truncated != tc.truncated {
				t.Fatalf("logPath = %q, %t, %t", got[:min(len(got), 80)], failed, truncated)
			}
		})
	}
	// Redaction grows a target that fits the bound past it; the cut happens
	// afterwards, so no secret byte survives.
	query := strings.Repeat("token=s&", (MaxPathBytes-16)/8) + "token=s"
	if len("/?"+query) > MaxPathBytes {
		t.Fatal("the raw target must fit the bound")
	}
	got, failed, truncated := logPath("/", query, false, defaults)
	if !truncated || failed || len(got) != MaxPathBytes || strings.Contains(got, "=s") || !strings.HasPrefix(got, "/?token=[REDACTED]&token=[REDACTED]") {
		t.Fatalf("logPath = %q..., %t, %t (%d bytes)", got[:64], failed, truncated, len(got))
	}
	// The cut never splits a UTF-8 sequence.
	got, _, truncated = logPath("/"+strings.Repeat("a", MaxPathBytes-2)+"☃", "", false, nil)
	if !truncated || !utf8.ValidString(got) || len(got) != MaxPathBytes-1 {
		t.Fatalf("cut %d bytes, valid %t", len(got), utf8.ValidString(got))
	}
}

func baseRecord() *Record {
	start := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	return &Record{Method: "GET", Host: "docs.example.com", EscapedPath: "/guide/", RawQuery: "lang=en&token=abc",
		RequestID: "ingress-123", RequestIDSource: "proxy", ClientIP: netip.MustParseAddr("198.51.100.8"),
		PeerIP: netip.MustParseAddr("10.1.1.1"), ClientIPSource: "x_forwarded_for", Started: start,
		Admitted: start, FirstByte: start.Add(1500 * time.Microsecond), Finished: start.Add(2153 * time.Microsecond),
		Status: 200, BytesWritten: 2048, OriginKey: "docs/guide/index.html", Resolution: "index",
		Outcome: OutcomeSuccess, CacheOutcome: "hit", CacheBytes: 2048}
}

func accessEvents(t *testing.T, cfg config.Observability, level slog.Leveler, records ...*Record) []event {
	t.Helper()
	var out lockedBuffer
	a := NewAccessLogger(NewHandler(&out, &HandlerOptions{Level: level}), cfg)
	for _, r := range records {
		a.Sink(r)
	}
	return decodeEvents(t, out.String())
}

func TestAccessEventFieldsAndOrder(t *testing.T) {
	cfg := observability(t)
	e := accessEvents(t, cfg, nil, baseRecord())[0]
	want := "ts level event request_id request_id_source host method path status outcome resolution cache origin_key duration_ms ttfb_ms bytes_written client_ip peer_ip client_ip_source"
	if strings.Join(e.keys, " ") != want {
		t.Fatalf("keys %v\nwant %s", e.keys, want)
	}
	for k, v := range map[string]string{"ts": "2026-11-06T09:00:00.002Z", "level": "info", "event": "http_request", "request_id": "ingress-123",
		"request_id_source": "proxy", "host": "docs.example.com", "method": "GET", "path": "/guide/?lang=en&token=[REDACTED]",
		"status": "200", "outcome": "success", "resolution": "index", "cache": "hit", "origin_key": "docs/guide/index.html",
		"duration_ms": "2.153", "ttfb_ms": "1.5", "bytes_written": "2048", "client_ip": "198.51.100.8", "peer_ip": "10.1.1.1",
		"client_ip_source": "x_forwarded_for"} {
		if e.values[k] != v {
			t.Errorf("%s = %q, want %q", k, e.values[k], v)
		}
	}

	// Every optional field, in order.
	r := baseRecord()
	r.RequestIDSource, r.RequestIDReason = "generated", "invalid_request_id"
	r.EscapedPath, r.RawQuery = "/"+strings.Repeat("p", MaxPathBytes), "token=SECRET&bad=%zz"
	r.CacheOutcome, r.CacheReason, r.CacheCoalesced = "bypass", "waiters_full", true
	r.Outcome, r.TerminationReason, r.ErrorClass = OutcomeIncomplete, "origin_read_failure", "origin_unavailable"
	r.CacheDiagnostic, r.AttributionReason, r.ClientIPSource = "malformed_origin_cache_metadata", "invalid_forwarded_chain", "peer"
	r.ClientIP = r.PeerIP
	e = accessEvents(t, cfg, nil, r)[0]
	want = "ts level event request_id request_id_source request_id_reason host method path query_redaction_failed path_truncated status outcome resolution cache cache_reason cache_coalesced origin_key duration_ms ttfb_ms bytes_written error_class termination_reason cache_diagnostic client_ip peer_ip client_ip_source client_ip_reason"
	if strings.Join(e.keys, " ") != want || e.values["level"] != "error" || e.values["cache_coalesced"] != "true" || e.values["path_truncated"] != "true" ||
		len(e.values["path"]) != MaxPathBytes || strings.Contains(e.values["path"], "SECRET") {
		t.Fatalf("keys %v\nwant %s\nvalues %v", e.keys, want, e.values)
	}

	// A rejected request without a parseable target or host.
	r = &Record{Method: strings.Repeat("M", 100), Authority: strings.Repeat("h", 1000), RequestID: "id", RequestIDSource: "generated",
		Started: baseRecord().Started, Finished: baseRecord().Finished, FirstByte: baseRecord().FirstByte, Status: 400, BytesWritten: 12,
		Rejected: true, Reason: "malformed_host", TerminationReason: "", PeerIP: netip.MustParseAddr("192.0.2.1"), ClientIPSource: "peer"}
	r.ClientIP = r.PeerIP
	e = accessEvents(t, cfg, nil, r)[0]
	want = "ts level event request_id request_id_source authority method status duration_ms ttfb_ms bytes_written error_class client_ip peer_ip client_ip_source"
	if strings.Join(e.keys, " ") != want || e.values["level"] != "warn" || e.values["error_class"] != "malformed_host" ||
		len(e.values["authority"]) != maxAuthorityBytes || len(e.values["method"]) != maxMethodBytes {
		t.Fatalf("keys %v\nwant %s\nvalues %v", e.keys, want, e.values)
	}

	// Without a committed status there is no ttfb_ms; a failed redaction
	// omits the query and says so.
	r = baseRecord()
	r.FirstByte, r.Status, r.RawQuery = time.Time{}, 0, "token=SECRET&bad=%zz"
	r.Outcome, r.TerminationReason, r.Reason = OutcomeIncomplete, "", "handler_panic"
	e = accessEvents(t, cfg, nil, r)[0]
	if _, ok := e.values["ttfb_ms"]; ok || e.values["path"] != "/guide/" || e.values["query_redaction_failed"] != "true" ||
		e.values["status"] != "0" || e.values["error_class"] != "handler_panic" || e.values["level"] != "error" {
		t.Fatalf("values %v", e.values)
	}
}

func TestAccessLevels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Record)
		level  string
	}{
		{"success", func(*Record) {}, "info"},
		{"expected missing object", func(r *Record) { r.Status, r.OriginKey, r.Resolution = 404, "", "not_found" }, "info"},
		{"redirect", func(r *Record) { r.Status, r.OriginKey, r.Reason = 308, "", "redirect" }, "info"},
		{"masked denial", func(r *Record) {
			r.Status, r.OriginKey, r.Outcome, r.TerminationReason, r.ErrorClass = 404, "", OutcomeOriginFailure, "origin_failure", "origin_denied"
		}, "error"},
		{"origin preparation timeout", func(r *Record) {
			r.Status, r.Outcome, r.TerminationReason = 504, OutcomeOriginFailure, "preparation_timeout"
		}, "error"},
		{"origin read failure", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "origin_read_failure" }, "error"},
		{"origin read timeout", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "origin_read_timeout" }, "error"},
		{"checksum mismatch", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "checksum_mismatch" }, "error"},
		{"client disconnect", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "disconnect" }, "warn"},
		{"client write timeout", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "client_write_timeout" }, "warn"},
		{"client write failure", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "client_write_failure" }, "warn"},
		{"disconnect before origin headers", func(r *Record) {
			r.Outcome, r.TerminationReason, r.ErrorClass = OutcomeOriginFailure, "disconnect", "origin_canceled"
		}, "warn"},
		{"shutdown", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "shutdown" }, "warn"},
		{"total deadline mid-transfer", func(r *Record) { r.Outcome, r.TerminationReason = OutcomeIncomplete, "total_deadline" }, "warn"},
		{"boundary preparation timeout", func(r *Record) { r.Status, r.TerminationReason = 504, "preparation_timeout" }, "warn"},
		{"malformed request", func(r *Record) { r.Status, r.Outcome, r.Rejected, r.Reason = 400, "", true, "invalid_target" }, "warn"},
		{"overload", func(r *Record) { r.Status, r.Outcome, r.Rejected, r.Reason = 503, "", true, "request_capacity" }, "warn"},
		{"rejection whose response failed", func(r *Record) {
			r.Status, r.Outcome, r.Rejected, r.Reason, r.TerminationReason = 400, "", true, "malformed_host", "client_write_failure"
		}, "warn"},
		{"panic", func(r *Record) { r.Outcome, r.Reason = OutcomeIncomplete, "handler_panic" }, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := baseRecord()
			tc.mutate(r)
			if e := accessEvents(t, observability(t), slog.LevelDebug, r)[0]; e.values["level"] != tc.level {
				t.Fatalf("level %s, want %s (%v)", e.values["level"], tc.level, e.values)
			}
		})
	}
}

func TestAccessLogSwitchesAndLevelFilter(t *testing.T) {
	cfg := observability(t)
	cfg.LogClientIP = false
	e := accessEvents(t, cfg, nil, baseRecord())[0]
	for _, k := range []string{"client_ip", "peer_ip", "client_ip_source", "client_ip_reason"} {
		if _, ok := e.values[k]; ok {
			t.Fatalf("log_client_ip: false logged %s", k)
		}
	}
	cfg.AccessLog = false
	if events := accessEvents(t, cfg, nil, baseRecord()); len(events) != 0 {
		t.Fatalf("access_log: false logged %v", events)
	}
	rejected := baseRecord()
	rejected.Rejected, rejected.Reason, rejected.Status = true, "unknown_host", 421
	events := accessEvents(t, observability(t), slog.LevelWarn, baseRecord(), rejected)
	if len(events) != 1 || events[0].values["error_class"] != "unknown_host" {
		t.Fatalf("level warn kept %v", events)
	}
	NewAccessLogger(nil, observability(t)).Sink(baseRecord())
}

func TestNewLogger(t *testing.T) {
	cfg := observability(t)
	var out lockedBuffer
	logger, output, err := NewLogger(&out, cfg)
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("hidden")
	logger.Info("startup", "cache_enabled", true)
	NewAccessLogger(logger.Handler(), cfg).Sink(baseRecord())
	if err := output.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := decodeEvents(t, out.String())
	if len(events) != 2 || events[0].values["event"] != "startup" || events[1].values["event"] != "http_request" {
		t.Fatalf("events %v", events)
	}
	cfg.LogLevel = "verbose"
	if _, _, err := NewLogger(&out, cfg); err == nil {
		t.Fatal("invalid level accepted")
	}
}
