package config

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/redirect"
	"golang.org/x/net/idna"
)

func (c *Config) validate() error {
	if c.present("hosts") {
		if len(c.Hosts) == 0 {
			return c.err("hosts", "must be nonempty when supplied")
		}
		seen := map[string]bool{}
		for i, h := range c.Hosts {
			p := fmt.Sprintf("hosts[%d]", i)
			normalized, err := normalizeHost(h)
			if err != nil {
				return c.err(p, "expected an exact DNS hostname or IP literal without port")
			}
			if seen[normalized] {
				return c.err(p, "duplicate normalized hostname")
			}
			seen[normalized] = true
			c.Hosts[i] = normalized
		}
	}
	if err := c.validateServer(); err != nil {
		return err
	}
	if !validListener(c.Admin.Listen) {
		return c.err("admin.listen", "expected a valid host:port listener")
	}
	if listenersOverlap(c.Server.Listen, c.Admin.Listen) {
		return c.err("admin.listen", "must not share an address and port with the public listener")
	}
	if !safeBucket(c.S3.Bucket) {
		return c.err("s3.bucket", "required fixed bucket name; templates, paths and ARNs are not allowed")
	}
	if unsupportedBucketSuffix(c.S3.Bucket) {
		return c.err("s3.bucket", "bucket names ending in --x-s3, .mrap, --ol-s3, -s3alias or --table-s3 are not supported")
	}
	if c.present("s3.region") && !regionRE.MatchString(c.S3.Region) {
		return c.err("s3.region", "expected a nonempty signing region of ASCII letters, digits, hyphens and underscores")
	}
	prefix := strings.Trim(c.S3.Prefix, "/")
	if prefix != "" {
		if !safeRelativeKey(prefix) {
			return c.err("s3.prefix", "unsafe publication prefix")
		}
		prefix += "/"
	}
	c.S3.Prefix = prefix
	if len(prefix) > 1024 {
		return c.err("s3.prefix", "exceeds S3 key size")
	}
	if c.present("s3.endpoint") {
		u, err := url.Parse(c.S3.Endpoint)
		if err != nil || strings.Contains(c.S3.Endpoint, "#") || !fixedOrigin(u, true) {
			return c.err("s3.endpoint", "expected an absolute HTTP(S) origin without credentials, query, fragment or non-root path")
		}
		if u.Scheme == "http" && !c.S3.AllowHTTP {
			return c.err("s3.allow_http", "must be true for an HTTP origin")
		}
		if u.Scheme == "http" && c.S3.InsecureSkipVerify {
			return c.err("s3.insecure_skip_verify", "requires an HTTPS origin")
		}
	}
	if c.S3.ResponseHeaderTimeout <= 0 {
		return c.err("s3.response_header_timeout", "must be positive")
	}
	if c.S3.ReadIdleTimeout <= 0 {
		return c.err("s3.read_idle_timeout", "must be positive")
	}
	if c.S3.MaxAttempts < 1 || c.S3.MaxAttempts > 3 {
		return c.err("s3.max_attempts", "must be between 1 and 3")
	}
	if !safeRelativeKey(c.Index) || strings.Contains(c.Index, "/") {
		return c.err("index", "must be a safe filename")
	}
	if err := c.keySize("index", c.Index); err != nil {
		return err
	}
	if c.present("not_found") {
		if !safeRelativeKey(c.NotFound) {
			return c.err("not_found", "must be a safe relative object key")
		}
		if err := c.keySize("not_found", c.NotFound); err != nil {
			return err
		}
	}
	if c.Errors.AccessDenied != "not_found" && c.Errors.AccessDenied != "bad_gateway" {
		return c.err("errors.access_denied", "must be not_found or bad_gateway")
	}
	if err := c.validateHealth(); err != nil {
		return err
	}
	if err := c.validateCache(); err != nil {
		return err
	}
	for i := range c.Redirects {
		p := fmt.Sprintf("redirects[%d]", i)
		if !c.present(p + ".status") {
			return c.err(p+".status", "required redirect status")
		}
		if c.Redirects[i].Query == "" {
			return c.err(p+".query", "must be drop, preserve or unmatched")
		}
	}
	set, err := redirect.Compile(c.Redirects)
	if err != nil {
		// Compile's diagnostics contain an indexed path and a fixed reason, never values.
		path, reason, ok := strings.Cut(err.Error(), ": ")
		if ok {
			return c.err(path, reason)
		}
		return c.err("redirects", "invalid redirect rules")
	}
	c.redirectSet = set
	if err := c.validateHeaders(); err != nil {
		return err
	}
	if !validCacheControl(c.CacheControl.Default) {
		return c.err("cache_control.default", "expected auto or a valid Cache-Control policy")
	}
	for i, r := range c.CacheControl.Rules {
		p := fmt.Sprintf("cache_control.rules[%d]", i)
		if err := c.validateMatch(p+".match", r.Match, false); err != nil {
			return err
		}
		if !validCacheControl(r.Value) {
			return c.err(p+".value", "expected auto or a valid Cache-Control policy")
		}
	}
	if c.CORS != nil {
		if len(c.CORS.AllowedOrigins) == 0 {
			return c.err("cors.allowed_origins", "must contain at least one exact HTTP(S) origin")
		}
		seen := map[string]bool{}
		for i, s := range c.CORS.AllowedOrigins {
			p := fmt.Sprintf("cors.allowed_origins[%d]", i)
			u, err := url.Parse(s)
			if err != nil || strings.Contains(s, "#") || !fixedOrigin(u, false) || u.Path != "" {
				return c.err(p, "expected an exact HTTP(S) origin")
			}
			host, err := normalizeHost(u.Hostname())
			if err != nil {
				return c.err(p, "invalid origin hostname")
			}
			port := u.Port()
			if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
				port = ""
			}
			canonical := u.Scheme + "://" + host
			if strings.Contains(host, ":") {
				canonical = u.Scheme + "://[" + host + "]"
			}
			if port != "" {
				canonical += ":" + port
			}
			if seen[canonical] {
				return c.err(p, "duplicate equivalent origin")
			}
			seen[canonical] = true
		}
	}
	if c.Compression.Mode != "off" && c.Compression.Mode != "precompressed" {
		return c.err("compression.mode", "must be off or precompressed")
	}
	if c.Compression.Mode == "off" && c.present("compression.encodings") {
		return c.err("compression.encodings", "not allowed in off mode")
	}
	if c.Compression.Mode == "precompressed" {
		if len(c.Compression.Encodings) == 0 {
			return c.err("compression.encodings", "must be nonempty")
		}
		seen := map[string]bool{}
		for i, e := range c.Compression.Encodings {
			p := fmt.Sprintf("compression.encodings[%d]", i)
			if (e != "br" && e != "gzip") || seen[e] {
				return c.err(p, "must be a unique br or gzip encoding")
			}
			seen[e] = true
		}
	}
	if s := c.SPA; s != nil {
		if !safeRelativeKey(s.Fallback) {
			return c.err("spa.fallback", "required safe relative fallback key")
		}
		if err := c.keySize("spa.fallback", s.Fallback); err != nil {
			return err
		}
		if c.present("spa.include_prefixes") && len(s.IncludePrefixes) == 0 {
			return c.err("spa.include_prefixes", "must be nonempty when supplied")
		}
		for _, group := range []struct {
			p    string
			list []string
		}{{"spa.include_prefixes", s.IncludePrefixes}, {"spa.exclude_prefixes", s.ExcludePrefixes}} {
			seen := map[string]bool{}
			for i, prefix := range group.list {
				p := fmt.Sprintf("%s[%d]", group.p, i)
				if !canonicalPrefix(prefix) || seen[prefix] {
					return c.err(p, "expected a unique canonical absolute directory prefix")
				}
				seen[prefix] = true
			}
		}
	}
	switch c.Observability.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return c.err("observability.log_level", "must be debug, info, warn or error")
	}
	seen := map[string]bool{}
	for i, name := range c.Observability.RedactQueryParams {
		p := fmt.Sprintf("observability.redact_query_params[%d]", i)
		fold := asciiLower(name)
		if name == "" || !plain(name) || seen[fold] {
			return c.err(p, "expected a nonempty unique parameter name without controls")
		}
		seen[fold] = true
	}
	return nil
}

func (c *Config) validateServer() error {
	s := &c.Server
	if !validListener(s.Listen) {
		return c.err("server.listen", "expected a valid host:port listener")
	}
	if t := s.TLS; t != nil {
		if t.CertFile == "" || !plain(t.CertFile) {
			return c.err("server.tls.cert_file", "required nonempty certificate file path")
		}
		if t.KeyFile == "" || !plain(t.KeyFile) {
			return c.err("server.tls.key_file", "required nonempty private-key file path")
		}
		if t.MinVersion != "1.2" && t.MinVersion != "1.3" {
			return c.err("server.tls.min_version", "must be the string 1.2 or 1.3")
		}
	}
	for i, s := range s.TrustedProxies {
		p := fmt.Sprintf("server.trusted_proxies[%d]", i)
		cidr, err := netip.ParsePrefix(s)
		if err != nil || cidr.Bits() == 0 {
			return c.err(p, "expected a CIDR narrower than a trust-all range")
		}
		if cidr.Addr().Is4In6() {
			return c.err(p, "use native IPv4 CIDRs for IPv4 proxy addresses")
		}
		c.Server.TrustedProxies[i] = cidr.Masked().String()
	}
	s.ClientIPHeader = http.CanonicalHeaderKey(s.ClientIPHeader)
	if s.ClientIPHeader != "X-Forwarded-For" && s.ClientIPHeader != "X-Real-Ip" {
		return c.err("server.client_ip_header", "must be X-Forwarded-For or X-Real-IP")
	}
	if !headerName(s.RequestIDHeader) || forbiddenRequestID(s.RequestIDHeader) {
		return c.err("server.request_id_header", "header is invalid or reserved")
	}
	s.RequestIDHeader = http.CanonicalHeaderKey(s.RequestIDHeader)
	if s.MaxForwardedHops <= 0 || s.MaxForwardedHops > 256 {
		return c.err("server.max_forwarded_hops", "must be between 1 and 256")
	}
	for _, v := range []struct {
		p string
		d time.Duration
	}{{"read_header_timeout", s.ReadHeaderTimeout}, {"response_header_timeout", s.ResponseHeaderTimeout}, {"write_idle_timeout", s.WriteIdleTimeout}, {"idle_timeout", s.IdleTimeout}, {"shutdown_timeout", s.ShutdownTimeout}} {
		if v.d <= 0 {
			return c.err("server."+v.p, "must be positive")
		}
	}
	if s.RequestTimeout < 0 {
		return c.err("server.request_timeout", "must be zero or positive")
	}
	if s.MaxHeaderBytes <= 0 || s.MaxHeaderBytes > 1<<30 {
		return c.err("server.max_header_bytes", "must be between 1 and 1073741824")
	}
	if s.MaxConnections <= 0 {
		return c.err("server.max_connections", "must be positive")
	}
	if s.MaxConcurrentRequests <= 0 {
		return c.err("server.max_concurrent_requests", "must be positive")
	}
	return nil
}

func (c *Config) keySize(p, key string) error {
	if len(c.S3.Prefix)+len(key) > 1024 {
		return c.err(p, "object key including publication prefix exceeds 1024 bytes")
	}
	return nil
}
func (c *Config) validateHealth() error {
	h := c.Admin.OriginHealth
	if h == nil {
		return nil
	}
	if !safeRelativeKey(h.Key) {
		return c.err("admin.origin_health.key", "must be a safe relative object key")
	}
	if err := c.keySize("admin.origin_health.key", h.Key); err != nil {
		return err
	}
	for _, v := range []struct {
		p string
		d time.Duration
	}{{"interval", h.Interval}, {"timeout", h.Timeout}, {"max_staleness", h.MaxStaleness}} {
		if v.d <= 0 {
			return c.err("admin.origin_health."+v.p, "must be positive")
		}
	}
	if h.FailureThreshold <= 0 {
		return c.err("admin.origin_health.failure_threshold", "must be positive")
	}
	if h.SuccessThreshold <= 0 {
		return c.err("admin.origin_health.success_threshold", "must be positive")
	}
	if h.Timeout >= h.Interval {
		return c.err("admin.origin_health.timeout", "must be less than interval")
	}
	if h.MaxStaleness <= h.Interval || h.MaxStaleness-h.Interval <= h.Timeout {
		return c.err("admin.origin_health.max_staleness", "must exceed interval plus timeout")
	}
	return nil
}
func (c *Config) validateCache() error {
	k := c.Cache
	for _, v := range []struct {
		p string
		n int64
	}{{"max_bytes", int64(k.MaxBytes)}, {"max_entries", int64(k.MaxEntries)}, {"max_object_bytes", int64(k.MaxObjectBytes)}, {"max_fill_bytes", int64(k.MaxFillBytes)}, {"max_concurrent_fills", int64(k.MaxConcurrentFills)}, {"max_waiters_per_key", int64(k.MaxWaitersPerKey)}} {
		if v.n <= 0 {
			return c.err("cache."+v.p, "must be positive")
		}
	}
	if k.MaxObjectBytes > k.MaxBytes || k.MaxObjectBytes > k.MaxFillBytes {
		return c.err("cache.max_object_bytes", "must not exceed max_bytes or max_fill_bytes")
	}
	if k.FillTimeout <= 0 {
		return c.err("cache.fill_timeout", "must be positive")
	}
	for _, v := range []struct {
		p string
		d time.Duration
	}{{"ttl", k.TTL}, {"negative_ttl", k.NegativeTTL}, {"stale_if_error", k.StaleIfError}} {
		if v.d < 0 {
			return c.err("cache."+v.p, "must be zero or positive")
		}
	}
	for i, r := range k.Rules {
		p := fmt.Sprintf("cache.rules[%d]", i)
		if err := c.validateMatch(p+".match", r.Match, true); err != nil {
			return err
		}
		for _, v := range []struct {
			p string
			d *time.Duration
		}{{"ttl", r.TTL}, {"negative_ttl", r.NegativeTTL}, {"stale_if_error", r.StaleIfError}} {
			if v.d != nil && *v.d < 0 {
				return c.err(p+"."+v.p, "must be zero or positive")
			}
		}
	}
	return nil
}

func (c *Config) validateMatch(p string, m Match, objectPaths bool) error {
	if c.present(p+".exact") == c.present(p+".prefix") {
		return c.err(p, "require exactly one of exact or prefix")
	}
	if c.present(p + ".exact") {
		if !canonicalPath(m.Exact) {
			return c.err(p+".exact", "expected a canonical absolute object path")
		}
		// Memory cache rules match object paths, which never end in a slash:
		// a directory request resolves to its index object.
		if objectPaths && strings.HasSuffix(m.Exact, "/") {
			return c.err(p+".exact", "object paths never end in /; match the index object instead")
		}
	} else if !canonicalPrefix(m.Prefix) {
		return c.err(p+".prefix", "expected a canonical absolute directory prefix")
	}
	return nil
}

func matches(m Match, p string) bool {
	if m.Exact != "" {
		return m.Exact == p
	}
	return m.Prefix == "/" || p == strings.TrimSuffix(m.Prefix, "/") || strings.HasPrefix(p, m.Prefix)
}

// PolicyFor selects the first matching object rule. Global disablement always wins.
func (c Cache) PolicyFor(path string) CachePolicy {
	p := CachePolicy{c.Enabled, c.TTL, c.NegativeTTL, c.StaleIfError}
	for _, r := range c.Rules {
		if !matches(r.Match, path) {
			continue
		}
		if r.Enabled != nil {
			p.Enabled = c.Enabled && *r.Enabled
		}
		if r.TTL != nil {
			p.TTL = *r.TTL
		}
		if r.NegativeTTL != nil {
			p.NegativeTTL = *r.NegativeTTL
		}
		if r.StaleIfError != nil {
			p.StaleIfError = *r.StaleIfError
		}
		break
	}
	return p
}

// ValueFor selects the first matching public-request HTTP freshness rule.
func (c CacheControl) ValueFor(path string) string {
	for _, r := range c.Rules {
		if matches(r.Match, path) {
			return r.Value
		}
	}
	return c.Default
}

func plain(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func safeRelativeKey(s string) bool {
	if s == "" || !plain(s) || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.Contains(s, "\\") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func canonicalPath(s string) bool {
	if s == "/" {
		return true
	}
	if !strings.HasPrefix(s, "/") || !plain(s) {
		return false
	}
	// Rules use the decoded canonical path, rather than URL syntax. Ordinary
	// percent, question and fragment characters remain valid segment data.
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && isHex(s[i+1]) && isHex(s[i+2]) {
			return false
		}
	}
	return safeRelativeKey(strings.TrimSuffix(s[1:], "/"))
}

func isHex(b byte) bool             { return b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F' }
func canonicalPrefix(s string) bool { return strings.HasSuffix(s, "/") && canonicalPath(s) }

var bucketRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// regionRE is the signing-region syntax the origin client accepts.
var regionRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// unsupportedBucketSuffix reports names the origin client refuses: directory
// buckets, Multi-Region and Object Lambda access points, access point aliases
// and table buckets.
func unsupportedBucketSuffix(s string) bool {
	for _, suffix := range []string{"--x-s3", ".mrap", "--ol-s3", "-s3alias", "--table-s3"} {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

func safeBucket(s string) bool {
	return len(s) <= 255 && bucketRE.MatchString(s) && !strings.Contains(s, ":")
}

func normalizeHost(s string) (string, error) {
	if s == "" || !plain(s) || strings.ContainsAny(s, " /\\?#@[]%") {
		return "", fmt.Errorf("invalid host")
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		return ip.String(), nil
	}
	if strings.Contains(s, ":") {
		return "", fmt.Errorf("ports are not allowed")
	}
	s = strings.TrimSuffix(s, ".")
	// A dotted all-numeric name is an address, never a DNS name: accept it only
	// when it parses as one (192.0.2.010 does not).
	if strings.Contains(s, ".") && strings.Trim(s, "0123456789.") == "" {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return "", fmt.Errorf("invalid IP address")
		}
		return ip.String(), nil
	}
	ascii, err := idna.Lookup.ToASCII(s)
	if err != nil {
		return "", err
	}
	ascii = strings.ToLower(ascii)
	if len(ascii) == 0 || len(ascii) > 253 {
		return "", fmt.Errorf("invalid hostname length")
	}
	for _, label := range strings.Split(ascii, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid hostname label")
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return "", fmt.Errorf("invalid hostname character")
			}
		}
	}
	return ascii, nil
}
func validListener(s string) bool {
	if !plain(s) {
		return false
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	if !canonicalPort(p) {
		return false
	}
	if h == "" {
		return true
	}
	_, err = normalizeHost(h)
	return err == nil
}

// listenersOverlap reports whether two valid listeners would compete for the
// same socket: equal ports and equal hosts, a wildcard host on either side, or
// loopback hosts on both sides.
func listenersOverlap(a, b string) bool {
	ah, ap, _ := net.SplitHostPort(a)
	bh, bp, _ := net.SplitHostPort(b)
	an, _ := strconv.Atoi(ap)
	bn, _ := strconv.Atoi(bp)
	if an != bn {
		return false
	}
	ah, _ = normalizeHost(ah)
	bh, _ = normalizeHost(bh)
	if ah == bh || sameAddress(ah, bh) || wildcardHost(ah) || wildcardHost(bh) {
		return true
	}
	return loopbackHost(ah) && loopbackHost(bh)
}

// sameAddress compares IP hosts after unmapping, so ::ffff:192.0.2.1 equals
// 192.0.2.1.
func sameAddress(a, b string) bool {
	ai, aerr := netip.ParseAddr(a)
	bi, berr := netip.ParseAddr(b)
	return aerr == nil && berr == nil && ai.Unmap() == bi.Unmap()
}

func wildcardHost(h string) bool {
	ip, err := netip.ParseAddr(h)
	return h == "" || err == nil && ip.Unmap().IsUnspecified()
}

func loopbackHost(h string) bool {
	ip, err := netip.ParseAddr(h)
	return h == "localhost" || err == nil && ip.Unmap().IsLoopback()
}

func fixedOrigin(u *url.URL, rootPath bool) bool {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || !plain(u.String()) {
		return false
	}
	if rootPath {
		if u.Path != "" && u.Path != "/" {
			return false
		}
	} else if u.Path != "" {
		return false
	}
	if _, err := normalizeHost(u.Hostname()); err != nil {
		return false
	}
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(u.Hostname())
		if err != nil || !addr.Is6() {
			return false
		}
	}
	p := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if p != "" && !canonicalPort(p) {
		return false
	}
	return u.RawPath == ""
}

// canonicalPort reports whether p is a port in canonical decimal form: 1 to 5
// ASCII digits without a sign or leading zero, from 1 to 65535.
func canonicalPort(p string) bool {
	if len(p) == 0 || len(p) > 5 || p[0] == '0' {
		return false
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return false
		}
	}
	n, _ := strconv.Atoi(p)
	return n <= 65535
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
func headerName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))) {
			return false
		}
	}
	return true
}

var hopHeaders = map[string]bool{"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true, "proxy-connection": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true}

func forbiddenRequestID(s string) bool {
	s = asciiLower(s)
	return hopHeaders[s] || s == "host" || s == "authorization" || s == "www-authenticate" || s == "cookie" || s == "set-cookie" || s == "content-length" || s == "x-forwarded-for" || s == "x-real-ip" || s == "forwarded"
}
func forbiddenResponseHeader(s string) bool {
	s = asciiLower(s)
	if hopHeaders[s] || strings.HasPrefix(s, "access-control-") {
		return true
	}
	switch s {
	case "host", "set-cookie", "location", "x-request-id", "content-length", "content-type", "content-encoding", "content-range", "content-disposition", "content-language", "etag", "last-modified", "accept-ranges", "cache-control", "expires", "age", "date", "vary", "x-content-type-options", "www-authenticate", "authorization", "cookie", "proxy-connection":
		return true
	}
	return false
}
func (c *Config) validateHeaders() error {
	// Check headers in file order so a duplicate names its later spelling.
	names := make([]string, 0, len(c.Headers.Set))
	for name := range c.Headers.Set {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		li, lj := c.nodes[childPath("headers.set", names[i])], c.nodes[childPath("headers.set", names[j])]
		if li.line != lj.line {
			return li.line < lj.line
		}
		if li.column != lj.column {
			return li.column < lj.column
		}
		return names[i] < names[j]
	})
	seen := map[string]bool{}
	normalized := map[string]string{}
	for _, name := range names {
		value := c.Headers.Set[name]
		if !headerName(name) {
			return c.err("headers.set", "invalid response header name")
		}
		p := childPath("headers.set", name)
		if forbiddenResponseHeader(name) {
			return c.err(p, "reserved response header name")
		}
		canonical := http.CanonicalHeaderKey(name)
		if seen[canonical] {
			return c.err(p, "duplicate header after canonicalization")
		}
		seen[canonical] = true
		if value == "" {
			return c.err(p, "header value must not be empty")
		}
		if !validHeaderValue(value) {
			return c.err(p, "header value must not contain control characters other than tab")
		}
		normalized[canonical] = value
		if c.tainted[p] {
			c.tainted[childPath("headers.set", canonical)] = true
		}
	}
	c.Headers.Set = normalized
	return nil
}
func validHeaderValue(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != '\t' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validCacheControl accepts auto or a Cache-Control policy under the grammar
// the response layer and memory cache apply to origin metadata.
func validCacheControl(s string) bool { return s == "auto" || httpcache.Valid(s) }
