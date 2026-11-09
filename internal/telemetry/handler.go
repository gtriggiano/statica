package telemetry

import (
	"bytes"
	"context"
	"encoding"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-logfmt/logfmt"
)

// MaxValueBytes bounds every encoded value, before escaping. Longer values
// are cut at a UTF-8 boundary.
const MaxValueBytes = 16 << 10

// timestampLayout is RFC 3339 with milliseconds; UTC times end in Z.
const timestampLayout = "2006-01-02T15:04:05.000Z07:00"

// HandlerOptions configures a Handler.
type HandlerOptions struct {
	// Level is the minimum level of the events written; nil means info.
	Level slog.Leveler
}

// Handler is a log/slog Handler writing one logfmt event per line. Every
// line starts with ts (the UTC event time, RFC 3339 with milliseconds), level
// and event (the slog message), followed by the logger's attributes, then
// the record's, in the order they were added. Groups are flattened into
// dotted keys (group.key). Each key appears once: a later attribute with an
// already written key is dropped, and attributes named ts, level, event or
// time never replace the built-in fields. Keys keep only printable ASCII
// other than '=' and '"'. Values are bounded to MaxValueBytes and escaped by
// the go-logfmt encoder, so a hostile value cannot start a second event; C1
// controls and Unicode line or paragraph separators become U+FFFD, as does
// invalid UTF-8. Callers must never pass secrets, raw provider errors or
// URLs as values.
type Handler struct {
	w      io.Writer
	mu     *sync.Mutex
	level  slog.Leveler
	fields []field // the logger's attributes, flattened
	prefix string  // open groups, each followed by '.'
}

type field struct{ key, value string }

// NewHandler returns a Handler writing each event with one Write call to w,
// serialized by the handler, typically an Output.
func NewHandler(w io.Writer, opts *HandlerOptions) *Handler {
	h := &Handler{w: w, mu: new(sync.Mutex), level: slog.LevelInfo}
	if opts != nil && opts.Level != nil {
		h.level = opts.Level
	}
	return h
}

func (h *Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := *h
	collect := &collector{fields: h.fields[:len(h.fields):len(h.fields)]}
	for _, a := range attrs {
		flatten(collect, h.prefix, a)
	}
	c.fields = collect.fields
	return &c
}

func (h *Handler) WithGroup(name string) slog.Handler {
	name = cleanKey(name)
	if name == "" {
		return h
	}
	c := *h
	c.prefix = h.prefix + name + "."
	return &c
}

func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	s := statePool.Get().(*encodeState)
	defer s.release()
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	s.keys = append(s.keys, "time")
	s.raw("ts", t.UTC().AppendFormat(s.scratch(), timestampLayout))
	s.str("level", levelText(r.Level))
	s.str("event", r.Message)
	for _, f := range h.fields {
		s.str(f.key, f.value)
	}
	r.Attrs(func(a slog.Attr) bool {
		flatten(s, h.prefix, a)
		return true
	})
	if err := s.enc.EndRecord(); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(s.buf.Bytes())
	return err
}

func levelText(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "debug"
	case slog.LevelInfo:
		return "info"
	case slog.LevelWarn:
		return "warn"
	case slog.LevelError:
		return "error"
	}
	return strings.ToLower(l.String())
}

// sink receives flattened attributes: str for text, raw for ASCII scalars
// formatted into its scratch buffer.
type sink interface {
	str(key, value string)
	raw(key string, value []byte)
	scratch() []byte
}

// flatten resolves a and passes its scalar leaves to s, following the slog
// Handler rules for empty attributes and groups.
func flatten(s sink, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Key == "" && a.Value.Equal(slog.Value{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		attrs := a.Value.Group()
		if len(attrs) == 0 {
			return
		}
		if key := cleanKey(a.Key); key != "" {
			prefix += key + "."
		}
		for _, g := range attrs {
			flatten(s, prefix, g)
		}
		return
	}
	key := cleanKey(a.Key)
	if key == "" {
		return
	}
	key = prefix + key
	v := a.Value
	switch v.Kind() {
	case slog.KindString:
		s.str(key, v.String())
	case slog.KindInt64:
		s.raw(key, strconv.AppendInt(s.scratch(), v.Int64(), 10))
	case slog.KindUint64:
		s.raw(key, strconv.AppendUint(s.scratch(), v.Uint64(), 10))
	case slog.KindFloat64:
		s.raw(key, strconv.AppendFloat(s.scratch(), v.Float64(), 'f', -1, 64))
	case slog.KindBool:
		s.raw(key, strconv.AppendBool(s.scratch(), v.Bool()))
	case slog.KindDuration:
		s.str(key, v.Duration().String())
	case slog.KindTime:
		s.raw(key, v.Time().UTC().AppendFormat(s.scratch(), timestampLayout))
	default:
		s.str(key, anyText(v.Any()))
	}
}

// anyText formats an arbitrary value, containing panics of its methods.
func anyText(v any) (text string) {
	defer func() {
		if recover() != nil {
			text = "!PANIC"
		}
	}()
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	case encoding.TextMarshaler:
		b, err := x.MarshalText()
		if err != nil {
			return "!ERROR"
		}
		return string(b)
	case []byte:
		return string(x)
	}
	return fmt.Sprint(v)
}

// cleanKey keeps the printable ASCII bytes of key other than '=' and '"'.
func cleanKey(key string) string {
	valid := func(c byte) bool { return c > ' ' && c < 0x7f && c != '=' && c != '"' }
	for i := 0; i < len(key); i++ {
		if !valid(key[i]) {
			b := make([]byte, 0, len(key))
			for j := 0; j < len(key); j++ {
				if valid(key[j]) {
					b = append(b, key[j])
				}
			}
			return string(b)
		}
	}
	return key
}

// cleanValue bounds value to MaxValueBytes and replaces C1 controls and the
// Unicode line and paragraph separators, which the encoder leaves raw, with
// U+FFFD. The encoder escapes ASCII controls and replaces invalid UTF-8.
func cleanValue(value string) string {
	if len(value) > MaxValueBytes {
		cut := MaxValueBytes
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = value[:cut]
	}
	unsafe := func(r rune) bool { return r >= 0x80 && r <= 0x9f || r == ' ' || r == ' ' }
	for i := 0; i < len(value); i++ {
		if value[i] < utf8.RuneSelf {
			continue
		}
		if strings.IndexFunc(value[i:], unsafe) < 0 {
			return value
		}
		return strings.Map(func(r rune) rune {
			if unsafe(r) {
				return utf8.RuneError
			}
			return r
		}, value)
	}
	return value
}

// collector accumulates the logger's attributes for WithAttrs.
type collector struct{ fields []field }

func (c *collector) str(key, value string) {
	c.fields = append(c.fields, field{key, cleanValue(value)})
}
func (c *collector) raw(key string, value []byte) {
	c.fields = append(c.fields, field{key, string(value)})
}
func (c *collector) scratch() []byte { return nil }

// encodeState encodes one event. The key and value adapters are pointers, so
// passing them to the encoder's interface parameters does not allocate.
type encodeState struct {
	buf  bytes.Buffer
	enc  *logfmt.Encoder
	key  text
	val  text
	num  octets
	keys []string // keys already written, including reserved ones
}

type text struct{ s string }

func (t *text) String() string { return t.s }

type octets struct{ b []byte }

func (o *octets) MarshalText() ([]byte, error) { return o.b, nil }

var statePool = sync.Pool{New: func() any {
	s := &encodeState{keys: make([]string, 0, 32)}
	s.enc = logfmt.NewEncoder(&s.buf)
	return s
}}

func (s *encodeState) release() {
	if s.buf.Cap() > 64<<10 {
		return
	}
	s.buf.Reset()
	s.enc.Reset()
	clear(s.keys)
	s.keys = s.keys[:0]
	statePool.Put(s)
}

// claim records key, reporting false when it was already written.
func (s *encodeState) claim(key string) bool {
	for _, k := range s.keys {
		if k == key {
			return false
		}
	}
	s.keys = append(s.keys, key)
	return true
}

func (s *encodeState) str(key, value string) {
	if s.claim(key) {
		s.key.s, s.val.s = key, cleanValue(value)
		_ = s.enc.EncodeKeyval(&s.key, &s.val)
	}
}

func (s *encodeState) scratch() []byte { return s.num.b[:0] }

func (s *encodeState) raw(key string, value []byte) {
	if s.claim(key) {
		s.key.s, s.num.b = key, value
		_ = s.enc.EncodeKeyval(&s.key, &s.num)
	}
}
