package cache

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/config"
	"github.com/gtriggiano/statica/internal/httpcache"
	"github.com/gtriggiano/statica/internal/storage"
)

var (
	policyT0   = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	testPolicy = config.CachePolicy{Enabled: true, TTL: 5 * time.Minute, NegativeTTL: 5 * time.Second, StaleIfError: time.Minute}
)

// policySnap is a snapshot of kind requested and received at policyT0.
func policySnap(kind Kind, m storage.Metadata) Snapshot {
	s := Snapshot{Kind: kind, Metadata: m, RequestTime: policyT0, ResponseTime: policyT0}
	if kind == KindBody {
		s.Body = []byte{}
	}
	return s
}

func originCC(cacheControl string) storage.Metadata {
	return storage.Metadata{CacheControl: cacheControl}
}
func originDate(d time.Duration) string { return policyT0.Add(d).Format(http.TimeFormat) }

// unstamped is policySnap with the request or response time cleared.
func unstamped(kind Kind, clearRequest, clearResponse bool) Snapshot {
	s := policySnap(kind, storage.Metadata{})
	if clearRequest {
		s.RequestTime = time.Time{}
	}
	if clearResponse {
		s.ResponseTime = time.Time{}
	}
	return s
}

func maxAge(n int64) httpcache.Request {
	return httpcache.Request{MaxAge: httpcache.Delta{Seconds: n, Present: true}}
}

func policyWith(p config.CachePolicy, mutate func(*config.CachePolicy)) config.CachePolicy {
	mutate(&p)
	return p
}

func TestAdmissible(t *testing.T) {
	disabled := policyWith(testPolicy, func(p *config.CachePolicy) { p.Enabled = false })
	noNegative := policyWith(testPolicy, func(p *config.CachePolicy) { p.NegativeTTL = 0 })
	zeroTTL := policyWith(testPolicy, func(p *config.CachePolicy) { p.TTL = 0 })
	noStore := httpcache.Request{NoStore: true}
	for _, tc := range []struct {
		name string
		p    config.CachePolicy
		req  httpcache.Request
		s    Snapshot
		want Admission
	}{
		{"plain body", testPolicy, httpcache.Request{}, policySnap(KindBody, storage.Metadata{}), AdmissionEligible},
		{"metadata only", testPolicy, httpcache.Request{}, policySnap(KindMetadata, originCC("public, max-age=60")), AdmissionEligible},
		{"origin no-cache", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("no-cache")), AdmissionEligible},
		{"origin must-revalidate", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("must-revalidate")), AdmissionEligible},
		{"zero ttl", zeroTTL, httpcache.Request{}, policySnap(KindBody, storage.Metadata{}), AdmissionEligible},
		{"saturated max-age", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("max-age=99999999999999999999")), AdmissionEligible},
		{"negative", testPolicy, httpcache.Request{}, policySnap(KindNegative, storage.Metadata{}), AdmissionEligible},
		{"disabled", disabled, httpcache.Request{}, policySnap(KindBody, storage.Metadata{}), AdmissionDisabled},
		{"disabled negative", disabled, httpcache.Request{}, policySnap(KindNegative, storage.Metadata{}), AdmissionDisabled},
		{"request no-store", testPolicy, noStore, policySnap(KindBody, storage.Metadata{}), AdmissionRequestNoStore},
		{"unknown kind", testPolicy, httpcache.Request{}, policySnap(Kind("partial"), storage.Metadata{}), AdmissionInvalid},
		{"malformed directives", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("max-age=-1")), AdmissionMalformed},
		{"malformed no-store", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("no-store, no-store")), AdmissionMalformed},
		{"malformed expires", testPolicy, httpcache.Request{}, policySnap(KindBody, storage.Metadata{Expires: "tomorrow"}), AdmissionMalformed},
		{"malformed date", testPolicy, httpcache.Request{}, policySnap(KindMetadata, storage.Metadata{RawDate: "yesterday"}), AdmissionMalformed},
		{"malformed age", testPolicy, httpcache.Request{}, policySnap(KindBody, storage.Metadata{RawAge: "-1"}), AdmissionMalformed},
		{"negative decoded age", testPolicy, httpcache.Request{}, policySnap(KindBody, storage.Metadata{Age: -time.Second}), AdmissionMalformed},
		{"origin no-store", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("public, no-store")), AdmissionNoStore},
		{"no-store before private", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("private, no-store")), AdmissionNoStore},
		{"origin private", testPolicy, httpcache.Request{}, policySnap(KindBody, originCC("private, max-age=60")), AdmissionPrivate},
		{"qualified private", testPolicy, httpcache.Request{}, policySnap(KindMetadata, originCC(`private="Set-Cookie"`)), AdmissionPrivate},
		{"zero negative ttl", noNegative, httpcache.Request{}, policySnap(KindNegative, storage.Metadata{}), AdmissionNegativeTTL},
		{"zero negative ttl spares positives", noNegative, httpcache.Request{}, policySnap(KindBody, storage.Metadata{}), AdmissionEligible},
		{"body without request time", testPolicy, httpcache.Request{}, unstamped(KindBody, true, false), AdmissionUnstamped},
		{"body without response time", testPolicy, httpcache.Request{}, unstamped(KindBody, false, true), AdmissionUnstamped},
		{"metadata without request time", testPolicy, httpcache.Request{}, unstamped(KindMetadata, true, false), AdmissionUnstamped},
		{"negative without response time", testPolicy, httpcache.Request{}, unstamped(KindNegative, false, true), AdmissionUnstamped},
		{"negative needs no request time", testPolicy, httpcache.Request{}, unstamped(KindNegative, true, false), AdmissionEligible},
	} {
		if got := Admissible(tc.p, tc.req, tc.s); got != tc.want {
			t.Errorf("%s: Admissible = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestEffectiveLifetime(t *testing.T) {
	zeroTTL := policyWith(testPolicy, func(p *config.CachePolicy) { p.TTL = 0 })
	for _, tc := range []struct {
		name string
		p    config.CachePolicy
		kind Kind
		m    storage.Metadata
		want time.Duration
	}{
		{"ttl without origin lifetime", testPolicy, KindBody, storage.Metadata{}, 5 * time.Minute},
		{"origin max-age caps ttl", testPolicy, KindBody, originCC("max-age=60"), time.Minute},
		{"origin max-age never extends ttl", testPolicy, KindBody, originCC("public, max-age=3600"), 5 * time.Minute},
		{"origin s-maxage over max-age", testPolicy, KindBody, originCC("max-age=3600, s-maxage=30"), 30 * time.Second},
		{"origin expires caps ttl", testPolicy, KindBody, storage.Metadata{RawDate: originDate(0), Expires: originDate(time.Minute)}, time.Minute},
		{"origin expires never extends ttl", testPolicy, KindBody, storage.Metadata{RawDate: originDate(0), Expires: originDate(time.Hour)}, 5 * time.Minute},
		{"origin past expires", testPolicy, KindBody, storage.Metadata{RawDate: originDate(0), Expires: originDate(-time.Hour)}, 0},
		{"origin max-age zero", testPolicy, KindBody, originCC("max-age=0"), 0},
		{"origin no-cache", testPolicy, KindBody, originCC("no-cache, max-age=3600"), 0},
		{"zero ttl", zeroTTL, KindBody, storage.Metadata{}, 0},
		{"zero ttl with origin lifetime", zeroTTL, KindBody, originCC("max-age=3600"), 0},
		{"metadata only", testPolicy, KindMetadata, originCC("max-age=90"), 90 * time.Second},
		{"negative uses negative ttl", zeroTTL, KindNegative, storage.Metadata{}, 5 * time.Second},
	} {
		f := Evaluate(tc.p, httpcache.Request{}, policySnap(tc.kind, tc.m), policyT0)
		wantState := StateStale
		if tc.want > 0 {
			wantState = StateFresh
		}
		if f.Lifetime != tc.want || f.Age != 0 || f.State != wantState || f.Remaining != tc.want {
			t.Errorf("%s: Evaluate = %+v, want lifetime %v and %s", tc.name, f, tc.want, wantState)
		}
	}
}

func TestEvaluateFreshnessBoundary(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    storage.Metadata
		at   time.Duration
		want Freshness
	}{
		{"new hit keeps remaining", storage.Metadata{}, 2 * time.Minute, Freshness{StateFresh, 2 * time.Minute, 5 * time.Minute, 3 * time.Minute}},
		{"just before lifetime", storage.Metadata{}, 5*time.Minute - 1, Freshness{StateFresh, 5*time.Minute - 1, 5 * time.Minute, 1}},
		{"age equals lifetime", storage.Metadata{}, 5 * time.Minute, Freshness{StateStale, 5 * time.Minute, 5 * time.Minute, 0}},
		{"past lifetime", storage.Metadata{}, 5*time.Minute + time.Second, Freshness{StateStale, 5*time.Minute + time.Second, 5 * time.Minute, 0}},
		{"origin age counts", storage.Metadata{RawAge: "60"}, 4 * time.Minute, Freshness{StateStale, 5 * time.Minute, 5 * time.Minute, 0}},
		{"origin date counts", storage.Metadata{RawDate: originDate(-4 * time.Minute)}, 59 * time.Second, Freshness{StateFresh, 4*time.Minute + 59*time.Second, 5 * time.Minute, time.Second}},
		{"clock behind response", storage.Metadata{}, -time.Hour, Freshness{StateFresh, 0, 5 * time.Minute, 5 * time.Minute}},
	} {
		if got := Evaluate(testPolicy, httpcache.Request{}, policySnap(KindBody, tc.m), policyT0.Add(tc.at)); got != tc.want {
			t.Errorf("%s: Evaluate = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	unset := unstamped(KindBody, true, false)
	if got := Evaluate(testPolicy, httpcache.Request{}, unset, policyT0); got != (Freshness{State: StateStale}) {
		t.Fatalf("snapshot without a request time is reusable: %+v", got)
	}
	if StaleIfError(testPolicy, httpcache.Request{}, unset, policyT0.Add(5*time.Minute+time.Second), context.DeadlineExceeded) {
		t.Fatal("snapshot without a request time served stale")
	}
}

func TestEvaluateRequestDirectives(t *testing.T) {
	disabled := policyWith(testPolicy, func(p *config.CachePolicy) { p.Enabled = false })
	for _, tc := range []struct {
		name string
		p    config.CachePolicy
		req  httpcache.Request
		at   time.Duration
		want State
	}{
		{"none", testPolicy, httpcache.Request{}, time.Minute, StateFresh},
		{"no-cache", testPolicy, httpcache.Request{NoCache: true}, time.Minute, StateStale},
		{"max-age zero", testPolicy, maxAge(0), 0, StateStale},
		{"max-age at age", testPolicy, maxAge(60), time.Minute, StateFresh},
		{"max-age within the second", testPolicy, maxAge(60), 60*time.Second + 500*time.Millisecond, StateFresh},
		{"max-age below age", testPolicy, maxAge(60), 61 * time.Second, StateStale},
		{"max-age never extends", testPolicy, maxAge(600), 5 * time.Minute, StateStale},
		{"no-store", testPolicy, httpcache.Request{NoStore: true}, time.Minute, StateBypass},
		{"no-store with no-cache", testPolicy, httpcache.Request{NoStore: true, NoCache: true}, time.Minute, StateBypass},
		{"disabled", disabled, httpcache.Request{}, time.Minute, StateBypass},
	} {
		f := Evaluate(tc.p, tc.req, policySnap(KindBody, storage.Metadata{}), policyT0.Add(tc.at))
		if f.State != tc.want {
			t.Errorf("%s: state %s, want %s", tc.name, f.State, tc.want)
		}
		switch {
		case tc.want == StateBypass && f != Freshness{State: StateBypass}:
			t.Errorf("%s: bypass reports %+v", tc.name, f)
		case tc.want == StateStale && (f.Remaining != 0 || f.Age != tc.at || f.Lifetime != 5*time.Minute):
			t.Errorf("%s: forced stale reports %+v", tc.name, f)
		}
	}
}

func TestEvaluateNegative(t *testing.T) {
	noNegative := policyWith(testPolicy, func(p *config.CachePolicy) { p.NegativeTTL = 0 })
	neg := policySnap(KindNegative, storage.Metadata{})
	for _, tc := range []struct {
		name string
		p    config.CachePolicy
		req  httpcache.Request
		at   time.Duration
		want Freshness
	}{
		{"fresh", testPolicy, httpcache.Request{}, 2 * time.Second, Freshness{StateFresh, 2 * time.Second, 5 * time.Second, 3 * time.Second}},
		{"just before ttl", testPolicy, httpcache.Request{}, 5*time.Second - 1, Freshness{StateFresh, 5*time.Second - 1, 5 * time.Second, 1}},
		{"at ttl", testPolicy, httpcache.Request{}, 5 * time.Second, Freshness{StateStale, 5 * time.Second, 5 * time.Second, 0}},
		{"zero negative ttl", noNegative, httpcache.Request{}, 0, Freshness{State: StateStale}},
		{"no-cache", testPolicy, httpcache.Request{NoCache: true}, time.Second, Freshness{StateStale, time.Second, 5 * time.Second, 0}},
		{"max-age zero", testPolicy, maxAge(0), time.Second, Freshness{StateStale, time.Second, 5 * time.Second, 0}},
		{"max-age within the second", testPolicy, maxAge(2), 2500 * time.Millisecond, Freshness{StateFresh, 2500 * time.Millisecond, 5 * time.Second, 2500 * time.Millisecond}},
		{"max-age below age", testPolicy, maxAge(2), 3 * time.Second, Freshness{StateStale, 3 * time.Second, 5 * time.Second, 0}},
		{"no-store", testPolicy, httpcache.Request{NoStore: true}, time.Second, Freshness{State: StateBypass}},
	} {
		if got := Evaluate(tc.p, tc.req, neg, policyT0.Add(tc.at)); got != tc.want {
			t.Errorf("%s: Evaluate = %+v, want %+v", tc.name, got, tc.want)
		}
	}
	for name, s := range map[string]Snapshot{
		"unknown kind":     policySnap(Kind("partial"), storage.Metadata{}),
		"stored malformed": policySnap(KindBody, originCC("max-age=60, max-age=60")),
		"stored no-store":  policySnap(KindBody, originCC("no-store")),
	} {
		if got := Evaluate(testPolicy, httpcache.Request{}, s, policyT0); got != (Freshness{State: StateStale}) {
			t.Errorf("%s: Evaluate = %+v, want stale with zero lifetime", name, got)
		}
	}
}

func TestStaleIfErrorWindow(t *testing.T) {
	outage := &storage.Error{Kind: storage.Unavailable, Size: -1}
	noStale := policyWith(testPolicy, func(p *config.CachePolicy) { p.StaleIfError = 0 })
	disabled := policyWith(testPolicy, func(p *config.CachePolicy) { p.Enabled = false })
	for _, tc := range []struct {
		name string
		p    config.CachePolicy
		kind Kind
		at   time.Duration
		want bool
	}{
		{"not yet stale", testPolicy, KindBody, 5*time.Minute - 1, false},
		{"stale at lifetime", testPolicy, KindBody, 5 * time.Minute, true},
		{"inside window", testPolicy, KindBody, 5*time.Minute + 30*time.Second, true},
		{"exactly at limit", testPolicy, KindBody, 6 * time.Minute, true},
		{"beyond limit", testPolicy, KindBody, 6*time.Minute + 1, false},
		{"metadata only", testPolicy, KindMetadata, 5*time.Minute + 30*time.Second, true},
		{"negative never stale", testPolicy, KindNegative, 6 * time.Second, false},
		{"window disabled", noStale, KindBody, 5*time.Minute + 30*time.Second, false},
		{"cache disabled", disabled, KindBody, 5*time.Minute + 30*time.Second, false},
	} {
		if got := StaleIfError(tc.p, httpcache.Request{}, policySnap(tc.kind, storage.Metadata{}), policyT0.Add(tc.at), outage); got != tc.want {
			t.Errorf("%s: StaleIfError = %v, want %v", tc.name, got, tc.want)
		}
	}
	zeroTTL := policyWith(testPolicy, func(p *config.CachePolicy) { p.TTL = 0 })
	if !StaleIfError(zeroTTL, httpcache.Request{}, policySnap(KindBody, storage.Metadata{}), policyT0.Add(time.Minute), outage) {
		t.Fatal("zero TTL is a configured revalidation, not an origin directive forbidding stale service")
	}
}

func TestStaleIfErrorDirectives(t *testing.T) {
	outage := &storage.Error{Kind: storage.Timeout, Size: -1}
	stale := 5*time.Minute + 30*time.Second
	for _, tc := range []struct {
		name string
		m    storage.Metadata
		req  httpcache.Request
		at   time.Duration
		want bool
	}{
		{"origin max-age", originCC("max-age=600"), httpcache.Request{}, stale, true},
		{"origin must-revalidate", originCC("max-age=600, must-revalidate"), httpcache.Request{}, stale, false},
		{"origin proxy-revalidate", originCC("max-age=600, proxy-revalidate"), httpcache.Request{}, stale, false},
		{"origin no-cache", originCC("no-cache"), httpcache.Request{}, 30 * time.Second, false},
		{"origin s-maxage", originCC("s-maxage=600"), httpcache.Request{}, stale, false},
		{"origin stale-if-error ignored", originCC("max-age=600, stale-if-error=0"), httpcache.Request{}, stale, true},
		{"origin stale-if-error never widens", originCC("max-age=600, stale-if-error=99999"), httpcache.Request{}, 7 * time.Minute, false},
		{"malformed metadata", originCC("max-age=600, max-age=600"), httpcache.Request{}, stale, false},
		{"request no-cache", storage.Metadata{}, httpcache.Request{NoCache: true}, stale, false},
		{"request max-age zero", storage.Metadata{}, maxAge(0), stale, false},
		{"request max-age refuses age", storage.Metadata{}, maxAge(60), stale, false},
		{"request max-age accepts age", storage.Metadata{}, maxAge(3600), stale, true},
		{"request no-store", storage.Metadata{}, httpcache.Request{NoStore: true}, stale, false},
	} {
		if got := StaleIfError(testPolicy, tc.req, policySnap(KindBody, tc.m), policyT0.Add(tc.at), outage); got != tc.want {
			t.Errorf("%s: StaleIfError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStaleIfErrorCauses(t *testing.T) {
	allowed := map[storage.Kind]bool{storage.Timeout: true, storage.Throttled: true, storage.Unavailable: true}
	kinds := []storage.Kind{storage.NotFound, storage.AmbiguousNotFound, storage.Denied, storage.Configuration, storage.Unavailable,
		storage.NotModified, storage.Precondition, storage.InvalidRange, storage.Throttled, storage.Timeout, storage.Canceled,
		storage.InvalidResponse, storage.ChecksumMismatch, storage.Unsupported, storage.InvalidKey}
	causes := map[string]error{}
	want := map[string]bool{}
	for _, k := range kinds {
		causes[string(k)] = &storage.Error{Kind: k, Size: -1}
		want[string(k)] = allowed[k]
		causes["wrapped "+string(k)] = fmt.Errorf("fill: %w", &storage.Error{Kind: k, Size: -1})
		want["wrapped "+string(k)] = allowed[k]
	}
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"deadline":                     {context.DeadlineExceeded, true},
		"wrapped deadline":             {fmt.Errorf("fill timeout: %w", context.DeadlineExceeded), true},
		"denied joined with deadline":  {errors.Join(&storage.Error{Kind: storage.Denied, Size: -1}, context.DeadlineExceeded), false},
		"not found joined w/ deadline": {errors.Join(&storage.Error{Kind: storage.NotFound, Size: -1}, context.DeadlineExceeded), false},
		"canceled":                     {context.Canceled, false},
		"opaque":                       {errors.New("local failure"), false},
		"nil":                          {nil, false},
	} {
		causes[name], want[name] = c.err, c.want
	}
	s := policySnap(KindBody, storage.Metadata{})
	for name, cause := range causes {
		if got := StaleIfError(testPolicy, httpcache.Request{}, s, policyT0.Add(5*time.Minute+30*time.Second), cause); got != want[name] {
			t.Errorf("%s: StaleIfError = %v, want %v", name, got, want[name])
		}
	}
}

func TestValidator(t *testing.T) {
	for _, tc := range []struct {
		name        string
		kind        Kind
		etag        string
		conditional bool
		want        string
	}{
		{"strong", KindBody, `"v"`, true, `"v"`},
		{"weak", KindBody, `W/"v"`, true, `W/"v"`},
		{"metadata only", KindMetadata, `"v"`, true, `"v"`},
		{"invalid", KindBody, "v", true, ""},
		{"missing", KindBody, "", true, ""},
		{"conditional reads off", KindBody, `"v"`, false, ""},
		{"negative", KindNegative, `"v"`, true, ""},
	} {
		if got := Validator(policySnap(tc.kind, storage.Metadata{ETag: tc.etag}), tc.conditional); got != tc.want {
			t.Errorf("%s: Validator = %q, want %q", tc.name, got, tc.want)
		}
	}
}
