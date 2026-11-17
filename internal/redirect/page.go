package redirect

import (
	"bytes"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// The detectors of client_redirects, as the configuration and the access log
// name them.
const (
	DetectMetaRefresh = "meta_refresh"
	DetectScript      = "script"
)

// FindPageRedirect returns the destination of the redirect that an HTML
// redirect page declares, resolved against base, the escaped path of the
// page's URL, and the detector to report for it. The page's redirects are
// the meta refreshes whose delay the HTML declarative refresh steps read as
// zero and that have a URL, and the scripts that lead to one destination:
// recognized ones (see scriptURL) and consistent ones (see classifyScript).
// They must all lead to the same destination, whatever the detectors. A
// redirect originates from:
//
//   - with metaRefresh, a meta refresh whose delay is written with zeros only
//     (see refreshURL);
//   - with script, a recognized script.
//
// The other redirects never originate one: they only confirm it. A script
// that mentions location without being recognized or consistent makes the
// page declare none, and so do a base element with an href, an svg or math
// element and a CDATA section, which change how the page is read. The
// detector is DetectMetaRefresh when a meta refresh originates the redirect,
// DetectScript otherwise. The contents of template, which are inert, and of
// noscript, which depend on the browser, are ignored.
func FindPageRedirect(page []byte, base string, metaRefresh, script bool) (destination, detector string, ok bool) {
	if bytes.Contains(page, []byte("<![CDATA[")) {
		return "", "", false
	}
	var s pageScan
	s.scan(page)
	if s.ineligible || s.inconsistent {
		return "", "", false
	}
	// A page repeats its destination: each reference is resolved once.
	base = pagePath(base)
	resolved := map[string]string{}
	found := false
	for _, references := range [][]string{s.metas, s.confirming, s.recognized, s.consistent} {
		for _, reference := range references {
			d, ok := resolved[reference]
			if !ok {
				d = resolveReference(reference, base)
				resolved[reference] = d
			}
			if found && d != destination {
				return "", "", false
			}
			destination, found = d, true
		}
	}
	switch {
	case metaRefresh && len(s.metas) > 0:
		return destination, DetectMetaRefresh, true
	case script && len(s.recognized) > 0:
		return destination, DetectScript, true
	}
	return "", "", false
}

// pageScan collects the redirects of one page, as written: the meta
// refreshes that can originate a redirect, those that can only confirm one,
// and the scripts.
type pageScan struct {
	ineligible, inconsistent                  bool
	metas, confirming, recognized, consistent []string
}

// scan tokenizes page. The content of noscript, and of script, is raw text
// to the tokenizer, so the tags that noscript holds are never seen.
func (s *pageScan) scan(page []byte) {
	z := html.NewTokenizer(bytes.NewReader(page))
	templates := 0
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return
		}
		if tt == html.EndTagToken {
			if name, _ := z.TagName(); templates > 0 && string(name) == "template" {
				templates--
			}
			continue
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, more := z.TagName()
		switch tag := string(name); {
		case tag == "svg" || tag == "math":
			s.ineligible = true
			continue
		case tag == "template":
			templates++
			continue
		case templates > 0 || tag != "base" && tag != "meta" && tag != "script":
			continue
		}
		tag := string(name)
		attrs := map[string]string{}
		for more {
			var key, value []byte
			key, value, more = z.TagAttr()
			attrs[string(key)] = string(value)
		}
		switch tag {
		case "base":
			if _, ok := attrs["href"]; ok {
				s.ineligible = true
			}
		case "meta":
			if asciiEqualFold(attrs["http-equiv"], "refresh") {
				if reference, strict, ok := refreshURL(attrs["content"]); ok && strict {
					s.metas = append(s.metas, reference)
				} else if ok {
					s.confirming = append(s.confirming, reference)
				}
			}
		case "script":
			if z.Next() != html.TextToken {
				continue
			}
			text := string(z.Text())
			// A script that the page's end cuts short never runs.
			if z.Next() != html.EndTagToken || !runs(attrs) {
				continue
			}
			switch reference, kind := classifyScript(text); kind {
			case scriptRecognized:
				s.recognized = append(s.recognized, reference)
			case scriptConsistent:
				s.consistent = append(s.consistent, reference)
			case scriptInconsistent:
				s.inconsistent = true
			}
		}
	}
}

// htmlSpace is ASCII whitespace as HTML defines it.
const htmlSpace = "\t\n\f\r "

// refreshURL returns the URL of the content of a meta refresh whose delay
// the HTML shared declarative refresh steps read as zero, as they read it,
// and whether statica accepts that delay as written: digits that are all
// zero, optionally followed by a dot and zeros. The steps also read as zero
// a delay such as .5 or 0.5, which statica does not accept. A content
// without a URL, such as "0", reloads the page and is not a redirect.
func refreshURL(content string) (url string, strict, ok bool) {
	s := strings.TrimLeft(content, htmlSpace)
	digits := s[:len(s)-len(strings.TrimLeft(s, "0123456789"))]
	if digits == "" && !strings.HasPrefix(s, ".") || strings.Trim(digits, "0") != "" {
		return "", false, false
	}
	s = s[len(digits):]
	fraction := s[:len(s)-len(strings.TrimLeft(s, "0123456789."))]
	strict = digits != "" && (fraction == "" || fraction[0] == '.' && strings.Trim(fraction[1:], "0") == "")
	s = s[len(fraction):]
	if s == "" || s[0] != ';' && s[0] != ',' && strings.IndexByte(htmlSpace, s[0]) < 0 {
		return "", false, false
	}
	s = strings.TrimLeft(s, htmlSpace)
	if s != "" && (s[0] == ';' || s[0] == ',') {
		s = s[1:]
	}
	s = strings.TrimLeft(s, htmlSpace)
	url = s
	switch {
	case s == "":
	case s[0] == 'u' || s[0] == 'U':
		// "url" and "=" are skipped together, or not at all.
		if len(s) >= 3 && asciiEqualFold(s[:3], "url") {
			if rest := strings.TrimLeft(s[3:], htmlSpace); strings.HasPrefix(rest, "=") {
				url = unquote(strings.TrimLeft(rest[1:], htmlSpace))
			}
		}
	default:
		url = unquote(s)
	}
	url = strings.Trim(url, htmlSpace)
	if url == "" {
		return "", false, false
	}
	return url, strict, true
}

// unquote removes a leading quote from s, and from the next same quote on.
func unquote(s string) string {
	if s == "" || s[0] != '"' && s[0] != '\'' {
		return s
	}
	quote := s[0]
	s = s[1:]
	if i := strings.IndexByte(s, quote); i >= 0 {
		s = s[:i]
	}
	return s
}

// scriptTypes are the types of a script element that run its text: the
// JavaScript MIME type essences of HTML, module, and the empty type.
var scriptTypes = map[string]bool{
	"": true, "module": true,
	"application/ecmascript": true, "application/javascript": true, "application/x-ecmascript": true, "application/x-javascript": true,
	"text/ecmascript": true, "text/javascript": true, "text/javascript1.0": true, "text/javascript1.1": true, "text/javascript1.2": true,
	"text/javascript1.3": true, "text/javascript1.4": true, "text/javascript1.5": true, "text/jscript": true, "text/livescript": true,
	"text/x-ecmascript": true, "text/x-javascript": true,
}

// runs reports whether a script element with attrs runs its own text as
// JavaScript: no src, and a type in scriptTypes, compared ignoring ASCII case
// and surrounding whitespace, or a language without a type, which HTML reads
// as the type text/<language>. A classic script runs only without nomodule
// and, when it has for and event attributes, only for the window's onload,
// as HTML defines.
func runs(attrs map[string]string) bool {
	if _, ok := attrs["src"]; ok {
		return false
	}
	typ, ok := attrs["type"]
	if !ok && attrs["language"] != "" {
		typ = "text/" + attrs["language"]
	}
	typ = asciiLower(strings.Trim(typ, htmlSpace))
	if !scriptTypes[typ] {
		return false
	}
	if typ == "module" {
		return true
	}
	if _, ok := attrs["nomodule"]; ok {
		return false
	}
	target, hasFor := attrs["for"]
	event, hasEvent := attrs["event"]
	if hasFor && hasEvent {
		event = strings.Trim(event, htmlSpace)
		return asciiEqualFold(strings.Trim(target, htmlSpace), "window") && (asciiEqualFold(event, "onload") || asciiEqualFold(event, "onload()"))
	}
	return true
}

// asciiLower lowercases the ASCII letters of s, as HTML compares keywords.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// asciiEqualFold reports whether s and t are equal ignoring ASCII case only,
// unlike strings.EqualFold, which also folds characters such as U+017F.
func asciiEqualFold(s, t string) bool { return len(s) == len(t) && asciiLower(s) == asciiLower(t) }

// ResolveReference resolves reference, a destination found in a page whose
// URL path is base, as RFC 3986 section 5.2 describes: a relative reference
// takes the path of base, or is merged with its directory, and loses its dot
// segments. base is first percent-encoded where it holds bytes that a URI
// does not allow, as browsers encode the URL of a page. A reference with a
// scheme or an authority, or with a colon in its first segment, is returned
// unchanged, for ObjectPolicy.Location to judge.
func ResolveReference(reference, base string) string {
	return resolveReference(reference, pagePath(base))
}

// resolveReference is ResolveReference for a base that pagePath encoded.
func resolveReference(reference, base string) string {
	end := strings.IndexAny(reference, "/?#")
	if end < 0 {
		end = len(reference)
	}
	if strings.Contains(reference[:end], ":") || strings.HasPrefix(reference, "//") {
		return reference
	}
	path, rest := reference, ""
	if i := strings.IndexAny(reference, "?#"); i >= 0 {
		path, rest = reference[:i], reference[i:]
	}
	switch {
	case path == "":
		path = base
	case !strings.HasPrefix(path, "/"):
		path = base[:strings.LastIndexByte(base, '/')+1] + path
	}
	return removeDotSegments(path) + rest
}

// pagePath percent-encodes the bytes of an escaped path that are not allowed
// in the path of a URI, a '%' that starts no escape included.
func pagePath(path string) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~!$&'()*+,;=:@/", c) >= 0:
			b.WriteByte(c)
		case c == '%' && i+2 < len(path) && isHex(path[i+1]) && isHex(path[i+2]):
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		}
	}
	return b.String()
}

// removeDotSegments removes the "." and ".." segments of an absolute path, as
// RFC 3986 section 5.2.4 does.
func removeDotSegments(path string) string {
	if !strings.HasPrefix(path, "/") {
		return path
	}
	in := strings.Split(path[1:], "/")
	out := make([]string, 0, len(in))
	for i, segment := range in {
		last := i == len(in)-1
		switch segment {
		case ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, segment)
			continue
		}
		if last {
			out = append(out, "")
		}
	}
	return "/" + strings.Join(out, "/")
}

// PageLocation is Location for destination, found by FindPageRedirect in a
// page whose URL path is base, an index page whose path ends with a slash.
// It refuses a destination that leads back to the page, whose path is the
// page's, with or without its trailing slash: one without a query of its
// own, as an empty or fragment-only reference resolves, and a Location with
// the request's query.
func (p ObjectPolicy) PageLocation(destination, base, host, rawQuery string, forceQuery bool) (string, bool, error) {
	location, ok, err := p.Location(destination, host, rawQuery, forceQuery)
	if err != nil || !ok {
		return "", false, err
	}
	page := normalizePath(pagePath(base))
	samePage := func(path string) bool {
		return path == page || path+"/" == page && path != ""
	}
	if path, _, hasQuery, local := pageTarget(destination, host); local && samePage(path) && !hasQuery {
		return "", false, nil
	}
	if path, query, hasQuery, local := pageTarget(location, host); local && samePage(path) && query == rawQuery && hasQuery == (rawQuery != "" || forceQuery) {
		return "", false, nil
	}
	return location, true, nil
}

// pageTarget returns the normalized path and the query of an accepted
// destination or Location, without its fragment, and whether it leads to the
// site that the request reached: a local path does, and an https URL that
// names host, without a port or with port 443.
func pageTarget(reference, host string) (path, query string, hasQuery, local bool) {
	reference, _, _ = strings.Cut(reference, "#")
	if !strings.HasPrefix(reference, "/") {
		u, err := url.Parse(reference)
		if err != nil || u.Port() != "" && u.Port() != "443" {
			return "", "", false, false
		}
		name := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if ip, err := netip.ParseAddr(name); err == nil {
			name = ip.String()
		}
		if name != host {
			return "", "", false, false
		}
		// The scheme, "://" and the authority, as written.
		reference = reference[len(u.Scheme)+3:]
		reference = reference[strings.IndexAny(reference+"/", "/?"):]
		if !strings.HasPrefix(reference, "/") {
			reference = "/" + reference
		}
	}
	path, query, hasQuery = strings.Cut(reference, "?")
	return normalizePath(path), query, hasQuery, true
}

// normalizePath normalizes an absolute path as RFC 3986 section 6.2.2 does:
// its percent-escapes in uppercase, those of unreserved characters decoded,
// and its dot segments removed.
func normalizePath(path string) string {
	const digits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		if path[i] != '%' || i+2 >= len(path) || !isHex(path[i+1]) || !isHex(path[i+2]) {
			b.WriteByte(path[i])
			continue
		}
		c := byte(hexValue(path[i+1 : i+3]))
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteByte('%')
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&15])
		}
		i += 2
	}
	return removeDotSegments(b.String())
}
