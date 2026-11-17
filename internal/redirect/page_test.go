package redirect

import (
	"net/url"
	"strings"
	"testing"
)

// The pages that site generators publish for a moved page, with
// example.com destinations, as found in their templates. base is the URL
// path of the page in every case.
const pageBase = "/docs/old/"

func TestFindPageRedirectGenerators(t *testing.T) {
	for _, tc := range []struct {
		name, page, destination, detector string
	}{
		{"Hugo alias", `<!DOCTYPE html>
<html lang="en-us">
  <head>
    <title>https://www.example.com/posts/new/</title>
    <link rel="canonical" href="https://www.example.com/posts/new/">
    <meta name="robots" content="noindex">
    <meta charset="utf-8">
    <meta http-equiv="refresh" content="0; url=https://www.example.com/posts/new/">
  </head>
</html>`, "https://www.example.com/posts/new/", DetectMetaRefresh},
		{"Jekyll redirect-from", `<!DOCTYPE html>
<html lang="en-US">
  <meta charset="utf-8">
  <title>Redirecting&hellip;</title>
  <link rel="canonical" href="https://www.example.com/new/?a=1&amp;b=2">
  <script>location="https://www.example.com/new/?a=1\u0026b=2"</script>
  <meta http-equiv="refresh" content="0; url=https://www.example.com/new/?a=1&amp;b=2">
  <meta name="robots" content="noindex">
  <h1>Redirecting&hellip;</h1>
  <a href="https://www.example.com/new/?a=1&amp;b=2">Click here if you are not redirected.</a>
</html>`, "https://www.example.com/new/?a=1&b=2", DetectMetaRefresh},
		{"Antora", `<!DOCTYPE html><meta charset="utf-8"><link rel="canonical" href="../new/"><script>location="../new/"</script><meta http-equiv="refresh" content="0; url=../new/"><meta name="robots" content="noindex"><title>Redirect Notice</title><h1>Redirect Notice</h1><p>The page you requested has been relocated to <a href="../new/">../new/</a>.</p>`, "/docs/new/", DetectMetaRefresh},
		{"Docusaurus", `<!DOCTYPE html>
<html>
  <head>
    <meta charset="UTF-8">
    <meta http-equiv="refresh" content="0; url=/docs/new">
    <link rel="canonical" href="/docs/new" />
  </head>
  <script>
    window.location.href = '/docs/new' + window.location.search + window.location.hash;
  </script>
</html>`, "/docs/new", DetectMetaRefresh},
		{"SvelteKit", `<script>location.href="/docs/new/";</script><meta http-equiv="refresh" content="0;url=/docs/new/">`, "/docs/new/", DetectMetaRefresh},
		{"Gatsby meta redirect", `<meta http-equiv="refresh" content="0; URL='/docs/new/'" />`, "/docs/new/", DetectMetaRefresh},
		{"Gatsby client-side redirect", `<script>window.location.href="/docs/new"</script>`, "/docs/new", DetectScript},
		{"Eleventy redirects", `
<!DOCTYPE html>
<html>
  <head>
    <title>Redirecting...</title><link rel="canonical" href="/docs/new/?a=1&amp;b=2"/><meta name="robots" content="noindex"><meta charset="utf-8"/><meta http-equiv="refresh" content="0; url=/docs/new/?a=1&amp;b=2"/>
  </head>
</html>
`, "/docs/new/?a=1&b=2", DetectMetaRefresh},
		{"MkDocs redirects", `<!doctype html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <title>Redirecting...</title>
    <link rel="canonical" href="../new/">
    <meta name="robots" content="noindex">
    <script>var anchor=window.location.hash.substr(1);location.href="../new/"+(anchor?"#"+anchor:"")</script>
    <meta http-equiv="refresh" content="0; url=../new/">
</head>
<body>
Redirecting...
</body>
</html>`, "/docs/new/", DetectMetaRefresh},
		{"VuePress", "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><meta http-equiv=\"refresh\" content=\"0; url=/docs/new.html\"><script>const anchor = window.location.hash.slice(1); window.location.replace(`/docs/new.html${anchor? `#${anchor}`: \"\"}`)</script></head><body>Redirecting...</body></html>", "/docs/new.html", DetectMetaRefresh},
		{"sphinxext-rediraffe", `
<html>
    <head>
        <noscript>
            <meta http-equiv="refresh" content="0; url=../new.html"/>
        </noscript>
    </head>
    <body>
        <script>
            window.location.href = '../new.html' + (window.location.search || '') + (window.location.hash || '');
        </script>
        <p>You should have been redirected.</p>
        <a href="../new.html">If not, click here to continue.</a>
    </body>
</html>
`, "/docs/new.html", DetectScript},
		{"Astro", `<!doctype html><title>Redirecting to: /docs/new</title><meta http-equiv="refresh" content="0;url=/docs/new"><meta name="robots" content="noindex"><link rel="canonical" href="https://www.example.com/docs/new"><body>	<a href="/docs/new">Redirecting from <code>/docs/old</code> to <code>/docs/new</code></a></body>`, "/docs/new", DetectMetaRefresh},
		{"script that ignores location", `<meta http-equiv="refresh" content="0; url=/docs/new/"><script>console.log("moved")</script><script src="/analytics.js"></script>`, "/docs/new/", DetectMetaRefresh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination, detector, ok := FindPageRedirect([]byte(tc.page), pageBase, true, true)
			if !ok || destination != tc.destination || detector != tc.detector {
				t.Fatalf("got %q, %q, %v; want %q, %q", destination, detector, ok, tc.destination, tc.detector)
			}
		})
	}
}

func TestFindPageRedirectRefusesOtherPages(t *testing.T) {
	for _, tc := range []struct{ name, page string }{
		{"plain page", `<html><head><title>Docs</title></head><body><a href="/new/">New</a></body></html>`},
		{"Zola", `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <link rel="canonical" href="https://www.example.com/new/">
  <noscript><meta http-equiv="refresh" content="0; url=https://www.example.com/new/"></noscript>
  <script>
    const target = "https://www.example.com/new/";
    const hash = window.location.hash || "";
    window.location.replace(target + hash);
  </script>
  <title>Redirect</title>
</head>
<body><a href="https://www.example.com/new/">Click here</a> to be redirected.</body>
</html>`},
		{"sphinx-reredirects", `<html><head><noscript><meta http-equiv="refresh" content="0; url=../new.html" /></noscript><script>var target = "../new.html";if (window.location.hash) {window.location.replace(target + window.location.hash);} else {window.location.replace(target);}</script></head></html>`},
		{"mdBook", `<!DOCTYPE HTML>
<html lang="en">
    <head>
        <meta charset="utf-8">
        <title>Redirecting...</title>
        <meta http-equiv="refresh" content="0; URL=../new.html">
        <link rel="canonical" href="../new.html">
    </head>
    <body>
        <p>Redirecting to... <a href="../new.html">../new.html</a>.</p>
        <script>
            // This handles redirects that involve fragments.
            document.addEventListener('DOMContentLoaded', function() {
                const fragmentMap = {"#install": "../setup.html#install"};
                const fragment = window.location.hash;
                if (fragment) {
                    let redirectUrl = "../new.html";
                    const target = fragmentMap[fragment];
                    if (target) {
                        let url = new URL(target, window.location.href);
                        redirectUrl = url.href;
                    } else {
                        let url = new URL(redirectUrl, window.location.href);
                        url.hash = window.location.hash;
                        redirectUrl = url.href;
                    }
                    window.location.replace(redirectUrl);
                }
            });
        </script>
    </body>
</html>`},
		{"language detection", `<html><head><meta http-equiv="refresh" content="0; url=/en/"><script>
var language = (navigator.language || 'en').slice(0, 2);
window.location.replace(language === 'it' ? '/it/' : '/en/');
</script></head></html>`},
		{"single-page app without scripts", `<!doctype html><html><head><noscript><meta http-equiv="refresh" content="0; url=/no-javascript/"></noscript><script type="module" src="/assets/app.js"></script></head><body><div id="app"></div></body></html>`},
		{"noscript meta without a script", `<noscript><meta http-equiv="refresh" content="0; url=/new/"></noscript>`},
		{"meta refresh only in noscript, confirmed by a script", `<noscript><meta http-equiv="refresh" content="0; url=/new/"></noscript><script>var u = "/new/"; location.replace(u)</script>`},
		{"shell with a fallback for old browsers", `<!doctype html><html><head><noscript><meta http-equiv="refresh" content="0; url=/no-javascript/"></noscript><script>if (!window.fetch) location.href = "/old-browser/";</script><script type="module" src="/assets/app.js"></script></head></html>`},
		{"svg", `<meta http-equiv="refresh" content="0; url=/new/"><svg><script>location = "/other/"</script></svg>`},
		{"math", `<meta http-equiv="refresh" content="0; url=/new/"><math><mi>x</mi></math>`},
		{"svg in a template", `<meta http-equiv="refresh" content="0; url=/new/"><template><svg></svg></template>`},
		{"CDATA section", `<meta http-equiv="refresh" content="0; url=/new/"><![CDATA[ x ]]>`},
		{"meta refresh read as zero by browsers disagrees", `<meta http-equiv="refresh" content="0; url=/a/"><meta http-equiv="refresh" content="0.5; url=/b/">`},
		{"meta refresh read as zero by browsers alone", `<meta http-equiv="refresh" content=".5; url=/a/">`},
		{"HTML comment in a script", `<meta http-equiv="refresh" content="0; url=/new/"><script><!--
location.href = "/new/";
//--></script>`},
		{"analytics with location", `<meta http-equiv="refresh" content="0; url=/new/"><script>gtag('config', 'G-EXAMPLE', {page_location: location.href});</script>`},
		{"location in a regular expression", `<meta http-equiv="refresh" content="0; url=/new/"><script>if (/old/.test(location.pathname)) location.href = "/new/";</script>`},
		{"unterminated string", `<meta http-equiv="refresh" content="0; url=/new/"><script>location.href = "/new/;</script>`},
		{"delay", `<meta http-equiv="refresh" content="5; url=/new/">`},
		{"Astro 302", `<!doctype html><title>Redirecting to: /new</title><meta http-equiv="refresh" content="2;url=/new">`},
		{"reload", `<meta http-equiv="refresh" content="0">`},
		{"other http-equiv", `<meta http-equiv="content-security-policy" content="0; url=/new/">`},
		{"comment", `<!-- <meta http-equiv="refresh" content="0; url=/new/"> -->`},
		{"inside a title", `<title><meta http-equiv="refresh" content="0; url=/new/"></title>`},
		{"inside a template", `<template><meta http-equiv="refresh" content="0; url=/new/"><script>location = "/new/"</script></template>`},
		{"inside a nested template", `<template><template></template><meta http-equiv="refresh" content="0; url=/new/"></template>`},
		{"base href", `<base href="https://www.example.com/"><meta http-equiv="refresh" content="0; url=new/">`},
		{"base href after", `<meta http-equiv="refresh" content="0; url=/new/"><base href="/">`},
		{"meta and script disagree", `<meta http-equiv="refresh" content="0; url=/a/"><script>location = "/b/"</script>`},
		{"two meta refresh", `<meta http-equiv="refresh" content="0; url=/a/"><meta http-equiv="refresh" content="0; url=/b/">`},
		{"two scripts", `<script>location = "/a/"</script><script>location = "/b/"</script>`},
		{"two destinations in a script", `<script>location.href = Math.random() < 0.5 ? "/a/" : "/b/";</script>`},
		{"location without navigation", `<script>if (!location.hash) location.hash = "#top";</script>`},
		{"src", `<script src="/redirect.js">location = "/new/"</script>`},
		{"data block", `<script type="application/json">location = "/new/"</script>`},
		{"type with parameters", `<script type="text/javascript; charset=utf-8">location = "/new/"</script>`},
		{"other language", `<script language="vbscript">location = "/new/"</script>`},
		{"nomodule", `<script nomodule>location = "/new/"</script>`},
		{"script inside noscript", `<noscript><script>location = "/new/"</script></noscript>`},
		{"empty", ``},
		{"unterminated script", `<script>location="/b"`},
		{"refresh with a Unicode letter", "<meta http-equiv=\"refre\u017fh\" content=\"0; url=/new/\">"},
		{"top location alone", `<script>top.location = "/new/"</script>`},
		{"top location replace alone", `<script>window.top.location.replace("/new/")</script>`},
		{"top location disagrees", `<meta http-equiv="refresh" content="0; url=/new/"><script>top.location.href = "/other/"</script>`},
		{"not html", "\x00\x01\x02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if destination, detector, ok := FindPageRedirect([]byte(tc.page), pageBase, true, true); ok {
				t.Fatalf("found %q with %s", destination, detector)
			}
		})
	}
}

func TestFindPageRedirectDetectors(t *testing.T) {
	for _, tc := range []struct {
		name, page          string
		metaRefresh, script bool
		destination         string // empty: no redirect
		detector            string
	}{
		{"script destinations always agree", `<meta http-equiv="refresh" content="0; url=/a/"><script>location = "/b/"</script>`, true, false, "", ""},
		{"meta refresh destinations always agree", `<meta http-equiv="refresh" content="0; url=/a/"><script>location = "/b/"</script>`, false, true, "", ""},
		{"noscript ignored by the script detector", `<noscript><meta http-equiv="refresh" content="0; url=/b/"></noscript><script>location = "/a/"</script>`, false, true, "/a/", DetectScript},
		{"noscript ignored by the meta refresh detector", `<meta http-equiv="refresh" content="0; url=/a/"><noscript><meta http-equiv="refresh" content="0; url=/b/"><script>location = "/c/"</script></noscript>`, true, true, "/a/", DetectMetaRefresh},
		{"no detector", `<meta http-equiv="refresh" content="0; url=/a/">`, false, false, "", ""},
		{"agreement after resolution", `<script>location = "/docs/new/"</script><meta http-equiv="refresh" content="0; url=../new/">`, true, true, "/docs/new/", DetectMetaRefresh},
		{"script reported without meta refresh", `<meta http-equiv="refresh" content="0; url=/a/"><script>location = "/a/"</script>`, false, true, "/a/", DetectScript},
		{"meta refresh without the script detector", `<meta http-equiv="refresh" content="0; url=/a/"><script>location = "/a/"</script>`, true, false, "/a/", DetectMetaRefresh},
		{"recognized script without meta refresh", `<script>location = "/a/"</script>`, true, true, "/a/", DetectScript},
		{"recognized script without its detector", `<script>location = "/a/"</script>`, true, false, "", ""},
		{"noscript meta and a consistent script", `<noscript><meta http-equiv="refresh" content="0; url=/a/"></noscript><script>var u = "/a/"; location.replace(u)</script>`, true, true, "", ""},
		{"noscript meta and a recognized script without its detector", `<noscript><meta http-equiv="refresh" content="0; url=/a/"></noscript><script>location.replace("/a/")</script>`, true, false, "", ""},
		{"meta refresh read as zero by browsers confirms", `<meta http-equiv="refresh" content="0; url=/a/"><meta http-equiv="refresh" content=".5; url=/a/">`, true, true, "/a/", DetectMetaRefresh},
		{"meta refresh read as zero by browsers never originates", `<meta http-equiv="refresh" content="0.5; url=/a/"><script>location = "/a/"</script>`, true, true, "/a/", DetectScript},
		{"meta refresh read as zero by browsers and a script disagree", `<meta http-equiv="refresh" content="0.5; url=/b/"><script>location = "/a/"</script>`, true, true, "", ""},
		{"script for another event ignored", `<meta http-equiv="refresh" content="0; url=/a/"><script for="document" event="onload">location = "/b/"</script>`, true, true, "/a/", DetectMetaRefresh},
		{"top location confirms", `<meta http-equiv="refresh" content="0; url=/a/"><script>window.top.location.replace("/a/")</script>`, true, true, "/a/", DetectMetaRefresh},
		{"script for the window's load", `<meta http-equiv="refresh" content="0; url=/a/"><script for=" WINDOW " event="onload()">location = "/b/"</script>`, true, true, "", ""},
		{"use strict", "<meta http-equiv=\"refresh\" content=\"0; url=/a/\"><script>'use strict'\nvar u = '/a/'; location.replace(u)</script>", true, true, "/a/", DetectMetaRefresh},
		{"consistent script confirms a meta refresh", `<meta http-equiv="refresh" content="0; url=/a/"><script>var u = "/a/"; location.replace(u)</script>`, true, true, "/a/", DetectMetaRefresh},
		{"consistent script alone", `<script>var u = "/a/"; location.replace(u)</script>`, true, true, "", ""},
		{"inconsistent script whatever the detectors", `<meta http-equiv="refresh" content="0; url=/a/"><script>location.replace(navigator.language == 'it' ? '/it/' : '/a/')</script>`, true, false, "", ""},
		{"repeated destination", `<meta http-equiv="refresh" content="0; url=/a/"><meta http-equiv="refresh" content="0; url=../../a/">`, true, true, "/a/", DetectMetaRefresh},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination, detector, ok := FindPageRedirect([]byte(tc.page), pageBase, tc.metaRefresh, tc.script)
			if ok != (tc.destination != "") || destination != tc.destination || detector != tc.detector {
				t.Fatalf("got %q, %q, %v; want %q, %q", destination, detector, ok, tc.destination, tc.detector)
			}
		})
	}
}

// Scripts that set the location only in some cases, later or to a value
// built at run time are not redirect pages: they never originate a redirect,
// and their literal must agree with the page's other redirects.
func TestFindPageRedirectConditionalScripts(t *testing.T) {
	for _, script := range []string{
		`if (top !== self) top.location = "/";`,
		`if (screen.width < 600) location.href = "/m/";`,
		`setTimeout(function(){ location.href = "/next/" }, 5000)`,
		`x.onclick = function(){ location.href = "/next/" }`,
		`var venue = {}; venue.location = "Rome";`,
		`if (location.hostname === "www.example.com") location.href = other;`,
		`location.href = "/docs/" + latest;`,
	} {
		t.Run(script, func(t *testing.T) {
			for _, page := range []string{
				"<script>" + script + "</script>",
				`<meta http-equiv="refresh" content="0; url=/docs/new/"><script>` + script + "</script>",
			} {
				if destination, detector, ok := FindPageRedirect([]byte(page), pageBase, true, true); ok {
					t.Fatalf("%s: found %q with %s", page, destination, detector)
				}
			}
		})
	}
}

func TestFindPageRedirectScriptTypes(t *testing.T) {
	for _, attributes := range []string{
		``, `type=""`, `type="module"`, `type=" TEXT/JavaScript "`, `type="application/ecmascript"`, `type="application/javascript"`,
		`type="application/x-ecmascript"`, `type="application/x-javascript"`, `type="text/ecmascript"`, `type="text/javascript1.0"`,
		`type="text/javascript1.5"`, `type="text/jscript"`, `type="text/livescript"`, `type="text/x-ecmascript"`, `type="text/x-javascript"`,
		`language="JavaScript1.2"`, `language="jscript"`, `language="livescript"`, `language=""`, `language="vbscript" type="text/javascript"`,
		`for="window" event="onload"`, `for=" Window " event=" ONLOAD() "`, `for="document"`, `event="onclick"`, `type="module" for="document" event="onclick"`,
	} {
		page := "<script " + attributes + `>location = "/new/"</script>`
		if destination, _, ok := FindPageRedirect([]byte(page), pageBase, true, true); !ok || destination != "/new/" {
			t.Errorf("%s: got %q, %v", page, destination, ok)
		}
	}
	for _, attributes := range []string{
		`type="text/javascript1.6"`, `type="text/javascript; charset=utf-8"`, `type="application/json"`, `type="text/plain"`,
		`language="vbscript"`, `type="text/vbscript" language="javascript"`, `src="/a.js"`, `nomodule`,
		`for="document" event="onload"`, `for="window" event="onclick"`, `for="window" event="onload();"`,
	} {
		page := "<script " + attributes + `>location = "/new/"</script>`
		if destination, _, ok := FindPageRedirect([]byte(page), pageBase, true, true); ok {
			t.Errorf("%s: found %q", page, destination)
		}
	}
}

func TestFindPageRedirectAttributes(t *testing.T) {
	for _, tc := range []struct{ name, page, want string }{
		{"uppercase", `<META HTTP-EQUIV="REFRESH" CONTENT="0; URL=/new/">`, "/new/"},
		{"unquoted", `<meta http-equiv=refresh content=0;url=/new/>`, "/new/"},
		{"entities", `<meta http-equiv="refresh" content="0; url=/new/?a=1&amp;b=2">`, "/new/?a=1&b=2"},
		{"numeric entity", `<meta http-equiv="refresh" content="0; url=&#x2F;new&#47;">`, "/new/"},
		{"self-closing", `<meta http-equiv="refresh" content="0; url=/new/" />`, "/new/"},
		{"first attribute wins", `<meta http-equiv="refresh" content="0; url=/new/" content="0; url=/other/">`, "/new/"},
		{"in the body", `<body><p>Moved</p><meta http-equiv="refresh" content="0; url=/new/"></body>`, "/new/"},
		{"after a template", `<template><p>inert</p></template><meta http-equiv="refresh" content="0; url=/new/">`, "/new/"},
		{"script type module", `<script type="module">location.assign("/new/")</script>`, "/new/"},
		{"script language", `<script language="JavaScript">self.location = '/new/'</script>`, "/new/"},
		{"module with nomodule", `<script type="module" nomodule>location.assign("/new/")</script>`, "/new/"},
		{"unterminated script after a meta refresh", `<meta http-equiv="refresh" content="0; url=/new/"><script>location = "/other/"`, "/new/"},
		{"script empty type", `<script type="">self.location.href="/new/"</script>`, "/new/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if destination, _, ok := FindPageRedirect([]byte(tc.page), pageBase, true, true); !ok || destination != tc.want {
				t.Fatalf("got %q, %v; want %q", destination, ok, tc.want)
			}
		})
	}
}

func TestRefreshURL(t *testing.T) {
	for _, tc := range []struct {
		content, want string
		strict        bool
	}{
		{"0; url=/new/", "/new/", true},
		{"0;url=/new/", "/new/", true},
		{"0,url=/new/", "/new/", true},
		{"0 url=/new/", "/new/", true},
		{" 0 ; URL = /new/ ", "/new/", true},
		{"00; url=/new/", "/new/", true},
		{"0.0; url=/new/", "/new/", true},
		{"0.; url=/new/", "/new/", true},
		{"0; url='/new/'", "/new/", true},
		{`0; url="/new/" ignored`, "/new/", true},
		{"0; url='/new/", "/new/", true},
		{"0; '/new/'", "/new/", true},
		{"0; /new/", "/new/", true},
		{"0; uber/", "uber/", true},
		{"0; url/", "url/", true},
		{"0; urlx=/new/", "urlx=/new/", true},
		{"0; url", "url", true},
		{"0.5; url=/new/", "/new/", false},
		{".5; url=/new/", "/new/", false},
		{".; url=/new/", "/new/", false},
		{"0.0.0; url=/new/", "/new/", false},
		{"00.9.1,/new/", "/new/", false},
		{"1; url=/new/", "", false},
		{"10; url=/new/", "", false},
		{"-0; url=/new/", "", false},
		{"0", "", false},
		{"0;", "", false},
		{"0.5", "", false},
		{"0; url=", "", false},
		{"0; url=''", "", false},
		{"0/new/", "", false},
		{"", "", false},
	} {
		got, strict, ok := refreshURL(tc.content)
		if got != tc.want || ok != (tc.want != "") || strict != tc.strict {
			t.Errorf("refreshURL(%q) = %q, %v, %v; want %q, %v", tc.content, got, strict, ok, tc.want, tc.strict)
		}
	}
}

func TestResolveReference(t *testing.T) {
	for _, tc := range []struct{ reference, base, want string }{
		{"/new/", "/old/", "/new/"},
		{"new/", "/old/", "/old/new/"},
		{"../new/", "/old/", "/new/"},
		{"../../new/", "/old/", "/new/"},
		{"./", "/docs/old/", "/docs/old/"},
		{"..", "/docs/old/", "/docs/"},
		{".", "/docs/old/", "/docs/old/"},
		{"../new.html#install", "/docs/old/", "/docs/new.html#install"},
		{"?q=1", "/old/", "/old/?q=1"},
		{"#f", "/old/", "/old/#f"},
		{"", "/old/", "/old/"},
		{"/a/./b/../c", "/old/", "/a/c"},
		{"https://www.example.com/new/", "/old/", "https://www.example.com/new/"},
		{"//www.example.com/new/", "/old/", "//www.example.com/new/"},
		{"javascript:alert(1)", "/old/", "javascript:alert(1)"},
		{"a:b/c", "/old/", "a:b/c"},
		{"b/c:d", "/old/", "/old/b/c:d"},
		{".//www.example.com/", "/", "//www.example.com/"},
		{"..//evil.example/x", "/old/", "//evil.example/x"},
		{"../new/", "/café/old/", "/caf%C3%A9/new/"},
		{"new/", "/café/old/", "/caf%C3%A9/old/new/"},
		{"new/", "/caf%C3%A9/old/", "/caf%C3%A9/old/new/"},
		{"new/", `/a|b"c/`, "/a%7Cb%22c/new/"},
		{"new/", "/100%/", "/100%25/new/"},
		{"/new/", "/café/", "/new/"},
	} {
		if got := ResolveReference(tc.reference, tc.base); got != tc.want {
			t.Errorf("ResolveReference(%q, %q) = %q, want %q", tc.reference, tc.base, got, tc.want)
		}
	}
}

// A destination resolved against the page is judged like a website
// redirect's, and must not lead back to the page.
func TestPageLocation(t *testing.T) {
	const host = "www.example.com"
	preserve, drop, external := ObjectPolicy{Query: "preserve"}, ObjectPolicy{Query: "drop"}, ObjectPolicy{Query: "drop", External: true}
	for _, tc := range []struct {
		name, reference, base string
		policy                ObjectPolicy
		rawQuery, want        string // want empty: not accepted
	}{
		{"relative", "../new/", "/docs/old/", preserve, "ref=feed", "/docs/new/?ref=feed"},
		{"own query", "?lang=en", "/docs/old/", preserve, "ref=feed", "/docs/old/?lang=en"},
		{"same host", "https://www.example.com/new/", "/old/", preserve, "", "https://www.example.com/new/"},
		{"other host", "https://docs.example.com/new/", "/old/", preserve, "", ""},
		{"other host allowed", "https://docs.example.com/new/", "/old/", external, "ref=feed", "https://docs.example.com/new/"},
		{"network path", "//www.example.com/new/", "/old/", external, "", ""},
		{"resolved to a network path", ".//docs.example.com/", "/", external, "", ""},
		{"resolved to a network path from a page", "..//evil.example/x", "/old/", external, "", ""},
		{"http", "http://www.example.com/new/", "/old/", external, "", ""},
		{"space", "new page/", "/old/", preserve, "", ""},
		{"non-ASCII", "/café/", "/old/", preserve, "", ""},
		{"non-ASCII page", "../new/", "/café/old/", preserve, "", "/caf%C3%A9/new/"},
		{"too long", strings.Repeat("a", MaxTargetBytes), "/old/", preserve, "", ""},
		{"empty", "", "/old/", preserve, "", ""},
		{"fragment only", "#install", "/old/", drop, "ref=feed", ""},
		{"same path", "./", "/old/", drop, "ref=feed", ""},
		{"same path behind the trailing slash", "/old/", "/old/", preserve, "ref=feed", ""},
		{"same path and query", "/old/?ref=feed", "/old/", drop, "ref=feed", ""},
		{"same URL on the request's host", "https://WWW.example.com./old/#top", "/old/", drop, "", ""},
		{"same path on the request's host without a slash", "https://www.example.com?ref=feed", "/", drop, "ref=feed", ""},
		{"same path, other query", "/old/?lang=en", "/old/", drop, "ref=feed", "/old/?lang=en"},
		{"same path on another host", "https://docs.example.com/old/", "/old/", external, "", "https://docs.example.com/old/"},
		{"same encoded page", "../old/", "/café/old/", preserve, "", ""},
		{"page without its slash", "/old", "/old/", drop, "ref=feed", ""},
		{"page without its slash and the request's query", "/old?ref=feed", "/old/", drop, "ref=feed", ""},
		{"page without its slash and another query", "/old?lang=en", "/old/", drop, "", "/old?lang=en"},
		{"root page", "https://www.example.com", "/", drop, "", ""},
		{"same host with dot segments", "https://www.example.com/a/../old/./", "/old/", drop, "", ""},
		{"same host with lowercase escapes", "https://www.example.com/caf%c3%a9/", "/caf%C3%A9/", drop, "", ""},
		{"same host with an escaped unreserved character", "https://www.example.com/%6Fld/", "/old/", drop, "", ""},
		{"same host with port 443", "https://www.example.com:443/old/", "/old/", drop, "", ""},
		{"same host with another port", "https://www.example.com:8443/old/", "/old/", drop, "", "https://www.example.com:8443/old/"},
		{"escaped slash is another path", "/old%2F", "/old/", drop, "", "/old%2F"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			destination := ResolveReference(tc.reference, tc.base)
			got, ok, err := tc.policy.PageLocation(destination, tc.base, host, tc.rawQuery, false)
			if err != nil || ok != (tc.want != "") || got != tc.want {
				t.Fatalf("got %q, %v, %v; want %q", got, ok, err, tc.want)
			}
		})
	}
}

// FuzzFindPageRedirect checks that no page or script makes the detection
// fail, and that an accepted Location without external destinations stays on
// the site: a local absolute path, or an https URL to the request's host.
func FuzzFindPageRedirect(f *testing.F) {
	for _, seed := range []string{
		`<meta http-equiv="refresh" content="0; url=../new/"><script>location.replace("../new/" + location.search)</script>`,
		`<noscript><meta http-equiv=refresh content="0;url='/a/'"></noscript><script>const t = "/a/"; window.location.replace(t + (window.location.hash || ""))</script>`,
		"<script>window.location.replace(`/b/${location.hash ? `#${location.hash}` : ''}`)</script>",
		`<template><meta http-equiv="refresh" content="0; url=/c/"></template><base href="/">`,
		`<script>location = "https:\/\/www.example.com\/d\/\u003f"</script>`,
		`<script>/* location */ if (/x/.test(location)) location.href = '\x2fe/';</script>`,
	} {
		f.Add([]byte(seed), "/docs/old/")
	}
	const host = "www.example.com"
	f.Fuzz(func(t *testing.T, page []byte, base string) {
		// A base is the path of an index object, whose key is at most 1024 bytes.
		base = "/" + strings.TrimLeft(base[:min(len(base), 1024)], "/")
		destination, detector, ok := FindPageRedirect(page, base, true, true)
		if again, _, okAgain := FindPageRedirect(page, base, true, true); again != destination || okAgain != ok {
			t.Fatal("not deterministic")
		}
		if !ok {
			if destination != "" || detector != "" {
				t.Fatal("a result without a redirect")
			}
		} else if detector != DetectMetaRefresh && detector != DetectScript {
			t.Fatalf("detector %q", detector)
		}
		if _, _, ok := FindPageRedirect(page, base, false, false); ok {
			t.Fatal("a redirect without detectors")
		}
		_, _ = classifyScript(string(page))
		location, accepted, _ := ObjectPolicy{Query: "preserve"}.PageLocation(destination, base, host, "q=1", false)
		if !accepted {
			return
		}
		if !uriText(location) {
			t.Fatalf("Location %q has characters outside a URI", location)
		}
		if strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
			return
		}
		u, err := url.Parse(location)
		if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil || strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")) != host {
			t.Fatalf("Location %q leaves the site", location)
		}
	})
}
