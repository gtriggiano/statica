package app_test

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/app"
	"github.com/gtriggiano/statica/internal/config"
	origins3 "github.com/gtriggiano/statica/internal/storage/s3"
)

const noCache = "cache:\n  enabled: false\n"

func TestStartupServesReportsAndStopsOnContext(t *testing.T) {
	o := newOrigin(t)
	cfg := newConfig(t, o.server.URL, settings{
		server: "  trusted_proxies: [10.0.0.0/8]\n",
		top:    "hosts: [127.0.0.1, canary-host.example]\n",
	})
	i := start(t, cfg)

	// Without origin health, readiness holds as soon as both listeners are
	// bound, which precedes the startup event.
	i.wantProbe("/readyz", http.StatusOK, "ready\n")
	i.wantProbe("/livez", http.StatusOK, "ok\n")
	wantGet(t, i.public, "/hello.txt", http.StatusOK, "hello world")

	access := i.waitEvent("http_request", path("/hello.txt"))
	want(t, access, map[string]string{
		"level": "info", "host": "127.0.0.1", "method": "GET", "status": "200", "outcome": "success",
		"origin_key": "pub/hello.txt", "cache": "miss", "bytes_written": "11", "request_id_source": "generated",
		"client_ip": "127.0.0.1", "peer_ip": "127.0.0.1",
	})
	for _, key := range []string{"ts", "request_id", "duration_ms", "ttfb_ms"} {
		if access[key] == "" {
			t.Errorf("access event lacks %s: %v", key, access)
		}
	}
	if _, err := strconv.ParseFloat(access["duration_ms"], 64); err != nil {
		t.Errorf("duration_ms=%q", access["duration_ms"])
	}

	exposition := i.metrics()
	wantMetrics(t, exposition,
		`statica_http_requests_total{host="127.0.0.1",method="GET",outcome="success",status="200"} 1`,
		`statica_build_info{go_version="go-test",revision="rev-test",version="v-test"} 1`,
		"statica_cache_enabled 1",
		"statica_cache_entries ",
		"statica_cache_limit_bytes 1.34217728e+08",
		`statica_cache_requests_total{host="127.0.0.1",outcome="miss"} 1`,
		`statica_origin_operations_total{operation="get",purpose="cache_fill",result="success"} 1`,
		`statica_origin_bytes_total{purpose="cache_fill"} 11`,
		"statica_ready 1",
		"statica_origin_health_enabled 0",
		"statica_log_events_dropped_total 0",
	)

	startup := i.event("startup", nil)
	want(t, startup, map[string]string{
		"level": "info", "version": "v-test", "revision": "rev-test", "go_version": "go-test",
		"public_listen": i.public, "admin_listen": i.admin, "tls": "off",
		"hosts": "allowlist", "hosts_count": "2", "trusted_proxies": "1",
		"s3_endpoint": "custom", "s3_region": "eu-west-1", "s3_path_style": "true", "s3_allow_http": "true",
		"s3_insecure_skip_verify": "false", "s3_conditional_reads": "true", "s3_range_reads": "true",
		"s3_checksums": "false", "s3_max_attempts": "1", "s3_response_header_timeout": "10s",
		"response_header_timeout": "30s", "write_idle_timeout": "1m0s", "request_timeout": "0s",
		"shutdown_timeout": "30s", "max_connections": "1024", "max_concurrent_requests": "256",
		"cache": "on", "cache_max_bytes": "134217728", "cache_max_entries": "10000",
		"cache_max_object_bytes": "2097152", "cache_fill_timeout": "30s", "cache_ttl": "5m0s",
		"compression": "off", "spa": "off", "origin_health": "off", "access_log": "true", "log_level": "info",
	})
	if _, ok := startup["tls_min_version"]; ok {
		t.Errorf("tls_min_version without TLS: %v", startup)
	}
	if _, ok := i.find("origin_tls_verification_disabled", nil); ok {
		t.Error("TLS warning with verification enabled")
	}

	i.cancel()
	if err := i.wait(waitTimeout); err != nil {
		t.Fatalf("context shutdown: %v", err)
	}
	want(t, i.event("shutdown_started", nil), map[string]string{"level": "info", "reason": "context"})
	want(t, i.event("shutdown_complete", nil), map[string]string{"level": "info", "reason": "context", "outcome": "drained"})

	logs := i.logs.String()
	originHost := strings.TrimPrefix(o.server.URL, "http://")
	for _, secret := range []string{bucket, accessKey, secretKey, sessionToken, originHost, "canary-host.example", "Credential", "private-origin-message"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs reveal %q:\n%s", secret, logs)
		}
	}
}

func TestReadinessWaitsForFirstOriginHealthSuccess(t *testing.T) {
	o := newOrigin(t)
	cfg := newConfig(t, o.server.URL, settings{
		admin: "  origin_health:\n    enabled: true\n    key: health.json\n    interval: 2s\n    timeout: 1s\n    max_staleness: 10s\n",
		// The serving client may retry; the health client never does.
		s3: "  max_attempts: 3\n",
	})
	i := start(t, cfg)
	want(t, i.event("startup", nil), map[string]string{"origin_health": "on", "origin_health_interval": "2s", "origin_health_timeout": "1s", "s3_max_attempts": "3"})

	// The first check starts with the worker and fails; the next is about
	// two seconds later, so the counts below are stable.
	waitFor(t, "the first failed check", func() bool {
		exposition := i.metrics()
		return hasMetric(exposition, `statica_origin_health_checks_total{result="unavailable"} 1`)
	})
	i.wantProbe("/readyz", http.StatusServiceUnavailable, "not ready: origin_unknown\n")
	i.wantProbe("/livez", http.StatusOK, "ok\n")
	if heads := o.headCount("pub/health.json"); heads != 1 {
		t.Fatalf("one failed check made %d origin attempts, want 1", heads)
	}
	exposition := i.metrics()
	wantMetrics(t, exposition, "statica_ready 0", "statica_origin_health_enabled 1",
		`statica_origin_attempts_total{operation="head",purpose="health",result="unavailable"} 1`)

	o.healthy.Store(true)
	waitFor(t, "readiness after the first success", func() bool {
		status, _ := i.probe("/readyz")
		return status == http.StatusOK
	})
	i.wantProbe("/livez", http.StatusOK, "ok\n")
	i.waitEvent("origin_health_changed", func(e event) bool { return e["state"] == "healthy" })
}

func TestInsecureOriginTLSWarns(t *testing.T) {
	cfg := newConfig(t, "https://127.0.0.1:1", settings{s3: "  max_attempts: 1\n  insecure_skip_verify: true\n"})
	cfg.S3.AllowHTTP = false
	i := start(t, cfg)
	want(t, i.event("startup", nil), map[string]string{"s3_insecure_skip_verify": "true"})
	want(t, i.waitEvent("origin_tls_verification_disabled", nil), map[string]string{"level": "warn"})
}

func TestConstructionFailureBindsNothing(t *testing.T) {
	for _, tc := range []struct {
		stage, message string
		breakConfig    func(*config.Config)
	}{
		{"origin_client", "cannot initialize the origin client", func(c *config.Config) { c.S3.Bucket = "invalid bucket" }},
		// TLS material is read when the public server is prepared.
		{"public_server", "cannot initialize the public server", func(c *config.Config) {
			c.Server.TLS = &config.TLS{CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem", MinVersion: "1.2"}
		}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			cfg := newConfig(t, "http://127.0.0.1:1", settings{})
			tc.breakConfig(cfg)
			i := launch(t, cfg)
			if err := i.wait(waitTimeout); err == nil || err.Error() != tc.message {
				t.Fatalf("Run = %v", err)
			}
			want(t, i.event("startup_failed", nil), map[string]string{"level": "error", "stage": tc.stage})
			if len(i.binder.calls) != 0 {
				t.Fatalf("bound %q after a construction failure", i.binder.calls)
			}
			if logs := i.logs.String(); strings.Contains(logs, "invalid bucket") || strings.Contains(logs, "nonexistent") {
				t.Fatalf("logs reveal configuration values:\n%s", logs)
			}
		})
	}
}

func TestBindFailureReleasesTheOtherListener(t *testing.T) {
	for _, failing := range []string{"admin", "public"} {
		t.Run(failing, func(t *testing.T) {
			busy, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer busy.Close()
			cfg := newConfig(t, "http://127.0.0.1:1", settings{})
			if failing == "admin" {
				cfg.Admin.Listen = busy.Addr().String()
			} else {
				cfg.Server.Listen = busy.Addr().String()
			}
			i := launch(t, cfg)
			err = i.wait(waitTimeout)
			if err == nil || err.Error() != "cannot bind the "+failing+" listener" {
				t.Fatalf("Run = %v", err)
			}
			want(t, i.event("startup_failed", nil), map[string]string{"level": "error", "stage": "bind_" + failing})
			for _, name := range []string{"startup", "shutdown_started", "shutdown_complete"} {
				if _, ok := i.find(name, nil); ok {
					t.Errorf("unexpected %s event", name)
				}
			}
			if failing == "public" {
				if l := i.binder.get(adminPlaceholder); l != nil {
					t.Fatal("bound the admin listener after the public bind failed")
				}
				return
			}
			// The public listener was bound, then closed: its port is free.
			address := i.binder.get(publicPlaceholder).Addr().String()
			again, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatalf("public listener still bound: %v", err)
			}
			_ = again.Close()
		})
	}
}

func TestDrainCompletesInFlightRequest(t *testing.T) {
	o := newOrigin(t)
	cfg := newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 20s\n"})
	i := start(t, cfg)

	// The request waits on a shared cache fill, which the drain must not
	// cancel before the request ends.
	type result struct {
		status int
		body   string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		response, err := client.Get("http://" + i.public + "/slow.txt")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		done <- result{response.StatusCode, string(body), err}
	}()
	o.awaitSlow(t)
	i.signals <- syscall.SIGTERM

	waitFor(t, "draining readiness", func() bool {
		status, body := i.probe("/readyz")
		return status == http.StatusServiceUnavailable && body == "not ready: draining\n"
	})
	i.wantProbe("/livez", http.StatusOK, "ok\n")
	waitFor(t, "the public listener to refuse connections", func() bool {
		c, err := net.DialTimeout("tcp", i.public, time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	})
	exposition := i.metrics()
	wantMetrics(t, exposition, "statica_ready 0", "statica_http_requests_in_flight 1")
	if !i.running() {
		t.Fatal("Run returned before the in-flight request completed")
	}

	o.finish()
	select {
	case r := <-done:
		if r.err != nil || r.status != http.StatusOK || r.body != slowText {
			t.Fatalf("drained request: status=%d len=%d err=%v", r.status, len(r.body), r.err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("the drained request did not complete")
	}
	if err := i.wait(waitTimeout); err != nil {
		t.Fatalf("signal shutdown: %v", err)
	}
	want(t, i.event("shutdown_started", nil), map[string]string{"reason": "signal"})
	if e, ok := i.find("listener_failed", nil); ok {
		t.Errorf("an orderly drain logged %v", e)
	}
	want(t, i.event("http_request", path("/slow.txt")), map[string]string{"status": "200", "outcome": "success", "bytes_written": strconv.Itoa(len(slowText))})
	complete := i.event("shutdown_complete", nil)
	want(t, complete, map[string]string{"level": "info", "reason": "signal", "outcome": "drained"})
	if _, ok := complete["cancelled_requests"]; ok {
		t.Errorf("a drained shutdown reported cancelled requests: %v", complete)
	}
}

func TestDrainBudgetExpiryCancelsDownloads(t *testing.T) {
	o := newOrigin(t)
	const budget = time.Second
	cfg := newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 1s\n", top: noCache})
	i := start(t, cfg)
	body := download(t, o, i.public, "/slow.bin", len(slowBody))
	defer body.Close()

	started := time.Now()
	i.signals <- syscall.SIGTERM
	err := i.wait(waitTimeout)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("a forced drain after a signal must succeed: %v", err)
	}
	// The drain lasts until the budget less its 250ms tail.
	if elapsed < budget/2 || elapsed > budget+500*time.Millisecond {
		t.Fatalf("shutdown took %v with a %v budget", elapsed, budget)
	}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("the cancelled download completed")
	}
	// Run flushed the logs before returning.
	want(t, i.event("http_request", path("/slow.bin")), map[string]string{
		"level": "warn", "status": "200", "outcome": "incomplete", "termination_reason": "shutdown", "origin_key": "pub/slow.bin",
	})
	want(t, i.event("shutdown_complete", nil), map[string]string{
		"level": "warn", "reason": "signal", "outcome": "forced", "forced_by": "budget", "cancelled_requests": "1",
	})
}

func TestSecondSignalForcesImmediateClose(t *testing.T) {
	o := newOrigin(t)
	cfg := newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 30s\n", top: noCache})
	i := start(t, cfg)
	body := download(t, o, i.public, "/slow.bin", len(slowBody))
	defer body.Close()

	i.signals <- syscall.SIGTERM
	i.waitEvent("shutdown_started", nil)
	started := time.Now()
	i.signals <- syscall.SIGINT
	if err := i.wait(waitTimeout); err != nil {
		t.Fatalf("forced shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("forced close took %v", elapsed)
	}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("the download survived a forced close")
	}
	want(t, i.event("shutdown_complete", nil), map[string]string{
		"level": "warn", "reason": "signal", "outcome": "forced", "forced_by": "signal", "cancelled_requests": "1",
	})
}

func TestListenerFailureShutsDownWithError(t *testing.T) {
	o := newOrigin(t)
	i := start(t, newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 2s\n"}))
	_ = i.binder.get(publicPlaceholder).Close()
	err := i.wait(waitTimeout)
	if err == nil || err.Error() != "public listener failed" {
		t.Fatalf("Run = %v", err)
	}
	want(t, i.event("listener_failed", nil), map[string]string{"level": "error", "listener": "public"})
	want(t, i.event("shutdown_started", nil), map[string]string{"reason": app.ReasonListenerFailure})
	// The failed public server closed its connections: nothing was drained.
	want(t, i.event("shutdown_complete", nil), map[string]string{"level": "warn", "reason": app.ReasonListenerFailure,
		"outcome": "forced", "forced_by": "listener_failure", "cancelled_requests": "0"})
}

func TestPublicListenerFailureCancelsActiveRequests(t *testing.T) {
	o := newOrigin(t)
	i := start(t, newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 2s\n", top: noCache}))
	body := download(t, o, i.public, "/slow.bin", len(slowBody))
	defer body.Close()
	_ = i.binder.get(publicPlaceholder).Close()
	if err := i.wait(waitTimeout); err == nil || err.Error() != "public listener failed" {
		t.Fatalf("Run = %v", err)
	}
	if _, err := io.ReadAll(body); err == nil {
		t.Fatal("the download survived the listener failure")
	}
	complete := i.event("shutdown_complete", nil)
	want(t, complete, map[string]string{"level": "warn", "reason": app.ReasonListenerFailure, "outcome": "forced", "forced_by": "listener_failure"})
	if _, ok := complete["cancelled_requests"]; !ok {
		t.Fatalf("shutdown_complete lacks cancelled_requests: %v", complete)
	}
}

// failingClose is a listener whose Close reports an error, so the drain of
// the public server ends with an error that is not its deadline.
type failingClose struct{ net.Listener }

func (l failingClose) Close() error {
	_ = l.Listener.Close()
	return errors.New("close failed")
}

func TestDrainErrorIsAForcedShutdown(t *testing.T) {
	o := newOrigin(t)
	i := newInstance(t)
	i.listen = func(network, address string) (net.Listener, error) {
		l, err := i.binder.listen(network, address)
		if err == nil && address == publicPlaceholder {
			l = failingClose{l}
		}
		return l, err
	}
	i.start(newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 2s\n"}))
	i.signals <- syscall.SIGTERM
	if err := i.wait(waitTimeout); err != nil {
		t.Fatalf("signal shutdown: %v", err)
	}
	want(t, i.event("shutdown_complete", nil), map[string]string{"level": "warn", "reason": "signal",
		"outcome": "forced", "forced_by": "error", "cancelled_requests": "0"})
}

func TestRequestCapacityLeavesAdminResponsive(t *testing.T) {
	o := newOrigin(t)
	i := start(t, newConfig(t, o.server.URL, settings{server: "  max_concurrent_requests: 2\n", top: noCache}))
	bodies := []io.ReadCloser{
		download(t, o, i.public, "/slow.bin", len(slowBody)),
		download(t, o, i.public, "/slow.bin", len(slowBody)),
	}
	wantGet(t, i.public, "/hello.txt", http.StatusServiceUnavailable, "")
	i.wantProbe("/livez", http.StatusOK, "ok\n")
	i.wantProbe("/readyz", http.StatusOK, "ready\n")
	exposition := i.metrics()
	wantMetrics(t, exposition, "statica_http_requests_in_flight 2", `statica_http_rejected_requests_total{reason="request_capacity"} 1`)

	o.finish()
	for _, body := range bodies {
		if rest, err := io.ReadAll(body); err != nil || len(rest) != len(slowBody)/2 {
			t.Fatalf("slow download: %d bytes, %v", len(rest), err)
		}
		_ = body.Close()
	}
	wantGet(t, i.public, "/hello.txt", http.StatusOK, "hello world")
}

func TestConnectionCapacityLeavesAdminResponsive(t *testing.T) {
	o := newOrigin(t)
	i := start(t, newConfig(t, o.server.URL, settings{server: "  max_connections: 2\n"}))
	var idle []net.Conn
	for range 2 {
		c, err := net.Dial("tcp", i.public)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		idle = append(idle, c)
	}
	waiting, err := net.Dial("tcp", i.public)
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Close()
	if _, err := io.WriteString(waiting, "GET /hello.txt HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = waiting.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, err := waiting.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatal("a connection beyond max_connections was served")
	}

	i.wantProbe("/livez", http.StatusOK, "ok\n")
	i.wantProbe("/readyz", http.StatusOK, "ready\n")
	if status, _ := i.probe("/metrics"); status != http.StatusOK {
		t.Fatalf("metrics status %d", status)
	}

	// Releasing one slot admits the waiting connection.
	_ = idle[0].Close()
	_ = waiting.SetReadDeadline(time.Now().Add(waitTimeout))
	response, err := io.ReadAll(waiting)
	if err != nil || !strings.HasPrefix(string(response), "HTTP/1.1 200 OK") || !strings.HasSuffix(string(response), "hello world") {
		t.Fatalf("waiting connection: %q %v", response, err)
	}
}

func TestSlowReaderHitsWriteIdleTimeoutAlone(t *testing.T) {
	o := newOrigin(t)
	i := start(t, newConfig(t, o.server.URL, settings{server: "  write_idle_timeout: 300ms\n", top: noCache}))
	stalled, err := net.Dial("tcp", i.public)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	_ = stalled.(*net.TCPConn).SetReadBuffer(4 << 10)
	if _, err := io.WriteString(stalled, "GET /big.bin HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// Other clients are served while that one stops reading.
	for range 3 {
		wantGet(t, i.public, "/hello.txt", http.StatusOK, "hello world")
	}
	access := i.waitEvent("http_request", path("/big.bin"))
	want(t, access, map[string]string{"level": "warn", "status": "200", "outcome": "incomplete", "termination_reason": "client_write_timeout"})
	if written, _ := strconv.Atoi(access["bytes_written"]); written <= 0 || written >= len(bigBody) {
		t.Fatalf("bytes_written=%q", access["bytes_written"])
	}
	wantGet(t, i.public, "/hello.txt", http.StatusOK, "hello world")
	i.wantProbe("/livez", http.StatusOK, "ok\n")
}

func TestOriginFailuresSurfaceWhileLivenessHolds(t *testing.T) {
	o := newOrigin(t)
	i := start(t, newConfig(t, o.server.URL, settings{}))
	wantGet(t, i.public, "/broken.txt", http.StatusBadGateway, "")
	want(t, i.waitEvent("http_request", path("/broken.txt")), map[string]string{
		"level": "error", "status": "502", "outcome": "origin_failure", "error_class": "origin_unavailable",
	})
	i.wantProbe("/livez", http.StatusOK, "ok\n")
	i.wantProbe("/readyz", http.StatusOK, "ready\n")
	exposition := i.metrics()
	wantMetrics(t, exposition,
		`statica_http_requests_total{host="127.0.0.1",method="GET",outcome="origin_failure",status="502"} 1`,
		`statica_origin_operations_total{operation="get",purpose="cache_fill",result="unavailable"} 1`,
		`statica_origin_attempts_total{operation="get",purpose="cache_fill",result="unavailable"} 1`,
	)
	if logs := i.logs.String(); strings.Contains(logs, "private-origin-message") || strings.Contains(logs, "InternalError") {
		t.Fatalf("logs reveal the provider error:\n%s", logs)
	}
}

func TestSignalDuringStartupAbortsCleanly(t *testing.T) {
	i := newInstance(t)
	i.signals <- syscall.SIGTERM
	i.run(newConfig(t, "http://127.0.0.1:1", settings{}))
	if err := i.wait(waitTimeout); err != nil {
		t.Fatalf("a signal-aborted startup must succeed: %v", err)
	}
	want(t, i.event("startup_aborted", nil), map[string]string{"level": "info", "reason": "signal"})
	for _, name := range []string{"startup", "startup_failed", "shutdown_started", "shutdown_complete"} {
		if _, ok := i.find(name, nil); ok {
			t.Errorf("unexpected %s event", name)
		}
	}
	// Nothing stays bound, even if the signal was only seen after binding.
	for _, address := range []string{publicPlaceholder, adminPlaceholder} {
		if l := i.binder.get(address); l != nil {
			again, err := net.Listen("tcp", l.Addr().String())
			if err != nil {
				t.Fatalf("%s still bound: %v", address, err)
			}
			_ = again.Close()
		}
	}
}

func TestContextCancelledBeforeBindingFailsStartup(t *testing.T) {
	i := newInstance(t)
	i.cancel()
	i.run(newConfig(t, "http://127.0.0.1:1", settings{}))
	if err := i.wait(waitTimeout); err == nil || err.Error() != "startup canceled" {
		t.Fatalf("Run = %v", err)
	}
	want(t, i.event("startup_aborted", nil), map[string]string{"level": "warn", "reason": "context"})
	if len(i.binder.calls) != 0 {
		t.Fatalf("bound %q", i.binder.calls)
	}
}

func TestStuckAdminClientLeavesTimeToFlushLogs(t *testing.T) {
	o := newOrigin(t)
	const budget = time.Second // tail 250ms: the admin server gets until 937.5ms
	i := start(t, newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 1s\n"}))
	// A request still arriving keeps the admin server from draining.
	stuck, err := net.Dial("tcp", i.admin)
	if err != nil {
		t.Fatal(err)
	}
	defer stuck.Close()
	if _, err := io.WriteString(stuck, "GET /metrics HTTP/1.1\r\nHost: 127.0.0.1\r\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the admin server reads the partial request

	started := time.Now()
	i.signals <- syscall.SIGTERM
	err = i.wait(waitTimeout)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("signal shutdown: %v", err)
	}
	if elapsed < budget/2 || elapsed > budget+500*time.Millisecond {
		t.Fatalf("shutdown took %v with a %v budget and a stuck admin client", elapsed, budget)
	}
	// Run flushed the final event before returning.
	want(t, i.event("shutdown_complete", nil), map[string]string{"reason": "signal", "outcome": "drained"})
}

// stallingClient sends origin requests through next, except health checks,
// which hang ignoring cancellation until released.
type stallingClient struct {
	next    *http.Client
	stalled chan struct{}
	release chan struct{}
}

func (c *stallingClient) Do(r *http.Request) (*http.Response, error) {
	if strings.HasSuffix(r.URL.Path, "/pub/health.json") {
		select {
		case c.stalled <- struct{}{}:
		default:
		}
		<-c.release
		return nil, errors.New("released")
	}
	return c.next.Do(r)
}

func TestStuckHealthCheckDoesNotDelayPublicDrain(t *testing.T) {
	o := newOrigin(t)
	stalling := &stallingClient{next: &http.Client{Transport: &http.Transport{}}, stalled: make(chan struct{}, 1), release: make(chan struct{})}
	defer close(stalling.release)
	i := newInstance(t)
	i.origin = []origins3.Option{origins3.WithHTTPClient(stalling)}
	i.start(newConfig(t, o.server.URL, settings{
		server: "  shutdown_timeout: 2s\n",
		admin:  "  origin_health:\n    enabled: true\n    key: health.json\n    interval: 2s\n    timeout: 1s\n    max_staleness: 10s\n",
	}))
	select {
	case <-stalling.stalled:
	case <-time.After(waitTimeout):
		t.Fatal("no health check")
	}
	started := time.Now()
	i.signals <- syscall.SIGTERM
	waitFor(t, "the public listener to refuse connections", func() bool {
		c, err := net.DialTimeout("tcp", i.public, time.Second)
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	})
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("public intake stopped %v after the signal, behind the health worker", elapsed)
	}
	if err := i.wait(waitTimeout); err != nil {
		t.Fatalf("signal shutdown: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("the worker stop was not bounded by the budget: %v", elapsed)
	}
	want(t, i.event("shutdown_complete", nil), map[string]string{"outcome": "drained"})
}

func TestListenerFailureDuringDrainIsLogged(t *testing.T) {
	o := newOrigin(t)
	i := start(t, newConfig(t, o.server.URL, settings{server: "  shutdown_timeout: 20s\n", top: noCache}))
	body := download(t, o, i.public, "/slow.bin", len(slowBody))
	defer body.Close()
	i.signals <- syscall.SIGTERM
	i.waitEvent("shutdown_started", nil)
	_ = i.binder.get(adminPlaceholder).Close()
	want(t, i.waitEvent("listener_failed", nil), map[string]string{"level": "error", "listener": "admin"})
	o.finish()
	if rest, err := io.ReadAll(body); err != nil || len(rest) != len(slowBody)/2 {
		t.Fatalf("drained download: %d bytes, %v", len(rest), err)
	}
	// The exit status follows the cause of the shutdown, a signal.
	if err := i.wait(waitTimeout); err != nil {
		t.Fatalf("signal shutdown: %v", err)
	}
	var order []string
	for _, e := range parseEvents(t, i.logs.String()) {
		if e["event"] == "listener_failed" || e["event"] == "shutdown_complete" {
			order = append(order, e["event"]+":"+e["listener"])
		}
	}
	if strings.Join(order, ",") != "listener_failed:admin,shutdown_complete:" {
		t.Fatalf("events %q", order)
	}
}

func TestBindFailureOutranksPendingSignal(t *testing.T) {
	i := newInstance(t)
	i.listen = func(string, string) (net.Listener, error) {
		i.signals <- syscall.SIGTERM // races the failure
		return nil, errors.New("address in use")
	}
	i.run(newConfig(t, "http://127.0.0.1:1", settings{}))
	if err := i.wait(waitTimeout); err == nil || err.Error() != "cannot bind the public listener" {
		t.Fatalf("Run = %v", err)
	}
	want(t, i.event("startup_failed", nil), map[string]string{"level": "error", "stage": "bind_public", "signal_pending": "true"})
	if e, ok := i.find("startup_aborted", nil); ok {
		t.Fatalf("a failure was reported as %v", e)
	}
}
