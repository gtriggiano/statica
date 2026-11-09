// Package storage defines the read-only origin boundary. Keys are literal S3
// keys, never URLs. Callers own every successfully returned body and must close it.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

type Reader interface {
	Get(context.Context, string, ReadOptions) (*Object, error)
	Head(context.Context, string) (Metadata, error)
}

type ReadOptions struct{ Range, IfMatch, IfNoneMatch string }

// Purpose is the finite reason an origin operation runs, carried in its
// context for the origin metrics' purpose label. It never names a host or key.
// Operations are attributed to their initiator: a GET a cache fill issues is a
// PurposeCacheFill operation even when its stream is then handed to one
// request. Bytes of a stream handed to a request are attributed to
// PurposeRequest; see PurposeSetter.
type Purpose string

const (
	PurposeRequest   Purpose = "request"    // on behalf of one client request; the default
	PurposeCacheFill Purpose = "cache_fill" // a shared cache fill or revalidation, owned by no request
	PurposeHealth    Purpose = "health"     // a background origin health probe
)

type purposeKey struct{}

// WithPurpose marks the origin operations made with ctx as serving p.
func WithPurpose(ctx context.Context, p Purpose) context.Context {
	return context.WithValue(ctx, purposeKey{}, p)
}

// PurposeSetter is implemented by origin bodies that attribute the bytes they
// read by purpose, initially the purpose of the operation that opened them.
// A caller that hands a body to an owner with another purpose retags it with
// SetPurpose before reading from it.
type PurposeSetter interface{ SetPurpose(Purpose) }

// PurposeFrom returns the purpose ctx carries, PurposeRequest when it carries
// none.
func PurposeFrom(ctx context.Context) Purpose {
	if p, ok := ctx.Value(purposeKey{}).(Purpose); ok && p != "" {
		return p
	}
	return PurposeRequest
}

type Object struct {
	Metadata Metadata
	Body     io.ReadCloser
}

// ChecksumStatus describes SDK response checksum validation. Pending becomes
// Verified only after a successful EOF; absent checksums remain Unavailable.
type ChecksumStatus string

const (
	ChecksumUnavailable ChecksumStatus = "unavailable"
	ChecksumPending     ChecksumStatus = "pending"
	ChecksumVerified    ChecksumStatus = "verified"
)

type Metadata struct {
	Size                                                int64 // -1 means unknown; for a partial response this is the body size.
	ETag                                                string
	LastModified                                        time.Time
	ContentType, ContentEncoding, CacheControl, Expires string
	ContentDisposition, ContentLanguage                 string
	Date                                                time.Time
	Age                                                 time.Duration
	// RawDate and RawAge preserve cache inputs, including malformed values, so
	// the HTTP/cache policy can fail closed rather than treat them as absent.
	RawDate, RawAge string
	ContentRange    string
	Checksum        ChecksumStatus
}

type Capabilities struct{ ConditionalReads, RangeReads, Checksums bool }

// Kind is a safe, bounded diagnostic. Only NotFound confirms object absence.
type Kind string

const (
	NotFound          Kind = "not_found"
	AmbiguousNotFound Kind = "ambiguous_not_found"
	Denied            Kind = "denied"
	Configuration     Kind = "configuration"
	Unavailable       Kind = "unavailable"
	NotModified       Kind = "not_modified"
	Precondition      Kind = "precondition"
	InvalidRange      Kind = "invalid_range"
	Throttled         Kind = "throttled"
	Timeout           Kind = "timeout"
	Canceled          Kind = "canceled"
	InvalidResponse   Kind = "invalid_response"
	ChecksumMismatch  Kind = "checksum_mismatch"
	Unsupported       Kind = "unsupported"
	InvalidKey        Kind = "invalid_key"
)

// Error deliberately does not wrap provider errors: SDK errors may contain
// credentials, URLs, object names or untrusted response text.
type Error struct {
	Kind Kind
	Size int64 // known complete size for InvalidRange, otherwise -1
	// Metadata is set only for NotModified: the validated fields a 304 may
	// carry (ETag, Cache-Control, Expires, Date, Age and Last-Modified, with
	// Size -1), which revalidation merges into the stored metadata.
	Metadata *Metadata
}

func (e *Error) Error() string { return "origin: " + string(e.Kind) }
func IsKind(err error, kind Kind) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == kind
}
