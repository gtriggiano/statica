package redirect

import (
	"errors"
	"strings"
	"testing"
)

func TestObjectPolicyLocation(t *testing.T) {
	const host = "www.example.com"
	preserve, drop, external := ObjectPolicy{Query: "preserve"}, ObjectPolicy{Query: "drop"}, ObjectPolicy{Query: "preserve", External: true}
	for _, tc := range []struct {
		name       string
		policy     ObjectPolicy
		value      string
		host       string
		rawQuery   string
		forceQuery bool
		want       string // empty: not accepted
	}{
		{"local path", preserve, "/new/", host, "", false, "/new/"},
		{"local path keeps query", preserve, "/new/", host, "ref=feed&x=1", false, "/new/?ref=feed&x=1"},
		{"bare question mark", preserve, "/new/", host, "", true, "/new/?"},
		{"drop", drop, "/new/", host, "ref=feed", false, "/new/"},
		{"own query wins", preserve, "/new/?lang=en", host, "ref=feed", false, "/new/?lang=en"},
		{"fragment kept", preserve, "/new/#install", host, "", false, "/new/#install"},
		{"query before fragment", preserve, "/new/#install", host, "ref=feed", false, "/new/?ref=feed#install"},
		{"empty fragment kept", drop, "/new/#", host, "", false, "/new/#"},
		{"query bytes escaped", preserve, "/new/", host, "a=\"<b>", false, "/new/?a=%22%3Cb%3E"},
		{"encoded path", drop, "/caf%C3%A9/", host, "", false, "/caf%C3%A9/"},
		{"same host", preserve, "https://www.example.com/new/", host, "ref=feed", false, "https://www.example.com/new/?ref=feed"},
		{"same host case and dot", drop, "https://WWW.Example.com./new/", host, "", false, "https://WWW.Example.com./new/"},
		{"same host with port", drop, "https://www.example.com:8443/new/", host, "", false, "https://www.example.com:8443/new/"},
		{"same host without path", drop, "https://www.example.com", host, "", false, "https://www.example.com"},
		{"same IPv6 host", drop, "https://[2001:DB8::1]/new/", "2001:db8::1", "", false, "https://[2001:DB8::1]/new/"},
		{"other host", preserve, "https://docs.example.com/new/", host, "", false, ""},
		{"other host allowed", external, "https://docs.example.com/new/", host, "ref=feed", false, "https://docs.example.com/new/?ref=feed"},
		{"uppercase scheme", drop, "HTTPS://www.example.com/new/", host, "", false, "HTTPS://www.example.com/new/"},
		{"http", external, "http://www.example.com/new/", host, "", false, ""},
		{"other scheme", external, "javascript:alert(1)", host, "", false, ""},
		{"network path", external, "//docs.example.com/new/", host, "", false, ""},
		{"relative path", external, "new/", host, "", false, ""},
		{"credentials", external, "https://user@docs.example.com/", host, "", false, ""},
		{"empty port", external, "https://docs.example.com:/", host, "", false, ""},
		{"leading zero port", external, "https://docs.example.com:0443/", host, "", false, ""},
		{"bad host", external, "https://docs_example.com/", host, "", false, ""},
		{"space", preserve, "/new page/", host, "", false, ""},
		{"backslash", preserve, "/new\\page/", host, "", false, ""},
		{"control", preserve, "/new/\x00", host, "", false, ""},
		{"non-ASCII", preserve, "/café/", host, "", false, ""},
		{"bad escape", preserve, "/new/%zz", host, "", false, ""},
		{"truncated escape", preserve, "/new/%2", host, "", false, ""},
		{"second fragment", preserve, "/new/#a#b", host, "", false, ""},
		{"empty", preserve, "", host, "", false, ""},
		{"too long", preserve, "/" + strings.Repeat("a", MaxTargetBytes), host, "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := tc.policy.Location(tc.value, tc.host, tc.rawQuery, tc.forceQuery)
			if err != nil {
				t.Fatalf("unexpected error %v", err)
			}
			if tc.want == "" {
				if ok || got != "" {
					t.Fatalf("accepted as %q", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
}

func TestObjectPolicyLocationTooLong(t *testing.T) {
	value := "/" + strings.Repeat("a", MaxTargetBytes-1)
	if _, ok, err := (ObjectPolicy{Query: "drop"}).Location(value, "www.example.com", "q=1", false); err != nil || !ok {
		t.Fatalf("a destination of exactly the limit: %v, %v", ok, err)
	}
	_, ok, err := (ObjectPolicy{Query: "preserve"}).Location(value, "www.example.com", "q=1", false)
	var request *RequestError
	if ok || !errors.As(err, &request) || request.Status != 414 {
		t.Fatalf("got %v, %v; want a 414 request error", ok, err)
	}
}
