package content

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

// A malformed condition is ignored as a whole. Its presence still suppresses
// the lower-priority date condition, so a broken validator cannot change the
// precedence of otherwise valid conditions.
func precondition(h http.Header, m storage.Metadata) int {
	if values := h.Values("If-Match"); len(values) > 0 {
		if tags, wildcard, valid := entityTags(strings.Join(values, ",")); valid && !wildcard && !tagMatches(tags, m.ETag, true) {
			return http.StatusPreconditionFailed
		}
	} else if date, ok := singleDate(h, "If-Unmodified-Since"); ok && !m.LastModified.IsZero() && m.LastModified.Truncate(time.Second).After(date) {
		return http.StatusPreconditionFailed
	}
	if values := h.Values("If-None-Match"); len(values) > 0 {
		if tags, wildcard, valid := entityTags(strings.Join(values, ",")); valid && (wildcard || tagMatches(tags, m.ETag, false)) {
			return http.StatusNotModified
		}
	} else if date, ok := singleDate(h, "If-Modified-Since"); ok && !m.LastModified.IsZero() && !m.LastModified.Truncate(time.Second).After(date) {
		return http.StatusNotModified
	}
	return 0
}

func singleHeader(h http.Header, name string) string {
	values := h.Values(name)
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

// singleDate parses a single HTTP-date field. An absent, repeated or empty
// field is skipped without calling http.ParseTime, which would only fail.
func singleDate(h http.Header, name string) (time.Time, bool) {
	value := singleHeader(h, name)
	if value == "" {
		return time.Time{}, false
	}
	date, err := http.ParseTime(value)
	return date, err == nil
}

func requestIfRangeMatches(h http.Header, m storage.Metadata) bool {
	values := h.Values("If-Range")
	if len(values) == 0 {
		return true
	}
	return len(values) == 1 && values[0] != "" && ifRangeMatches(values[0], m)
}

func entityTags(value string) ([]string, bool, bool) {
	value = strings.TrimSpace(value)
	if value == "*" {
		return nil, true, true
	}
	var tags []string
	for value != "" {
		end := 0
		if strings.HasPrefix(value, "W/") {
			end = 2
		}
		if end >= len(value) || value[end] != '"' {
			return nil, false, false
		}
		close := strings.IndexByte(value[end+1:], '"')
		if close < 0 {
			return nil, false, false
		}
		end += close + 2
		tag := value[:end]
		if !httpcache.ValidETag(tag) {
			return nil, false, false
		}
		tags = append(tags, tag)
		value = strings.TrimSpace(value[end:])
		if value == "" {
			return tags, false, true
		}
		if value[0] != ',' {
			return nil, false, false
		}
		value = strings.TrimSpace(value[1:])
		if value == "" {
			return nil, false, false
		}
	}
	return nil, false, false
}

func tagMatches(tags []string, selected string, strong bool) bool {
	if !httpcache.ValidETag(selected) {
		return false
	}
	for _, tag := range tags {
		if strong {
			if httpcache.StrongETag(tag) && httpcache.StrongETag(selected) && tag == selected {
				return true
			}
		} else if strings.TrimPrefix(tag, "W/") == strings.TrimPrefix(selected, "W/") {
			return true
		}
	}
	return false
}

type byteRange struct{ start, end, size int64 }

func (r byteRange) length() int64 { return r.end - r.start + 1 }
func (r byteRange) request() string {
	return "bytes=" + strconv.FormatInt(r.start, 10) + "-" + strconv.FormatInt(r.end, 10)
}
func (r byteRange) contentRange() string {
	return "bytes " + strconv.FormatInt(r.start, 10) + "-" + strconv.FormatInt(r.end, 10) + "/" + strconv.FormatInt(r.size, 10)
}

// valid=false means ignore the entire Range. A valid unsatisfiable range has
// satisfiable=false. Integers are parsed without accepting signs or overflow.
func parseRange(value string, size int64) (r byteRange, valid, satisfiable bool) {
	if size < 0 || !strings.HasPrefix(value, "bytes=") {
		return r, false, false
	}
	value = strings.TrimSpace(value[6:])
	a, b, ok := strings.Cut(value, "-")
	if !ok || strings.ContainsAny(value, ", \t") || a == "" && b == "" {
		return r, false, false
	}
	decimal := func(s string) (int64, bool) {
		if s == "" {
			return 0, false
		}
		for i := range s {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
		}
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	}
	r.size = size
	if a == "" {
		suffix, ok := decimal(b)
		if !ok {
			return r, false, false
		}
		if suffix == 0 || size == 0 {
			return r, true, false
		}
		if suffix > size {
			suffix = size
		}
		r.start, r.end = size-suffix, size-1
		return r, true, true
	}
	start, ok := decimal(a)
	if !ok {
		return r, false, false
	}
	end := size - 1
	if b != "" {
		end, ok = decimal(b)
		if !ok || end < start {
			return r, false, false
		}
	}
	if start >= size {
		return r, true, false
	}
	if end >= size {
		end = size - 1
	}
	r.start, r.end = start, end
	return r, true, true
}

func ifRangeMatches(value string, m storage.Metadata) bool {
	if value == "" {
		return true
	}
	if httpcache.StrongETag(value) {
		return httpcache.StrongETag(m.ETag) && value == m.ETag
	}
	date, err := http.ParseTime(value)
	return err == nil && !m.LastModified.IsZero() && m.LastModified.Truncate(time.Second).Equal(date) &&
		validHTTPTime(m.Date) && !m.LastModified.After(m.Date.Add(-60*time.Second))
}
