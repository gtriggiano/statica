package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

type checkBody struct {
	closes   int
	closeErr error
}

func (b *checkBody) Read([]byte) (int, error) { panic("check-origin must not read object bytes") }
func (b *checkBody) Close() error             { b.closes++; return b.closeErr }

type checkReader struct {
	calls                     []string
	headErr, getErr, closeErr error
	object                    *storage.Object
	closes                    int
	head                      func(context.Context)
}

func (r *checkReader) Head(ctx context.Context, key string) (storage.Metadata, error) {
	r.calls = append(r.calls, "HEAD "+key)
	if r.head != nil {
		r.head(ctx)
	}
	return storage.Metadata{Size: 100}, r.headErr
}
func (r *checkReader) Get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	if opts != (storage.ReadOptions{}) {
		panic("unexpected origin options")
	}
	r.calls = append(r.calls, "GET "+key)
	return r.object, r.getErr
}
func (r *checkReader) Close() error { r.closes++; return r.closeErr }
func runCheck(t *testing.T, ctx context.Context, contents string, factory OriginFactory) (int, string, string) {
	t.Helper()
	path := configFile(t, t.TempDir(), "origin.yaml", contents)
	var out, errOut bytes.Buffer
	code := Run(ctx, []string{"config", "check-origin", "--config", path}, Options{Out: &out, Err: &errOut, OriginFactory: factory})
	return code, out.String(), errOut.String()
}

func TestCheckOriginKnownKeyAndFactoryDeadline(t *testing.T) {
	for _, tc := range []struct{ name, contents, key string }{
		{"index", minimal + "  prefix: /release/\nindex: home.html\n", "release/home.html"},
		{"explicit health", minimal + "  prefix: /release/\nadmin:\n  origin_health:\n    enabled: false\n    key: health/readiness.json\n", "release/health/readiness.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := &checkReader{}
			factories := 0
			factory := func(ctx context.Context, cfg config.S3) (Origin, error) {
				factories++
				if cfg.Bucket != "example-websites" || cfg.Prefix != "release/" {
					t.Fatalf("configuration = %+v", cfg)
				}
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second {
					t.Fatalf("invalid deadline %v", deadline)
				}
				return origin, nil
			}
			code, out, errOut := runCheck(t, context.Background(), tc.contents, factory)
			if code != 0 || out != "origin object accessible\n" || errOut != "" || factories != 1 || origin.closes != 1 || !reflect.DeepEqual(origin.calls, []string{"HEAD " + tc.key}) {
				t.Fatalf("code%d out%q err%q factories%d closes%d calls%q", code, out, errOut, factories, origin.closes, origin.calls)
			}
		})
	}
}

func TestCheckOriginAmbiguousHeadMetadataOnlyGet(t *testing.T) {
	body := &checkBody{}
	origin := &checkReader{headErr: &storage.Error{Kind: storage.AmbiguousNotFound, Size: -1}, object: &storage.Object{Metadata: storage.Metadata{Size: 1000000}, Body: body}}
	code, out, errOut := runCheck(t, context.Background(), minimal, func(context.Context, config.S3) (Origin, error) { return origin, nil })
	if code != 0 || out != "origin object accessible\n" || errOut != "" || body.closes != 1 || origin.closes != 1 || !reflect.DeepEqual(origin.calls, []string{"HEAD index.html", "GET index.html"}) {
		t.Fatalf("code%d out%q err%q bodycloses%d origincloses%d calls%q", code, out, errOut, body.closes, origin.closes, origin.calls)
	}
}

func TestCheckOriginRawFailuresRemainRuntimeAndSafe(t *testing.T) {
	const secret = "https://secret-endpoint/secret-key?token=secret-credential"
	for _, tc := range []struct {
		name                                                string
		factoryErr, headErr, getErr, closeErr, bodyCloseErr error
		ambiguous, object                                   bool
	}{
		{name: "factory", factoryErr: errors.New(secret)},
		{name: "denied", headErr: &storage.Error{Kind: storage.Denied, Size: -1}},
		{name: "missing", headErr: &storage.Error{Kind: storage.NotFound, Size: -1}},
		{name: "raw head", headErr: errors.New(secret)},
		{name: "untrusted kind", headErr: &storage.Error{Kind: storage.Kind(secret), Size: -1}},
		{name: "get denied", ambiguous: true, getErr: &storage.Error{Kind: storage.Denied, Size: -1}, object: true},
		{name: "get missing", ambiguous: true, getErr: &storage.Error{Kind: storage.NotFound, Size: -1}},
		{name: "get raw", ambiguous: true, getErr: errors.New(secret), object: true},
		{name: "get nil result", ambiguous: true},
		{name: "body close", ambiguous: true, object: true, bodyCloseErr: errors.New(secret)},
		{name: "origin close", closeErr: errors.New(secret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := &checkReader{headErr: tc.headErr, getErr: tc.getErr, closeErr: tc.closeErr}
			if tc.ambiguous {
				origin.headErr = &storage.Error{Kind: storage.AmbiguousNotFound, Size: -1}
			}
			body := &checkBody{closeErr: tc.bodyCloseErr}
			if tc.object {
				origin.object = &storage.Object{Body: body}
			}
			code, out, errOut := runCheck(t, context.Background(), minimal, func(context.Context, config.S3) (Origin, error) { return origin, tc.factoryErr })
			want := "statica: origin check failed\n"
			switch tc.name {
			case "denied", "get denied":
				want = "statica: origin check failed: access denied\n"
			case "missing", "get missing":
				want = "statica: origin check failed: object not found\n"
			case "get nil result":
				want = "statica: origin check failed: invalid origin response\n"
			}
			if code != 1 || out != "" || errOut != want || strings.Contains(errOut, secret) || origin.closes != 1 {
				t.Fatalf("code%d out%q err%q closes%d", code, out, errOut, origin.closes)
			}
			if tc.object && body.closes != 1 {
				t.Fatalf("body closed %d times", body.closes)
			}
			if !tc.ambiguous && len(origin.calls) > 1 {
				t.Fatalf("unexpected fallback: %q", origin.calls)
			}
		})
	}
	code, out, errOut := runCheck(t, context.Background(), minimal, func(context.Context, config.S3) (Origin, error) { return nil, nil })
	if code != 1 || out != "" || errOut != "statica: origin check failed\n" {
		t.Fatalf("nil origin: code%d out%q err%q", code, out, errOut)
	}
}

func TestCheckOriginDeadlineAndCancellation(t *testing.T) {
	for _, stage := range []string{"factory", "head"} {
		t.Run(stage, func(t *testing.T) {
			origin := &checkReader{}
			factory := func(ctx context.Context, _ config.S3) (Origin, error) {
				if stage == "factory" {
					<-ctx.Done()
					return nil, errors.New("private factory failure")
				}
				origin.head = func(ctx context.Context) { <-ctx.Done() }
				return origin, nil
			}
			code, out, errOut := runCheck(t, context.Background(), minimal+"server:\n  response_header_timeout: 2ms\n", factory)
			if code != 1 || out != "" || errOut != "statica: origin check timed out\n" {
				t.Fatalf("code%d out%q err%q", code, out, errOut)
			}
			if stage == "head" && origin.closes != 1 {
				t.Fatalf("origin closed %d times", origin.closes)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, out, errOut := runCheck(t, ctx, minimal, func(context.Context, config.S3) (Origin, error) {
		t.Fatal("canceled command created origin")
		return nil, nil
	})
	if code != 1 || out != "" || errOut == "" {
		t.Fatalf("code%d out%q err%q", code, out, errOut)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	origin := &checkReader{head: func(context.Context) { cancel() }, headErr: &storage.Error{Kind: storage.AmbiguousNotFound, Size: -1}}
	code, out, errOut = runCheck(t, ctx, minimal, func(context.Context, config.S3) (Origin, error) { return origin, nil })
	if code != 1 || out != "" || errOut != "statica: origin check canceled\n" || origin.closes != 1 || len(origin.calls) != 1 {
		t.Fatalf("code%d out%q err%q closes%d", code, out, errOut, origin.closes)
	}
}

func TestCheckOriginWarningAndCheckedWriters(t *testing.T) {
	contents := minimal + "  insecure_skip_verify: true\n"
	origin := &checkReader{}
	factory := func(context.Context, config.S3) (Origin, error) { return origin, nil }
	code, out, errOut := runCheck(t, context.Background(), contents, factory)
	if code != 0 || out != "origin object accessible\n" || errOut != "statica: warning: origin TLS certificate verification is disabled\n" {
		t.Fatalf("code%d out%q err%q", code, out, errOut)
	}
	path := configFile(t, t.TempDir(), "config.yaml", contents)
	code = Run(context.Background(), []string{"config", "check-origin", "--config", path}, Options{Out: io.Discard, Err: failingWriter{}, OriginFactory: func(context.Context, config.S3) (Origin, error) {
		t.Fatal("failed warning must stop before origin creation")
		return nil, nil
	}})
	if code != 1 {
		t.Fatalf("failed warning exit %d", code)
	}
	for _, writer := range []io.Writer{failingWriter{}, shortWriter{}} {
		origin = &checkReader{}
		var errBuffer bytes.Buffer
		code = Run(context.Background(), []string{"config", "check-origin", "--config", path}, Options{Out: writer, Err: &errBuffer, OriginFactory: factory})
		if code != 1 || origin.closes != 1 || !strings.Contains(errBuffer.String(), "cannot write output") {
			t.Fatalf("code%d closes%d err%q", code, origin.closes, errBuffer.String())
		}
	}
}

func TestCheckOriginConfigurationFailureIsOfflineExitTwo(t *testing.T) {
	for _, contents := range []string{"version: 99\n", minimal + "  endpoint: invalid-secret-origin\n"} {
		code, out, errOut := runCheck(t, context.Background(), contents, func(context.Context, config.S3) (Origin, error) {
			t.Fatal("invalid configuration created origin")
			return nil, nil
		})
		if code != 2 || out != "" || errOut == "" {
			t.Fatalf("code%d out%q err%q", code, out, errOut)
		}
	}
}

func TestOfflineCommandsNeverCreateOrigin(t *testing.T) {
	path := configFile(t, t.TempDir(), "config.yaml", minimal)
	for _, args := range [][]string{
		nil, {"--help"}, {"version"}, {"config"}, {"help", "config", "check-origin"},
		{"config", "check-origin", "--help", "--config", "/nonexistent/private.yaml"},
		{"config", "validate", "--config", path}, {"config", "print", "--config", path},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := Run(context.Background(), args, Options{Out: &out, Err: &errOut, OriginFactory: func(context.Context, config.S3) (Origin, error) {
				t.Fatal("offline command created origin")
				return nil, nil
			}})
			if code != 0 || out.Len() == 0 || errOut.Len() != 0 {
				t.Fatalf("code%d out%q err%q", code, out.String(), errOut.String())
			}
		})
	}
}

func TestCheckOriginConfigurationPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	configFile(t, dir, "config.yaml", minimal+"index: default.html\n")
	envPath := configFile(t, dir, "environment.yaml", minimal+"index: environment.html\n")
	flagPath := configFile(t, dir, "flag.yaml", minimal+"index: flag.html\n")
	for _, tc := range []struct {
		args []string
		env  string
		key  string
	}{
		{[]string{"config", "check-origin"}, "", "default.html"},
		{[]string{"config", "check-origin"}, envPath, "environment.html"},
		{[]string{"config", "check-origin", "--config", flagPath}, envPath, "flag.html"},
		{[]string{"config", "--config", flagPath, "check-origin"}, envPath, "flag.html"},
	} {
		origin := &checkReader{}
		var out, errOut bytes.Buffer
		code := Run(context.Background(), tc.args, Options{Out: &out, Err: &errOut, LookupEnv: func(name string) (string, bool) { return tc.env, name == "STATICA_CONFIG" && tc.env != "" }, OriginFactory: func(context.Context, config.S3) (Origin, error) { return origin, nil }})
		if code != 0 || !reflect.DeepEqual(origin.calls, []string{"HEAD " + tc.key}) {
			t.Fatalf("args%q code%d calls%q err%q", tc.args, code, origin.calls, errOut.String())
		}
	}
}
