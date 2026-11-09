// Package httpserver implements the public HTTP admission and identity boundary.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/redirect"
	"github.com/gtriggiano/statica/internal/site"
	"github.com/gtriggiano/statica/internal/telemetry"
	"golang.org/x/net/idna"
)

type ObservationEvent string

const (
	ObservationStart  ObservationEvent = "start"
	ObservationFinish ObservationEvent = "finish"
	ObservationReject ObservationEvent = "reject"
)

// Observation deliberately excludes raw targets, rejected authorities, IPs and
// correlation IDs. Reason is a fixed classification; Host is set only after
// admission. Inflight is a process-local snapshot of admitted requests.
type Observation struct {
	Event    ObservationEvent
	Host     string
	Status   int
	Reason   string
	Inflight int64
}

type HandlerOption func(*handler)

// WithObserver installs a synchronous observer. It must be concurrency-safe,
// prompt and must not panic. Each acquired slot emits start/finish; a rejection
// also emits reject, including overload requests which never acquire a slot.
func WithObserver(observer func(Observation)) HandlerOption {
	return func(h *handler) { h.observe = observer }
}

// WithRecordSink makes the handler create a telemetry.Record for each
// request, carried in the request context for the content handler, and
// deliver it once, finalized, to sink: for an admitted request after its
// finish Observation, otherwise (overload) after its reject Observation,
// always after the request slot is released and before a panic propagates.
// The record carries targets, addresses and request IDs, which never reach
// the observer. sink must be concurrency-safe, prompt, must not panic and
// must not retain the record.
func WithRecordSink(sink func(*telemetry.Record)) HandlerOption {
	return func(h *handler) { h.sink = sink }
}

type handler struct {
	next                          http.Handler
	hosts                         map[string]bool
	identity                      identityPolicy
	redirects                     *redirect.Set
	mapper                        *site.Mapper
	static                        map[string]string
	cors                          corsPolicy
	slots                         chan struct{}
	inflight                      atomic.Int64
	observe                       func(Observation)
	sink                          func(*telemetry.Record)
	preparation, writeIdle, total time.Duration
}

// NewHandler constructs an immutable runtime snapshot from validated
// configuration. next is the content boundary (the content handler in
// production), and is required; it can obtain the safe initial object
// candidate using MetadataFromContext.
func NewHandler(cfg *config.Config, next http.Handler, options ...HandlerOption) (http.Handler, error) {
	if cfg == nil || next == nil {
		return nil, fmt.Errorf("HTTP handler requires configuration and content handler")
	}
	if cfg.Server.MaxConcurrentRequests <= 0 || cfg.Server.MaxForwardedHops <= 0 || cfg.Server.MaxForwardedHops > 256 {
		return nil, fmt.Errorf("invalid HTTP request limits")
	}
	mapper, err := site.NewMapper(cfg.S3.Prefix, cfg.Index)
	if err != nil {
		return nil, fmt.Errorf("HTTP object mapper: %w", err)
	}
	redirects, err := redirect.Compile(cfg.Redirects)
	if err != nil {
		return nil, err
	}
	h := &handler{next: next, mapper: mapper, redirects: redirects,
		hosts: make(map[string]bool, len(cfg.Hosts)), static: make(map[string]string, len(cfg.Headers.Set)),
		slots:    make(chan struct{}, cfg.Server.MaxConcurrentRequests),
		identity: identityPolicy{clientHeader: cfg.Server.ClientIPHeader, requestIDHeader: cfg.Server.RequestIDHeader, maxHops: cfg.Server.MaxForwardedHops}}
	h.preparation, h.writeIdle, h.total = cfg.Server.ResponseHeaderTimeout, cfg.Server.WriteIdleTimeout, cfg.Server.RequestTimeout
	if !strings.EqualFold(h.identity.clientHeader, "X-Forwarded-For") && !strings.EqualFold(h.identity.clientHeader, "X-Real-IP") {
		return nil, fmt.Errorf("invalid client IP header")
	}
	if h.identity.requestIDHeader == "" {
		return nil, fmt.Errorf("invalid request ID header")
	}
	for _, raw := range cfg.Hosts {
		host := raw
		if ip, err := netip.ParseAddr(raw); err == nil {
			host = ip.String()
		} else {
			host, err = normalizeAuthority(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid configured host")
			}
		}
		h.hosts[host] = true
	}
	for _, raw := range cfg.Server.TrustedProxies {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || prefix.Addr().Is4In6() {
			return nil, fmt.Errorf("invalid trusted proxy CIDR")
		}
		h.identity.trusted = append(h.identity.trusted, prefix.Masked())
	}
	for key, value := range cfg.Headers.Set {
		h.static[key] = value
	}
	if cfg.CORS != nil {
		h.cors = corsPolicy{enabled: true, origins: make(map[string]bool, len(cfg.CORS.AllowedOrigins))}
		for _, raw := range cfg.CORS.AllowedOrigins {
			// Configured origins support IDNA; raw visitor origins do not gain
			// an implicit Unicode transformation.
			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid configured CORS origin")
			}
			host := u.Hostname()
			if _, ipErr := netip.ParseAddr(host); ipErr != nil {
				host, err = idna.Lookup.ToASCII(host)
				if err != nil {
					return nil, fmt.Errorf("invalid configured CORS origin")
				}
			}
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			if u.Port() != "" {
				host += ":" + u.Port()
			}
			u.Host = host
			canonical, err := canonicalOrigin(u.String())
			if err != nil {
				return nil, fmt.Errorf("invalid configured CORS origin")
			}
			h.cors.origins[canonical] = true
		}
	}
	for _, option := range options {
		if option != nil {
			option(h)
		}
	}
	return h, nil
}

func (h *handler) emit(event ObservationEvent, host string, status int, reason string) {
	if h.observe != nil {
		h.observe(Observation{Event: event, Host: host, Status: status, Reason: reason, Inflight: h.inflight.Load()})
	}
}

// deliver finalizes record from the response and hands it to the sink.
// reason is the boundary classification, which never replaces a rejection
// reason; aborted reports a response that did not complete.
func (h *handler) deliver(record *telemetry.Record, r *http.Request, response *responseWriter, reason string, aborted bool) {
	if record == nil {
		return
	}
	record.Finished = time.Now()
	record.Status = response.status
	record.FirstByte = response.committed
	record.BytesWritten = response.written
	if !record.Rejected {
		record.Reason = reason
	}
	if record.TerminationReason == "" {
		record.TerminationReason = response.generatedReason
	}
	// Rejections have no outcome: metrics count them by reason only.
	switch {
	case record.Rejected:
	case aborted:
		record.Outcome = telemetry.OutcomeIncomplete
	case record.Outcome == "":
		record.Outcome = telemetry.OutcomeSuccess
	}
	if record.Host == "" {
		record.Authority = r.Host
	}
	if record.EscapedPath == "" {
		// A request rejected before target validation logs its target
		// only when the same validation accepts it.
		if target, err := site.ParseTarget(r); err == nil {
			record.EscapedPath, record.RawQuery, record.ForceQuery = target.EscapedPath, target.RawQuery, target.ForceQuery
		}
	}
	h.sink(record)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	metadata := h.identity.resolve(r)
	started := time.Now()
	response := &responseWriter{ResponseWriter: w, head: r.Method == http.MethodHead,
		requestID: metadata.RequestID, static: h.static, cors: make(http.Header),
		generated: generatedBudget{preparation: started.Add(h.preparation), writeIdle: h.writeIdle}}
	if h.total > 0 {
		response.generated.total = started.Add(h.total)
	}
	if deadline, ok := r.Context().Deadline(); ok && (response.generated.total.IsZero() || deadline.Before(response.generated.total)) {
		response.generated.total = deadline
	}
	out := preserveCapabilities(response)
	var record *telemetry.Record
	if h.sink != nil {
		record = &telemetry.Record{Method: r.Method, Started: started, RequestID: metadata.RequestID,
			RequestIDSource: metadata.RequestIDSource, RequestIDReason: metadata.RequestIDReason,
			ClientIP: metadata.ClientIP, PeerIP: metadata.PeerIP, ClientIPSource: metadata.ClientIPSource,
			AttributionReason: metadata.AttributionReason}
	}
	acquired := false
	reason := ""
	reject := func(status int, classification string) {
		reason = classification
		if record != nil {
			record.Rejected, record.Reason = true, classification
		}
		stopRequestBody(out, r)
		completed := false
		defer func() {
			actual := response.status
			if actual == 0 {
				actual = status
			}
			h.emit(ObservationReject, metadata.Host, actual, classification)
			if !acquired {
				h.deliver(record, r, response, classification, !completed)
			}
		}()
		generatedResponse(out, r, status)
		completed = true
	}
	select {
	case h.slots <- struct{}{}:
		h.inflight.Add(1)
		acquired = true
		metadata.AdmittedAt = time.Now()
		response.generated.preparation = metadata.AdmittedAt.Add(h.preparation)
		if record != nil {
			record.Admitted = metadata.AdmittedAt
		}
	default:
		reject(http.StatusServiceUnavailable, "request_capacity")
		return
	}
	finished := false
	defer func() {
		panicValue := recover()
		// Release before invoking user-supplied observation code, including on
		// a panic from the content handler or a cancelled request.
		h.inflight.Add(-1)
		<-h.slots
		status := response.status
		if !finished {
			// An aborted redirect or preflight keeps its classification;
			// the abort itself is the aborted flag and generated reason.
			switch {
			case panicValue != http.ErrAbortHandler:
				reason = "handler_panic"
			case reason == "":
				reason = "response_aborted"
			}
		}
		classification := reason
		if response.generatedReason != "" {
			reason = response.generatedReason
		}
		h.emit(ObservationFinish, metadata.Host, status, reason)
		h.deliver(record, r, response, classification, !finished)
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	h.emit(ObservationStart, "", 0, "")
	host, err := normalizeAuthority(r.Host)
	if err != nil {
		reject(http.StatusBadRequest, "malformed_host")
		finished = true
		return
	}
	if len(h.hosts) != 0 && !h.hosts[host] {
		reject(http.StatusMisdirectedRequest, "unknown_host")
		finished = true
		return
	}
	if record != nil {
		record.Host = host
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodOptions && h.cors.enabled) {
		allow := "GET, HEAD"
		if h.cors.enabled {
			allow += ", OPTIONS"
		}
		out.Header().Set("Allow", allow)
		reject(http.StatusMethodNotAllowed, "unsupported_method")
		finished = true
		return
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && hasRequestBody(r) {
		reject(http.StatusBadRequest, "request_body")
		finished = true
		return
	}
	target, err := site.ParseTarget(r)
	if err != nil {
		status := http.StatusBadRequest
		var input *site.Error
		if errors.As(err, &input) {
			status = input.Status
		}
		reject(status, "invalid_target")
		finished = true
		return
	}
	metadata.Host = host
	metadata.Target = target
	if record != nil {
		record.EscapedPath, record.RawQuery, record.ForceQuery = target.EscapedPath, target.RawQuery, target.ForceQuery
	}
	status, corsReason := h.cors.apply(response.cors, r)
	if status != 0 {
		if status == http.StatusForbidden {
			reject(status, corsReason)
		} else {
			reason = corsReason
			stopRequestBody(out, r)
			generatedResponse(out, r, status)
		}
		finished = true
		return
	}
	result, matched, err := h.redirects.Resolve(target.EscapedPath, target.RawQuery, target.ForceQuery)
	if err != nil {
		status := http.StatusBadRequest
		var input *redirect.RequestError
		if errors.As(err, &input) {
			status = input.Status
		}
		reject(status, "invalid_redirect_input")
		finished = true
		return
	}
	if matched {
		out.Header().Set("Location", result.Location)
		reason = "redirect"
		generatedResponse(out, r, result.Status)
		finished = true
		return
	}
	object, err := h.mapper.Map(target.Path)
	if err != nil {
		status := http.StatusBadRequest
		var input *site.Error
		if errors.As(err, &input) {
			status = input.Status
		}
		reject(status, "invalid_object_key")
		finished = true
		return
	}
	metadata.Object = &object
	ctx := context.WithValue(r.Context(), metadataKey{}, metadata)
	if record != nil {
		ctx = telemetry.WithRecord(ctx, record)
	}
	r = r.WithContext(ctx)
	h.next.ServeHTTP(out, r)
	if response.status == 0 {
		response.WriteHeader(http.StatusOK)
	}
	finished = true
}
