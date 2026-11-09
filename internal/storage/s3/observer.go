package s3

import (
	"context"
	"errors"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go/middleware"
	"github.com/gtriggiano/statica/internal/storage"
)

// Operation is the finite name of a logical origin operation.
type Operation string

const (
	OperationGet  Operation = "get"
	OperationHead Operation = "head"
)

// Observer receives the adapter's origin telemetry (see WithObserver). Every
// argument is finite and key-free: a result is the storage.Kind of the error,
// "" for success, and a purpose is the storage.PurposeFrom of the operation's
// context. Methods are called synchronously on request and fill paths; they
// must be fast, safe for concurrent use and must not panic.
type Observer interface {
	// ObserveOperation reports one logical Get or Head that reached the SDK,
	// when it returns: result is the Kind of the error it returns, and
	// headers the time from the call to its response headers or failure,
	// retries and backoff included. Calls rejected before any origin I/O
	// (invalid keys, unsupported features) are not operations.
	ObserveOperation(op Operation, purpose storage.Purpose, result storage.Kind, headers time.Duration)
	// ObserveAttempt reports one HTTP round trip of an operation, every
	// retry included, classified like an operation's error. Only round trips
	// are attempts: an operation that fails before reaching the transport,
	// e.g. on a credential or signing error, is reported with no attempt, so
	// operations may exceed attempts.
	ObserveAttempt(op Operation, purpose storage.Purpose, result storage.Kind)
	// ObserveBodyRead reports n > 0 body bytes read from the origin by a Get
	// stream, attributed to the stream's purpose at the time of the read:
	// initially its operation's, then whatever storage.PurposeSetter set.
	ObserveBodyRead(purpose storage.Purpose, n int)
}

// attemptObserver reports each transport attempt. It is added last in the
// Finalize step, after the SDK's retry, identity and signing middleware, so
// it runs once per HTTP round trip and sees that attempt's own deserialized
// error, which classify maps exactly as it maps an operation's.
type attemptObserver struct{ observer Observer }

func (attemptObserver) ID() string { return "StaticaAttemptObserver" }

func (m attemptObserver) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
	out, metadata, err := next.HandleFinalize(ctx, in)
	op, head := OperationGet, false
	if awsmiddleware.GetOperationName(ctx) == "HeadObject" {
		op, head = OperationHead, true
	}
	var result storage.Kind
	if err != nil {
		result = kindOf(classify(ctx, err, head))
	}
	m.observer.ObserveAttempt(op, storage.PurposeFrom(ctx), result)
	return out, metadata, err
}

// kindOf is the result of a call that returned err: "" for success.
func kindOf(err error) storage.Kind {
	if err == nil {
		return ""
	}
	var e *storage.Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return storage.Unavailable
}

// purposes lets a body hold its purpose as a pointer without allocating for
// the defined values.
var purposes = [...]storage.Purpose{storage.PurposeRequest, storage.PurposeCacheFill, storage.PurposeHealth}

func purposeRef(p storage.Purpose) *storage.Purpose {
	for i := range purposes {
		if purposes[i] == p {
			return &purposes[i]
		}
	}
	return &p
}
