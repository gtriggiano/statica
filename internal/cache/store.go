package cache

import (
	"maps"
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"github.com/hashicorp/golang-lru/v2/simplelru"
)

// Store indexes at most one entry per key and is safe for concurrent use.
//
// Charged bytes and entries cover indexed (resident) entries plus retired
// ones: removed from the index while pinned, still readable through their
// handles and freed exactly when the last one is released. Admission is
// plan-then-commit: it evicts only unpinned entries, oldest first, and only
// when the plan makes the new entry fit both ceilings; otherwise nothing
// changes. Every index mutation is a compare-and-swap on the key's Version,
// so a slow caller cannot overwrite newer state. Fill reservations are
// charged to a separate budget.
type Store struct {
	limits Limits

	mu sync.Mutex
	// index orders resident entries. Its capacity equals MaxEntries, which no
	// admission plan exceeds, so it must never evict on its own. Every removal
	// from it, explicit or not, reaches retire through the eviction callback.
	index                       *simplelru.LRU[string, *entry]
	retired, detached           map[*entry]struct{}
	reservations                map[*Reservation]struct{} // unconsumed
	residentBytes, retiredBytes int64
	residentEntries             int
	fillBytes, detachedBytes    int64
	lastVersion                 Version
	evictions                   map[Kind]uint64
	expirations                 map[Kind]uint64
	admissions                  map[Result]uint64
	overflows                   uint64 // simplelru evictions; nonzero breaks an invariant
}

type entryState uint8

const (
	statePending entryState = iota
	stateIndexed
	stateRetired
	stateDetached
	stateFreed
)

// entry owns one immutable snapshot; refs counts live handles. A body entry in
// the resident budget references a body record, which Refresh shares with the
// entry it supersedes. A detached entry instead holds a fill reservation.
type entry struct {
	key     string
	snap    Snapshot
	version Version
	own     int64 // charge excluding body bytes
	body    *body // nil for metadata, negative and detached entries
	fill    int64 // reservation held by a detached entry
	refs    int
	state   entryState
	expired bool // counted in expirations; see MarkExpired
}

// body charges bytes once while any entry references them. They are resident
// while an indexed entry references them, otherwise retired.
type body struct {
	size    int64
	refs    int
	indexed bool
}

// reclaimable is the charge freed by freeing unpinned e. It excludes a body
// still referenced by another entry. Only Refresh shares a body with the
// incoming entry, and its expected handle pins old, so a reclaimed entry never
// shares the incoming body.
func (e *entry) reclaimable() int64 {
	n := e.own
	if b := e.body; b != nil && b.refs == 1 {
		n += b.size
	}
	return n
}

// NewStore returns an empty Store enforcing validated limits.
func NewStore(l Limits) (*Store, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	// A random first version keeps one Store's versions from matching
	// another's by accident.
	s := &Store{limits: l, lastVersion: Version(rand.Uint64()), retired: map[*entry]struct{}{}, detached: map[*entry]struct{}{}, reservations: map[*Reservation]struct{}{}, evictions: map[Kind]uint64{}, expirations: map[Kind]uint64{}, admissions: map[Result]uint64{}}
	for _, k := range kinds {
		s.evictions[k], s.expirations[k] = 0, 0
	}
	for _, r := range results {
		s.admissions[r] = 0
	}
	index, err := simplelru.NewLRU(l.MaxEntries, s.evicted)
	if err != nil {
		return nil, err
	}
	s.index = index
	return s, nil
}

func (s *Store) evicted(_ string, e *entry) { s.retire(e) }

// stamp issues the next version, wrapping around past Absent.
func (s *Store) stamp() Version {
	if s.lastVersion++; s.lastVersion == Absent {
		s.lastVersion++
	}
	return s.lastVersion
}

func (s *Store) newEntry(key string, snap Snapshot) *entry {
	if key == "" || !snap.valid() {
		return nil
	}
	e := &entry{key: key, snap: snap, version: s.stamp(), own: ownCharge(key, snap.Metadata)}
	if snap.Kind == KindBody {
		e.body = &body{size: int64(len(snap.Body))}
	}
	return e
}

// matches reports whether expected names key's indexed entry, which it also
// returns; Absent matches only a key with no indexed entry. It leaves LRU
// order unchanged.
func (s *Store) matches(key string, expected Version) (*entry, bool) {
	old, ok := s.index.Peek(key)
	if !ok {
		return nil, expected == Absent
	}
	return old, old.version == expected
}

// Peek reports the version and kind of key's indexed entry without pinning
// it or changing LRU order, e.g. to re-read an expected version right before
// Promote.
func (s *Store) Peek(key string) (Version, Kind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index.Peek(key)
	if !ok {
		return Absent, "", false
	}
	return e.version, e.snap.Kind, true
}

// Acquire pins the entry indexed under key and marks it most recently used.
// The caller owns the handle and must release it.
func (s *Store) Acquire(key string) (*Handle, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index.Get(key)
	if !ok {
		return nil, false
	}
	e.refs++
	return &Handle{store: s, entry: e}, true
}

// Admit indexes snap under key if expected still names the key's indexed
// entry (Absent: none). Otherwise it returns ResultConflict and changes
// nothing.
func (s *Store) Admit(key string, expected Version, snap Snapshot) Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := ResultInvalid
	if e := s.newEntry(key, snap); e != nil {
		r = ResultConflict
		if old, ok := s.matches(key, expected); ok {
			r = s.admit(e, old)
		}
	}
	s.admissions[r]++
	return r
}

// Remove invalidates key if expected is its indexed version. A pinned entry is
// retired, an unpinned one freed.
func (s *Store) Remove(key string, expected Version) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.index.Peek(key); !ok || e.version != expected {
		return false
	}
	return s.index.Remove(key)
}

// Refresh is a compare-and-swap of the entry pinned by expected for a new
// entry, with a new Version, carrying next's metadata and timing. next must
// have the same kind and share the expected body: its Body is nil or the very
// same bytes. If expected's entry is no longer indexed the result is
// ResultConflict. Readers of expected keep the old snapshot, which stays
// charged without its shared bytes until released. On ResultRefreshed the
// returned handle pins the new entry; on any other result nothing changes.
// Like every use of a released handle, a released expected panics.
func (s *Store) Refresh(expected *Handle, next Snapshot) (*Handle, Result) {
	if expected != nil {
		expected.live("Refresh")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, r := s.refresh(expected, next)
	s.admissions[r]++
	return h, r
}

func (s *Store) refresh(expected *Handle, next Snapshot) (*Handle, Result) {
	if expected == nil || expected.store != s {
		return nil, ResultInvalid
	}
	old := expected.entry
	prev := old.snap.Body
	if next.Kind != old.snap.Kind || next.Body != nil && (len(next.Body) != len(prev) || len(prev) > 0 && &next.Body[0] != &prev[0]) {
		return nil, ResultInvalid
	}
	next.Body = prev
	if !next.valid() {
		return nil, ResultInvalid
	}
	if old.state != stateIndexed {
		return nil, ResultConflict
	}
	e := &entry{key: old.key, snap: next, version: s.stamp(), own: ownCharge(old.key, next.Metadata), body: old.body}
	if r := s.admit(e, old); r != ResultReplaced {
		return nil, r
	}
	e.refs = 1
	return &Handle{store: s, entry: e}, ResultRefreshed
}

// Reserve charges n bytes of fill buffer to the fill budget without blocking.
// On success the result is ResultAdmitted. Otherwise the reservation is nil
// and the result is ResultInvalid for a negative n, ResultTooLarge above
// MaxObjectBytes (such objects stream without a fill buffer) or ResultNoSpace
// when the fill budget is full.
func (s *Store) Reserve(n int64) (*Reservation, Result) {
	switch {
	case n < 0:
		return nil, ResultInvalid
	case n > s.limits.MaxObjectBytes:
		return nil, ResultTooLarge
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fillBytes+n > s.limits.MaxFillBytes {
		return nil, ResultNoSpace
	}
	s.fillBytes += n
	r := &Reservation{store: s, size: n}
	s.reservations[r] = struct{}{}
	return r, ResultAdmitted
}

// Promote publishes a completed fill without copying its bytes and consumes r
// on every path: the caller must not use r afterwards and only releases the
// returned handle, if any. snap must be a valid body snapshot no larger than r.
//   - ResultAdmitted or ResultReplaced: r is released and the handle pins the
//     entry now indexed under key, because expected matched (Absent: none).
//   - ResultConflict, ResultTooLarge or ResultNoSpace: the handle pins a
//     detached entry that no lookup finds and that keeps r's bytes charged to
//     the fill budget until its last handle is released.
//   - ResultInvalid: r is released and the handle is nil.
func (s *Store) Promote(key string, expected Version, r *Reservation, snap Snapshot) (*Handle, Result) {
	if r != nil && r.store != s {
		r.Release()
		r = nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, res := s.promote(key, expected, r, snap)
	s.admissions[res]++
	return h, res
}

func (s *Store) promote(key string, expected Version, r *Reservation, snap Snapshot) (*Handle, Result) {
	if r == nil || r.done {
		return nil, ResultInvalid
	}
	s.consume(r)
	e := s.newEntry(key, snap)
	if e == nil || snap.Kind != KindBody || e.body.size > r.size {
		s.fillBytes -= r.size
		return nil, ResultInvalid
	}
	e.refs = 1
	h := &Handle{store: s, entry: e}
	res := ResultConflict
	if old, ok := s.matches(key, expected); ok {
		res = s.admit(e, old)
	}
	if res == ResultAdmitted || res == ResultReplaced {
		s.fillBytes -= r.size
		return h, res
	}
	e.body, e.fill, e.state = nil, r.size, stateDetached
	s.detached[e] = struct{}{}
	s.detachedBytes += r.size
	return h, res
}

// consume marks r as no longer owned by its caller. Its bytes stay in
// fillBytes until the caller of consume releases or transfers them.
func (s *Store) consume(r *Reservation) {
	r.done = true
	delete(s.reservations, r)
}

// Stats returns a consistent copy of the accounting.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{
		Limits:        s.limits,
		ResidentBytes: s.residentBytes, RetiredBytes: s.retiredBytes, ChargedBytes: s.residentBytes + s.retiredBytes,
		ResidentEntries: s.residentEntries, RetiredEntries: len(s.retired),
		FillBytes: s.fillBytes, DetachedBytes: s.detachedBytes, DetachedEntries: len(s.detached), Reservations: len(s.reservations),
		Evictions: maps.Clone(s.evictions), Expirations: maps.Clone(s.expirations), Admissions: maps.Clone(s.admissions),
	}
}

// admit indexes e in place of old, the key's indexed entry or nil, when an
// eviction plan fits both ceilings. It changes nothing unless it returns
// ResultAdmitted or ResultReplaced.
func (s *Store) admit(e, old *entry) Result {
	size := int64(len(e.snap.Body))
	if size > s.limits.MaxObjectBytes || e.own+size > s.limits.MaxBytes {
		return ResultTooLarge
	}
	need := e.own
	if e.body != nil && e.body.refs == 0 {
		need += size
	}
	walked, ok := s.plan(old, need)
	if !ok {
		return ResultNoSpace
	}
	// Evict the unpinned entries the plan walked past. A pinned one is being
	// served, so it is in use: marking it recently used keeps a pinned tail
	// from forcing a walk on every admission.
	for _, w := range walked {
		switch {
		case w == old:
		case w.refs > 0:
			s.index.Get(w.key)
		default:
			s.evictions[w.snap.Kind]++
			s.index.Remove(w.key)
		}
	}
	if old == nil {
		s.link(e)
		return ResultAdmitted
	}
	// Remove first: simplelru updates an existing key in place without its
	// eviction callback, which would leave old unaccounted.
	s.index.Remove(e.key)
	s.link(e)
	return ResultReplaced
}

// plan walks entries oldest first until evicting the unpinned ones among them
// makes adding need bytes and one entry fit both ceilings, once old, the entry
// being replaced, leaves the index. A pinned old entry stays charged as
// retired. It returns the walked entries; committing evicts the unpinned ones
// other than old and marks the pinned ones recently used.
//
// Trade-off: when the oldest entry is evictable and enough, which is the
// steady state, planning is O(1). Otherwise simplelru offers no way to walk
// from its oldest end without copying, so the plan copies the index: O(n) time
// and one n-pointer allocation under the lock. A pinned tail costs that once,
// since committing moves it to the front; a rejected plan changes nothing and
// pays it every time. BenchmarkAdmitUnderPressure measures these cases;
// revisit the primitive only on evidence.
func (s *Store) plan(old *entry, need int64) ([]*entry, bool) {
	bytes := s.residentBytes + s.retiredBytes + need
	entries := s.residentEntries + len(s.retired) + 1
	if old != nil && old.refs == 0 {
		bytes -= old.reclaimable()
		entries--
	}
	fits := func() bool { return bytes <= s.limits.MaxBytes && entries <= s.limits.MaxEntries }
	if fits() {
		return nil, true
	}
	// walk counts e as evicted unless it is old or pinned and reports whether
	// the admission then fits.
	walk := func(e *entry) bool {
		if e != old && e.refs == 0 {
			bytes -= e.reclaimable()
			entries--
		}
		return fits()
	}
	_, oldest, ok := s.index.GetOldest()
	if !ok {
		return nil, false
	}
	if walk(oldest) {
		return []*entry{oldest}, true
	}
	all := s.index.Values()
	for i := 1; i < len(all); i++ {
		if walk(all[i]) {
			return all[:i+1], true
		}
	}
	return nil, false
}

// link charges e as resident and indexes it. Bytes it shares with retired
// entries move from retired to resident instead of being charged twice.
func (s *Store) link(e *entry) {
	e.state = stateIndexed
	s.residentBytes += e.own
	s.residentEntries++
	if b := e.body; b != nil {
		if b.refs > 0 {
			s.retiredBytes -= b.size
		}
		s.residentBytes += b.size
		b.refs++
		b.indexed = true
	}
	if s.index.Add(e.key, e) {
		s.overflows++
	}
}

// retire moves e, just removed from the index, out of the resident budget. It
// stays charged as retired while handles pin it.
func (s *Store) retire(e *entry) {
	e.state = stateRetired
	s.residentBytes -= e.own
	s.residentEntries--
	s.retiredBytes += e.own
	if b := e.body; b != nil {
		b.indexed = false
		s.residentBytes -= b.size
		s.retiredBytes += b.size
	}
	if e.refs > 0 {
		s.retired[e] = struct{}{}
		return
	}
	s.free(e)
}

// free drops the charge of a retired entry nobody pins. Shared body bytes
// stay charged until the last entry referencing them is freed.
func (s *Store) free(e *entry) {
	delete(s.retired, e)
	e.state = stateFreed
	s.retiredBytes -= e.own
	if b := e.body; b != nil {
		if b.refs--; b.refs == 0 {
			s.retiredBytes -= b.size
		}
	}
}

// Handle pins one snapshot for one owner, who must Release it; Release is
// idempotent. Retain gives each additional owner its own handle. Using a
// released handle is an ownership bug and panics.
type Handle struct {
	store    *Store
	entry    *entry
	released atomic.Bool
}

func (h *Handle) live(op string) {
	if h.released.Load() {
		panic("cache: " + op + " of a released handle")
	}
}

// Snapshot returns the pinned snapshot. Its Body bytes are read-only and must
// not be used after Release.
func (h *Handle) Snapshot() Snapshot {
	h.live("Snapshot")
	return h.entry.snap
}

// Version identifies the pinned entry for Admit, Promote and Remove.
func (h *Handle) Version() Version {
	h.live("Version")
	return h.entry.version
}

// MarkExpired records that a lookup found the pinned entry expired by its own
// freshness, which only the caller judges. Stats counts each entry version by
// kind the first time it is marked while indexed or retired, and MarkExpired
// reports whether this call counted it. A detached entry, which no lookup
// finds, is never counted.
func (h *Handle) MarkExpired() bool {
	s := h.store
	s.mu.Lock()
	defer s.mu.Unlock()
	h.live("MarkExpired")
	e := h.entry
	if e.expired || e.state != stateIndexed && e.state != stateRetired {
		return false
	}
	e.expired = true
	s.expirations[e.snap.Kind]++
	return true
}

// Retain returns a new handle pinning the same snapshot.
func (h *Handle) Retain() *Handle {
	s := h.store
	s.mu.Lock()
	defer s.mu.Unlock()
	h.live("Retain")
	h.entry.refs++
	return &Handle{store: s, entry: h.entry}
}

// Release unpins the snapshot, freeing a retired or detached entry when this
// was its last handle.
func (h *Handle) Release() {
	if h.released.Swap(true) {
		return
	}
	s := h.store
	s.mu.Lock()
	defer s.mu.Unlock()
	e := h.entry
	if e.refs--; e.refs > 0 {
		return
	}
	switch e.state {
	case stateRetired:
		s.free(e)
	case stateDetached:
		delete(s.detached, e)
		e.state = stateFreed
		s.fillBytes -= e.fill
		s.detachedBytes -= e.fill
	}
}

// Reservation holds fill-budget bytes for one fill buffer until Release, which
// is idempotent, or until Promote consumes it.
type Reservation struct {
	store *Store
	size  int64
	done  bool // released or consumed; guarded by store.mu
}

// Size is the number of reserved bytes.
func (r *Reservation) Size() int64 { return r.size }

// Release returns the reservation to the fill budget once.
func (r *Reservation) Release() {
	s := r.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.done {
		return
	}
	s.consume(r)
	s.fillBytes -= r.size
}
