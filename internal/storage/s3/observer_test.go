package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/storage"
)

type operationEvent struct {
	op      Operation
	purpose storage.Purpose
	result  storage.Kind
	headers time.Duration
}

type attemptEvent struct {
	op      Operation
	purpose storage.Purpose
	result  storage.Kind
}

type recorder struct {
	mu         sync.Mutex
	operations []operationEvent
	attempts   []attemptEvent
	bytes      map[storage.Purpose]int
}

func (r *recorder) ObserveOperation(op Operation, p storage.Purpose, k storage.Kind, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operations = append(r.operations, operationEvent{op, p, k, d})
}

func (r *recorder) ObserveAttempt(op Operation, p storage.Purpose, k storage.Kind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts = append(r.attempts, attemptEvent{op, p, k})
}

func (r *recorder) ObserveBodyRead(p storage.Purpose, n int) {
	if n <= 0 {
		panic("non-positive body read reported")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.bytes == nil {
		r.bytes = map[storage.Purpose]int{}
	}
	r.bytes[p] += n
}

func (r *recorder) snapshot() ([]operationEvent, []attemptEvent, map[storage.Purpose]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bytes := map[storage.Purpose]int{}
	for k, v := range r.bytes {
		bytes[k] = v
	}
	return slices.Clone(r.operations), slices.Clone(r.attempts), bytes
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.operations, r.attempts, r.bytes = nil, nil, nil
}

func TestObserverRetriedGetIsOneOperationTwoAttempts(t *testing.T) {
	const delay = 20 * time.Millisecond
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		if calls.Add(1) == 1 {
			xmlError(w, 503, "ServiceUnavailable")
			return
		}
		object(w, "payload")
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.MaxAttempts = 2
	rec := &recorder{}
	c := newClient(t, cfg, WithObserver(rec))
	o, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(o.Body)
	_ = o.Body.Close()
	if err != nil || string(b) != "payload" {
		t.Fatalf("body %q %v", b, err)
	}
	operations, attempts, bytes := rec.snapshot()
	if calls.Load() != 2 {
		t.Fatalf("origin calls %d", calls.Load())
	}
	if len(operations) != 1 || operations[0].op != OperationGet || operations[0].purpose != storage.PurposeRequest || operations[0].result != "" {
		t.Fatalf("operations %+v", operations)
	}
	if operations[0].headers < 2*delay {
		t.Fatalf("header duration %v excludes the retry", operations[0].headers)
	}
	want := []attemptEvent{{OperationGet, storage.PurposeRequest, storage.Unavailable}, {OperationGet, storage.PurposeRequest, ""}}
	if !slices.Equal(attempts, want) {
		t.Fatalf("attempts %+v want %+v", attempts, want)
	}
	if len(bytes) != 1 || bytes[storage.PurposeRequest] != len("payload") {
		t.Fatalf("bytes %v", bytes)
	}
}

func TestObserverPurposeFromContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public-bucket/docs/missing" {
			w.WriteHeader(404)
			return
		}
		object(w, "abc")
	}))
	defer srv.Close()
	rec := &recorder{}
	c := newClient(t, testConfig(srv.URL), WithObserver(rec))
	for _, p := range []storage.Purpose{storage.PurposeRequest, storage.PurposeCacheFill, storage.PurposeHealth} {
		rec.reset()
		ctx := storage.WithPurpose(context.Background(), p)
		if _, err := c.Head(ctx, "docs/key"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Head(ctx, "docs/missing"); !storage.IsKind(err, storage.AmbiguousNotFound) {
			t.Fatal(err)
		}
		o, err := c.Get(ctx, "docs/key", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, o.Body)
		_ = o.Body.Close()
		operations, attempts, bytes := rec.snapshot()
		wantOps := []operationEvent{{OperationHead, p, "", 0}, {OperationHead, p, storage.AmbiguousNotFound, 0}, {OperationGet, p, "", 0}}
		for i := range operations {
			operations[i].headers = 0
		}
		if !slices.Equal(operations, wantOps) {
			t.Fatalf("%s operations %+v", p, operations)
		}
		wantAttempts := []attemptEvent{{OperationHead, p, ""}, {OperationHead, p, storage.AmbiguousNotFound}, {OperationGet, p, ""}}
		if !slices.Equal(attempts, wantAttempts) {
			t.Fatalf("%s attempts %+v", p, attempts)
		}
		if len(bytes) != 1 || bytes[p] != 3 {
			t.Fatalf("%s bytes %v", p, bytes)
		}
	}
}

func TestObserverBodyRetaggedMidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { object(w, "abcdefgh") }))
	defer srv.Close()
	rec := &recorder{}
	c := newClient(t, testConfig(srv.URL), WithObserver(rec))
	o, err := c.Get(storage.WithPurpose(context.Background(), storage.PurposeCacheFill), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Body.Close()
	head := make([]byte, 3)
	if _, err := io.ReadFull(o.Body, head); err != nil {
		t.Fatal(err)
	}
	setter, ok := o.Body.(storage.PurposeSetter)
	if !ok {
		t.Fatal("s3 body does not implement storage.PurposeSetter")
	}
	setter.SetPurpose(storage.PurposeRequest)
	rest, err := io.ReadAll(o.Body)
	if err != nil || string(head)+string(rest) != "abcdefgh" {
		t.Fatalf("body %q%q %v", head, rest, err)
	}
	operations, _, bytes := rec.snapshot()
	if len(operations) != 1 || operations[0].purpose != storage.PurposeCacheFill {
		t.Fatalf("operation purpose %+v", operations)
	}
	if bytes[storage.PurposeCacheFill] != 3 || bytes[storage.PurposeRequest] != 5 || len(bytes) != 2 {
		t.Fatalf("bytes %v", bytes)
	}
}

func TestObserverResults(t *testing.T) {
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch status.Load() {
		case 403:
			xmlError(w, 403, "AccessDenied")
		case 503:
			xmlError(w, 503, "SlowDown")
		case 304:
			w.Header().Set("ETag", `"revision"`)
			w.WriteHeader(304)
		default:
			object(w, "ok")
		}
	}))
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.MaxAttempts = 2
	rec := &recorder{}
	c := newClient(t, cfg, WithObserver(rec))
	for _, tc := range []struct {
		status   int32
		attempts []storage.Kind
		result   storage.Kind
	}{
		{403, []storage.Kind{storage.Denied}, storage.Denied},
		{503, []storage.Kind{storage.Throttled, storage.Throttled}, storage.Throttled},
		{304, []storage.Kind{storage.NotModified}, storage.NotModified},
	} {
		rec.reset()
		status.Store(tc.status)
		_, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{IfNoneMatch: `"revision"`})
		if !storage.IsKind(err, tc.result) {
			t.Fatalf("%d: %v", tc.status, err)
		}
		operations, attempts, _ := rec.snapshot()
		if len(operations) != 1 || operations[0].result != tc.result {
			t.Fatalf("%d operations %+v", tc.status, operations)
		}
		var got []storage.Kind
		for _, a := range attempts {
			got = append(got, a.result)
		}
		if !slices.Equal(got, tc.attempts) {
			t.Fatalf("%d attempts %v want %v", tc.status, got, tc.attempts)
		}
	}

	// Calls rejected before any origin I/O are not operations.
	rec.reset()
	status.Store(200)
	if _, err := c.Get(context.Background(), "other/key", storage.ReadOptions{}); !storage.IsKind(err, storage.InvalidKey) {
		t.Fatal(err)
	}
	if _, err := c.Head(context.Background(), "docs/../key"); !storage.IsKind(err, storage.InvalidKey) {
		t.Fatal(err)
	}
	if _, err := c.Get(context.Background(), "docs/key", storage.ReadOptions{Range: "bytes=0-1\r\n"}); !storage.IsKind(err, storage.InvalidResponse) {
		t.Fatal(err)
	}
	if operations, attempts, _ := rec.snapshot(); len(operations) != 0 || len(attempts) != 0 {
		t.Fatalf("pre-I/O rejections observed: %+v %+v", operations, attempts)
	}

	// A canceled caller is an operation with a canceled result.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Get(ctx, "docs/key", storage.ReadOptions{}); !storage.IsKind(err, storage.Canceled) {
		t.Fatal(err)
	}
	operations, attempts, _ := rec.snapshot()
	if len(operations) != 1 || operations[0].result != storage.Canceled {
		t.Fatalf("canceled operation %+v", operations)
	}
	for _, a := range attempts {
		if a.result != storage.Canceled {
			t.Fatalf("canceled attempt %+v", a)
		}
	}
}

func TestNilObserverIsInert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { object(w, "ok") }))
	defer srv.Close()
	c := newClient(t, testConfig(srv.URL), WithObserver(nil))
	if c.observer != nil {
		t.Fatal("nil observer installed")
	}
	o, err := c.Get(storage.WithPurpose(context.Background(), storage.PurposeCacheFill), "docs/key", storage.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b := o.Body.(*body)
	if b.observer != nil || b.purpose.Load() != nil {
		t.Fatal("uninstrumented body carries telemetry state")
	}
	b.SetPurpose(storage.PurposeRequest) // still a PurposeSetter, with nothing to report
	if got, err := io.ReadAll(o.Body); err != nil || string(got) != "ok" {
		t.Fatalf("%q %v", got, err)
	}
	_ = o.Body.Close()
}
