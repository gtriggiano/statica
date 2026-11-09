package httpcache

import (
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gtriggiano/statica/internal/storage"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func httpDate(t time.Time) string { return t.UTC().Format(http.TimeFormat) }

func TestMalformed(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    storage.Metadata
		want bool
	}{
		{"absent", storage.Metadata{}, false},
		{"valid", storage.Metadata{CacheControl: "public, max-age=60", Expires: httpDate(t0), RawDate: httpDate(t0), RawAge: "5", Age: 5 * time.Second}, false},
		{"rfc850 date", storage.Metadata{RawDate: "Friday, 02-Jan-26 03:04:05 GMT"}, false},
		{"bad directives", storage.Metadata{CacheControl: "max-age=-1"}, true},
		{"duplicate directives", storage.Metadata{CacheControl: "max-age=1, max-age=2"}, true},
		{"negative decoded age", storage.Metadata{Age: -time.Second}, true},
		{"bad expires", storage.Metadata{Expires: "tomorrow"}, true},
		{"zero expires", storage.Metadata{Expires: "0"}, true},
		{"two expires", storage.Metadata{Expires: httpDate(t0) + ", " + httpDate(t0)}, true},
		{"bad date", storage.Metadata{RawDate: "yesterday"}, true},
		{"negative age", storage.Metadata{RawAge: "-1"}, true},
		{"fractional age", storage.Metadata{RawAge: "1.5"}, true},
		{"two ages", storage.Metadata{RawAge: "5, 6"}, true},
		// Formerly malformed in the response layer; RFC 9111 §1.2.2 saturates.
		{"saturated age", storage.Metadata{RawAge: "10000000000"}, false},
		{"huge age", storage.Metadata{RawAge: "99999999999999999999999"}, false},
		{"saturated max-age", storage.Metadata{CacheControl: "max-age=99999999999999999999"}, false},
	} {
		if got := Malformed(tc.m); got != tc.want {
			t.Errorf("%s: Malformed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCurrentAge(t *testing.T) {
	sec := func(n int) time.Duration { return time.Duration(n) * time.Second }
	at := func(n int) time.Time { return t0.Add(sec(n)) }
	for _, tc := range []struct {
		name                   string
		m                      storage.Metadata
		request, response, now time.Time
		want                   time.Duration
	}{
		{"missing date counts residence", storage.Metadata{}, t0, t0, at(10), sec(10)},
		{"missing date counts response delay", storage.Metadata{}, t0, at(2), at(12), sec(12)},
		{"apparent age from date", storage.Metadata{RawDate: httpDate(at(-30))}, t0, t0, at(5), sec(35)},
		{"origin age plus delay", storage.Metadata{RawDate: httpDate(t0), RawAge: "100"}, at(-1), t0, at(9), sec(110)},
		{"apparent age beats corrected age", storage.Metadata{RawDate: httpDate(at(-30)), RawAge: "10"}, at(-2), t0, t0, sec(30)},
		{"corrected age beats apparent age", storage.Metadata{RawDate: httpDate(at(-30)), RawAge: "40"}, at(-2), t0, t0, sec(42)},
		{"future date adds nothing", storage.Metadata{RawDate: httpDate(at(3600))}, t0, t0, at(3), sec(3)},
		{"decoded date without raw date", storage.Metadata{Date: at(-20)}, t0, t0, at(1), sec(21)},
		{"decoded fields", storage.Metadata{Date: at(-20), Age: sec(50)}, t0, t0, at(1), sec(51)},
		{"raw fields win", storage.Metadata{RawDate: httpDate(at(-5)), Date: at(-500), RawAge: "7", Age: sec(1)}, t0, t0, t0, sec(7)},
		{"clock stepped back", storage.Metadata{RawAge: "4"}, at(1), t0, at(-10), sec(4)},
		{"saturated origin age", storage.Metadata{RawAge: "99999999999"}, t0, t0, at(1), maxAge},
		{"ancient date", storage.Metadata{RawDate: "Mon, 01 Jan 0001 00:00:00 GMT"}, t0, t0, t0, maxAge},
		{"unset request time", storage.Metadata{}, time.Time{}, t0, t0, maxAge},
		{"far future now", storage.Metadata{RawAge: "99999999999"}, t0, t0, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), maxAge},
	} {
		if got := CurrentAge(tc.m, tc.request, tc.response, tc.now); got != tc.want {
			t.Errorf("%s: CurrentAge = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLifetimePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    storage.Metadata
		want time.Duration
		ok   bool
	}{
		{"s-maxage over max-age and expires", storage.Metadata{CacheControl: "max-age=60, s-maxage=120", RawDate: httpDate(t0), Expires: httpDate(t0.Add(time.Hour))}, 120 * time.Second, true},
		{"max-age over expires", storage.Metadata{CacheControl: "max-age=60", RawDate: httpDate(t0), Expires: httpDate(t0.Add(time.Hour))}, time.Minute, true},
		{"expires minus date", storage.Metadata{CacheControl: "public", RawDate: httpDate(t0.Add(-time.Minute)), Expires: httpDate(t0.Add(time.Hour))}, 61 * time.Minute, true},
		{"expires without date uses response time", storage.Metadata{Expires: httpDate(t0.Add(30 * time.Minute))}, 30 * time.Minute, true},
		{"expires minus decoded date", storage.Metadata{Date: t0.Add(-time.Minute), Expires: httpDate(t0.Add(time.Hour))}, 61 * time.Minute, true},
		{"expires minus future date", storage.Metadata{RawDate: httpDate(t0.Add(time.Hour)), Expires: httpDate(t0.Add(2 * time.Hour))}, time.Hour, true},
		{"expires at date", storage.Metadata{RawDate: httpDate(t0), Expires: httpDate(t0)}, 0, true},
		{"past expires", storage.Metadata{RawDate: httpDate(t0), Expires: httpDate(t0.Add(-time.Hour))}, 0, true},
		{"invalid expires is expired", storage.Metadata{Expires: "0"}, 0, true},
		{"far expires saturates", storage.Metadata{Expires: "Fri, 31 Dec 9999 23:59:59 GMT"}, maxAge, true},
		{"max-age zero", storage.Metadata{CacheControl: "max-age=0"}, 0, true},
		{"saturated s-maxage", storage.Metadata{CacheControl: "s-maxage=99999999999"}, maxAge, true},
		{"none", storage.Metadata{}, 0, false},
		{"no-cache assigns none", storage.Metadata{CacheControl: "no-cache, must-revalidate"}, 0, false},
	} {
		d, _ := Parse(tc.m.CacheControl)
		if got, ok := Lifetime(d, tc.m, t0); got != tc.want || ok != tc.ok {
			t.Errorf("%s: Lifetime = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRequestDirectives(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Header
		want Request
	}{
		{"absent", http.Header{}, Request{}},
		{"no-cache", http.Header{"Cache-Control": {"no-cache"}}, Request{NoCache: true}},
		{"no-store", http.Header{"Cache-Control": {"no-store"}}, Request{NoStore: true}},
		{"max-age zero", http.Header{"Cache-Control": {"max-age=0"}}, Request{MaxAge: Delta{0, true}}},
		{"combined", http.Header{"Cache-Control": {`max-age="30", No-Cache`}}, Request{NoCache: true, MaxAge: Delta{30, true}}},
		{"several lines", http.Header{"Cache-Control": {"no-cache", "max-age=5"}}, Request{NoCache: true, MaxAge: Delta{5, true}}},
		// A malformed header keeps no-store and no-cache members, never max-age.
		{"malformed keeps no-cache", http.Header{"Cache-Control": {"no-cache, max-age=abc"}}, Request{NoCache: true}},
		{"duplicate across lines", http.Header{"Cache-Control": {"no-store, max-age=5", "max-age=6"}}, Request{NoStore: true}},
		{"empty line", http.Header{"Cache-Control": {"no-store", ""}}, Request{NoStore: true}},
		{"malformed line", http.Header{"Cache-Control": {"max-age=60", "\tNO-CACHE=\"Set-Cookie\" , bad="}}, Request{NoCache: true}},
		{"malformed qualified no-store", http.Header{"Cache-Control": {"No-Store=x,,"}}, Request{NoStore: true}},
		{"malformed without members", http.Header{"Cache-Control": {"max-age=abc"}}, Request{}},
		{"malformed lookalikes", http.Header{"Cache-Control": {"no-\u017ftore, no-cache-x, xno-cache, no cache, ,"}}, Request{}},
		{"pragma ignored", http.Header{"Pragma": {"no-cache"}}, Request{}},
		{"min-fresh and max-stale ignored", http.Header{"Cache-Control": {"min-fresh=10, max-stale"}}, Request{}},
	} {
		if got := RequestDirectives(tc.h); got != tc.want {
			t.Errorf("%s: RequestDirectives = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestMergeNotModified(t *testing.T) {
	stored := storage.Metadata{Size: 10, ETag: `"v1"`, LastModified: t0.Add(-time.Hour), ContentType: "text/plain", ContentEncoding: "gzip",
		CacheControl: "max-age=60", Expires: httpDate(t0), ContentDisposition: "inline", ContentLanguage: "en", Date: t0, Age: 5 * time.Second,
		RawDate: httpDate(t0), RawAge: "5", ContentRange: "bytes 0-9/10", Checksum: storage.ChecksumVerified}

	// Every field of the 304 differs from the stored value except ETag. Only
	// the freshness and validator fields may be taken from it.
	replaced := map[string]bool{"CacheControl": true, "Expires": true, "LastModified": true, "Date": true, "RawDate": true, "Age": true, "RawAge": true}
	var full storage.Metadata
	v := reflect.ValueOf(&full).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch name := v.Type().Field(i).Name; {
		case name == "ETag":
			f.SetString(stored.ETag)
		case f.Kind() == reflect.String:
			f.SetString("304 " + name)
		case f.Type() == reflect.TypeFor[time.Time]():
			f.Set(reflect.ValueOf(t0.Add(time.Duration(i) * time.Minute)))
		case f.Kind() == reflect.Int64:
			f.SetInt(int64(1000 + i))
		default:
			t.Fatalf("field %s has an unclassified type %s", name, f.Type())
		}
	}
	merged, err := MergeNotModified(stored, full)
	if err != nil {
		t.Fatal(err)
	}
	got, from304, kept := reflect.ValueOf(merged), reflect.ValueOf(full), reflect.ValueOf(stored)
	for i := range got.NumField() {
		name, want := got.Type().Field(i).Name, kept.Field(i)
		if replaced[name] {
			want = from304.Field(i)
		}
		if !reflect.DeepEqual(got.Field(i).Interface(), want.Interface()) {
			t.Errorf("merged %s = %v, want %v", name, got.Field(i), want)
		}
	}

	// A 304 omitting optional fields keeps the stored policy and validators,
	// while its missing Date and Age restart age accounting at the exchange.
	merged, err = MergeNotModified(stored, storage.Metadata{Size: -1})
	want := stored
	want.Date, want.RawDate, want.Age, want.RawAge = time.Time{}, "", 0, ""
	if err != nil || merged != want {
		t.Fatalf("minimal 304 merged = %+v, %v; want %+v", merged, err, want)
	}

	for _, tag := range []string{`"v2"`, `W/"v1"`, "v1"} {
		if _, err := MergeNotModified(stored, storage.Metadata{ETag: tag}); !errors.Is(err, ErrETagChanged) {
			t.Errorf("ETag %s: err = %v, want ErrETagChanged", tag, err)
		}
	}
}
