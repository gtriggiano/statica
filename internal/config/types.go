// Package config loads and validates the offline, single-site configuration.
package config

import (
	"regexp"
	"time"

	"github.com/gtriggiano/statica/internal/redirect"
)

type LookupEnv func(string) (string, bool)

// ByteSize is an integral number of bytes, expressed in YAML using binary units.
type ByteSize int64

type Config struct {
	Hosts         []string        `yaml:"hosts,omitempty"`
	Server        Server          `yaml:"server"`
	Admin         Admin           `yaml:"admin"`
	S3            S3              `yaml:"s3"`
	Cache         Cache           `yaml:"cache"`
	Index         string          `yaml:"index"`
	NotFound      string          `yaml:"not_found,omitempty"`
	Errors        Errors          `yaml:"errors"`
	Redirects     []redirect.Rule `yaml:"redirects,omitempty"`
	Headers       Headers         `yaml:"headers"`
	CacheControl  CacheControl    `yaml:"cache_control"`
	CORS          *CORS           `yaml:"cors,omitempty"`
	Compression   Compression     `yaml:"compression"`
	SPA           *SPA            `yaml:"spa,omitempty"`
	Observability Observability   `yaml:"observability"`
	tainted       map[string]bool
	nodes         map[string]location
	redirectSet   *redirect.Set
}

type Server struct {
	Listen                string        `yaml:"listen"`
	TLS                   *TLS          `yaml:"tls,omitempty"`
	TrustedProxies        []string      `yaml:"trusted_proxies"`
	ClientIPHeader        string        `yaml:"client_ip_header"`
	RequestIDHeader       string        `yaml:"request_id_header"`
	MaxForwardedHops      int           `yaml:"max_forwarded_hops"`
	ReadHeaderTimeout     time.Duration `yaml:"read_header_timeout"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	WriteIdleTimeout      time.Duration `yaml:"write_idle_timeout"`
	IdleTimeout           time.Duration `yaml:"idle_timeout"`
	RequestTimeout        time.Duration `yaml:"request_timeout"`
	ShutdownTimeout       time.Duration `yaml:"shutdown_timeout"`
	MaxHeaderBytes        int           `yaml:"max_header_bytes"`
	MaxConnections        int           `yaml:"max_connections"`
	MaxConcurrentRequests int           `yaml:"max_concurrent_requests"`
}

type TLS struct {
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	MinVersion string `yaml:"min_version"`
}

type Admin struct {
	Listen       string        `yaml:"listen"`
	OriginHealth *OriginHealth `yaml:"origin_health,omitempty"`
}

type OriginHealth struct {
	Enabled          bool          `yaml:"enabled"`
	Key              string        `yaml:"key"`
	Interval         time.Duration `yaml:"interval"`
	Timeout          time.Duration `yaml:"timeout"`
	FailureThreshold int           `yaml:"failure_threshold"`
	SuccessThreshold int           `yaml:"success_threshold"`
	MaxStaleness     time.Duration `yaml:"max_staleness"`
}

type S3 struct {
	Bucket                string        `yaml:"bucket"`
	Region                string        `yaml:"region,omitempty"`
	Prefix                string        `yaml:"prefix"`
	Endpoint              string        `yaml:"endpoint,omitempty"`
	PathStyle             bool          `yaml:"path_style"`
	AllowHTTP             bool          `yaml:"allow_http"`
	InsecureSkipVerify    bool          `yaml:"insecure_skip_verify"`
	ResponseHeaderTimeout time.Duration `yaml:"response_header_timeout"`
	ReadIdleTimeout       time.Duration `yaml:"read_idle_timeout"`
	MaxAttempts           int           `yaml:"max_attempts"`
	Features              Features      `yaml:"features"`
}

type Features struct {
	ConditionalReads bool `yaml:"conditional_reads"`
	RangeReads       bool `yaml:"range_reads"`
	Checksums        bool `yaml:"checksums"`
}

type Cache struct {
	Enabled            bool          `yaml:"enabled"`
	MaxBytes           ByteSize      `yaml:"max_bytes"`
	MaxEntries         int           `yaml:"max_entries"`
	MaxObjectBytes     ByteSize      `yaml:"max_object_bytes"`
	MaxFillBytes       ByteSize      `yaml:"max_fill_bytes"`
	MaxConcurrentFills int           `yaml:"max_concurrent_fills"`
	MaxWaitersPerKey   int           `yaml:"max_waiters_per_key"`
	FillTimeout        time.Duration `yaml:"fill_timeout"`
	TTL                time.Duration `yaml:"ttl"`
	TTLMode            string        `yaml:"ttl_mode"`
	NegativeTTL        time.Duration `yaml:"negative_ttl"`
	StaleIfError       time.Duration `yaml:"stale_if_error"`
	RequestDirectives  string        `yaml:"request_directives"`
	Rules              []CacheRule   `yaml:"rules,omitempty"`
}

// The values of cache.ttl_mode: the role of the TTL against the freshness
// lifetime the origin assigns, where an origin no-cache assigns zero. The TTL
// applies alone when the origin assigns none. TTLModeCeiling caps the origin's
// lifetime at the TTL, TTLModeFloor raises it to the TTL, TTLModeFallback
// keeps it. With TTLModeFixed the origin's freshness is ignored altogether:
// the TTL alone decides, counted from the response time.
const (
	TTLModeCeiling  = "ceiling"
	TTLModeFloor    = "floor"
	TTLModeFallback = "fallback"
	TTLModeFixed    = "fixed"
)

// The values of cache.request_directives: which request Cache-Control
// directives the memory cache applies. RequestDirectivesHonor applies
// no-store, no-cache and max-age; RequestDirectivesRevalidateOnly drops
// no-store, so a client can force a revalidation but never a bypass;
// RequestDirectivesIgnore drops all three.
const (
	RequestDirectivesHonor          = "honor"
	RequestDirectivesRevalidateOnly = "revalidate_only"
	RequestDirectivesIgnore         = "ignore"
)

// Match selects paths with exactly one of Exact, Prefix or Regex. Validation
// compiles Regex, anchored at both ends, into re; the compiled expression is
// shared by copies of the Match and never appears in YAML output.
type Match struct {
	Exact  string `yaml:"exact,omitempty"`
	Prefix string `yaml:"prefix,omitempty"`
	Regex  string `yaml:"regex,omitempty"`
	re     *regexp.Regexp
}

type CacheRule struct {
	Match             Match          `yaml:"match"`
	Enabled           *bool          `yaml:"enabled,omitempty"`
	TTL               *time.Duration `yaml:"ttl,omitempty"`
	TTLMode           *string        `yaml:"ttl_mode,omitempty"`
	NegativeTTL       *time.Duration `yaml:"negative_ttl,omitempty"`
	StaleIfError      *time.Duration `yaml:"stale_if_error,omitempty"`
	RequestDirectives *string        `yaml:"request_directives,omitempty"`
}

// CachePolicy is the effective memory cache policy of one object. An empty
// TTLMode means TTLModeCeiling and an empty RequestDirectives means
// RequestDirectivesHonor, so a zero-valued mode keeps the defaults.
type CachePolicy struct {
	Enabled           bool
	TTL               time.Duration
	TTLMode           string
	NegativeTTL       time.Duration
	StaleIfError      time.Duration
	RequestDirectives string
}

type Errors struct {
	AccessDenied string `yaml:"access_denied"`
}
type Headers struct {
	Set map[string]string `yaml:"set"`
}
type CacheControl struct {
	Default string             `yaml:"default"`
	Rules   []CacheControlRule `yaml:"rules,omitempty"`
}
type CacheControlRule struct {
	Match Match  `yaml:"match"`
	Value string `yaml:"value"`
}
type CORS struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}
type Compression struct {
	Mode      string   `yaml:"mode"`
	Encodings []string `yaml:"encodings,omitempty"`
}
type SPA struct {
	Fallback        string   `yaml:"fallback"`
	IncludePrefixes []string `yaml:"include_prefixes,omitempty"`
	ExcludePrefixes []string `yaml:"exclude_prefixes,omitempty"`
}
type Observability struct {
	LogLevel          string   `yaml:"log_level"`
	AccessLog         bool     `yaml:"access_log"`
	LogClientIP       bool     `yaml:"log_client_ip"`
	RedactQueryParams []string `yaml:"redact_query_params"`
}

func defaults() *Config {
	return &Config{
		Server: Server{Listen: "127.0.0.1:8080", ClientIPHeader: "X-Forwarded-For", RequestIDHeader: "X-Request-ID", MaxForwardedHops: 16, ReadHeaderTimeout: 5 * time.Second, ResponseHeaderTimeout: 30 * time.Second, WriteIdleTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, ShutdownTimeout: 30 * time.Second, MaxHeaderBytes: 32768, MaxConnections: 1024, MaxConcurrentRequests: 256, TrustedProxies: []string{}},
		Admin:  Admin{Listen: "127.0.0.1:9090"},
		S3:     S3{ResponseHeaderTimeout: 10 * time.Second, ReadIdleTimeout: 30 * time.Second, MaxAttempts: 2, Features: Features{ConditionalReads: true, RangeReads: true}},
		Cache:  Cache{Enabled: true, MaxBytes: 128 << 20, MaxEntries: 10000, MaxObjectBytes: 2 << 20, MaxFillBytes: 32 << 20, MaxConcurrentFills: 16, MaxWaitersPerKey: 64, FillTimeout: 30 * time.Second, TTL: 5 * time.Minute, TTLMode: TTLModeCeiling, NegativeTTL: 5 * time.Second, RequestDirectives: RequestDirectivesHonor},
		Index:  "index.html", Errors: Errors{AccessDenied: "not_found"}, Headers: Headers{Set: map[string]string{}}, CacheControl: CacheControl{Default: "auto"}, Compression: Compression{Mode: "off"},
		Observability: Observability{LogLevel: "info", AccessLog: true, LogClientIP: true, RedactQueryParams: []string{"token", "access_token", "api_key", "password", "secret", "signature", "X-Amz-Credential", "X-Amz-Signature", "X-Amz-Security-Token"}},
		tainted:       map[string]bool{}, nodes: map[string]location{},
	}
}

// CompiledRedirects returns the validated redirect set; it performs no I/O.
func (c *Config) CompiledRedirects() *redirect.Set { return c.redirectSet }
