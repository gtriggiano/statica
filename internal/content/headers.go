package content

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/site"
	"github.com/gtriggiano/statica/internal/storage"
)

func safeHeader(value string) bool {
	for i := range value {
		if value[i] < 0x20 && value[i] != '\t' || value[i] == 0x7f {
			return false
		}
	}
	return true
}

func contentType(value, logicalPath string) string {
	if safeHeader(value) && value != "" {
		if _, _, err := mime.ParseMediaType(value); err == nil {
			return value
		}
	}
	return site.ExtensionContentType(logicalPath)
}

func validHTTPTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1601 && t.Year() <= 9999 }

func mergeVary(h http.Header, value string) {
	for _, line := range h.Values("Vary") {
		for _, token := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(token), value) || strings.TrimSpace(token) == "*" {
				return
			}
		}
	}
	h.Add("Vary", value)
}

func vary(h http.Header, s *site.Selection) {
	if s.VaryAccept {
		mergeVary(h, "Accept")
	}
	if s.VaryEncoding {
		mergeVary(h, "Accept-Encoding")
	}
}

func (h *handler) objectHeaders(out http.Header, s *site.Selection, publicPath string, now time.Time) string {
	m := s.Metadata
	vary(out, s)
	declared := s.ContentType
	if declared == "" {
		declared = m.ContentType
	}
	out.Set("Content-Type", contentType(declared, s.LogicalPath))
	if m.Size >= 0 {
		out.Set("Content-Length", strconv.FormatInt(m.Size, 10))
	}
	if m.ContentEncoding == "gzip" || m.ContentEncoding == "br" {
		out.Set("Content-Encoding", m.ContentEncoding)
	}
	if m.ContentDisposition != "" && safeHeader(m.ContentDisposition) {
		if _, _, err := mime.ParseMediaType(m.ContentDisposition); err == nil {
			out.Set("Content-Disposition", m.ContentDisposition)
		}
	}
	if validLanguage(m.ContentLanguage) {
		out.Set("Content-Language", m.ContentLanguage)
	}
	if s.Status != http.StatusOK {
		out.Set("Cache-Control", "no-store")
		return ""
	}
	if httpcache.ValidETag(m.ETag) {
		out.Set("ETag", m.ETag)
	}
	if validHTTPTime(m.LastModified) {
		out.Set("Last-Modified", m.LastModified.UTC().Format(http.TimeFormat))
	}
	if s.Resolution == site.ResolutionSPA {
		out.Set("Cache-Control", "no-store")
		return ""
	}
	return h.cacheHeaders(out, m, publicPath, s.ObjectPath, now)
}

func validLanguage(s string) bool {
	if s == "" {
		return false
	}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return false
		}
		for i := range item {
			c := item[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func (h *handler) cacheHeaders(out http.Header, m storage.Metadata, publicPath, objectPath string, now time.Time) string {
	policy := h.cacheControl.ValueFor(publicPath)
	if policy != "" && policy != "auto" {
		out.Set("Cache-Control", policy)
		// A downstream policy override does not make a snapshot younger.
		ageHeaders(out, m, now)
		return ""
	}
	// Origin policy is untrusted. The memory cache shares this predicate, so it
	// never stores metadata served as no-store here. An Age or Cache-Control
	// delta-seconds beyond 2^31 saturates (RFC 9111 §1.2.2) and is not malformed.
	if httpcache.Malformed(m) {
		out.Set("Cache-Control", "no-store")
		return "malformed_origin_cache_metadata"
	}
	if m.CacheControl != "" {
		out.Set("Cache-Control", m.CacheControl)
	}
	if m.Expires != "" {
		out.Set("Expires", m.Expires)
	}
	ageHeaders(out, m, now)
	if m.CacheControl != "" || m.Expires != "" {
		return ""
	}
	cache := h.cache.PolicyFor(objectPath)
	if cache.Enabled && cache.TTL >= time.Second {
		out.Set("Cache-Control", "max-age="+strconv.FormatInt(int64(cache.TTL/time.Second), 10))
	} else {
		out.Set("Cache-Control", "no-cache")
	}
	return ""
}

func ageHeaders(out http.Header, m storage.Metadata, now time.Time) {
	var age uint64
	if m.Age > 0 {
		age = uint64(m.Age / time.Second)
	}
	if seconds, ok := httpcache.DeltaSeconds(m.RawAge); ok && uint64(seconds) > age {
		age = uint64(seconds)
	}
	date := m.Date
	if m.RawDate != "" {
		if parsed, err := http.ParseTime(m.RawDate); err == nil {
			date = parsed
		}
	}
	if validHTTPTime(date) {
		out.Set("Date", date.UTC().Format(http.TimeFormat))
		// HTTP dates cover more than time.Duration's roughly 290 years.
		if apparent := now.Unix() - date.Unix(); apparent > 0 && uint64(apparent) > age {
			age = uint64(apparent)
		}
	}
	// The largest source saturates at 2^31 seconds (RFC 9111 §1.2.2).
	age = min(age, httpcache.MaxDelta)
	if age > 0 {
		out.Set("Age", strconv.FormatUint(age, 10))
	}
}
