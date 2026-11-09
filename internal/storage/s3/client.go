// Package s3 implements the storage boundary with AWS SDK for Go v2. It issues
// only GetObject and HeadObject, using one configured destination and namespace.
package s3

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go/logging"
	"github.com/aws/smithy-go/middleware"
	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}
type options struct {
	credentials aws.CredentialsProvider
	client      HTTPClient
	observer    Observer
}
type Option func(*options)

// WithCredentials supplies a refreshable credential provider. The normal
// constructor uses the SDK's standard chain, never static application secrets.
func WithCredentials(v aws.CredentialsProvider) Option { return func(o *options) { o.credentials = v } }

// WithHTTPClient replaces only the origin HTTP client (useful for transport
// instrumentation and tests). Credential discovery always uses verified TLS.
func WithHTTPClient(v HTTPClient) Option { return func(o *options) { o.client = v } }

// WithObserver reports logical operations, transport attempts and body bytes
// to v. Without it, or with a nil v, the adapter does no telemetry work.
func WithObserver(v Observer) Option { return func(o *options) { o.observer = v } }

type Client struct {
	sdk                 *awss3.Client
	cfg                 config.S3
	transport           *http.Transport
	credentialTransport *http.Transport
	region              string
	observer            Observer
}

var _ storage.Reader = (*Client)(nil)
var bucketPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,254}$`)
var regionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func New(ctx context.Context, cfg config.S3, opts ...Option) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, classify(ctx, err, false)
	}
	if !validConfig(cfg) {
		return nil, &storage.Error{Kind: storage.Configuration, Size: -1}
	}
	o := options{}
	for _, apply := range opts {
		apply(&o)
	}
	// Two private transports prevent the origin's optional TLS exception from
	// ever weakening credential-provider, IMDS, or STS traffic.
	credentialTransport := privateTransport(cfg.ResponseHeaderTimeout)
	credentialTransport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	credentialTransport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	credentialClient := &http.Client{Transport: credentialTransport, CheckRedirect: noRedirect}
	load := []func(*awsconfig.LoadOptions) error{awsconfig.WithHTTPClient(credentialClient), awsconfig.WithLogger(logging.Nop{})}
	if cfg.Region != "" {
		load = append(load, awsconfig.WithRegion(cfg.Region))
	}
	if o.credentials != nil {
		load = append(load, awsconfig.WithCredentialsProvider(o.credentials))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, load...)
	if err != nil {
		credentialTransport.CloseIdleConnections()
		return nil, &storage.Error{Kind: storage.Configuration, Size: -1}
	}
	if !regionPattern.MatchString(awsCfg.Region) {
		credentialTransport.CloseIdleConnections()
		return nil, &storage.Error{Kind: storage.Configuration, Size: -1}
	}
	// ConfigSources includes ambient endpoint overrides. Credential providers
	// have already been built; remove only the service's later resolution input.
	awsCfg.ConfigSources = nil
	awsCfg.BaseEndpoint = nil
	awsCfg.EndpointResolver = nil
	awsCfg.EndpointResolverWithOptions = nil
	transport := privateTransport(cfg.ResponseHeaderTimeout)
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = cfg.ResponseHeaderTimeout
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.InsecureSkipVerify} // explicit operator opt-out, origin only
	originClient := HTTPClient(&http.Client{Transport: transport, CheckRedirect: noRedirect})
	if o.client != nil {
		originClient = o.client
	}
	observer := o.observer
	sdk := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.HTTPClient = originClient
		o.UsePathStyle = cfg.PathStyle
		o.DisableS3ExpressSessionAuth = aws.Bool(true)
		o.DisableMultiRegionAccessPoints = true
		o.UseARNRegion = false
		o.UseAccelerate = false
		o.BaseEndpoint = nil
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.EndpointResolver = nil
		o.EndpointResolverV2 = awss3.NewDefaultEndpointResolverV2()
		o.Logger = logging.Nop{}
		o.ClientLogMode = 0
		o.DisableLogOutputChecksumValidationSkipped = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		if cfg.Features.Checksums {
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenSupported
		}
		o.Retryer = retry.NewStandard(func(r *retry.StandardOptions) { r.MaxAttempts = cfg.MaxAttempts; r.MaxBackoff = 500 * time.Millisecond })
		if observer != nil {
			o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
				return stack.Finalize.Add(attemptObserver{observer}, middleware.After)
			})
		}
	})
	return &Client{sdk: sdk, cfg: cfg, transport: transport, credentialTransport: credentialTransport, region: awsCfg.Region, observer: observer}, nil
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// privateTransport builds a transport whose connection setup, the dial and
// the TLS handshake each, takes at most limit, the response header timeout:
// an unreachable origin then fails like a slow one.
func privateTransport(limit time.Duration) *http.Transport {
	return &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: min(30*time.Second, limit), KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: min(10*time.Second, limit), ExpectContinueTimeout: time.Second, MaxResponseHeaderBytes: 1 << 20}
}
func (c *Client) Region() string { return c.region }
func (c *Client) Capabilities() storage.Capabilities {
	return storage.Capabilities{ConditionalReads: c.cfg.Features.ConditionalReads, RangeReads: c.cfg.Features.RangeReads, Checksums: c.cfg.Features.Checksums}
}
func (c *Client) Close() error {
	c.transport.CloseIdleConnections()
	c.credentialTransport.CloseIdleConnections()
	return nil
}

func validConfig(c config.S3) bool {
	if !bucketPattern.MatchString(c.Bucket) || c.ResponseHeaderTimeout <= 0 || c.ReadIdleTimeout <= 0 || c.MaxAttempts < 1 || c.MaxAttempts > 3 {
		return false
	}
	if strings.HasSuffix(c.Bucket, "--x-s3") || strings.HasSuffix(c.Bucket, ".mrap") || strings.HasSuffix(c.Bucket, "--ol-s3") || strings.HasSuffix(c.Bucket, "-s3alias") || strings.HasSuffix(c.Bucket, "--table-s3") {
		return false
	}
	if c.Region != "" && !regionPattern.MatchString(c.Region) {
		return false
	}
	if c.Prefix != "" && (!strings.HasSuffix(c.Prefix, "/") || !validKey(strings.TrimSuffix(c.Prefix, "/"), "")) {
		return false
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.Endpoint, "#") || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && u.Scheme != "http") {
			return false
		}
		if u.Scheme == "http" && (!c.AllowHTTP || c.InsecureSkipVerify) {
			return false
		}
	}
	return true
}

func (c *Client) Get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	if !validKey(key, c.cfg.Prefix) {
		return nil, &storage.Error{Kind: storage.InvalidKey, Size: -1}
	}
	if (!c.cfg.Features.RangeReads && opts.Range != "") || (!c.cfg.Features.ConditionalReads && (opts.IfMatch != "" || opts.IfNoneMatch != "")) {
		return nil, &storage.Error{Kind: storage.Unsupported, Size: -1}
	}
	if strings.ContainsAny(opts.Range+opts.IfMatch+opts.IfNoneMatch, "\r\n\x00") {
		return nil, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	if c.observer == nil {
		return c.get(ctx, key, opts)
	}
	start := time.Now()
	obj, err := c.get(ctx, key, opts)
	c.observer.ObserveOperation(OperationGet, storage.PurposeFrom(ctx), kindOf(err), time.Since(start))
	return obj, err
}

// get performs a validated Get.
func (c *Client) get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	opCtx, cancel := context.WithCancelCause(ctx)
	in := &awss3.GetObjectInput{Bucket: aws.String(c.cfg.Bucket), Key: aws.String(key)}
	if opts.Range != "" {
		in.Range = aws.String(opts.Range)
	}
	if opts.IfMatch != "" {
		in.IfMatch = aws.String(opts.IfMatch)
	}
	if opts.IfNoneMatch != "" {
		in.IfNoneMatch = aws.String(opts.IfNoneMatch)
	}
	if c.cfg.Features.Checksums {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	out, err := c.sdk.GetObject(opCtx, in)
	if err != nil {
		mapped := classify(opCtx, err, false)
		cancel(context.Canceled)
		return nil, mapped
	}
	meta, err := metadata(out.ResultMetadata)
	if err == nil && opts.Range == "" && meta.ContentRange != "" {
		err = &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	if err != nil || out.Body == nil {
		cancel(context.Canceled)
		if out.Body != nil {
			_ = out.Body.Close()
		}
		if err == nil {
			err = &storage.Error{Kind: storage.InvalidResponse, Size: -1}
		}
		return nil, err
	}
	if v, ok := awss3.GetChecksumValidationMetadata(out.ResultMetadata); ok && len(v.AlgorithmsUsed) > 0 {
		meta.Checksum = storage.ChecksumPending
	}
	obj := &storage.Object{Metadata: meta}
	b := &body{source: out.Body, ctx: opCtx, cancel: cancel, idle: c.cfg.ReadIdleTimeout, meta: &obj.Metadata, observer: c.observer}
	if c.observer != nil {
		b.SetPurpose(storage.PurposeFrom(ctx))
	}
	obj.Body = b
	return obj, nil
}

func (c *Client) Head(ctx context.Context, key string) (storage.Metadata, error) {
	if !validKey(key, c.cfg.Prefix) {
		return storage.Metadata{}, &storage.Error{Kind: storage.InvalidKey, Size: -1}
	}
	if c.observer == nil {
		return c.head(ctx, key)
	}
	start := time.Now()
	m, err := c.head(ctx, key)
	c.observer.ObserveOperation(OperationHead, storage.PurposeFrom(ctx), kindOf(err), time.Since(start))
	return m, err
}

// head performs a validated Head.
func (c *Client) head(ctx context.Context, key string) (storage.Metadata, error) {
	in := &awss3.HeadObjectInput{Bucket: aws.String(c.cfg.Bucket), Key: aws.String(key)}
	if c.cfg.Features.Checksums {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	out, err := c.sdk.HeadObject(ctx, in)
	if err != nil {
		return storage.Metadata{}, classify(ctx, err, true)
	}
	m, err := metadata(out.ResultMetadata)
	if err == nil && m.ContentRange != "" {
		return storage.Metadata{}, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	return m, err
}
