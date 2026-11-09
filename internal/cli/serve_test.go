package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/gtriggiano/statica/internal/app"
	"github.com/gtriggiano/statica/internal/config"
	origins3 "github.com/gtriggiano/statica/internal/storage/s3"
)

// serveConfig needs no reachable origin: with static credentials and a
// region, constructing the client performs no network access.
const serveConfig = "server:\n  listen: 127.0.0.1:1\n  shutdown_timeout: 2s\nadmin:\n  listen: 127.0.0.1:2\ns3:\n  bucket: serve-bucket\n  region: eu-west-1\n  endpoint: http://127.0.0.1:1\n  allow_http: true\n"

func noServe(t *testing.T) ServeFunc {
	return func(context.Context, *config.Config, app.Options) error {
		t.Fatal("serve started")
		return nil
	}
}

func TestServeHelp(t *testing.T) {
	code, out, errOut := execute([]string{"--help"}, nil)
	if code != 0 || errOut != "" || !strings.Contains(out, "\n  serve ") || !strings.Contains(out, "serve and config check-origin are online") {
		t.Fatalf("root help does not list serve: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, args := range [][]string{{"serve", "--help"}, {"help", "serve"}} {
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), args, Options{Out: &stdout, Err: &stderr, Serve: noServe(t),
			LookupEnv: func(string) (string, bool) { t.Fatal("help inspected the environment"); return "", false }})
		help := stdout.String()
		if code != 0 || stderr.Len() != 0 || !strings.Contains(help, "--config") || !strings.Contains(help, "another signal forces exit") {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, help, stderr.String())
		}
	}
}

func TestServeConfigurationErrorsExitTwo(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	invalid := configFile(t, dir, "invalid.yaml", "version: 99\n")
	secretEndpoint := configFile(t, dir, "endpoint.yaml", minimal+"  endpoint: ${ENDPOINT}\n")
	tls := configFile(t, dir, "tls.yaml", minimal+"server:\n  tls:\n    cert_file: missing-cert.pem\n    key_file: missing-key.pem\n")
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"default missing", []string{"serve"}, nil},
		{"flag missing", []string{"serve", "--config", "missing.yaml"}, nil},
		{"empty environment", []string{"serve"}, map[string]string{"STATICA_CONFIG": ""}},
		{"empty flag", []string{"serve", "--config="}, map[string]string{"STATICA_CONFIG": invalid}},
		{"invalid", []string{"serve"}, map[string]string{"STATICA_CONFIG": invalid}},
		{"invalid secret value", []string{"serve", "--config", secretEndpoint}, map[string]string{"ENDPOINT": "sensitive-expanded-endpoint"}},
		{"unreadable TLS material", []string{"serve", "--config", tls}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, Options{Out: &stdout, Err: &stderr, Serve: noServe(t),
				LookupEnv: func(name string) (string, bool) { value, ok := tc.env[name]; return value, ok }})
			if code != 2 || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "statica: ") || strings.Contains(stderr.String(), "sensitive-expanded-endpoint") {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestServeRuntimeFailureExitsOne(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	configFile(t, dir, "config.yaml", minimal)
	flagPath := configFile(t, dir, "flag.yaml", "s3:\n  bucket: flag-bucket\n")
	signals := make(chan os.Signal, 1)
	var stdout, stderr bytes.Buffer
	var got app.Options
	var bucket string
	code := Run(context.Background(), []string{"serve", "--config", flagPath}, Options{
		Out: &stdout, Err: &stderr, Signals: signals,
		Build: BuildInfo{Version: "v1.2.3", Revision: "abc", GoVersion: "go1.test", BuildTime: "now"},
		Serve: func(ctx context.Context, cfg *config.Config, opts app.Options) error {
			got, bucket = opts, cfg.S3.Bucket
			return errors.New("cannot bind the public listener")
		},
	})
	if code != 1 || stdout.Len() != 0 || stderr.String() != "statica: cannot bind the public listener\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if bucket != "flag-bucket" || got.Logs != io.Writer(&stdout) || got.Signals != (<-chan os.Signal)(signals) ||
		got.Build.Version != "v1.2.3" || got.Build.Revision != "abc" || got.Build.GoVersion != "go1.test" {
		t.Fatalf("serve options: bucket=%q %+v", bucket, got)
	}
	// A cancelled context is a runtime failure before anything starts.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stderr.Reset()
	if code := Run(ctx, []string{"serve"}, Options{Err: &stderr, Serve: noServe(t)}); code != 1 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("cancelled: code=%d stderr=%q", code, stderr.String())
	}
}

// TestSignalRouting checks that serve receives the raw signals while every
// other command keeps being cancelled by the first one.
func TestSignalRouting(t *testing.T) {
	path := configFile(t, t.TempDir(), "config.yaml", minimal)
	signals := make(chan os.Signal, 2)
	code := Run(context.Background(), []string{"serve", "--config", path}, Options{Signals: signals,
		Serve: func(ctx context.Context, _ *config.Config, opts app.Options) error {
			signals <- syscall.SIGTERM
			select {
			case <-opts.Signals:
			case <-time.After(5 * time.Second):
				return errors.New("serve did not receive the signal")
			}
			if ctx.Err() != nil {
				return errors.New("the signal cancelled serve's context")
			}
			return nil
		}})
	if code != 0 {
		t.Fatalf("serve exit %d", code)
	}

	heading := make(chan struct{})
	origin := &checkReader{head: func(ctx context.Context) {
		close(heading)
		<-ctx.Done()
	}}
	go func() {
		<-heading
		signals <- syscall.SIGINT
	}()
	var stderr bytes.Buffer
	code = Run(context.Background(), []string{"config", "check-origin", "--config", path}, Options{Err: &stderr, Signals: signals,
		OriginFactory: func(context.Context, config.S3) (Origin, error) { return origin, nil }})
	if code != 1 || stderr.String() != "statica: origin check canceled\n" {
		t.Fatalf("check-origin code=%d stderr=%q", code, stderr.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ephemeral binds the configuration's placeholder addresses to free ports
// and records them; other addresses bind as configured.
type ephemeral struct {
	mu    sync.Mutex
	bound map[string]string
}

func (e *ephemeral) listen(network, address string) (net.Listener, error) {
	target := address
	if address == "127.0.0.1:1" || address == "127.0.0.1:2" {
		target = "127.0.0.1:0"
	}
	l, err := net.Listen(network, target)
	if err == nil {
		e.mu.Lock()
		e.bound[address] = l.Addr().String()
		e.mu.Unlock()
	}
	return l, err
}

func (e *ephemeral) get(address string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.bound[address]
}

func realServe(e *ephemeral) ServeFunc {
	return func(ctx context.Context, cfg *config.Config, opts app.Options) error {
		opts.Listen = e.listen
		opts.OriginOptions = []origins3.Option{origins3.WithCredentials(credentials.NewStaticCredentialsProvider("serve-key", "serve-secret", ""))}
		return app.Run(ctx, cfg, opts)
	}
}

func TestServeRunsUntilSignal(t *testing.T) {
	path := configFile(t, t.TempDir(), "config.yaml", serveConfig)
	signals := make(chan os.Signal, 2)
	var stdout, stderr syncBuffer
	e := &ephemeral{bound: map[string]string{}}
	exit := make(chan int, 1)
	go func() {
		exit <- Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Signals: signals, Serve: realServe(e)})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stdout.String(), "event=startup ") {
		if time.Now().After(deadline) {
			t.Fatalf("no startup event: %q", stdout.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	signals <- syscall.SIGTERM
	select {
	case code := <-exit:
		out := stdout.String()
		if code != 0 || stderr.String() != "" || !strings.Contains(out, "event=shutdown_complete reason=signal outcome=drained") || strings.Contains(out, "serve-bucket") {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after a signal")
	}
}

func TestServeBindFailureExitsOne(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	path := configFile(t, t.TempDir(), "config.yaml", strings.Replace(serveConfig, "127.0.0.1:2", busy.Addr().String(), 1))
	var stdout, stderr bytes.Buffer
	e := &ephemeral{bound: map[string]string{}}
	code := Run(context.Background(), []string{"serve", "--config", path}, Options{Out: &stdout, Err: &stderr, Serve: realServe(e)})
	if code != 1 || stderr.String() != "statica: cannot bind the admin listener\n" || !strings.Contains(stdout.String(), "event=startup_failed stage=bind_admin") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	public := e.get("127.0.0.1:1")
	again, err := net.Listen("tcp", public)
	if err != nil {
		t.Fatalf("public listener %q still bound: %v", public, err)
	}
	_ = again.Close()
}
