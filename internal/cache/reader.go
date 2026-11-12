package cache

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

// NewReader returns a storage.Reader that serves origin reads through c, so
// resolution and response code keep talking to a storage.Reader:
//
//   - Get without options is Cache.Get. A lookup served from memory returns an
//     Object whose Body is a *Body over the pinned snapshot; a stream returns
//     the origin Object itself. Typed errors pass through, so a negative hit
//     is the NotFound that confirms absence.
//   - Get with any ReadOptions is rejected as Unsupported: the cache owns
//     revalidation, and partial reads after a streamed selection go to the
//     origin directly without a second cache lookup.
//   - Head is Cache.Head. A caller disambiguating an AmbiguousNotFound with a
//     plain Get, as the site resolver does, makes an ordinary Get: a confirmed
//     absence becomes a negative entry that later HEADs reuse, and an object
//     that does exist fills like any GET miss, within max_object_bytes.
//
// The request directives and the per-request record of lookups travel in the
// context; see WithRequest. Metadata served from memory, by Get or Head,
// states the snapshot's RFC 9111 current age in Age and RawAge, so neither a
// hit nor stale service advertises a renewed lifetime; streams and origin
// metadata are returned as the origin sent them. The response layer then sends
// the larger of that age and the apparent age since the origin Date, so a
// response read from the origin can carry an Age too.
func NewReader(c *Cache) storage.Reader { return &reader{c: c} }

type reader struct{ c *Cache }

func (r *reader) Get(ctx context.Context, key string, opts storage.ReadOptions) (*storage.Object, error) {
	if opts != (storage.ReadOptions{}) {
		return nil, &storage.Error{Kind: storage.Unsupported, Size: -1}
	}
	s := scopeOf(ctx)
	req, revalidate := s.request()
	l, err := r.c.get(ctx, key, req, revalidate)
	s.record(key, l)
	if err != nil {
		l.Release()
		return nil, err
	}
	if h := l.Handle(); h != nil {
		return &storage.Object{Metadata: present(l, r.c.now()), Body: newBody(l, h.Snapshot().Body)}, nil
	}
	if obj := l.TakeObject(); obj != nil {
		return obj, nil
	}
	l.Release() // unreachable: a successful Get serves a handle or a stream
	return nil, &storage.Error{Kind: storage.InvalidResponse, Size: -1}
}

func (r *reader) Head(ctx context.Context, key string) (storage.Metadata, error) {
	s := scopeOf(ctx)
	req, revalidate := s.request()
	l, err := r.c.head(ctx, key, req, revalidate)
	s.record(key, l)
	defer l.Release()
	switch {
	case err != nil:
		return storage.Metadata{}, err
	case l.Handle() != nil:
		return present(l, r.c.now()), nil
	}
	return l.Metadata, nil
}

// present returns the metadata of a lookup served from memory as a cache
// forwarding a stored response must state it (RFC 9111 §4.2.3, §5.1): Age and
// RawAge carry the current age at now in whole seconds, saturated at
// httpcache.MaxDelta, so a hit or a stale response never advertises a renewed
// lifetime. Date and freshness fields are the origin's. Malformed metadata,
// which only a 304 that could not be stored can carry, is left untouched so
// the response layer still fails closed on it.
func present(l *Lookup, now time.Time) storage.Metadata {
	m := l.Metadata
	if httpcache.Malformed(m) {
		return m
	}
	seconds := int64(httpcache.CurrentAge(m, l.RequestTime, l.ResponseTime, now) / time.Second)
	m.Age, m.RawAge = time.Duration(seconds)*time.Second, strconv.FormatInt(seconds, 10)
	return m
}

var errBodyClosed = &storage.Error{Kind: storage.Canceled, Size: -1}

// Body is the Body of an Object a Reader serves from memory: bytes of one
// complete snapshot, pinned until Close.
// Its type is how a caller tells a body read from memory apart from an
// origin stream. Read and Close are safe for concurrent use, so a request's
// cancellation may close it during a read; Close is idempotent, releases the
// pin and fails later reads with a storage.Canceled error.
type Body struct {
	mu     sync.Mutex
	lookup *Lookup
	data   []byte
	off    int
}

func newBody(l *Lookup, data []byte) *Body { return &Body{lookup: l, data: data} }

func (b *Body) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.lookup == nil:
		return 0, errBodyClosed
	case b.off >= len(b.data):
		return 0, io.EOF
	}
	n := copy(p, b.data[b.off:])
	b.off += n
	return n, nil
}

// Bytes returns all the body's bytes without copying, whatever Read has
// consumed, so a caller can write or slice them in place. They are read-only
// and pinned only until Close: a caller must finish using them before it
// closes the Body, and must not let a concurrent Close race that use. Bytes
// returns nil once the Body is closed.
func (b *Body) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data
}

// Close releases the snapshot.
func (b *Body) Close() error {
	b.mu.Lock()
	l := b.lookup
	b.lookup, b.data = nil, nil
	b.mu.Unlock()
	if l != nil {
		l.Release()
	}
	return nil
}

// Record is the cache classification of one lookup a Reader made for a
// request: Outcome, Reason with OutcomeBypass, and Coalesced when the lookup
// joined a flight another request started (Lookup.Coalesced).
type Record struct {
	Key       string
	Outcome   Outcome
	Reason    Reason
	Coalesced bool
}

type scopeKey struct{}

// scope is one request's directives and lookup records.
type scope struct {
	req        httpcache.Request
	revalidate bool
	mu         sync.Mutex
	records    []Record
	inline     [4]Record
}

// WithRequest scopes ctx to one request whose Cache-Control directives are
// req, typically httpcache.RequestDirectives of its header. A Reader reads
// the directives from the context, without which a lookup carries none and
// records nothing, and records the classification of every lookup it makes,
// in order, for Lookups and LastLookup. Each lookup applies the directives
// its object's request_directives allow.
func WithRequest(ctx context.Context, req httpcache.Request) context.Context {
	return withScope(ctx, &scope{req: req})
}

// WithRevalidation is WithRequest for a request whose every lookup must
// not reuse memory without asking the origin: each one applies no-cache on
// top of the directives of req that the object's request_directives allows,
// so it revalidates, or bypasses the cache when an applied no-store says so.
// It serves statica's own retry after an object changed during its
// selection, which a client directive policy must not turn off.
func WithRevalidation(ctx context.Context, req httpcache.Request) context.Context {
	return withScope(ctx, &scope{req: req, revalidate: true})
}

func withScope(ctx context.Context, s *scope) context.Context {
	s.records = s.inline[:0]
	return context.WithValue(ctx, scopeKey{}, s)
}

func scopeOf(ctx context.Context) *scope {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	return s
}

func (s *scope) request() (req httpcache.Request, revalidate bool) {
	if s == nil {
		return httpcache.Request{}, false
	}
	return s.req, s.revalidate
}

func (s *scope) record(key string, l *Lookup) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.records = append(s.records, Record{Key: key, Outcome: l.Outcome, Reason: l.Reason, Coalesced: l.Coalesced})
	s.mu.Unlock()
}

// Lookups returns a copy of the records of ctx's request scope, in order.
func Lookups(ctx context.Context) []Record {
	s := scopeOf(ctx)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

// LastLookup returns the latest record of key in ctx's request scope, or the
// latest record of any key when key is empty.
func LastLookup(ctx context.Context, key string) (Record, bool) {
	s := scopeOf(ctx)
	if s == nil {
		return Record{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.records) - 1; i >= 0; i-- {
		if key == "" || s.records[i].Key == key {
			return s.records[i], true
		}
	}
	return Record{}, false
}

// FromConfig builds the Store and the Cache that cfg describes over origin:
// capacity from LimitsFrom, policies and fill limits from cfg.Cache, the
// publication prefix and provider conditional reads from cfg.S3. Each option
// adjusts the Options before New, e.g. to set OnFill or Now. It returns nil
// and no error when cfg.Cache.Enabled is false: the caller serves uncached.
// lifetime is the Options.Lifetime.
func FromConfig(lifetime context.Context, cfg *config.Config, origin storage.Reader, options ...func(*Options)) (*Cache, error) {
	if cfg == nil {
		return nil, errors.New("cache: configuration is required")
	}
	if !cfg.Cache.Enabled {
		return nil, nil
	}
	store, err := NewStore(LimitsFrom(cfg.Cache))
	if err != nil {
		return nil, err
	}
	o := Options{Store: store, Origin: origin, Config: cfg.Cache, Prefix: cfg.S3.Prefix,
		ConditionalReads: cfg.S3.Features.ConditionalReads, Lifetime: lifetime}
	for _, option := range options {
		option(&o)
	}
	return New(o)
}
