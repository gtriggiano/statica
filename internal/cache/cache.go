// Package cache is statica's bounded in-memory cache of origin results. It has
// four layers:
//
//   - Store (store.go) owns memory: indexing, LRU order, byte and entry
//     accounting, reader pins, retirement and fill reservations. It performs
//     no I/O and does not interpret freshness; its callers decide what a
//     snapshot may satisfy.
//   - The policy functions (policy.go) do interpret it: admissibility,
//     freshness, stale_if_error and validators, as pure functions of the
//     effective policy, the request directives, a snapshot and the clock.
//   - Cache (lookup.go, fill.go) serves request-level reads through a Store:
//     hits, misses and HEADs, misses and revalidations of one key coalesced
//     into one bounded flight, streams for what a flight cannot buffer, and
//     finite telemetry that never carries a key.
//   - NewReader (reader.go) decorates a Cache as a storage.Reader, so
//     resolution and response code keep talking to the origin boundary,
//     with each request's directives and lookup records in its context.
//
// One Store serves one origin, so the exact S3 key, including the publication
// prefix and any coding suffix, is the whole identity. Every Store ownership
// transition happens under one mutex that is never held while blocking.
package cache

import (
	"errors"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

// Limits are process-wide ceilings. MaxBytes and MaxEntries bound indexed plus
// retired entries. MaxFillBytes separately bounds fill reservations, including
// the ones still held by detached snapshots.
type Limits struct {
	MaxBytes, MaxObjectBytes, MaxFillBytes int64
	MaxEntries                             int
}

// LimitsFrom selects the capacity ceilings of a validated cache configuration.
func LimitsFrom(c config.Cache) Limits {
	return Limits{MaxBytes: int64(c.MaxBytes), MaxObjectBytes: int64(c.MaxObjectBytes), MaxFillBytes: int64(c.MaxFillBytes), MaxEntries: c.MaxEntries}
}

func (l Limits) validate() error {
	if l.MaxBytes <= 0 || l.MaxEntries <= 0 || l.MaxObjectBytes <= 0 || l.MaxFillBytes <= 0 {
		return errors.New("cache: limits must be positive")
	}
	if l.MaxObjectBytes > l.MaxBytes || l.MaxObjectBytes > l.MaxFillBytes {
		return errors.New("cache: max_object_bytes must not exceed max_bytes or max_fill_bytes")
	}
	return nil
}

// Kind is the finite set of snapshot kinds.
type Kind string

const (
	// KindBody holds the complete object bytes and their metadata.
	KindBody Kind = "body"
	// KindMetadata holds a HEAD result. It never satisfies a GET.
	KindMetadata Kind = "metadata"
	// KindNegative records a confirmed missing object.
	KindNegative Kind = "negative"
)

var kinds = [...]Kind{KindBody, KindMetadata, KindNegative}

// Version identifies one entry for compare-and-swap without pinning it across
// origin I/O. It is meaningful only to the Store that issued it. Each Store
// starts from a random value and issues strictly increasing versions, wrapping
// around past Absent, which is never issued and names "no indexed entry".
type Version uint64

const Absent Version = 0

// Result is the finite outcome of Admit, Promote, Refresh and Reserve.
type Result string

const (
	ResultAdmitted  Result = "admitted"  // indexed under a key that had no entry, or reserved
	ResultReplaced  Result = "replaced"  // indexed, superseding the key's entry
	ResultRefreshed Result = "refreshed" // Refresh swapped the expected entry
	ResultTooLarge  Result = "too_large" // over max_object_bytes, or charge over max_bytes
	ResultNoSpace   Result = "no_space"  // pinned entries prevent making room, or Reserve finds the fill budget full (not counted as an admission: it surfaces as the bypass reason fill_budget)
	ResultInvalid   Result = "invalid"   // malformed key, snapshot, size, handle or reservation
	ResultConflict  Result = "conflict"  // the expected version or entry is not indexed
)

var results = [...]Result{ResultAdmitted, ResultReplaced, ResultRefreshed, ResultTooLarge, ResultNoSpace, ResultInvalid, ResultConflict}

// Snapshot is one immutable origin result. The Store copies the struct and
// never copies Body: once a snapshot is admitted or promoted, neither its
// producer nor any reader may modify the Body bytes. RequestTime and
// ResponseTime are kept for RFC 9111 age calculation and not interpreted here.
type Snapshot struct {
	Kind                      Kind
	Metadata                  storage.Metadata
	Body                      []byte // complete object bytes; KindBody only
	RequestTime, ResponseTime time.Time
}

// valid requires complete bytes of a known size for a body, and no body at all
// for metadata-only and negative snapshots.
func (s Snapshot) valid() bool {
	switch s.Kind {
	case KindBody:
		return s.Metadata.Size >= 0 && int64(len(s.Body)) == s.Metadata.Size
	case KindMetadata, KindNegative:
		return s.Body == nil
	}
	return false
}

// entryOverhead is charged once per entry for the memory the Store keeps
// besides the key, metadata strings and body bytes: the entry with its inline
// Snapshot, the shared body record, the simplelru list node and its map slot.
// A test keeps it above their measured and estimated sizes.
const entryOverhead = 640

// ownCharge is an entry's charge excluding body bytes, which are charged once
// per body record because Refresh may share them between snapshots.
func ownCharge(key string, m storage.Metadata) int64 {
	return int64(entryOverhead + len(key) + len(m.ETag) + len(m.ContentType) + len(m.ContentEncoding) +
		len(m.CacheControl) + len(m.Expires) + len(m.ContentDisposition) + len(m.ContentLanguage) +
		len(m.RawDate) + len(m.RawAge) + len(m.ContentRange) + len(m.Checksum))
}

// Stats is a point-in-time copy of the Store's accounting. ChargedBytes is
// bounded by Limits.MaxBytes and FillBytes, which includes DetachedBytes, by
// Limits.MaxFillBytes. Reservations counts unconsumed reservations. Evictions
// count capacity evictions only; Expirations count entry versions a lookup
// found expired, once each (Handle.MarkExpired); Admissions count every Admit,
// Promote and Refresh outcome. Every map holds every label.
type Stats struct {
	Limits                                    Limits
	ResidentBytes, RetiredBytes, ChargedBytes int64
	ResidentEntries, RetiredEntries           int
	FillBytes, DetachedBytes                  int64
	DetachedEntries, Reservations             int
	Evictions, Expirations                    map[Kind]uint64
	Admissions                                map[Result]uint64
}
