package s3

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/gtriggiano/statica/internal/storage"
)

var contentRangePattern = regexp.MustCompile(`^bytes ([0-9]+)-([0-9]+)/([0-9]+|\*)$`)

func metadata(result middleware.Metadata) (storage.Metadata, error) {
	raw, ok := awsmiddleware.GetRawResponse(result).(*smithyhttp.Response)
	if !ok || raw.Response == nil || (raw.StatusCode != 200 && raw.StatusCode != 206) {
		return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	h := raw.Header
	if raw.ContentLength < -1 {
		return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	m := storage.Metadata{Size: raw.ContentLength, ContentType: h.Get("Content-Type"), ContentEncoding: h.Get("Content-Encoding"), ContentDisposition: h.Get("Content-Disposition"), ContentLanguage: h.Get("Content-Language"), ContentRange: h.Get("Content-Range"), Checksum: storage.ChecksumUnavailable}
	if unsafeValue(m.ContentType, m.ContentEncoding, m.ContentDisposition, m.ContentLanguage, m.ContentRange) || !cacheFields(h, &m) {
		return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	if raw.StatusCode == 206 {
		p := contentRangePattern.FindStringSubmatch(m.ContentRange)
		if p == nil {
			return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
		}
		start, e1 := strconv.ParseInt(p[1], 10, 64)
		end, e2 := strconv.ParseInt(p[2], 10, 64)
		if e1 != nil || e2 != nil || end < start || end-start == 1<<63-1 || (m.Size >= 0 && m.Size != end-start+1) {
			return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
		}
		if p[3] != "*" {
			total, e := strconv.ParseInt(p[3], 10, 64)
			if e != nil || total <= end {
				return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
			}
		}
	} else if m.ContentRange != "" {
		return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	return m, nil
}

// cacheFields sets the allowlisted fields a 304 may also carry: ETag,
// Cache-Control, Expires, Date, Age and Last-Modified. It reports false for a
// value containing CR, LF or NUL or an unparseable Last-Modified.
func cacheFields(h http.Header, m *storage.Metadata) bool {
	m.ETag, m.CacheControl, m.Expires = h.Get("ETag"), strings.Join(h.Values("Cache-Control"), ", "), strings.Join(h.Values("Expires"), ", ")
	m.RawDate, m.RawAge = strings.Join(h.Values("Date"), ", "), strings.Join(h.Values("Age"), ", ")
	if unsafeValue(m.ETag, m.CacheControl, m.Expires, m.RawDate, m.RawAge) {
		return false
	}
	if v := h.Get("Last-Modified"); v != "" {
		parsed, err := http.ParseTime(v)
		if err != nil {
			return false
		}
		m.LastModified = parsed
	}
	m.Date, _ = http.ParseTime(m.RawDate)
	if seconds, err := strconv.ParseUint(m.RawAge, 10, 63); err == nil && seconds <= uint64((1<<63-1)/int64(time.Second)) {
		m.Age = time.Duration(seconds) * time.Second
	}
	return true
}

func unsafeValue(values ...string) bool {
	for _, v := range values {
		if strings.ContainsAny(v, "\r\n\x00") {
			return true
		}
	}
	return false
}

// notModified keeps a 304's cache fields for revalidation, validated like a
// 200's metadata; any other field of the 304 is ignored.
func notModified(h http.Header) error {
	m := storage.Metadata{Size: -1, Checksum: storage.ChecksumUnavailable}
	if !cacheFields(h, &m) {
		return &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	return &storage.Error{Kind: storage.NotModified, Size: -1, Metadata: &m}
}

func classify(ctx context.Context, err error, head bool) error {
	k := storage.Unavailable
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		k = storage.Timeout
	} else if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		k = storage.Canceled
	} else {
		var api smithy.APIError
		var response *smithyhttp.ResponseError
		var network net.Error
		var deserialize *smithy.DeserializationError
		status := 0
		if errors.As(err, &response) {
			status = response.HTTPStatusCode()
		}
		code := ""
		if errors.As(err, &api) {
			code = api.ErrorCode()
		}
		switch {
		case status == 403:
			k = storage.Denied
		case code == "NoSuchBucket" || code == "InvalidBucketName" || code == "AuthorizationHeaderMalformed" || code == "PermanentRedirect" || code == "IncorrectEndpoint" || code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch":
			k = storage.Configuration
		case status == 404 && code == "NoSuchKey" && !head:
			k = storage.NotFound
		case status == 404:
			k = storage.AmbiguousNotFound
		case status == 304:
			return notModified(response.Response.Header)
		case status == 412:
			k = storage.Precondition
		case status == 416:
			size := int64(-1)
			if response != nil {
				value := response.Response.Header.Get("Content-Range")
				if strings.HasPrefix(value, "bytes */") {
					if n, e := strconv.ParseInt(strings.TrimPrefix(value, "bytes */"), 10, 64); e == nil && n >= 0 {
						size = n
					}
				}
			}
			return &storage.Error{Kind: storage.InvalidRange, Size: size}
		case status == 429 || code == "SlowDown" || code == "Throttling" || code == "ThrottlingException":
			k = storage.Throttled
		case code == "RequestTimeout" || (errors.As(err, &network) && network.Timeout()):
			k = storage.Timeout
		// The SDK's checksum validation error is intentionally internal. Match
		// its fixed prefix, and never expose expected/actual checksum values.
		case strings.HasPrefix(err.Error(), "checksum did not match:"):
			k = storage.ChecksumMismatch
		case errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &deserialize):
			k = storage.InvalidResponse
		}
	}
	return &storage.Error{Kind: k, Size: -1}
}
