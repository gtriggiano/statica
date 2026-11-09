package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/gtriggiano/statica/internal/storage"
)

type staticCredentials struct{}

func (staticCredentials) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "fixture-key", SecretAccessKey: "fixture-secret", Source: "fixture"}, nil
}

func s3Object(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
	w.Header().Set("ETag", `"revision"`)
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, payload)
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>private-origin-detail</Message></Error>", code)
}

func operations(op, result, purpose string) string {
	return series("statica_origin_operations_total", "operation", op, "result", result, "purpose", purpose)
}
func attempts(op, result, purpose string) string {
	return series("statica_origin_attempts_total", "operation", op, "result", result, "purpose", purpose)
}
func headerCount(op, purpose string) string {
	return series("statica_origin_header_duration_seconds_count", "operation", op, "purpose", purpose)
}
func headerSum(op, purpose string) string {
	return series("statica_origin_header_duration_seconds_sum", "operation", op, "purpose", purpose)
}
func originBytes(purpose string) string {
	return series("statica_origin_bytes_total", "purpose", purpose)
}

func TestOriginMetricsFromS3Adapter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/flaky") && n == 1:
			s3Error(w, 503, "ServiceUnavailable")
		case strings.HasSuffix(r.URL.Path, "/denied"):
			s3Error(w, 403, "AccessDenied")
		case strings.HasSuffix(r.URL.Path, "/missing"):
			w.WriteHeader(404)
		default:
			s3Object(w, "0123456789")
		}
	}))
	defer srv.Close()
	m := newMetrics(t, nil)
	c := newS3Client(t, srv.URL, 2, m)
	read := func(ctx context.Context, key string, retag int) {
		t.Helper()
		o, err := c.Get(ctx, key, storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer o.Body.Close()
		if retag > 0 {
			if _, err := io.ReadFull(o.Body, make([]byte, retag)); err != nil {
				t.Fatal(err)
			}
			o.Body.(storage.PurposeSetter).SetPurpose(storage.PurposeRequest)
		}
		if _, err := io.Copy(io.Discard, o.Body); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct {
		name string
		do   func()
		want map[string]float64
	}{
		// 503 then 200 under max_attempts 2: one operation, two attempts.
		{"retried get", func() { read(context.Background(), "pub/flaky", 0) }, map[string]float64{
			operations("get", "success", "request"): 1, attempts("get", "unavailable", "request"): 1, attempts("get", "success", "request"): 1,
			headerCount("get", "request"): 1, originBytes("request"): 10}},
		// A fill's stream handed to a request mid-read: the operation is the
		// fill's, the bytes after the retag the request's.
		{"retagged fill stream", func() { read(storage.WithPurpose(context.Background(), storage.PurposeCacheFill), "pub/fill", 4) }, map[string]float64{
			operations("get", "success", "cache_fill"): 1, attempts("get", "success", "cache_fill"): 1,
			headerCount("get", "cache_fill"): 1, originBytes("cache_fill"): 4, originBytes("request"): 6}},
		{"health head", func() {
			if _, err := c.Head(storage.WithPurpose(context.Background(), storage.PurposeHealth), "pub/index.html"); err != nil {
				t.Fatal(err)
			}
		}, map[string]float64{operations("head", "success", "health"): 1, attempts("head", "success", "health"): 1, headerCount("head", "health"): 1}},
		{"denied get", func() {
			if _, err := c.Get(context.Background(), "pub/denied", storage.ReadOptions{}); !storage.IsKind(err, storage.Denied) {
				t.Fatal(err)
			}
		}, map[string]float64{operations("get", "denied", "request"): 1, attempts("get", "denied", "request"): 1, headerCount("get", "request"): 1}},
		{"missing head", func() {
			if _, err := c.Head(context.Background(), "pub/missing"); !storage.IsKind(err, storage.AmbiguousNotFound) {
				t.Fatal(err)
			}
		}, map[string]float64{operations("head", "ambiguous_not_found", "request"): 1, attempts("head", "ambiguous_not_found", "request"): 1, headerCount("head", "request"): 1}},
		{"invalid key", func() {
			if _, err := c.Get(context.Background(), "other/key", storage.ReadOptions{}); !storage.IsKind(err, storage.InvalidKey) {
				t.Fatal(err)
			}
		}, map[string]float64{}},
	}
	before := scrape(t, m)
	for _, step := range steps {
		step.do()
		after := scrape(t, m)
		got := changes(before, after)
		for k := range got {
			if strings.Contains(k, "_header_duration_seconds_sum") {
				if got[k] <= 0 {
					t.Errorf("%s: %s did not grow", step.name, k)
				}
				delete(got, k)
			}
		}
		assertChanges(t, step.name, got, step.want)
		before = after
	}
	if got := before[headerSum("get", "request")]; got <= 0 {
		t.Fatalf("header duration sum %v", got)
	}
	for _, v := range labelValues(t, m, "result") {
		if strings.Contains(v, "private") || strings.Contains(v, "Denied") || strings.Contains(v, " ") {
			t.Fatalf("result label carries error text: %q", v)
		}
	}
}
