package cache

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gtriggiano/statica/internal/storage"
)

// checkInvariants recomputes the accounting from the entries themselves under
// the Store lock and reports every disagreement with the running counters.
func checkInvariants(s *Store) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	type use struct {
		refs    int
		indexed bool
	}
	bodies := map[*body]*use{}
	track := func(e *entry, indexed bool) {
		if e.body == nil {
			return
		}
		u := bodies[e.body]
		if u == nil {
			u = &use{}
			bodies[e.body] = u
		}
		u.refs++
		u.indexed = u.indexed || indexed
	}
	versions := map[Version]bool{}
	issued := func(e *entry) {
		if e.version == Absent || versions[e.version] {
			fail("entry %q: version %d is Absent or shared with another live entry", e.key, e.version)
		}
		versions[e.version] = true
	}
	// Every live entry marked expired was counted once by its kind, so the
	// marked entries never outnumber the counter; no detached entry is marked.
	expired := map[Kind]uint64{}
	var resident, retired, detached int64
	keys := s.index.Keys()
	if len(keys) != s.residentEntries || s.index.Len() != s.residentEntries {
		fail("index holds %d entries, resident count is %d", len(keys), s.residentEntries)
	}
	for _, k := range keys {
		e, _ := s.index.Peek(k)
		if e.key != k || e.state != stateIndexed || e.refs < 0 || e.fill != 0 {
			fail("indexed entry %q: key %q state %d refs %d fill %d", k, e.key, e.state, e.refs, e.fill)
		}
		issued(e)
		if (e.body != nil) != (e.snap.Kind == KindBody) || e.body != nil && e.body.size != int64(len(e.snap.Body)) {
			fail("indexed entry %q: body record does not match its snapshot", k)
		}
		if e.expired {
			expired[e.snap.Kind]++
		}
		resident += e.own
		track(e, true)
	}
	for e := range s.retired {
		if e.state != stateRetired || e.refs <= 0 {
			fail("retired entry %q: state %d refs %d", e.key, e.state, e.refs)
		}
		if cur, ok := s.index.Peek(e.key); ok && cur == e {
			fail("retired entry %q is still indexed", e.key)
		}
		issued(e)
		if e.expired {
			expired[e.snap.Kind]++
		}
		retired += e.own
		track(e, false)
	}
	for b, u := range bodies {
		if b.refs != u.refs || b.indexed != u.indexed {
			fail("body record: refs %d indexed %t, want refs %d indexed %t", b.refs, b.indexed, u.refs, u.indexed)
		}
		if u.indexed {
			resident += b.size
		} else {
			retired += b.size
		}
	}
	if resident != s.residentBytes || retired != s.retiredBytes {
		fail("resident/retired bytes %d/%d, recomputed %d/%d", s.residentBytes, s.retiredBytes, resident, retired)
	}
	for e := range s.detached {
		if e.state != stateDetached || e.refs <= 0 || e.body != nil || e.fill < int64(len(e.snap.Body)) || e.expired {
			fail("detached entry %q: state %d refs %d fill %d expired %t", e.key, e.state, e.refs, e.fill, e.expired)
		}
		if cur, ok := s.index.Peek(e.key); ok && cur == e {
			fail("detached entry %q is indexed", e.key)
		}
		issued(e)
		detached += e.fill
	}
	if len(s.expirations) != len(kinds) {
		fail("expirations hold %d kinds, want %d", len(s.expirations), len(kinds))
	}
	for k, n := range expired {
		if n > s.expirations[k] {
			fail("%d live %s entries marked expired, %d counted", n, k, s.expirations[k])
		}
	}
	if detached != s.detachedBytes {
		fail("detached bytes %d, recomputed %d", s.detachedBytes, detached)
	}
	var reserved int64
	for r := range s.reservations {
		if r.store != s || r.done || r.size < 0 {
			fail("live reservation: foreign %t done %t size %d", r.store != s, r.done, r.size)
		}
		reserved += r.size
	}
	if s.fillBytes != reserved+s.detachedBytes {
		fail("fill bytes %d, recomputed %d reserved + %d detached", s.fillBytes, reserved, s.detachedBytes)
	}
	if charged := s.residentBytes + s.retiredBytes; charged > s.limits.MaxBytes {
		fail("charged bytes %d exceed max_bytes %d", charged, s.limits.MaxBytes)
	}
	if n := s.residentEntries + len(s.retired); n > s.limits.MaxEntries {
		fail("charged entries %d exceed max_entries %d", n, s.limits.MaxEntries)
	}
	if s.fillBytes > s.limits.MaxFillBytes {
		fail("fill bytes %d exceed max_fill_bytes %d", s.fillBytes, s.limits.MaxFillBytes)
	}
	if s.overflows != 0 {
		fail("simplelru evicted %d entries on its own", s.overflows)
	}
	return errors.Join(errs...)
}

var roomy = Limits{MaxBytes: 1 << 20, MaxObjectBytes: 1 << 10, MaxFillBytes: 1 << 10, MaxEntries: 100}

func newTestStore(t *testing.T, l Limits) *Store {
	t.Helper()
	s, err := NewStore(l)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func check(t *testing.T, s *Store) {
	t.Helper()
	if err := checkInvariants(s); err != nil {
		t.Fatal(err)
	}
}

func bodySnap(n int) Snapshot {
	return Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: int64(n)}, Body: make([]byte, n)}
}

func textSnap(s string) Snapshot {
	return Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: int64(len(s))}, Body: []byte(s)}
}

func charge(key string, snap Snapshot) int64 {
	return ownCharge(key, snap.Metadata) + int64(len(snap.Body))
}

// admit expects the key's current version, exercising capacity rather than
// compare-and-swap.
func admit(t *testing.T, s *Store, key string, snap Snapshot, want Result) {
	t.Helper()
	if got := s.Admit(key, version(s, key), snap); got != want {
		t.Fatalf("Admit(%q) = %s, want %s", key, got, want)
	}
	check(t, s)
}

// version is the key's indexed version, or Absent.
func version(s *Store, key string) Version {
	v, _, _ := s.Peek(key)
	return v
}

func remove(t *testing.T, s *Store, key string) {
	t.Helper()
	if !s.Remove(key, version(s, key)) {
		t.Fatalf("Remove(%q) missed", key)
	}
	check(t, s)
}

func reserve(t *testing.T, s *Store, n int64) *Reservation {
	t.Helper()
	r, res := s.Reserve(n)
	if res != ResultAdmitted || r == nil {
		t.Fatalf("Reserve(%d) = %v, %s", n, r, res)
	}
	check(t, s)
	return r
}

func acquire(t *testing.T, s *Store, key string) *Handle {
	t.Helper()
	h, ok := s.Acquire(key)
	if !ok {
		t.Fatalf("Acquire(%q) missed", key)
	}
	check(t, s)
	return h
}

func release(t *testing.T, s *Store, hs ...*Handle) {
	t.Helper()
	for _, h := range hs {
		h.Release()
	}
	check(t, s)
}

func keys(s *Store) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.index.Keys()
}
