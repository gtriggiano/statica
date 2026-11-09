package httpcache

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gtriggiano/statica/internal/storage"
)

// Malformed reports origin cache metadata that statica must neither forward
// nor store: an invalid Cache-Control, a negative decoded Age, or a present
// but unparseable Expires, Date or Age field. The response layer fails closed
// to no-store and the memory cache never admits or reuses such metadata, so
// both use this one predicate. Delta-seconds beyond MaxDelta, in Age or in
// Cache-Control, saturate and are not malformed.
func Malformed(m storage.Metadata) bool {
	if m.CacheControl != "" && !Valid(m.CacheControl) || m.Age < 0 {
		return true
	}
	for _, v := range []string{m.Expires, m.RawDate} {
		if v == "" {
			continue
		}
		if _, err := http.ParseTime(v); err != nil {
			return true
		}
	}
	_, ok := DeltaSeconds(m.RawAge)
	return m.RawAge != "" && !ok
}

// CurrentAge is the RFC 9111 §4.2.3 current age at now of a response requested
// at requestTime and received at responseTime:
//
//	apparent_age = max(0, response_time - date_value)
//	corrected_age_value = age_value + response_delay
//	current_age = max(apparent_age, corrected_age_value) + (now - response_time)
//
// date_value is the origin Date, or response_time when it is missing (RFC 9110
// §6.6.1); a Date in the future therefore adds nothing. age_value is the
// origin Age, saturated. A response delay or residence that clock steps make
// negative counts as zero, so the result is never negative; it saturates at
// MaxDelta seconds instead of overflowing. m must not be Malformed.
func CurrentAge(m storage.Metadata, requestTime, responseTime, now time.Time) time.Duration {
	apparent := clamp(responseTime.Sub(dateValue(m, responseTime)))
	corrected := ageValue(m) + clamp(responseTime.Sub(requestTime))
	return clamp(max(apparent, corrected) + clamp(now.Sub(responseTime)))
}

// Lifetime is the shared-cache freshness lifetime the origin assigned (RFC
// 9111 §4.2.1): s-maxage, else max-age, else Expires minus date_value, which
// is zero for an Expires not after it. ok is false when the origin assigned
// none. d must be the parsed m.CacheControl.
func Lifetime(d Directives, m storage.Metadata, responseTime time.Time) (lifetime time.Duration, ok bool) {
	switch {
	case d.SMaxAge.Present:
		return d.SMaxAge.Duration(), true
	case d.MaxAge.Present:
		return d.MaxAge.Duration(), true
	case m.Expires == "":
		return 0, false
	}
	expires, err := http.ParseTime(m.Expires)
	if err != nil {
		// Unreachable for callers that reject Malformed metadata first; kept
		// so a direct caller gets RFC 9111 §5.3 semantics: already expired.
		return 0, true
	}
	return clamp(expires.Sub(dateValue(m, responseTime))), true
}

// dateValue prefers RawDate, then a decoded Date, as the response layer does.
func dateValue(m storage.Metadata, responseTime time.Time) time.Time {
	if m.RawDate != "" { // ParseTime("") always fails, allocating an error
		if date, err := http.ParseTime(m.RawDate); err == nil {
			return date
		}
	}
	if !m.Date.IsZero() {
		return m.Date
	}
	return responseTime
}

// ageValue is the larger of RawAge and a decoded Age, in [0, maxAge].
func ageValue(m storage.Metadata) time.Duration {
	age := max(m.Age, 0)
	if seconds, ok := DeltaSeconds(m.RawAge); ok {
		age = max(age, time.Duration(seconds)*time.Second)
	}
	return min(age, maxAge)
}

// clamp bounds d to [0, maxAge]; sums of three clamped values cannot overflow.
func clamp(d time.Duration) time.Duration { return min(max(d, 0), maxAge) }

// Request holds the request directives statica honors (RFC 9111 §5.2.1).
// Pragma is ignored: RFC 9111 §5.4 deprecates it and clients that send it also
// send Cache-Control. min-fresh and max-stale are outside statica's contract,
// which neither tightens freshness nor serves stale content on request.
type Request struct {
	NoStore, NoCache bool
	MaxAge           Delta
}

// RequestDirectives strictly parses every Cache-Control line of h as one field
// value. When that fails, max-age is ignored but a comma-separated member
// named no-store or no-cache, compared ASCII case-insensitively after trimming
// spaces and tabs and with any argument ignored, is still honored: it only
// adds bypass or revalidation, so even a match inside a malformed quoted
// string errs on the safe side.
func RequestDirectives(h http.Header) Request {
	values := h.Values("Cache-Control")
	if len(values) == 0 {
		return Request{}
	}
	joined := strings.Join(values, ", ")
	if d, ok := Parse(joined); ok {
		return Request{NoStore: d.NoStore, NoCache: d.NoCache, MaxAge: d.MaxAge}
	}
	var r Request
	for _, member := range strings.Split(joined, ",") {
		name, _, _ := strings.Cut(member, "=")
		name = strings.Trim(name, " \t")
		r.NoStore = r.NoStore || equalFoldASCII(name, "no-store")
		r.NoCache = r.NoCache || equalFoldASCII(name, "no-cache")
	}
	return r
}

// equalFoldASCII compares s with lowercase ASCII lower, folding only A-Z.
func equalFoldASCII(s, lower string) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// ErrETagChanged reports a 304 whose entity-tag identifies a representation
// other than the stored one; the caller must refetch.
var ErrETagChanged = errors.New("httpcache: 304 entity-tag differs from the stored one")

// MergeNotModified freshens stored metadata with a 304 response (RFC 9111
// §4.3.4). A present Cache-Control or Expires and a non-zero Last-Modified
// replace the stored values. Date and Age always come from the 304: they
// describe the revalidation exchange, and keeping the stored ones would charge
// the elapsed residence again against the renewed freshness. Representation
// fields (Size, ContentType, ContentEncoding, ContentDisposition,
// ContentLanguage, ContentRange and Checksum) never change: statica stores
// processed metadata and RFC 9111 §3.2 lets a 304 omit them. A 304 ETag must
// equal the stored one exactly. A stored snapshot without an ETag is never
// revalidated conditionally (cache.Validator returns no validator for it), so
// a 304 carrying an ETag against one is refused by design. The caller
// re-checks the merged metadata for admission, which rejects a 304 that
// carried malformed cache metadata.
func MergeNotModified(stored, notModified storage.Metadata) (storage.Metadata, error) {
	if notModified.ETag != "" && notModified.ETag != stored.ETag {
		return storage.Metadata{}, ErrETagChanged
	}
	merged := stored
	if notModified.CacheControl != "" {
		merged.CacheControl = notModified.CacheControl
	}
	if notModified.Expires != "" {
		merged.Expires = notModified.Expires
	}
	if !notModified.LastModified.IsZero() {
		merged.LastModified = notModified.LastModified
	}
	merged.Date, merged.RawDate = notModified.Date, notModified.RawDate
	merged.Age, merged.RawAge = notModified.Age, notModified.RawAge
	return merged, nil
}
