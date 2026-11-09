package site

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
)

// Resolution describes why the selected object was used, independently of its
// public HTTP status and stored representation.
type Resolution string

const (
	ResolutionOrdinary Resolution = "ordinary"
	ResolutionIndex    Resolution = "index"
	ResolutionSPA      Resolution = "spa"
	ResolutionNotFound Resolution = "not_found"
)

type Request struct {
	Target Target
	Method string
	Header http.Header
}

// Selection owns Body, if nonnil. The caller must close it. Metadata and Body
// come from the same origin snapshot. A built-in 404 has an empty Key. A redirect
// has a Location and no Body. ObjectPath identifies the actual stored variant;
// LogicalPath identifies the resource before its coding suffix.
type Selection struct {
	Key, ObjectPath, LogicalPath, Coding, ContentType string
	Metadata                                          storage.Metadata
	Body                                              io.ReadCloser
	Resolution                                        Resolution
	Status                                            int
	Location                                          string
	VaryAccept, VaryEncoding, RangeCapable            bool
}

// Resolver is immutable and safe for concurrent requests. It performs no cache
// admission and never reads body bytes, including during HEAD disambiguation.
type Resolver struct {
	mapper     *Mapper
	reader     storage.Reader
	mode       string
	encodings  []string
	notFound   string
	spa        *config.SPA
	rangeReads bool
}

func NewResolver(cfg *config.Config, reader storage.Reader) (*Resolver, error) {
	if cfg == nil || reader == nil {
		return nil, errors.New("site resolver: configuration and reader required")
	}
	mapper, err := NewMapper(cfg.S3.Prefix, cfg.Index)
	if err != nil {
		return nil, err
	}
	mode := cfg.Compression.Mode
	if mode == "" {
		mode = "off"
	}
	if mode != "off" && mode != "precompressed" {
		return nil, errors.New("site resolver: invalid compression mode")
	}
	encodings := append([]string(nil), cfg.Compression.Encodings...)
	if mode == "off" && len(encodings) != 0 {
		return nil, errors.New("site resolver: encodings require precompressed mode")
	}
	if mode == "precompressed" && len(encodings) == 0 {
		encodings = []string{"br", "gzip"}
	}
	seen := map[string]bool{}
	for _, coding := range encodings {
		if coding != "br" && coding != "gzip" || seen[coding] {
			return nil, errors.New("site resolver: invalid encoding order")
		}
		seen[coding] = true
	}
	r := &Resolver{mapper: mapper, reader: reader, mode: mode, encodings: encodings, notFound: cfg.NotFound, rangeReads: cfg.S3.Features.RangeReads}
	for _, key := range []string{cfg.NotFound} {
		if key != "" && (!relativeKey(key) || strings.HasSuffix(key, "/")) {
			return nil, errors.New("site resolver: invalid fallback key")
		}
		if key != "" {
			if _, err := r.configuredObject(key); err != nil {
				return nil, err
			}
		}
	}
	if cfg.SPA != nil {
		cp := *cfg.SPA
		if cp.IncludePrefixes != nil && len(cp.IncludePrefixes) == 0 {
			return nil, errors.New("site resolver: empty SPA includes")
		}
		cp.IncludePrefixes = append([]string(nil), cp.IncludePrefixes...)
		cp.ExcludePrefixes = append([]string(nil), cp.ExcludePrefixes...)
		if !relativeKey(cp.Fallback) {
			return nil, errors.New("site resolver: invalid SPA fallback")
		}
		if _, err := r.configuredObject(cp.Fallback); err != nil {
			return nil, err
		}
		for _, group := range [][]string{cp.IncludePrefixes, cp.ExcludePrefixes} {
			for _, prefix := range group {
				if !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
					return nil, errors.New("site resolver: invalid SPA prefix")
				}
				for _, segment := range strings.Split(strings.Trim(prefix, "/"), "/") {
					if !safeSegment(segment) || segment == "" && prefix != "/" {
						return nil, errors.New("site resolver: invalid SPA prefix")
					}
				}
			}
		}
		r.spa = &cp
	}
	return r, nil
}

type resolvePlan struct {
	resolver    *Resolver
	request     Request
	preferences encodingPreferences
	missing     map[string]bool
	logical     map[string]bool
	keys        map[string]bool
}

func (r *Resolver) Resolve(ctx context.Context, req Request) (*Selection, error) {
	if r == nil || r.reader == nil {
		return nil, errors.New("site resolver: uninitialized")
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, reject(http.StatusMethodNotAllowed, "unsupported method")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := &resolvePlan{resolver: r, request: req, preferences: parseEncodings(req.Header), missing: map[string]bool{}, logical: map[string]bool{}, keys: map[string]bool{}}
	// Vary: Accept is needed only once ordinary resolution has missed and this
	// route could use the shell for a different Accept value.
	varyAccept := false
	finish := func(s *Selection, resolution Resolution, status int) (*Selection, error) {
		s.Resolution, s.Status, s.VaryAccept = resolution, status, varyAccept
		s.VaryEncoding = true
		return s, nil
	}
	// A candidate that exists without an acceptable representation ends
	// resolution; the 406 reports how that candidate was reached.
	unacceptable := func(err error, resolution Resolution) error {
		var input *Error
		if errors.As(err, &input) && input.Status == http.StatusNotAcceptable {
			input.Resolution = resolution
		}
		return err
	}
	exact, err := r.mapper.Map(req.Target.Path)
	if err != nil {
		return nil, err
	}
	s, err := p.candidate(ctx, exact)
	if err == nil {
		resolution := ResolutionOrdinary
		if strings.HasSuffix(req.Target.Path, "/") {
			resolution = ResolutionIndex
		}
		return finish(s, resolution, http.StatusOK)
	}
	if !storage.IsKind(err, storage.NotFound) {
		resolution := ResolutionOrdinary
		if strings.HasSuffix(req.Target.Path, "/") {
			resolution = ResolutionIndex
		}
		return nil, unacceptable(err, resolution)
	}
	if !strings.HasSuffix(req.Target.Path, "/") {
		index, err := r.mapper.Map(req.Target.Path + "/")
		if err != nil {
			return nil, err
		}
		s, err := p.candidate(ctx, index)
		if err == nil {
			closeSelection(s)
			location := req.Target.EscapedPath + "/"
			if req.Target.RawQuery != "" || req.Target.ForceQuery {
				location += "?" + req.Target.RawQuery
			}
			s.Location = location
			return finish(s, ResolutionIndex, http.StatusPermanentRedirect)
		}
		if !storage.IsKind(err, storage.NotFound) {
			return nil, unacceptable(err, ResolutionIndex)
		}
	}
	if r.spaRoute(req.Target.Path) {
		varyAccept = true
		if acceptsHTML(req.Header) {
			shell, err := r.configuredObject(r.spa.Fallback)
			if err != nil {
				return nil, err
			}
			s, err := p.candidate(ctx, shell)
			if err == nil {
				return finish(s, ResolutionSPA, http.StatusOK)
			}
			if !storage.IsKind(err, storage.NotFound) {
				return nil, unacceptable(err, ResolutionSPA)
			}
		}
	}
	if r.notFound != "" {
		object, err := r.configuredObject(r.notFound)
		if err != nil {
			return nil, err
		}
		s, err := p.candidate(ctx, object)
		if err == nil {
			return finish(s, ResolutionNotFound, http.StatusNotFound)
		}
		if !storage.IsKind(err, storage.NotFound) {
			return nil, unacceptable(err, ResolutionNotFound)
		}
	}
	return finish(&Selection{}, ResolutionNotFound, http.StatusNotFound)
}

func (r *Resolver) spaRoute(path string) bool {
	if r.spa == nil {
		return false
	}
	for _, prefix := range r.spa.ExcludePrefixes {
		if matchSPAPrefix(path, prefix) {
			return false
		}
	}
	if r.spa.IncludePrefixes != nil {
		for _, prefix := range r.spa.IncludePrefixes {
			if matchSPAPrefix(path, prefix) {
				return true
			}
		}
		return false
	}
	last := strings.TrimSuffix(path, "/")
	last = last[strings.LastIndex(last, "/")+1:]
	return !strings.Contains(last, ".")
}

func matchSPAPrefix(path, prefix string) bool {
	return prefix == "/" || path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, prefix)
}

func (p *resolvePlan) candidate(ctx context.Context, logical Object) (*Selection, error) {
	if p.logical[logical.Key] {
		return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
	}
	if len(p.logical) >= 4 {
		return nil, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	p.logical[logical.Key] = true
	// Fetch canonical metadata first: its declared coding can differ from its
	// filename. At most one canonical body is retained while preferred siblings
	// are tried. This avoids assuming canonical always means identity.
	canonical, err := p.read(ctx, logical)
	exists := err == nil
	if err != nil && !storage.IsKind(err, storage.NotFound) {
		return nil, err
	}
	defer func() { closeSelection(canonical) }()
	canonicalType := ""
	if canonical != nil {
		canonicalType = usableContentType(canonical.Metadata.ContentType)
		if err := identifyCoding(canonical, ""); err != nil {
			return nil, err
		}
	}
	if p.resolver.mode == "off" || !p.preferences.present {
		if canonical == nil {
			return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
		}
		if p.preferences.q(canonical.Coding) == 0 {
			return nil, reject(http.StatusNotAcceptable, "no acceptable representation")
		}
		p.decorate(canonical, logical, canonicalType)
		selected := canonical
		canonical = nil
		return selected, nil
	}
	candidates := append([]string(nil), p.resolver.encodings...)
	sort.SliceStable(candidates, func(i, j int) bool { return p.preferences.q(candidates[i]) > p.preferences.q(candidates[j]) })
	for _, coding := range candidates {
		if p.preferences.q(coding) == 0 {
			continue
		}
		if canonical != nil && p.precedes(canonical.Coding, coding) && p.preferences.q(canonical.Coding) > 0 {
			p.decorate(canonical, logical, canonicalType)
			selected := canonical
			canonical = nil
			return selected, nil
		}
		suffix := ".br"
		if coding == "gzip" {
			suffix = ".gz"
		}
		sibling := Object{Path: logical.Path + suffix, Key: logical.Key + suffix}
		selected, err := p.read(ctx, sibling)
		if storage.IsKind(err, storage.NotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := identifyCoding(selected, coding); err != nil {
			closeSelection(selected)
			return nil, err
		}
		p.decorate(selected, logical, canonicalType)
		return selected, nil
	}
	if canonical != nil && p.preferences.q(canonical.Coding) > 0 {
		p.decorate(canonical, logical, canonicalType)
		selected := canonical
		canonical = nil
		return selected, nil
	}
	if exists {
		return nil, reject(http.StatusNotAcceptable, "no acceptable representation")
	}
	return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
}

func (p *resolvePlan) precedes(a, b string) bool {
	qa, qb := p.preferences.q(a), p.preferences.q(b)
	if qa != qb {
		return qa > qb
	}
	if a == b {
		return true
	}
	rank := func(coding string) int {
		for i, c := range p.resolver.encodings {
			if c == coding {
				return i
			}
		}
		if coding == "identity" {
			return len(p.resolver.encodings) + 2
		}
		if coding == "br" {
			return len(p.resolver.encodings)
		}
		return len(p.resolver.encodings) + 1
	}
	return rank(a) < rank(b)
}

func (p *resolvePlan) read(ctx context.Context, object Object) (*Selection, error) {
	if len(object.Key) > MaxKeyBytes {
		return nil, reject(http.StatusRequestURITooLong, "object key exceeds byte limit")
	}
	if p.missing[object.Key] {
		return nil, &storage.Error{Kind: storage.NotFound, Size: -1}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.keys[object.Key] && len(p.keys) >= 12 {
		return nil, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	p.keys[object.Key] = true
	var metadata storage.Metadata
	var body io.ReadCloser
	var err error
	if p.request.Method == http.MethodHead {
		metadata, err = p.resolver.reader.Head(ctx, object.Key)
		if storage.IsKind(err, storage.AmbiguousNotFound) {
			var result *storage.Object
			result, err = p.resolver.reader.Get(ctx, object.Key, storage.ReadOptions{})
			if result != nil {
				metadata = result.Metadata
				if result.Body != nil {
					_ = result.Body.Close()
				}
			}
			if err == nil && result == nil {
				err = &storage.Error{Kind: storage.InvalidResponse, Size: -1}
			}
		}
	} else {
		var result *storage.Object
		result, err = p.resolver.reader.Get(ctx, object.Key, storage.ReadOptions{})
		if result != nil {
			metadata, body = result.Metadata, result.Body
		}
		if err == nil && (result == nil || body == nil) {
			err = &storage.Error{Kind: storage.InvalidResponse, Size: -1}
		}
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		if storage.IsKind(err, storage.NotFound) {
			p.missing[object.Key] = true
		}
		return nil, err
	}
	if metadata.Size < -1 || metadata.ContentRange != "" {
		if body != nil {
			_ = body.Close()
		}
		return nil, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	return &Selection{Key: object.Key, ObjectPath: object.Path, Metadata: metadata, Body: body, RangeCapable: p.resolver.rangeReads && metadata.Size >= 0}, nil
}

func identifyCoding(s *Selection, expected string) error {
	coding := strings.ToLower(strings.Trim(s.Metadata.ContentEncoding, " \t"))
	if coding == "" {
		coding = "identity"
		if expected != "" {
			coding = expected
		}
	}
	if coding != "identity" && coding != "br" && coding != "gzip" || expected != "" && coding != expected {
		return &storage.Error{Kind: storage.InvalidResponse, Size: -1}
	}
	s.Coding = coding
	s.Metadata.ContentEncoding = coding
	if coding == "identity" {
		s.Metadata.ContentEncoding = ""
	}
	return nil
}

func (p *resolvePlan) decorate(s *Selection, logical Object, canonicalType string) {
	s.LogicalPath = logical.Path
	s.ContentType = canonicalType
	if s.ContentType == "" {
		s.ContentType = ExtensionContentType(logical.Path)
	}
	s.Metadata.ContentType = s.ContentType
}

func usableContentType(value string) string {
	if value == "" || !validText(value) {
		return ""
	}
	if _, _, err := mime.ParseMediaType(value); err != nil {
		return ""
	}
	return value
}

// These common publication types are deterministic across operating systems;
// unknown extensions intentionally do not consult host MIME databases or bytes.
// Every response derives a missing or unusable Content-Type from this one
// table, so all responses for one object agree.
var extensionTypes = map[string]string{
	".html": "text/html; charset=utf-8", ".htm": "text/html; charset=utf-8",
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8",
	".mjs": "text/javascript; charset=utf-8", ".json": "application/json",
	".map": "application/json", ".txt": "text/plain; charset=utf-8",
	".xml": "application/xml", ".svg": "image/svg+xml", ".png": "image/png",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".avif": "image/avif", ".ico": "image/vnd.microsoft.icon",
	".pdf": "application/pdf", ".zip": "application/zip", ".gz": "application/gzip",
	".wasm": "application/wasm", ".woff": "font/woff", ".woff2": "font/woff2",
	".ttf": "font/ttf", ".otf": "font/otf", ".mp4": "video/mp4", ".webm": "video/webm",
	".mp3": "audio/mpeg", ".ogg": "audio/ogg", ".csv": "text/csv; charset=utf-8",
}

// ExtensionContentType returns the Content-Type for the extension of the
// last segment of p, ignoring case, or application/octet-stream.
func ExtensionContentType(p string) string {
	if value := extensionTypes[strings.ToLower(path.Ext(p))]; value != "" {
		return value
	}
	return "application/octet-stream"
}

func closeSelection(s *Selection) {
	if s != nil && s.Body != nil {
		_ = s.Body.Close()
		s.Body = nil
	}
}

// Configured fallback keys are literal S3 names, not public URL paths. Hidden
// segments and literal percent escapes are therefore retained exactly.
func (r *Resolver) configuredObject(key string) (Object, error) {
	if !relativeKey(key) {
		return Object{}, errors.New("site resolver: invalid configured object")
	}
	object := Object{Path: "/" + key, Key: r.mapper.prefix + key}
	if len(object.Key) > MaxKeyBytes {
		return Object{}, reject(http.StatusRequestURITooLong, "object key exceeds byte limit")
	}
	return object, nil
}
