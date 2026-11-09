package redirect

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func compile(t *testing.T, rules ...Rule) *Set {
	t.Helper()
	s, err := Compile(rules)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveQueryCapture(t *testing.T) {
	s := compile(t, Rule{From: "/page/x?postId={postId}", To: "/blog/post/{postId}", Status: 308,
		Captures: map[string]Capture{"postId": {Pattern: `[0-9]+`}}})
	for _, tt := range []struct {
		query, location string
		match           bool
		status          int
	}{
		{"postId=42", "/blog/post/42", true, 0},
		{"postId=42&utm_source=mail", "/blog/post/42", true, 0},
		{"postId=abc", "", false, 0},
		{"postId=42abc", "", false, 0},
		{"postId=", "", false, 0},
		{"", "", false, 0},
		{"postId=42&postId=43", "", false, 400},
		{"postId=42&post%49d=43", "", false, 400},
		{"postId=%zz", "", false, 400},
	} {
		t.Run(tt.query, func(t *testing.T) {
			result, match, err := s.Resolve("/page/x", tt.query, false)
			if tt.status != 0 {
				assertRequestError(t, err, tt.status)
				return
			}
			if err != nil || match != tt.match || result.Location != tt.location || match && result.Status != 308 {
				t.Fatalf("got %#v, %v, %v", result, match, err)
			}
		})
	}
}

func TestStructuralEncoding(t *testing.T) {
	s := compile(t,
		Rule{From: "/old/{slug}", To: "https://example.com/articles/{slug}?q={slug}&again={slug}#section%20one", Status: 302},
		Rule{From: "/query?value={value}", To: "/value/{value}?again={value}", Status: 307})
	for _, tt := range []struct{ path, query, want string }{
		{"/old/caf%C3%A9", "", "https://example.com/articles/caf%C3%A9?q=caf%C3%A9&again=caf%C3%A9#section%20one"},
		{"/old/a%20b", "", "https://example.com/articles/a%20b?q=a+b&again=a+b#section%20one"},
		{"/old/a+b", "", "https://example.com/articles/a+b?q=a%2Bb&again=a%2Bb#section%20one"},
		{"/query", "value=a+b", "/value/a%20b?again=a+b"},
		{"/query", "value=a%2Bb", "/value/a+b?again=a%2Bb"},
		{"/query", "value=a%26b%3Dc%23d%3Fe", "/value/a&b=c%23d%3Fe?again=a%26b%3Dc%23d%3Fe"},
	} {
		t.Run(tt.path+tt.query, func(t *testing.T) {
			result, match, err := s.Resolve(tt.path, tt.query, false)
			if err != nil || !match || result.Location != tt.want {
				t.Fatalf("got %#v, %v, %v; want %s", result, match, err, tt.want)
			}
		})
	}
}

func TestQueryForwarding(t *testing.T) {
	for _, mode := range []string{"drop", "preserve", "unmatched"} {
		t.Run(mode, func(t *testing.T) {
			s := compile(t, Rule{From: "/old?id={id}", To: "/new/{id}", Status: 301, Query: mode})
			result, match, err := s.Resolve("/old", "x=a%20b&id=42&x=c+d&flag&other=%2f", false)
			want := "/new/42"
			if mode == "preserve" {
				want += "?x=a%20b&id=42&x=c+d&flag&other=%2f"
			}
			if mode == "unmatched" {
				want += "?x=a%20b&x=c+d&flag&other=%2f"
			}
			if err != nil || !match || result.Location != want {
				t.Fatalf("got %#v, %v, %v; want %s", result, match, err, want)
			}
		})
	}
	// Unsafe raw query bytes must remain query data, never header or fragment syntax.
	s := compile(t, Rule{From: "/old", To: "/new", Status: 301, Query: "preserve"})
	result, _, err := s.Resolve("/old", `q=hello world#fragment"`, false)
	if err != nil || result.Location != "/new?q=hello%20world%23fragment%22" {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestEmptyQueryMarker(t *testing.T) {
	for _, mode := range []string{"preserve", "drop", "unmatched"} {
		for _, force := range []bool{false, true} {
			for _, fragment := range []string{"", "#section", "#"} {
				t.Run(fmt.Sprintf("%s/%t/%s", mode, force, fragment), func(t *testing.T) {
					s := compile(t, Rule{From: "/old", To: "/new" + fragment, Status: 308, Query: mode})
					result, matched, err := s.Resolve("/old", "", force)
					want := "/new"
					if mode == "preserve" && force {
						want += "?"
					}
					want += fragment
					if err != nil || !matched || result.Location != want {
						t.Fatalf("%+v match=%v err=%v want=%q", result, matched, err, want)
					}
				})
			}
		}
	}
	// The marker itself counts toward both request and expanded-target limits.
	path := "/" + strings.Repeat("a", MaxTargetBytes-1)
	_, _, err := (*Set)(nil).Resolve(path, "", true)
	assertRequestError(t, err, 414)
	if _, _, err := (*Set)(nil).Resolve(path, "", false); err != nil {
		t.Fatal(err)
	}
	s := compile(t, Rule{From: "/old", To: path, Status: 302, Query: "preserve"})
	_, _, err = s.Resolve("/old", "", true)
	assertRequestError(t, err, 414)
}

func TestPathAndQuerySafety(t *testing.T) {
	s := compile(t, Rule{From: "/old?x={x}", To: "/new/{x}", Status: 308})
	for _, path := range []string{"//evil.test/x", "/a//b", "/./x", "/../x", "/%2e%2e/x", "/%252e%252e/x", "/a%2fb", "/a%5cb", "/a\\b", "/%00", "/%7f", "/%ff", "/%zz", "relative", "/a?b"} {
		t.Run(path, func(t *testing.T) { _, _, err := s.Resolve(path, "", false); assertRequestError(t, err, 400) })
	}
	for _, value := range []string{"..", "%2e%2e", "a%2fb", "a%5cb", "a%252fb", "a%00b", "%ff", "%zz"} {
		t.Run("query="+value, func(t *testing.T) { _, _, err := s.Resolve("/old", "x="+value, false); assertRequestError(t, err, 400) })
	}
	// Malformed queries fail validation even when no rule matches the path.
	_, _, err := s.Resolve("/unmatched", "other=%zz", false)
	assertRequestError(t, err, 400)
	// Query captures used only in query positions may contain separators as data.
	queryOnly := compile(t, Rule{From: "/old?x={x}", To: "/new?x={x}", Status: 308})
	result, match, err := queryOnly.Resolve("/old", "x=a%2Fb%26q%3Dx%23frag", false)
	if err != nil || !match || result.Location != "/new?x=a%2Fb%26q%3Dx%23frag" {
		t.Fatalf("%#v %v %v", result, match, err)
	}
}

func TestLiteralTemplatesDecodeExactlyOnce(t *testing.T) {
	s := compile(t, Rule{From: "/caf%C3%A9%20page?name=hello+world", To: "/nouveau%20caf%C3%A9?name=hello%20world", Status: 308})
	result, match, err := s.Resolve("/caf%C3%A9%20page", "name=hello%20world", false)
	if err != nil || !match || result.Location != "/nouveau%20caf%C3%A9?name=hello+world" {
		t.Fatalf("%#v %v %v", result, match, err)
	}
	// Escaped braces are literal data rather than template syntax.
	s = compile(t, Rule{From: "/literal/%7Bvalue%7D", To: "/new/%7Bvalue%7D", Status: 308})
	_, match, err = s.Resolve("/literal/%7Bvalue%7D", "", false)
	if err != nil || !match {
		t.Fatalf("match=%v err=%v", match, err)
	}
	s = compile(t, Rule{From: "/old", To: "HTTPS://example.com", Status: 308})
	result, match, err = s.Resolve("/old", "", false)
	if err != nil || !match || result.Location != "https://example.com/" {
		t.Fatalf("%#v %v %v", result, match, err)
	}
}

func TestCompilationRejectsInvalidRulesWithoutLeakingValues(t *testing.T) {
	base := Rule{From: "/old/{id}", To: "/new/{id}", Status: 308}
	for _, tt := range []struct {
		name   string
		change func(*Rule)
	}{
		{"required status", func(r *Rule) { r.Status = 0 }},
		{"bad status", func(r *Rule) { r.Status = 303 }},
		{"query mode", func(r *Rule) { r.Query = "SECRET" }},
		{"partial path", func(r *Rule) { r.From = "/SECRET-{id}" }},
		{"partial query", func(r *Rule) { r.From = "/old?id=SECRET{id}" }},
		{"invalid name", func(r *Rule) { r.From = "/old/{SECRET-id}" }},
		{"duplicate binding", func(r *Rule) { r.From = "/old/{id}?q={id}" }},
		{"unknown destination", func(r *Rule) { r.To = "/new/{SECRET}" }},
		{"query name", func(r *Rule) { r.To = "/new?{SECRET}=x" }},
		{"orphan constraint", func(r *Rule) { r.Captures = map[string]Capture{"SECRET": {Pattern: ".*"}} }},
		{"invalid regex", func(r *Rule) { r.Captures = map[string]Capture{"id": {Pattern: "SECRET("}} }},
		{"empty regex", func(r *Rule) { r.Captures = map[string]Capture{"id": {Pattern: ""}} }},
		{"userinfo", func(r *Rule) { r.To = "https://SECRET@example.com/new" }},
		{"http origin", func(r *Rule) { r.To = "http://SECRET.example/new" }},
		{"scheme relative", func(r *Rule) { r.To = "//SECRET.example/new" }},
		{"dynamic authority", func(r *Rule) { r.To = "https://{id}.SECRET.example/new" }},
		{"bad authority", func(r *Rule) { r.To = "https://SECRET.example:99999/new" }},
		{"fragment capture", func(r *Rule) { r.To = "/new#SECRET{id}" }},
		{"source fragment", func(r *Rule) { r.From = "/old#SECRET" }},
		{"source authority", func(r *Rule) { r.From = "https://SECRET.example/old" }},
		{"dot segment", func(r *Rule) { r.To = "/SECRET/%2e%2e" }},
		{"encoded slash", func(r *Rule) { r.To = "/SECRET/a%2fb" }},
		{"double escape", func(r *Rule) { r.To = "/SECRET/%252f" }},
		{"preserve destination query", func(r *Rule) { r.Query = "preserve"; r.To = "/new?token=SECRET" }},
		{"unmatched destination query", func(r *Rule) { r.Query = "unmatched"; r.To = "/new?token=SECRET" }},
		{"duplicate query key", func(r *Rule) { r.From = "/old?q=SECRET&q=other" }},
		{"hidden source", func(r *Rule) { r.From = "/old/.SECRET/{id}" }},
		{"encoded hidden source", func(r *Rule) { r.From = "/%2eSECRET/{id}" }},
		{"nested well-known source", func(r *Rule) { r.From = "/SECRET/.well-known/{id}" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := base
			tt.change(&r)
			_, err := Compile([]Rule{r})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "redirects[0].") || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe or unindexed error: %v", err)
			}
		})
	}
}

func TestEquivalentSources(t *testing.T) {
	a := Rule{From: "/old/{id}?a={x}&b=caf%C3%A9", To: "/one", Status: 308, Captures: map[string]Capture{"id": {Pattern: `[0-9]+`}}}
	b := Rule{From: "/old/{renamed}?b=café&a={other}", To: "/two", Status: 308, Captures: map[string]Capture{"renamed": {Pattern: `[0-9]+`}}}
	if _, err := Compile([]Rule{a, b}); err == nil || !strings.Contains(err.Error(), "redirects[1].from") {
		t.Fatalf("got %v", err)
	}
	// Different full-string constraints distinguish useful overlapping rules.
	b.Captures["renamed"] = Capture{Pattern: `[a-z]+`}
	if _, err := Compile([]Rule{a, b}); err != nil {
		t.Fatal(err)
	}
}

func TestDestinationIPAuthorities(t *testing.T) {
	for _, authority := range []string{"[::1]", "[2001:db8::1]:443", "[::ffff:127.0.0.1]", "127.0.0.1:8443"} {
		t.Run(authority, func(t *testing.T) {
			want := "https://" + authority + "/new"
			s := compile(t, Rule{From: "/old", To: want, Status: 308})
			result, match, err := s.Resolve("/old", "", false)
			if err != nil || !match || result.Location != want {
				t.Fatalf("%#v %v %v", result, match, err)
			}
		})
	}
	for _, authority := range []string{"[example.com]", "[127.0.0.1]", "[example.com]:443", "[127.0.0.1]:443", "[fe80::1%25en0]", "[fe80::1%en0]", "[::1]:", "[::1]:65536", "2001:db8::1", "[]"} {
		t.Run(authority, func(t *testing.T) {
			_, err := Compile([]Rule{{From: "/old", To: "https://" + authority + "/new", Status: 308}})
			if err == nil || !strings.Contains(err.Error(), "redirects[0].to:") {
				t.Fatalf("want destination authority error, got %v", err)
			}
		})
	}
}

func TestLiteralCycles(t *testing.T) {
	for _, rules := range [][]Rule{
		{{From: "/a", To: "/a", Status: 308}},
		{{From: "/a", To: "/a?new=field", Status: 308}},
		{{From: "/a?x=1&y=2", To: "/a?y=2&x=1", Status: 308}},
		{{From: "/a", To: "/b", Status: 308}, {From: "/b", To: "/c", Status: 308}, {From: "/c", To: "/a", Status: 308}},
	} {
		if _, err := Compile(rules); err == nil {
			t.Fatalf("cycle accepted: %#v", rules)
		}
	}
	// External destinations and template cycles are outside static cycle proof.
	compile(t, Rule{From: "/a", To: "https://example.com/a", Status: 308})
	compile(t, Rule{From: "/a/{x}", To: "/a/{x}", Status: 308})
	compile(t, Rule{From: "/a?token=1", To: "/a", Status: 308, Query: "unmatched"})
}

func TestFirstMatchWarningsAndImmutableInput(t *testing.T) {
	rules := []Rule{
		{From: "/old/{id}", To: "/first/{id}", Status: 301},
		{From: "/old/special", To: "/second", Status: 302},
	}
	s := compile(t, rules...)
	rules[0].To = "/mutated"
	result, match, err := s.Resolve("/old/special", "", false)
	if err != nil || !match || result.Location != "/first/special" || result.Status != 301 {
		t.Fatalf("%#v %v %v", result, match, err)
	}
	w := s.Warnings()
	if len(w) != 1 || w[0] != (Warning{Rule: 1, ShadowedBy: 0}) {
		t.Fatalf("%#v", w)
	}
	w[0].Rule = 42
	if s.Warnings()[0].Rule != 1 {
		t.Fatal("warning slice is mutable")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if _, _, err := s.Resolve("/old/concurrent", "", false); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

func TestCaptureAndTargetLimits(t *testing.T) {
	s := compile(t, Rule{From: "/old/{x}", To: "/new/{x}", Status: 308})
	for _, size := range []int{256, 257} {
		_, match, err := s.Resolve("/old/"+strings.Repeat("x", size), "", false)
		if err != nil || match != (size == 256) {
			t.Fatalf("size %d: match=%v err=%v", size, match, err)
		}
	}
	_, _, err := s.Resolve("/"+strings.Repeat("x", MaxTargetBytes), "", false)
	assertRequestError(t, err, 414)
	// Reused captures can expand a short input beyond the Location limit.
	to := "/new" + strings.Repeat("/{x}", 40)
	s = compile(t, Rule{From: "/old/{x}", To: to, Status: 308})
	_, _, err = s.Resolve("/old/"+strings.Repeat("x", 256), "", false)
	assertRequestError(t, err, 414)
	// Literal Unicode is legal, but its encoded URL must still fit the budget.
	for _, field := range []string{"from", "to"} {
		rule := Rule{From: "/old", To: "/new", Status: 308}
		if field == "from" {
			rule.From = "/" + strings.Repeat("é", 2000)
		} else {
			rule.To = "https://example.com/" + strings.Repeat("é", 2000)
		}
		if _, err := Compile([]Rule{rule}); err == nil || !strings.Contains(err.Error(), "."+field+":") {
			t.Fatalf("expected %s encoded size failure, got %v", field, err)
		}
	}
}

func assertRequestError(t *testing.T, err error, status int) {
	t.Helper()
	var requestError *RequestError
	if !errors.As(err, &requestError) || requestError.Status != status {
		t.Fatalf("want request error %d, got %v", status, err)
	}
}

func ExampleSet_Resolve() {
	s, _ := Compile([]Rule{{From: "/page/x?postId={id}", To: "/blog/post/{id}", Status: 308, Captures: map[string]Capture{"id": {Pattern: `[0-9]+`}}}})
	result, matched, err := s.Resolve("/page/x", "postId=42", false)
	fmt.Println(result.Status, result.Location, matched, err)
	// Output: 308 /blog/post/42 true <nil>
}

func TestWellKnownSourceIsAllowed(t *testing.T) {
	s := compile(t, Rule{From: "/.well-known/old", To: "/.well-known/new", Status: 308}, Rule{From: "/a.b/{id}", To: "/c", Status: 308})
	got, ok, err := s.Resolve("/.well-known/old", "", false)
	if err != nil || !ok || got.Location != "/.well-known/new" {
		t.Fatalf("got %#v %v %v", got, ok, err)
	}
}
