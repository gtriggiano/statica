package metrics

import (
	"time"

	"github.com/gtriggiano/statica/internal/storage"
	"github.com/gtriggiano/statica/internal/storage/s3"
)

// originKey identifies an origin series. The header histogram leaves result
// empty.
type originKey struct{ operation, result, purpose string }

// OriginObserver returns the observer that feeds the origin series; install
// it with s3.WithObserver.
func (m *Metrics) OriginObserver() s3.Observer { return originObserver{m} }

type originObserver struct{ m *Metrics }

func (o originObserver) ObserveOperation(op s3.Operation, purpose storage.Purpose, result storage.Kind, headers time.Duration) {
	o.m.operations.get(originKey{orUnknown(string(op)), resultLabel(result), orUnknown(string(purpose))}).Inc()
	o.m.headers.get(originKey{operation: orUnknown(string(op)), purpose: orUnknown(string(purpose))}).Observe(headers.Seconds())
}

func (o originObserver) ObserveAttempt(op s3.Operation, purpose storage.Purpose, result storage.Kind) {
	o.m.attempts.get(originKey{orUnknown(string(op)), resultLabel(result), orUnknown(string(purpose))}).Inc()
}

func (o originObserver) ObserveBodyRead(purpose storage.Purpose, n int) {
	if n > 0 {
		o.m.bytes.get(orUnknown(string(purpose))).Add(float64(n))
	}
}

// resultLabel is success for no error, otherwise the finite storage error
// kind; the adapter never reports error text.
func resultLabel(k storage.Kind) string {
	if k == "" {
		return "success"
	}
	return string(k)
}
