package cache

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/storage"
	"github.com/hashicorp/golang-lru/v2/simplelru"
)

func TestNewStoreValidatesLimits(t *testing.T) {
	valid := Limits{MaxBytes: 100, MaxObjectBytes: 10, MaxFillBytes: 20, MaxEntries: 1}
	if _, err := NewStore(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Limits){
		"max_bytes":         func(l *Limits) { l.MaxBytes = 0 },
		"max_entries":       func(l *Limits) { l.MaxEntries = 0 },
		"max_object_bytes":  func(l *Limits) { l.MaxObjectBytes = -1 },
		"max_fill_bytes":    func(l *Limits) { l.MaxFillBytes = 0 },
		"object over bytes": func(l *Limits) { l.MaxFillBytes, l.MaxObjectBytes = 200, 101 },
		"object over fill":  func(l *Limits) { l.MaxObjectBytes = 21 },
	} {
		l := valid
		mutate(&l)
		if s, err := NewStore(l); err == nil || s != nil {
			t.Errorf("%s: NewStore(%+v) accepted invalid limits", name, l)
		}
	}
	got := LimitsFrom(config.Cache{Enabled: true, MaxBytes: 128 << 20, MaxEntries: 10000, MaxObjectBytes: 2 << 20, MaxFillBytes: 32 << 20, MaxConcurrentFills: 16})
	if want := (Limits{MaxBytes: 128 << 20, MaxEntries: 10000, MaxObjectBytes: 2 << 20, MaxFillBytes: 32 << 20}); got != want {
		t.Fatalf("LimitsFrom = %+v, want %+v", got, want)
	}
	if s := newTestStore(t, got); s.Stats().Limits != got {
		t.Fatal("Stats does not report the effective limits")
	}
}

func TestChargeCoversEntryMemory(t *testing.T) {
	var m storage.Metadata
	v := reflect.ValueOf(&m).Elem()
	want := int64(entryOverhead + len("key"))
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.String {
			f.SetString(strings.Repeat("x", i+1))
			want += int64(i + 1)
		}
	}
	if got := ownCharge("key", m); got != want {
		t.Fatalf("ownCharge = %d, want %d: a metadata string field is not charged", got, want)
	}
	// lruNode mirrors simplelru's internal list entry for these type arguments.
	type lruNode struct {
		next, prev, list unsafe.Pointer
		key              string
		value            *entry
		expiresAt        time.Time
		bucket           uint8
	}
	const mapSlot = 64 // string key, pointer value, control byte and load-factor slack
	if need := unsafe.Sizeof(entry{}) + unsafe.Sizeof(body{}) + unsafe.Sizeof(lruNode{}) + mapSlot; entryOverhead < need {
		t.Fatalf("entryOverhead %d below estimated per-entry memory %d", entryOverhead, need)
	}
	s := newTestStore(t, roomy)
	snap := bodySnap(10)
	snap.Metadata.ETag = `"abc"`
	admit(t, s, "k", snap, ResultAdmitted)
	if got := s.Stats(); got.ChargedBytes != entryOverhead+1+10+5 || got.ResidentBytes != got.ChargedBytes {
		t.Fatalf("stats = %+v", got)
	}
}

func TestAdmitExactByteBoundary(t *testing.T) {
	a, b := bodySnap(100), bodySnap(50)
	limit := charge("a", a) + charge("b", b)
	s := newTestStore(t, Limits{MaxBytes: limit, MaxObjectBytes: 100, MaxFillBytes: 100, MaxEntries: 10})
	admit(t, s, "a", a, ResultAdmitted)
	h := acquire(t, s, "a") // pinned: the remaining room cannot grow
	admit(t, s, "b", bodySnap(51), ResultNoSpace)
	admit(t, s, "b", b, ResultAdmitted)
	if got := s.Stats(); got.ChargedBytes != limit || got.ResidentEntries != 2 || got.Evictions[KindBody] != 0 {
		t.Fatalf("stats = %+v", got)
	}
	release(t, s, h)

	one := newTestStore(t, Limits{MaxBytes: charge("a", a), MaxObjectBytes: 101, MaxFillBytes: 101, MaxEntries: 1})
	admit(t, one, "a", a, ResultAdmitted)
	admit(t, one, "a", bodySnap(101), ResultTooLarge)
	if got := one.Stats(); got.ChargedBytes != charge("a", a) || got.Admissions[ResultTooLarge] != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestAdmitExactEntryBoundary(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 2})
	admit(t, s, "a", bodySnap(1), ResultAdmitted)
	admit(t, s, "b", bodySnap(1), ResultAdmitted)
	ha, hb := acquire(t, s, "a"), acquire(t, s, "b")
	admit(t, s, "c", bodySnap(1), ResultNoSpace)
	if got := keys(s); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("keys = %q", got)
	}
	release(t, s, hb)
	admit(t, s, "c", bodySnap(1), ResultAdmitted)
	if got := keys(s); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("keys = %q", got)
	}
	if got := s.Stats(); got.ResidentEntries != 2 || got.Evictions[KindBody] != 1 {
		t.Fatalf("stats = %+v", got)
	}
	release(t, s, ha)
}

func TestAdmitRejectsObjectsOverMaxObjectBytes(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 10})
	admit(t, s, "a", bodySnap(10), ResultAdmitted)
	admit(t, s, "b", bodySnap(11), ResultTooLarge)
	if _, ok := s.Acquire("b"); ok {
		t.Fatal("oversized object was indexed")
	}
	if got := s.Stats(); got.ResidentEntries != 1 || got.ChargedBytes != charge("a", bodySnap(10)) {
		t.Fatalf("stats = %+v", got)
	}
}

func TestAdmitRejectsInvalidSnapshots(t *testing.T) {
	s := newTestStore(t, roomy)
	cases := map[string]struct {
		key  string
		snap Snapshot
	}{
		"empty key":     {"", bodySnap(1)},
		"zero snapshot": {"k", Snapshot{}},
		"unknown kind":  {"k", Snapshot{Kind: "partial"}},
		"unknown size":  {"k", Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: -1}}},
		"short body":    {"k", Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: 3}, Body: []byte("ab")}},
		"metadata body": {"k", Snapshot{Kind: KindMetadata, Metadata: storage.Metadata{Size: 2}, Body: []byte("ab")}},
		"negative body": {"k", Snapshot{Kind: KindNegative, Body: []byte{}}},
		"long body":     {"k", Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: 0}, Body: []byte("x")}},
		"negative size": {"k", Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: -2}}},
	}
	for name, c := range cases {
		if got := s.Admit(c.key, Absent, c.snap); got != ResultInvalid {
			t.Errorf("%s: Admit = %s, want invalid", name, got)
		}
	}
	check(t, s)
	if got := s.Stats(); got.ChargedBytes != 0 || got.ResidentEntries != 0 || got.Admissions[ResultInvalid] != uint64(len(cases)) {
		t.Fatalf("stats = %+v", got)
	}
	admit(t, s, "empty", Snapshot{Kind: KindBody}, ResultAdmitted)
	h := acquire(t, s, "empty")
	if h.Snapshot().Body != nil {
		t.Fatal("empty object gained a body")
	}
	release(t, s, h)
}

func TestLRUOrderAndAcquireRecency(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 3})
	for _, k := range []string{"a", "b", "c"} {
		admit(t, s, k, bodySnap(1), ResultAdmitted)
	}
	release(t, s, acquire(t, s, "a"))
	if got := keys(s); !slices.Equal(got, []string{"b", "c", "a"}) {
		t.Fatalf("after Acquire keys = %q", got)
	}
	if _, ok := s.Acquire("missing"); ok {
		t.Fatal("missing key hit")
	}
	admit(t, s, "d", bodySnap(1), ResultAdmitted)
	if got := keys(s); !slices.Equal(got, []string{"c", "a", "d"}) {
		t.Fatalf("after eviction keys = %q", got)
	}
	if _, ok := s.Acquire("b"); ok {
		t.Fatal("least recently used entry survived")
	}
	admit(t, s, "c", bodySnap(2), ResultReplaced)
	if got := keys(s); !slices.Equal(got, []string{"a", "d", "c"}) {
		t.Fatalf("after replacement keys = %q", got)
	}
}

func TestEvictionSkipsPinnedEntries(t *testing.T) {
	unit := charge("a", bodySnap(100))
	s := newTestStore(t, Limits{MaxBytes: 3 * unit, MaxObjectBytes: 250, MaxFillBytes: 250, MaxEntries: 10})
	admit(t, s, "a", bodySnap(100), ResultAdmitted)
	ha := acquire(t, s, "a")
	admit(t, s, "b", bodySnap(100), ResultAdmitted)
	admit(t, s, "c", bodySnap(100), ResultAdmitted)
	admit(t, s, "d", bodySnap(100), ResultAdmitted) // oldest a is pinned: b goes, a is marked used
	if got := keys(s); !slices.Equal(got, []string{"c", "a", "d"}) {
		t.Fatalf("keys = %q", got)
	}
	admit(t, s, "e", bodySnap(200), ResultAdmitted) // needs both c and d
	if got := keys(s); !slices.Equal(got, []string{"a", "e"}) {
		t.Fatalf("keys = %q", got)
	}
	if got := s.Stats(); got.Evictions[KindBody] != 3 || got.ChargedBytes != unit+charge("e", bodySnap(200)) {
		t.Fatalf("stats = %+v", got)
	}
	release(t, s, ha)
}

func TestRejectedAdmissionEvictsNothing(t *testing.T) {
	unit := charge("a", bodySnap(100))
	s := newTestStore(t, Limits{MaxBytes: 3 * unit, MaxObjectBytes: 250, MaxFillBytes: 250, MaxEntries: 10})
	for _, k := range []string{"a", "b", "c"} {
		admit(t, s, k, bodySnap(100), ResultAdmitted)
	}
	ha, hb := acquire(t, s, "a"), acquire(t, s, "b")
	beforeKeys, before := keys(s), s.Stats()
	admit(t, s, "d", bodySnap(200), ResultNoSpace) // evicting c alone is not enough
	before.Admissions[ResultNoSpace]++
	if after := s.Stats(); !reflect.DeepEqual(after, before) {
		t.Fatalf("stats changed:\n got %+v\nwant %+v", after, before)
	}
	if got := keys(s); !slices.Equal(got, beforeKeys) {
		t.Fatalf("keys = %q, want %q", got, beforeKeys)
	}
	release(t, s, acquire(t, s, "c"), ha, hb)
}

func TestReplacePinnedEntryRetiresOld(t *testing.T) {
	s := newTestStore(t, roomy)
	old, next := textSnap("old!"), textSnap("new bytes")
	admit(t, s, "k", old, ResultAdmitted)
	h := acquire(t, s, "k")
	admit(t, s, "k", next, ResultReplaced)
	if got := string(h.Snapshot().Body); got != "old!" {
		t.Fatalf("old reader sees %q", got)
	}
	st := s.Stats()
	if st.RetiredEntries != 1 || st.RetiredBytes != charge("k", old) || st.ResidentEntries != 1 || st.ResidentBytes != charge("k", next) {
		t.Fatalf("stats = %+v", st)
	}
	h2 := acquire(t, s, "k")
	if got := string(h2.Snapshot().Body); got != "new bytes" {
		t.Fatalf("new reader sees %q", got)
	}
	release(t, s, h)
	if st := s.Stats(); st.RetiredEntries != 0 || st.RetiredBytes != 0 || st.ChargedBytes != charge("k", next) {
		t.Fatalf("stats after last release = %+v", st)
	}
	release(t, s, h2)

	// A pinned entry stays charged while replaced, so it needs a second slot.
	one := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 1})
	admit(t, one, "k", old, ResultAdmitted)
	h = acquire(t, one, "k")
	admit(t, one, "k", next, ResultNoSpace)
	cur := acquire(t, one, "k")
	if got := string(cur.Snapshot().Body); got != "old!" {
		t.Fatalf("rejected replacement changed the entry to %q", got)
	}
	release(t, one, cur, h)
}

func TestReplaceUnpinnedEntryFreesOld(t *testing.T) {
	old, next := textSnap("0123456789"), textSnap("abcdefghij")
	s := newTestStore(t, Limits{MaxBytes: charge("k", old), MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 1})
	admit(t, s, "k", old, ResultAdmitted)
	admit(t, s, "k", next, ResultReplaced)
	if st := s.Stats(); st.RetiredEntries != 0 || st.ChargedBytes != charge("k", next) || st.Evictions[KindBody] != 0 {
		t.Fatalf("stats = %+v", st)
	}
	h := acquire(t, s, "k")
	if got := string(h.Snapshot().Body); got != "abcdefghij" {
		t.Fatalf("reader sees %q", got)
	}
	release(t, s, h)
}

func TestRemovePinnedAndUnpinned(t *testing.T) {
	s := newTestStore(t, roomy)
	a := textSnap("aaaa")
	admit(t, s, "a", a, ResultAdmitted)
	admit(t, s, "b", textSnap("bb"), ResultAdmitted)
	ha := acquire(t, s, "a")
	remove(t, s, "a")
	if _, ok := s.Acquire("a"); ok {
		t.Fatal("removed entry still indexed")
	}
	remove(t, s, "b")
	if s.Remove("missing", Absent) || s.Remove("a", ha.Version()) {
		t.Fatal("Remove matched a key with no indexed entry")
	}
	check(t, s)
	if st := s.Stats(); st.RetiredEntries != 1 || st.ResidentEntries != 0 || st.ChargedBytes != charge("a", a) {
		t.Fatalf("stats = %+v", st)
	}
	if got := string(ha.Snapshot().Body); got != "aaaa" {
		t.Fatalf("retired reader sees %q", got)
	}
	release(t, s, ha)
	if st := s.Stats(); st.ChargedBytes != 0 || st.RetiredEntries != 0 || st.Evictions[KindBody] != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	s := newTestStore(t, roomy)
	admit(t, s, "k", textSnap("x"), ResultAdmitted)
	h := acquire(t, s, "k")
	h2 := h.Retain()
	release(t, s, h, h)
	remove(t, s, "k")
	if st := s.Stats(); st.RetiredEntries != 1 {
		t.Fatalf("double release dropped another owner's pin: %+v", st)
	}
	release(t, s, h2, h2)
	if st := s.Stats(); st.RetiredEntries != 0 || st.ChargedBytes != 0 {
		t.Fatalf("stats = %+v", st)
	}
	r := reserve(t, s, 10)
	r.Release()
	r.Release()
	check(t, s)
	if st := s.Stats(); st.FillBytes != 0 || st.Reservations != 0 {
		t.Fatalf("stats = %+v", st)
	}
	for name, use := range map[string]func(){
		"Retain":      func() { h.Retain() },
		"Snapshot":    func() { h.Snapshot() },
		"Version":     func() { h.Version() },
		"Refresh":     func() { s.Refresh(h, textSnap("x")) },
		"MarkExpired": func() { h.MarkExpired() },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s of a released handle did not panic", name)
				}
			}()
			use()
		}()
	}
	check(t, s)
}

func TestRefreshSwapsSnapshotSharingBody(t *testing.T) {
	s := newTestStore(t, roomy)
	t0, t1 := time.Unix(100, 0), time.Unix(200, 0)
	old := textSnap("hello")
	old.Metadata.ETag, old.RequestTime, old.ResponseTime = `"v1"`, t0, t0
	admit(t, s, "k", old, ResultAdmitted)
	h := acquire(t, s, "k")
	next := h.Snapshot()
	next.Metadata.CacheControl, next.ResponseTime = "max-age=60", t1
	nh, r := s.Refresh(h, next)
	check(t, s)
	if r != ResultRefreshed || nh == nil || nh.Version() == h.Version() || nh.Version() != version(s, "k") {
		t.Fatalf("Refresh = %v, %s", nh, r)
	}
	got, prev := nh.Snapshot(), h.Snapshot()
	if got.Metadata.CacheControl != "max-age=60" || !got.ResponseTime.Equal(t1) || unsafe.SliceData(got.Body) != unsafe.SliceData(old.Body) {
		t.Fatalf("refreshed snapshot = %+v", got)
	}
	if prev.Metadata.CacheControl != "" || !prev.ResponseTime.Equal(t0) || string(prev.Body) != "hello" {
		t.Fatalf("old reader sees %+v", prev)
	}
	oldOwn, newOwn := ownCharge("k", old.Metadata), ownCharge("k", next.Metadata)
	if st := s.Stats(); st.ResidentBytes != newOwn+5 || st.RetiredBytes != oldOwn || st.RetiredEntries != 1 || st.ChargedBytes != oldOwn+newOwn+5 {
		t.Fatalf("shared body charged twice or not at all: %+v", st)
	}
	if h2 := acquire(t, s, "k"); h2.Snapshot().Metadata.CacheControl != "max-age=60" {
		t.Fatal("lookup does not find the refreshed snapshot")
	} else {
		release(t, s, h2)
	}
	release(t, s, h)
	if st := s.Stats(); st.ChargedBytes != newOwn+5 || st.RetiredEntries != 0 {
		t.Fatalf("stats = %+v", st)
	}
	// Releasing the newest snapshot's readers first must keep shared bytes charged.
	again := nh.Snapshot()
	again.Body, again.Metadata.ETag = nil, `"v1-revalidated"`
	nh2, r := s.Refresh(nh, again)
	if r != ResultRefreshed || unsafe.SliceData(nh2.Snapshot().Body) != unsafe.SliceData(old.Body) {
		t.Fatalf("Refresh with nil Body = %s", r)
	}
	nh2.Release()
	remove(t, s, "k")
	if st := s.Stats(); st.RetiredBytes != ownCharge("k", next.Metadata)+5 || st.ResidentBytes != 0 {
		t.Fatalf("stats = %+v", st)
	}
	release(t, s, nh)
	if st := s.Stats(); st.ChargedBytes != 0 || s.Stats().Admissions[ResultRefreshed] != 2 {
		t.Fatalf("stats = %+v", st)
	}

	meta := newTestStore(t, roomy)
	admit(t, meta, "m", Snapshot{Kind: KindMetadata, Metadata: storage.Metadata{Size: 9}}, ResultAdmitted)
	hm := acquire(t, meta, "m")
	refreshed := Snapshot{Kind: KindMetadata, Metadata: storage.Metadata{Size: 9, ETag: `"m"`}}
	if nm, r := meta.Refresh(hm, refreshed); r != ResultRefreshed || nm.Snapshot().Metadata.ETag != `"m"` {
		t.Fatalf("metadata Refresh = %s", r)
	} else {
		release(t, meta, nm, hm)
	}
}

func TestRefreshRejectsConflictsAndInvalidSnapshots(t *testing.T) {
	s := newTestStore(t, roomy)
	other := newTestStore(t, roomy)
	admit(t, s, "k", textSnap("one"), ResultAdmitted)
	h := acquire(t, s, "k")
	admit(t, s, "k", textSnap("two"), ResultReplaced)
	before := s.Stats()
	if nh, r := s.Refresh(h, h.Snapshot()); r != ResultConflict || nh != nil {
		t.Fatalf("Refresh of a replaced entry = %v, %s", nh, r)
	}
	check(t, s)
	before.Admissions[ResultConflict]++
	if after := s.Stats(); !reflect.DeepEqual(after, before) {
		t.Fatalf("conflict changed stats:\n got %+v\nwant %+v", after, before)
	}
	h2 := acquire(t, s, "k")
	cur := h2.Snapshot()
	copied, kind, size := cur, cur, cur
	copied.Body = []byte("two")
	kind.Kind, kind.Body = KindMetadata, nil
	size.Metadata.Size = 4
	admit(t, other, "k", textSnap("two"), ResultAdmitted)
	foreign := acquire(t, other, "k")
	for name, c := range map[string]struct {
		h    *Handle
		snap Snapshot
	}{
		"different bytes": {h2, copied},
		"different kind":  {h2, kind},
		"wrong size":      {h2, size},
		"nil handle":      {nil, cur},
		"foreign handle":  {foreign, cur},
	} {
		if nh, r := s.Refresh(c.h, c.snap); r != ResultInvalid || nh != nil {
			t.Errorf("%s: Refresh = %v, %s", name, nh, r)
		}
	}
	check(t, s)
	remove(t, s, "k")
	if _, r := s.Refresh(h2, cur); r != ResultConflict {
		t.Fatalf("Refresh of a removed entry = %s", r)
	}
	release(t, s, h, h2)
	release(t, other, foreign)
}

func TestRefreshWithoutRoomChangesNothing(t *testing.T) {
	old := textSnap("hello")
	next := old
	next.Metadata.ETag = `"v2"`
	cases := map[string]struct {
		limits Limits
		want   Result
	}{
		"entry ceiling": {Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 1}, ResultNoSpace},
		"byte ceiling":  {Limits{MaxBytes: charge("k", old) + ownCharge("k", next.Metadata) - 1, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 10}, ResultNoSpace},
		"exact bytes":   {Limits{MaxBytes: charge("k", old) + ownCharge("k", next.Metadata), MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 10}, ResultRefreshed},
		"too large":     {Limits{MaxBytes: charge("k", next) - 1, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 10}, ResultTooLarge},
	}
	for name, c := range cases {
		s := newTestStore(t, c.limits)
		admit(t, s, "k", old, ResultAdmitted)
		h := acquire(t, s, "k")
		before := s.Stats()
		nh, r := s.Refresh(h, next)
		check(t, s)
		if r != c.want {
			t.Fatalf("%s: Refresh = %s, want %s", name, r, c.want)
		}
		if r == ResultRefreshed {
			if st := s.Stats(); st.ChargedBytes != c.limits.MaxBytes {
				t.Fatalf("%s: charged %d, want exactly %d", name, st.ChargedBytes, c.limits.MaxBytes)
			}
			release(t, s, nh, h)
			continue
		}
		before.Admissions[r]++
		if after := s.Stats(); nh != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("%s: failed Refresh changed state:\n got %+v\nwant %+v", name, after, before)
		}
		if cur := acquire(t, s, "k"); cur.Snapshot().Metadata.ETag != "" {
			t.Fatalf("%s: indexed snapshot changed", name)
		} else {
			release(t, s, cur, h)
		}
	}
}

func TestReserveBudgetBoundary(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 25, MaxEntries: 10})
	refuse := func(n int64, want Result) {
		t.Helper()
		if r, res := s.Reserve(n); res != want || r != nil {
			t.Fatalf("Reserve(%d) = %v, %s, want %s", n, r, res, want)
		}
		check(t, s)
	}
	r1, r2 := reserve(t, s, 10), reserve(t, s, 10)
	refuse(6, ResultNoSpace)
	r3 := reserve(t, s, 5)
	refuse(1, ResultNoSpace)
	r0 := reserve(t, s, 0)
	refuse(11, ResultTooLarge)
	refuse(-1, ResultInvalid)
	if st := s.Stats(); st.FillBytes != 25 || st.Reservations != 4 || r3.Size() != 5 {
		t.Fatalf("stats = %+v", st)
	}
	r1.Release()
	r4 := reserve(t, s, 10)
	for _, r := range []*Reservation{r0, r2, r3, r4} {
		r.Release()
	}
	check(t, s)
	if st := s.Stats(); st.FillBytes != 0 || st.Reservations != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPromoteAdmitsWithoutCopy(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 100, MaxFillBytes: 100, MaxEntries: 10})
	r := reserve(t, s, 100)
	buf := make([]byte, 60)
	snap := Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: 60}, Body: buf}
	h, res := s.Promote("k", Absent, r, snap)
	check(t, s)
	if res != ResultAdmitted || h == nil {
		t.Fatalf("Promote = %v, %s", h, res)
	}
	if st := s.Stats(); st.FillBytes != 0 || st.Reservations != 0 || st.ResidentBytes != charge("k", snap) || st.DetachedEntries != 0 {
		t.Fatalf("stats = %+v", st)
	}
	h2 := acquire(t, s, "k")
	if unsafe.SliceData(h.Snapshot().Body) != unsafe.SliceData(buf) || unsafe.SliceData(h2.Snapshot().Body) != unsafe.SliceData(buf) {
		t.Fatal("promotion copied the fill buffer")
	}
	if h.Version() != h2.Version() || h.Version() != version(s, "k") {
		t.Fatal("handles disagree on the indexed version")
	}
	r.Release() // consumed: a stray Release must not return the bytes twice
	check(t, s)
	if st := s.Stats(); st.FillBytes != 0 {
		t.Fatalf("fill bytes = %d", st.FillBytes)
	}
	if nh, res := s.Promote("k", h.Version(), r, snap); res != ResultInvalid || nh != nil {
		t.Fatalf("Promote with a consumed reservation = %s", res)
	}
	h3, res := s.Promote("k", h.Version(), reserve(t, s, 60), textSnap("fresh"))
	if res != ResultReplaced || h3.Version() == h.Version() {
		t.Fatalf("Promote over an entry = %s", res)
	}
	if st := s.Stats(); st.FillBytes != 0 || st.RetiredEntries != 1 || st.RetiredBytes != charge("k", snap) {
		t.Fatalf("stats = %+v", st)
	}
	release(t, s, h, h2, h3)
	if st := s.Stats(); st.ChargedBytes != charge("k", textSnap("fresh")) {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPromoteRejectionReturnsDetachedHandle(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 100, MaxFillBytes: 100, MaxEntries: 1})
	admit(t, s, "a", textSnap("a"), ResultAdmitted)
	ha := acquire(t, s, "a")
	resident := s.Stats().ResidentBytes
	r := reserve(t, s, 50)
	buf := make([]byte, 50)
	h, res := s.Promote("b", Absent, r, Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: 50}, Body: buf})
	check(t, s)
	if res != ResultNoSpace || h == nil || unsafe.SliceData(h.Snapshot().Body) != unsafe.SliceData(buf) {
		t.Fatalf("Promote = %v, %s", h, res)
	}
	if _, ok := s.Acquire("b"); ok {
		t.Fatal("detached entry is indexed")
	}
	if nh, res := s.Refresh(h, h.Snapshot()); res != ResultConflict || nh != nil {
		t.Fatalf("Refresh of a detached entry = %s", res)
	}
	if s.Remove("b", h.Version()) {
		t.Fatal("Remove matched a detached entry")
	}
	want := func(fill int64, detached int) {
		t.Helper()
		check(t, s)
		st := s.Stats()
		if st.FillBytes != fill || st.DetachedBytes != fill || st.DetachedEntries != detached || st.Reservations != 0 || st.ResidentBytes != resident {
			t.Fatalf("stats = %+v", st)
		}
	}
	want(50, 1)
	h2 := h.Retain()
	h3 := h2.Retain()
	r.Release() // consumed: a stray Release must not free the detached bytes
	h.Release()
	h3.Release()
	want(50, 1)
	h2.Release()
	want(0, 0)
	release(t, s, ha)

	small := newTestStore(t, Limits{MaxBytes: 700, MaxObjectBytes: 100, MaxFillBytes: 100, MaxEntries: 10})
	h, res = small.Promote("k", Absent, reserve(t, small, 100), bodySnap(100)) // charge 741 exceeds max_bytes
	if res != ResultTooLarge || h == nil {
		t.Fatalf("Promote = %v, %s", h, res)
	}
	if st := small.Stats(); st.DetachedBytes != 100 || st.ChargedBytes != 0 || st.Admissions[ResultTooLarge] != 1 {
		t.Fatalf("stats = %+v", st)
	}
	release(t, small, h)
	if st := small.Stats(); st.FillBytes != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPromoteInvalidConsumesReservation(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 20, MaxFillBytes: 20, MaxEntries: 10})
	other := newTestStore(t, roomy)
	consumed := func(name string, s *Store) {
		t.Helper()
		check(t, s)
		if st := s.Stats(); st.FillBytes != 0 || st.Reservations != 0 || st.ResidentEntries != 0 {
			t.Fatalf("%s: reservation not consumed: %+v", name, st)
		}
	}
	for name, c := range map[string]struct {
		key  string
		snap Snapshot
	}{
		"over reservation":  {"k", bodySnap(11)},
		"empty key":         {"", bodySnap(5)},
		"metadata snapshot": {"k", Snapshot{Kind: KindMetadata}},
		"invalid snapshot":  {"k", Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: 2}}},
	} {
		r := reserve(t, s, 10)
		if h, res := s.Promote(c.key, Absent, r, c.snap); res != ResultInvalid || h != nil {
			t.Fatalf("%s: Promote = %v, %s", name, h, res)
		}
		consumed(name, s)
		r.Release()
		consumed(name+" then Release", s)
	}
	if h, res := s.Promote("k", Absent, reserve(t, other, 10), bodySnap(5)); res != ResultInvalid || h != nil {
		t.Fatalf("Promote with a foreign reservation = %v, %s", h, res)
	}
	consumed("foreign reservation", other)
	if h, res := s.Promote("k", Absent, nil, bodySnap(5)); res != ResultInvalid || h != nil {
		t.Fatalf("Promote without a reservation = %v, %s", h, res)
	}
	gone := reserve(t, s, 5)
	gone.Release()
	if h, res := s.Promote("k", Absent, gone, bodySnap(5)); res != ResultInvalid || h != nil {
		t.Fatalf("Promote with a released reservation = %s", res)
	}
	consumed("released reservation", s)
	h, res := s.Promote("k", Absent, reserve(t, s, 10), bodySnap(10))
	if res != ResultAdmitted {
		t.Fatalf("Promote = %s", res)
	}
	release(t, s, h)
	if st := s.Stats(); st.FillBytes != 0 || st.Admissions[ResultInvalid] != 7 || st.Admissions[ResultAdmitted] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestAdmitCompareAndSwap(t *testing.T) {
	s := newTestStore(t, roomy)
	conflicts := func(name string, expected Version) {
		t.Helper()
		beforeKeys, before := keys(s), s.Stats()
		if got := s.Admit("k", expected, Snapshot{Kind: KindMetadata}); got != ResultConflict {
			t.Fatalf("%s: Admit = %s, want conflict", name, got)
		}
		check(t, s)
		before.Admissions[ResultConflict]++
		if after := s.Stats(); !reflect.DeepEqual(after, before) || !slices.Equal(keys(s), beforeKeys) {
			t.Fatalf("%s: conflict changed state:\n got %+v\nwant %+v", name, after, before)
		}
	}
	conflicts("version for an absent key", 42)
	if got := s.Admit("k", Absent, textSnap("one")); got != ResultAdmitted {
		t.Fatalf("Admit = %s", got)
	}
	admit(t, s, "j", textSnap("j"), ResultAdmitted)
	v1 := version(s, "k")
	conflicts("absent over an indexed entry", Absent) // e.g. a late HEAD result over a promoted body
	if got := s.Admit("k", v1, textSnap("two")); got != ResultReplaced {
		t.Fatalf("Admit = %s", got)
	}
	v2 := version(s, "k")
	if v2 == v1 {
		t.Fatalf("replacement kept version %d", v1)
	}
	conflicts("stale after replacement", v1)
	h := acquire(t, s, "k")
	nh, r := s.Refresh(h, h.Snapshot())
	if r != ResultRefreshed || nh.Version() == v2 {
		t.Fatalf("Refresh = %s", r)
	}
	v3 := nh.Version()
	release(t, s, h, nh)
	conflicts("stale after refresh", v2)
	if got := s.Admit("k", v3, Snapshot{Kind: KindNegative}); got != ResultReplaced {
		t.Fatalf("Admit = %s", got)
	}
	v4 := version(s, "k")
	remove(t, s, "k")
	conflicts("stale after removal", v4)
	if got := s.Admit("k", Absent, textSnap("three")); got != ResultAdmitted {
		t.Fatalf("Admit = %s", got)
	}
	check(t, s)
}

func TestRemoveCompareAndSwap(t *testing.T) {
	s := newTestStore(t, roomy)
	admit(t, s, "k", textSnap("one"), ResultAdmitted)
	stale := version(s, "k")
	admit(t, s, "k", textSnap("two"), ResultReplaced)
	admit(t, s, "j", textSnap("j"), ResultAdmitted)
	beforeKeys, before := keys(s), s.Stats()
	for _, v := range []Version{Absent, stale, version(s, "j"), version(s, "j") + 100} {
		if s.Remove("k", v) {
			t.Fatalf("Remove(k, %d) removed the newer entry", v)
		}
	}
	check(t, s)
	if after := s.Stats(); !reflect.DeepEqual(after, before) || !slices.Equal(keys(s), beforeKeys) {
		t.Fatalf("failed Remove changed state:\n got %+v\nwant %+v", after, before)
	}
	h := acquire(t, s, "k")
	if !s.Remove("k", h.Version()) || s.Remove("k", h.Version()) {
		t.Fatal("Remove with the indexed version did not remove exactly once")
	}
	check(t, s)
	if st := s.Stats(); st.RetiredEntries != 1 || st.ResidentEntries != 1 {
		t.Fatalf("stats = %+v", st)
	}
	release(t, s, h)
}

func TestPinnedTailMovesToFront(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 4})
	admit(t, s, "a", bodySnap(1), ResultAdmitted)
	admit(t, s, "b", bodySnap(1), ResultAdmitted)
	ha, hb := acquire(t, s, "a"), acquire(t, s, "b")
	admit(t, s, "c", bodySnap(1), ResultAdmitted)
	admit(t, s, "d", bodySnap(1), ResultAdmitted)
	if got := keys(s); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Fatalf("keys = %q", got)
	}
	admit(t, s, "e", bodySnap(1), ResultAdmitted) // skips pinned a and b, evicts c
	if got := keys(s); !slices.Equal(got, []string{"d", "a", "b", "e"}) {
		t.Fatalf("pinned tail not moved to the front: keys = %q", got)
	}
	admit(t, s, "f", bodySnap(1), ResultAdmitted) // the oldest entry is now evictable
	if got := keys(s); !slices.Equal(got, []string{"a", "b", "e", "f"}) {
		t.Fatalf("keys = %q", got)
	}
	if st := s.Stats(); st.Evictions[KindBody] != 2 {
		t.Fatalf("stats = %+v", st)
	}
	release(t, s, ha, hb)
}

func TestVersionsSkipAbsentAndDifferAcrossStores(t *testing.T) {
	s := newTestStore(t, roomy)
	s.mu.Lock()
	s.lastVersion = ^Version(0) - 1
	s.mu.Unlock()
	admit(t, s, "a", textSnap("a"), ResultAdmitted)
	admit(t, s, "b", textSnap("b"), ResultAdmitted)
	if va, vb := version(s, "a"), version(s, "b"); va != ^Version(0) || vb != 1 {
		t.Fatalf("versions around wraparound = %d, %d, want %d, 1", va, vb, ^Version(0))
	}
	// Independent random seeds: a collision has odds of 2^-64.
	one, two := newTestStore(t, roomy), newTestStore(t, roomy)
	admit(t, one, "k", textSnap("k"), ResultAdmitted)
	admit(t, two, "k", textSnap("k"), ResultAdmitted)
	v := version(one, "k")
	if v == version(two, "k") || two.Remove("k", v) || two.Admit("k", v, textSnap("x")) != ResultConflict {
		t.Fatal("a version issued by one Store matched in another")
	}
	check(t, two)
}

func TestPeekLeavesStateUnchanged(t *testing.T) {
	s := newTestStore(t, roomy)
	admit(t, s, "a", Snapshot{Kind: KindMetadata}, ResultAdmitted)
	admit(t, s, "b", textSnap("b"), ResultAdmitted)
	beforeKeys, before := keys(s), s.Stats()
	va, kind, ok := s.Peek("a")
	if !ok || kind != KindMetadata || va == Absent {
		t.Fatalf("Peek(a) = %d, %q, %t", va, kind, ok)
	}
	vb, kind, ok := s.Peek("b")
	if !ok || kind != KindBody || vb == va {
		t.Fatalf("Peek(b) = %d, %q, %t", vb, kind, ok)
	}
	if v, kind, ok := s.Peek("missing"); ok || v != Absent || kind != "" {
		t.Fatalf("Peek(missing) = %d, %q, %t", v, kind, ok)
	}
	if allocs := testing.AllocsPerRun(100, func() { s.Peek("a") }); allocs != 0 {
		t.Fatalf("Peek allocates %.0f times", allocs)
	}
	check(t, s)
	if after := s.Stats(); !reflect.DeepEqual(after, before) || !slices.Equal(keys(s), beforeKeys) {
		t.Fatalf("Peek changed state:\n got %+v\nwant %+v", after, before)
	}
	h := acquire(t, s, "b")
	if h.Version() != vb {
		t.Fatalf("Peek version %d, handle version %d", vb, h.Version())
	}
	release(t, s, h)
	remove(t, s, "a") // Peek left no pin: a is freed, not retired
	if st := s.Stats(); st.RetiredEntries != 0 || st.ResidentEntries != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestPromoteConflictReturnsDetachedHandle(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 100, MaxFillBytes: 100, MaxEntries: 10})
	admit(t, s, "k", textSnap("winner"), ResultAdmitted) // indexed while our fill ran
	beforeKeys, resident := keys(s), s.Stats().ResidentBytes
	buf := make([]byte, 40)
	h, res := s.Promote("k", Absent, reserve(t, s, 40), Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: 40}, Body: buf})
	check(t, s)
	if res != ResultConflict || h == nil || unsafe.SliceData(h.Snapshot().Body) != unsafe.SliceData(buf) {
		t.Fatalf("Promote = %v, %s", h, res)
	}
	if st := s.Stats(); st.FillBytes != 40 || st.DetachedBytes != 40 || st.DetachedEntries != 1 || st.Reservations != 0 || st.ResidentBytes != resident {
		t.Fatalf("stats = %+v", st)
	}
	if got := keys(s); !slices.Equal(got, beforeKeys) {
		t.Fatalf("keys = %q, want %q", got, beforeKeys)
	}
	cur := acquire(t, s, "k")
	if got := string(cur.Snapshot().Body); got != "winner" {
		t.Fatalf("conflicting Promote replaced the entry with %q", got)
	}
	stale := cur.Version()
	release(t, s, cur)
	admit(t, s, "k", textSnap("newer"), ResultReplaced)
	h2, res := s.Promote("k", stale, reserve(t, s, 10), textSnap("late"))
	if res != ResultConflict || h2 == nil {
		t.Fatalf("Promote with a stale version = %v, %s", h2, res)
	}
	h3, res := s.Promote("k", version(s, "k"), reserve(t, s, 10), textSnap("fresh"))
	if res != ResultReplaced {
		t.Fatalf("Promote with the indexed version = %s", res)
	}
	if st := s.Stats(); st.FillBytes != 50 || st.DetachedEntries != 2 {
		t.Fatalf("stats = %+v", st)
	}
	release(t, s, h, h2, h3)
	if st := s.Stats(); st.FillBytes != 0 || st.DetachedEntries != 0 || st.Admissions[ResultConflict] != 2 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestMetadataAndNegativeEntriesAreCharged(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 1 << 20, MaxObjectBytes: 10, MaxFillBytes: 10, MaxEntries: 3})
	m := Snapshot{Kind: KindMetadata, Metadata: storage.Metadata{Size: 1234, ETag: `"m"`, ContentType: "text/html"}}
	n := Snapshot{Kind: KindNegative}
	b := textSnap("body")
	admit(t, s, "m", m, ResultAdmitted)
	admit(t, s, "n", n, ResultAdmitted)
	admit(t, s, "b", b, ResultAdmitted)
	if st := s.Stats(); st.ResidentEntries != 3 || st.ResidentBytes != charge("m", m)+charge("n", n)+charge("b", b) {
		t.Fatalf("stats = %+v", st)
	}
	for _, k := range []string{"x", "y", "z"} {
		admit(t, s, k, Snapshot{Kind: KindNegative}, ResultAdmitted)
	}
	want := map[Kind]uint64{KindBody: 1, KindMetadata: 1, KindNegative: 1}
	if st := s.Stats(); !reflect.DeepEqual(st.Evictions, want) {
		t.Fatalf("evictions = %v, want %v", st.Evictions, want)
	}
	h := acquire(t, s, "x")
	if got := h.Snapshot(); got.Kind != KindNegative || got.Body != nil {
		t.Fatalf("snapshot = %+v", got)
	}
	release(t, s, h)
}

func TestInternalLRUEvictionIsRetiredAndDetected(t *testing.T) {
	s := newTestStore(t, roomy)
	s.index, _ = simplelru.NewLRU(1, s.evicted) // break the capacity invariant on purpose
	admit(t, s, "a", textSnap("a"), ResultAdmitted)
	ha := acquire(t, s, "a")
	if got := s.Admit("b", Absent, textSnap("b")); got != ResultAdmitted {
		t.Fatalf("Admit = %s", got)
	}
	if err := checkInvariants(s); err == nil || !strings.Contains(err.Error(), "simplelru evicted 1") {
		t.Fatalf("invariant checker missed the internal eviction: %v", err)
	}
	s.mu.Lock()
	s.overflows = 0
	s.mu.Unlock()
	check(t, s) // the evicted entry was still retired with correct accounting
	if st := s.Stats(); st.RetiredEntries != 1 || st.ResidentEntries != 1 {
		t.Fatalf("stats = %+v", st)
	}
	release(t, s, ha)
	if st := s.Stats(); st.ChargedBytes != charge("b", textSnap("b")) {
		t.Fatalf("stats = %+v", st)
	}
}

func TestConcurrentOwnership(t *testing.T) {
	s := newTestStore(t, Limits{MaxBytes: 6 * (entryOverhead + 72), MaxObjectBytes: 64, MaxFillBytes: 160, MaxEntries: 6})
	keySpace := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"}
	snapshot := func(rng *rand.Rand) Snapshot {
		m := storage.Metadata{ETag: strings.Repeat("e", rng.IntN(8))}
		switch rng.IntN(3) {
		case 0:
			return Snapshot{Kind: KindNegative, Metadata: m}
		case 1:
			return Snapshot{Kind: KindMetadata, Metadata: m}
		}
		m.Size = rng.Int64N(80)
		return Snapshot{Kind: KindBody, Metadata: m, Body: make([]byte, m.Size)}
	}
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(g), 7))
			var held []*Handle
			drop := func(i int) {
				held[i].Release()
				held[i].Release()
				held = slices.Delete(held, i, i+1)
			}
			// expect mixes possibly stale versions, Absent and the current one.
			expect := func(key string) Version {
				switch rng.IntN(3) {
				case 0:
					return Absent
				case 1:
					if len(held) > 0 {
						return held[rng.IntN(len(held))].Version()
					}
				}
				return version(s, key)
			}
			for i := range 3000 {
				key := keySpace[rng.IntN(len(keySpace))]
				switch rng.IntN(7) {
				case 0, 1:
					if h, ok := s.Acquire(key); ok {
						held = append(held, h)
					}
					// Mark an indexed, retired or detached entry expired.
					if len(held) > 0 && rng.IntN(2) == 0 {
						held[rng.IntN(len(held))].MarkExpired()
					}
				case 2:
					s.Admit(key, expect(key), snapshot(rng))
				case 3:
					s.Remove(key, expect(key))
				case 4:
					n := rng.Int64N(65)
					r, _ := s.Reserve(n)
					if r == nil {
						break
					}
					size := rng.Int64N(n + 1)
					if h, _ := s.Promote(key, expect(key), r, Snapshot{Kind: KindBody, Metadata: storage.Metadata{Size: size}, Body: make([]byte, size)}); h != nil {
						held = append(held, h, h.Retain())
					}
				case 5:
					if len(held) > 0 {
						h := held[rng.IntN(len(held))]
						next := h.Snapshot()
						next.ResponseTime = time.Unix(int64(i), 0)
						if nh, _ := s.Refresh(h, next); nh != nil {
							held = append(held, nh)
						}
					}
				case 6:
					if len(held) > 0 {
						drop(rng.IntN(len(held)))
					}
				}
				for len(held) > 6 {
					drop(0)
				}
				if i%500 == 0 {
					if err := checkInvariants(s); err != nil {
						t.Error(err)
						return
					}
				}
			}
			for len(held) > 0 {
				drop(0)
			}
		})
	}
	wg.Wait()
	check(t, s)
	st := s.Stats()
	if st.RetiredBytes != 0 || st.RetiredEntries != 0 || st.FillBytes != 0 || st.DetachedBytes != 0 || st.DetachedEntries != 0 || st.Reservations != 0 {
		t.Fatalf("handles released but charges remain: %+v", st)
	}
	for _, r := range []Result{ResultAdmitted, ResultReplaced, ResultRefreshed, ResultNoSpace, ResultConflict} {
		if st.Admissions[r] == 0 {
			t.Fatalf("stress run never produced %s: %v", r, st.Admissions)
		}
	}
	for _, k := range kinds {
		if st.Expirations[k] == 0 {
			t.Fatalf("stress run never expired a %s entry: %v", k, st.Expirations)
		}
	}
	// A leaked pin on an indexed entry would leave it retired after removal.
	for _, k := range keySpace {
		if h, ok := s.Acquire(k); ok {
			v := h.Version()
			h.Release()
			if !s.Remove(k, v) {
				t.Fatalf("Remove(%q, %d) missed after the run", k, v)
			}
		}
	}
	check(t, s)
	if st := s.Stats(); st.ChargedBytes != 0 || st.ResidentEntries != 0 || st.RetiredEntries != 0 || st.FillBytes != 0 || st.DetachedEntries != 0 {
		t.Fatalf("charges remain after removing every key: %+v", st)
	}
}

// BenchmarkAdmitUnderPressure admits into a full 10000-entry cache, so every
// admission must evict. oldest_unpinned is the O(1) steady state; pinned_tail
// starts with a pinned oldest entry, whose walk a committed admission pays
// once; all_pinned_rejected walks the whole index and rejects every time.
func BenchmarkAdmitUnderPressure(b *testing.B) {
	const n = 10000
	ring := make([]string, 2*n) // a key returns only after it was evicted
	for i := range ring {
		ring[i] = "key-" + strconv.Itoa(i)
	}
	for _, c := range []struct {
		name   string
		pinned int
		want   Result
	}{
		{"oldest_unpinned", 0, ResultAdmitted},
		{"pinned_tail", 1, ResultAdmitted},
		{"all_pinned_rejected", n, ResultNoSpace},
	} {
		b.Run(c.name, func(b *testing.B) {
			s, err := NewStore(Limits{MaxBytes: 1 << 30, MaxObjectBytes: 1 << 20, MaxFillBytes: 1 << 20, MaxEntries: n})
			if err != nil {
				b.Fatal(err)
			}
			snap := Snapshot{Kind: KindNegative}
			var pins []*Handle
			for i := range c.pinned {
				key := "pinned-" + strconv.Itoa(i)
				s.Admit(key, Absent, snap)
				h, _ := s.Acquire(key)
				pins = append(pins, h)
			}
			filled := n - c.pinned
			for i := range filled {
				s.Admit(ring[i], Absent, snap)
			}
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				if r := s.Admit(ring[(filled+i)%len(ring)], Absent, snap); r != c.want {
					b.Fatalf("Admit = %s, want %s", r, c.want)
				}
			}
			for _, h := range pins {
				h.Release()
			}
		})
	}
}
