package redirect

import "testing"

func TestScriptURL(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{`location = "/new/"`, "/new/"},
		{`location="/new/";`, "/new/"},
		{"\n  window.location.href = '/new/' ;\n", "/new/"},
		{`document.location.href="/new/"`, "/new/"},
		{`self.location.href = "/new/"`, "/new/"},
		{`window.top.location.href = "/new/"`, ""},
		{`top.location.replace("/new/")`, ""},
		{`location.replace("/new/")`, "/new/"},
		{`window.location.replace( '/new/' );`, "/new/"},
		{`document.location.assign("/new/")`, "/new/"},
		{`window.location.href = '/new/' + window.location.search + window.location.hash;`, "/new/"},
		{`location = "/new/" + location.hash + document.location.search`, "/new/"},
		{`location.replace("/new/" + location.search)`, "/new/"},
		{`window.location.href = '/new/' + (window.location.search || '') + (window.location.hash || "");`, "/new/"},
		{`location = "/new/" + ( location.hash||'' )`, "/new/"},
		{`location = "https:\/\/www.example.com\/new\/"`, "https://www.example.com/new/"},
		{`location = "/new/?a=1\u0026b=2"`, "/new/?a=1&b=2"},
		{`location = "/caf\u00e9/"`, "/café/"},
		{`location = "/\ud83d\ude00/"`, "/\U0001F600/"},
		{`location = 'it\'s/'`, "it's/"},
		{`location = "a\"b\\c"`, `a"b\c`},
		{`"use strict"; location = "/new/"`, "/new/"},
		{"'use strict'\n  location.replace('/new/')", "/new/"},
		{`"use strict" location = "/new/"`, ""},
		{`"use strict" + location = "/new/"`, ""},
		{`location = "/new/" + (location.hash || '#')`, ""},
		{`location = "/new/" + (location.hash || x)`, ""},
		{`location = "/new/" + (location.pathname || '')`, ""},
		{`location = "/new/" + (location.hash || ''`, ""},
		{`location = "/\ud83d/"`, ""},
		{`location = "/\u00e/"`, ""},
		{`location = "/\u{e9}/"`, ""},
		{`location = "/new/\n"`, ""},
		{`location = "/new/\x41"`, ""},
		{`location = "/new/` + "\n" + `"`, ""},
		{"location = `/new/`", ""},
		{`location == "/new/"`, ""},
		{`location += "/new/"`, ""},
		{`location => "/new/"`, ""},
		{`location.href.replace("/new/")`, ""},
		{`window.open("/new/")`, ""},
		{`foo.location = "/new/"`, ""},
		{`location . href = "/new/"`, ""},
		{`location.search = "?a"`, ""},
		{`location = "/new/" + "x"`, ""},
		{`location = "/new/" + location.pathname`, ""},
		{`location = "/new/" + location.search;;`, ""},
		{`location = "/new/"; location = "/other/"`, ""},
		{`location.replace("/new/"`, ""},
		{`location.replace("/new/"))`, ""},
		{`location = "/new/`, ""},
		{`location =`, ""},
		{`location.`, ""},
		{``, ""},
	} {
		got, ok := scriptURL(tc.text)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("scriptURL(%q) = %q, %v; want %q", tc.text, got, ok, tc.want)
		}
	}
}

func TestClassifyScript(t *testing.T) {
	for _, tc := range []struct {
		name, text  string
		kind        scriptKind
		destination string
	}{
		{"recognized", `window.location.replace("/new/")`, scriptRecognized, "/new/"},
		{"MkDocs", `var anchor=window.location.hash.substr(1);location.href="../new/"+(anchor?"#"+anchor:"")`, scriptConsistent, "../new/"},
		{"VuePress", "const anchor = window.location.hash.slice(1); window.location.replace(`/new.html${anchor? `#${anchor}`: \"\"}`)", scriptConsistent, "/new.html"},
		{"Zola", "\n const target = \"/new/\";\n const hash = window.location.hash || \"\";\n window.location.replace(target + hash);\n", scriptConsistent, "/new/"},
		{"sphinx-reredirects", `var target = "../new.html";if (window.location.hash) {window.location.replace(target + window.location.hash);} else {window.location.replace(target);}`, scriptConsistent, "../new.html"},
		{"comments", "// moved\nlocation.href = /* to */ '/new/' // here", scriptConsistent, "/new/"},
		{"query separators dropped", `location.href = "/new/" + (location.search ? "?" + location.search.slice(1) + "&" : "")`, scriptConsistent, "/new/"},
		{"same literal twice", `if (location.hash) { location.replace("/new/" + location.hash) } else { location.replace("/new/") }`, scriptConsistent, "/new/"},
		{"lenient escapes", `var t = '\x2fnew\u{2F}'; location = t`, scriptConsistent, "/new/"},
		{"line continuation", "var t = '/new\\\n/'; location = t", scriptConsistent, "/new/"},
		{"ignored", `console.log("hello")`, scriptIgnored, ""},
		{"location as a whole string", `console.log("location")`, scriptInconsistent, ""},
		{"location inside a string", `console.log("no location here")`, scriptIgnored, ""},
		{"location only in a comment", "// location\nconsole.log(1)", scriptIgnored, ""},
		{"location property of another object", `var a = {location: 1}; console.log(a.location)`, scriptInconsistent, ""},
		{"two destinations", `location.replace(navigator.language == 'it' ? '/it/' : '/en/')`, scriptInconsistent, ""},
		{"no navigation", `console.log(location.href, "/new/")`, scriptInconsistent, ""},
		{"hash assignment", `location.hash = "#top"`, scriptInconsistent, ""},
		{"comparison", `if (location == "/new/") {}`, scriptInconsistent, ""},
		{"no literal", `location.href = location.href.toLowerCase()`, scriptInconsistent, ""},
		{"regular expression", `if (/old/.test(location.pathname)) location.href = "/new/"`, scriptInconsistent, ""},
		{"division", `var x = 1 / 2; location.href = "/new/"`, scriptInconsistent, ""},
		{"unterminated string", `location.href = "/new/`, scriptInconsistent, ""},
		{"unterminated comment", `location.href = "/new/" /*`, scriptInconsistent, ""},
		{"unterminated template", "location.href = `/new/${x`", scriptInconsistent, ""},
		{"unbalanced braces", `}location.href = "/new/"`, scriptInconsistent, ""},
		{"octal escape", `location.href = "/new/\1"`, scriptInconsistent, ""},
		{"non-ASCII code", "location.href = '/new/'", scriptInconsistent, ""},
		{"escaped identifier", `\u006cocation.href = "/new/"`, scriptInconsistent, ""},
		{"backslash in a string only", `console.log("a\tb")`, scriptIgnored, ""},
		{"location as a string", `window["location"] = "/new/"`, scriptInconsistent, ""},
		{"parent", `if (top !== self) parent.location = "/new/"`, scriptConsistent, "/new/"},
		{"top", `top.location = "/new/"`, scriptConsistent, "/new/"},
		{"window top href", `window.top.location.href = "/new/";`, scriptConsistent, "/new/"},
		{"location of another object", `var venue = {}; venue.location = "Rome";`, scriptInconsistent, ""},
		{"location of a call", `frames().location.href = "/new/"`, scriptInconsistent, ""},
		{"optional location of another object", `venue?.location.replace("/new/")`, scriptInconsistent, ""},
		{"href of another location", `venue.location.href = "/new/"`, scriptInconsistent, ""},
		{"long member path", `a.b.c.d.e.location = "/new/"; location.x = 1`, scriptInconsistent, ""},
		{"stops at the second literal", `location.href = "/a/"; "/b/"; ` + "`${", scriptInconsistent, ""},
		{"use strict directive", `"use strict"; var u = "/new/"; location.replace(u)`, scriptConsistent, "/new/"},
		{"use strict directive before a line", "'use strict'\nvar u = '/new/'; location.replace(u)", scriptConsistent, "/new/"},
		{"use strict at the end", `"use strict"`, scriptIgnored, ""},
		{"use strict in an expression", `"use strict" + (location.href = "/new/")`, scriptInconsistent, ""},
		{"escaped use strict", `"use\x20strict"; var u = "/new/"; location.replace(u)`, scriptInconsistent, ""},
		{"use strict later", `var u = "/new/"; "use strict"; location.replace(u)`, scriptInconsistent, ""},
		{"HTML comment open", "<!--\nlocation.href = '/new/'", scriptInconsistent, ""},
		{"HTML comment open in a string", `location.href = "/new/<!--"; x()`, scriptInconsistent, ""},
		{"HTML comment close", "location.href = '/new/'\n--> done", scriptInconsistent, ""},
		{"line separator in a comment", "// moved\u2028location.href = '/other/'\nlocation.href = '/new/'", scriptInconsistent, ""},
		{"non-ASCII in a comment", "// déplacé\nlocation.href = '/new/'", scriptInconsistent, ""},
		{"non-ASCII in a block comment", "/* déplacé */ location.href = '/new/'", scriptConsistent, "/new/"},
		{"deep templates", "location.href = `" + repeat("${`", 20) + "/new/" + repeat("`}", 20) + "`", scriptInconsistent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination, kind := classifyScript(tc.text)
			if kind != tc.kind || destination != tc.destination {
				t.Fatalf("got %q, %d; want %q, %d", destination, kind, tc.destination, tc.kind)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := ""
	for range n {
		out += s
	}
	return out
}
