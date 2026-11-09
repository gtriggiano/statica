package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func execute(args []string, env map[string]string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, Options{
		Out: &stdout, Err: &stderr,
		LookupEnv: func(name string) (string, bool) { value, ok := env[name]; return value, ok },
		Build:     BuildInfo{Version: "v0.1.0", Revision: "abc123", GoVersion: "go1.test", BuildTime: "2026-11-06T12:00:00Z"},
	})
	return code, stdout.String(), stderr.String()
}

func configFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = "s3:\n  bucket: example-websites\n"

func TestHelpAndVersionDoNotInspectConfigurationOrEnvironment(t *testing.T) {
	for _, args := range [][]string{
		nil, {"--help"}, {"help"}, {"config"}, {"help", "config", "validate"},
		{"config", "validate", "--help", "--config", "/nonexistent/config.yaml"},
		{"config", "--config", "/nonexistent/config.yaml", "print", "--help"},
		{"version"}, {"version", "--help"},
		{"serve", "--help", "--config", "/nonexistent/config.yaml"}, {"help", "serve"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), args, Options{
				Out: &stdout, Err: &stderr,
				LookupEnv: func(string) (string, bool) { t.Fatal("unexpected environment discovery"); return "", false },
			})
			if code != 0 || stdout.Len() == 0 || stderr.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestVersionBuildInformation(t *testing.T) {
	code, out, errOut := execute([]string{"version"}, nil)
	want := "statica v0.1.0 (revision abc123; go1.test; built 2026-11-06T12:00:00Z)\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	var stdout bytes.Buffer
	if code := Run(context.Background(), []string{"version"}, Options{Out: &stdout}); code != 0 {
		t.Fatal(code)
	}
	if !strings.HasPrefix(stdout.String(), "statica dev (revision unknown; go") || strings.Contains(stdout.String(), "built") {
		t.Fatal(stdout.String())
	}
}

func TestConfigurationFilePrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	configFile(t, dir, "config.yaml", "s3:\n  bucket: default-bucket\n")
	envPath := configFile(t, dir, "environment.yaml", "s3:\n  bucket: environment-bucket\n")
	flagPath := configFile(t, dir, "flag.yaml", "s3:\n  bucket: flag-bucket\n")
	for _, test := range []struct {
		name   string
		args   []string
		env    map[string]string
		bucket string
	}{
		{"default", []string{"config", "print"}, nil, "default-bucket"},
		{"environment", []string{"config", "print"}, map[string]string{"STATICA_CONFIG": envPath}, "environment-bucket"},
		{"flag after subcommand", []string{"config", "print", "--config", flagPath}, map[string]string{"STATICA_CONFIG": envPath}, "flag-bucket"},
		{"flag before subcommand", []string{"config", "--config", flagPath, "print"}, map[string]string{"STATICA_CONFIG": envPath}, "flag-bucket"},
		{"flag overrides empty environment", []string{"config", "validate", "--config", flagPath}, map[string]string{"STATICA_CONFIG": ""}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, out, errOut := execute(test.args, test.env)
			if code != 0 || errOut != "" || !strings.Contains(out, test.bucket) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

func TestInvalidFileSelectionDoesNotFallBack(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	valid := configFile(t, dir, "config.yaml", minimal)
	for _, test := range []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"empty flag", []string{"config", "validate", "--config="}, map[string]string{"STATICA_CONFIG": valid}},
		{"empty flag before subcommand", []string{"config", "--config", "", "validate"}, nil},
		{"empty environment", []string{"config", "validate"}, map[string]string{"STATICA_CONFIG": ""}},
		{"missing flag", []string{"config", "validate", "--config", "missing.yaml"}, map[string]string{"STATICA_CONFIG": valid}},
		{"missing environment", []string{"config", "print"}, map[string]string{"STATICA_CONFIG": "missing.yaml"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, out, errOut := execute(test.args, test.env)
			if code != 2 || out != "" || !strings.HasPrefix(errOut, "statica: ") || strings.Count(errOut, "statica:") != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
}

func TestUsageErrorsDoNotEchoArgumentsOrReadConfiguration(t *testing.T) {
	const secret = "secret-accidentally-pasted-value"
	for _, args := range [][]string{
		{secret}, {"help", secret}, {"help", "config", secret}, {"version", secret}, {"config", "validate", secret},
		{"--" + secret}, {"version", "--" + secret},
		{"config", "print", "--" + secret}, {"config", "validate", "--config"},
		{"-version"}, {"serve", secret}, {"serve", "--" + secret}, {"serve", "--config"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), args, Options{
				Out: &stdout, Err: &stderr,
				LookupEnv: func(string) (string, bool) { t.Fatal("usage error must not inspect configuration"); return "", false },
			})
			if code != 2 || stdout.Len() != 0 || stderr.Len() == 0 || strings.Contains(stderr.String(), secret) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestValidateReportsDeferredRegionWithoutExpandedValues(t *testing.T) {
	path := configFile(t, t.TempDir(), "config.yaml", "s3:\n  bucket: ${BUCKET}\n")
	code, out, errOut := execute([]string{"config", "validate", "--config", path}, map[string]string{"BUCKET": "secret-bucket"})
	if code != 0 || errOut != "" || !strings.Contains(out, "configuration valid") || !strings.Contains(out, "region deferred") || strings.Contains(out, "secret-bucket") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	path = configFile(t, t.TempDir(), "config.yaml", minimal+"  region: eu-west-1\n")
	code, out, errOut = execute([]string{"config", "validate", "--config", path}, nil)
	if code != 0 || errOut != "" || out != "configuration valid\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

func TestPrintRedactsExpandedValuesAndDoesNotReadTLSFiles(t *testing.T) {
	path := configFile(t, t.TempDir(), "config.yaml", "s3:\n  bucket: ${BUCKET}\n  prefix: /docs/\nserver:\n  tls:\n    cert_file: ${CERT}\n    key_file: ${KEY}\n")
	env := map[string]string{"BUCKET": "secret-bucket", "CERT": "/missing/secret-cert.pem", "KEY": "/missing/secret-key.pem"}
	code, out, errOut := execute([]string{"config", "print", "--config", path}, env)
	if code != 0 || errOut != "" || !strings.Contains(out, "REDACTED") || !strings.Contains(out, "docs/") || !strings.Contains(out, "127.0.0.1:8080") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, value := range env {
		if strings.Contains(out, value) {
			t.Fatalf("print leaked %q", value)
		}
	}
	code, out, errOut = execute([]string{"config", "validate", "--config", path}, env)
	if code != 2 || out != "" || errOut == "" {
		t.Fatalf("TLS validation code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, value := range env {
		if strings.Contains(errOut, value) {
			t.Fatalf("TLS validation leaked %q", value)
		}
	}
}

func TestConfigErrorsDoNotRevealExpandedValues(t *testing.T) {
	const secret = "sensitive-expanded-endpoint"
	path := configFile(t, t.TempDir(), "config.yaml", minimal+"  endpoint: ${ENDPOINT}\n")
	for _, subcommand := range []string{"validate", "print"} {
		code, out, errOut := execute([]string{"config", subcommand, "--config", path}, map[string]string{"ENDPOINT": secret})
		if code != 2 || out != "" || errOut == "" || strings.Contains(errOut, secret) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
		}
	}
}

func TestValidateReportsSafeRedirectWarnings(t *testing.T) {
	path := configFile(t, t.TempDir(), "config.yaml", minimal+`redirects:
  - from: '/${ROUTE}/{slug}'
    to: /target/{slug}
    status: 308
  - from: '/${ROUTE}/fixed'
    to: /target/fixed
    status: 308
`)
	const secret = "sensitive-expanded-route"
	env := map[string]string{"ROUTE": secret}
	code, out, errOut := execute([]string{"config", "validate", "--config", path}, env)
	wantWarning := "statica: warning: redirects[1] may be shadowed by redirects[0]\n"
	if code != 0 || !strings.Contains(out, "configuration valid") || errOut != wantWarning || strings.Contains(out+errOut, secret) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut = execute([]string{"config", "print", "--config", path}, env)
	if code != 0 || errOut != "" || !strings.Contains(out, "REDACTED") || strings.Contains(out, secret) {
		t.Fatalf("print code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	for _, output := range []interface{ Write([]byte) (int, error) }{failingWriter{}, shortWriter{}} {
		var stdout bytes.Buffer
		code := Run(context.Background(), []string{"config", "validate", "--config", path}, Options{
			Out: &stdout, Err: output,
			LookupEnv: func(name string) (string, bool) { value, ok := env[name]; return value, ok },
		})
		if code != 1 || stdout.Len() != 0 {
			t.Fatalf("failed warning write code=%d stdout=%q", code, stdout.String())
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 0, nil }

func TestRuntimeFailuresReturnOne(t *testing.T) {
	var stderr bytes.Buffer
	for _, args := range [][]string{{"version"}, {"--help"}, nil} {
		for _, output := range []interface{ Write([]byte) (int, error) }{failingWriter{}, shortWriter{}} {
			stderr.Reset()
			if code := Run(context.Background(), args, Options{Out: output, Err: &stderr}); code != 1 || !strings.Contains(stderr.String(), "cannot write output") {
				t.Fatalf("args=%v code=%d stderr=%q", args, code, stderr.String())
			}
		}
	}
	for _, output := range []interface{ Write([]byte) (int, error) }{failingWriter{}, shortWriter{}} {
		if code := Run(context.Background(), []string{"unknown-command"}, Options{Err: output}); code != 1 {
			t.Fatalf("failed diagnostic write returned %d, want 1", code)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	stderr.Reset()
	if code := Run(ctx, []string{"version"}, Options{Out: &stdout, Err: &stderr}); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCommandsDoNotShareMutableFlagState(t *testing.T) {
	path := configFile(t, t.TempDir(), "valid.yaml", minimal)
	if code, out, errOut := execute([]string{"config", "validate", "--config", path}, nil); code != 0 {
		t.Fatalf("first call code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	code, out, errOut := execute([]string{"config", "validate"}, map[string]string{"STATICA_CONFIG": ""})
	if code != 2 || out != "" || !strings.Contains(errOut, "path must not be empty") {
		t.Fatalf("second call code=%d stdout=%q stderr=%q", code, out, errOut)
	}
}
