package redirect

import (
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type scriptKind int

const (
	scriptIgnored      scriptKind = iota // does not mention location
	scriptRecognized                     // a recognized redirect, which can originate one
	scriptConsistent                     // leads to one destination, which it can only confirm
	scriptInconsistent                   // mentions location, but not as a redirect to one destination
)

// classifyScript classifies the text of a script that runs. A recognized
// script (see scriptURL) leads to its destination. Any other script that
// mentions location is consistent when it can be lexed, navigates
// (location =, .href =, .replace( or .assign( on one of locationTargets),
// and its string literals, the static parts of template literals included,
// hold exactly one distinct value other than the empty string and strings
// made of '#', '?' and '&' only: that value is its destination. Otherwise it
// is inconsistent. A script mentions location when it has location as an
// identifier or as a whole string literal, as in window["location"], or a
// backslash outside strings, which can escape an identifier. A script whose
// text has neither the word location nor a backslash is ignored unread. A
// leading "use strict" directive is not a literal, and a script with an
// HTML-like comment, <!-- or -->, cannot be lexed.
func classifyScript(text string) (destination string, kind scriptKind) {
	if url, ok := scriptURL(text); ok {
		return url, scriptRecognized
	}
	if !strings.Contains(text, "location") && !strings.Contains(text, `\`) {
		return "", scriptIgnored
	}
	// HTML-like comments change how browsers read the rest of the script.
	if strings.Contains(text, "<!--") {
		return "", scriptInconsistent
	}
	c := &jsClassifier{s: text}
	ok := c.tokens(0, false)
	switch {
	case c.stopped || !ok:
		return "", scriptInconsistent
	case !c.mentions:
		return "", scriptIgnored
	case !c.navigates || c.literals != 1:
		return "", scriptInconsistent
	}
	return c.literal, scriptConsistent
}

var (
	// scriptTargets are the expressions a recognized script can assign a
	// destination to: those of the page's own window. The location of top or
	// parent navigates another window when the page is in a frame, which an
	// HTTP redirect cannot do; locationTargets includes them.
	scriptTargets = map[string]bool{
		"location": true, "location.href": true, "window.location": true, "window.location.href": true,
		"document.location": true, "document.location.href": true, "self.location": true, "self.location.href": true,
	}
	// scriptLocations are the objects whose replace and assign a recognized
	// script can call with a destination.
	scriptLocations = map[string]bool{
		"location": true, "window.location": true, "document.location": true, "self.location": true,
	}
	// scriptSuffixes are the locations whose search and hash a recognized
	// script can add to the destination: the browser keeps them, and
	// statica's query policy decides the query.
	scriptSuffixes = map[string]bool{"location": true, "window.location": true, "document.location": true}
)

// scriptURL returns the destination of a recognized script, whose whole text,
// after whitespace, a leading "use strict" directive and a final semicolon,
// is one of:
//
//	TARGET = LITERAL SUFFIX*
//	OBJECT.replace(LITERAL SUFFIX*)
//	OBJECT.assign(LITERAL SUFFIX*)
//
// with TARGET in scriptTargets and OBJECT in scriptLocations. A SUFFIX is
// + X.search or + X.hash, or the same with an empty string in either quotes
// as fallback, as in (X.search || ""), with X in scriptSuffixes. LITERAL is a
// single- or double-quoted string whose only escapes are \/, \\, \', \" and
// \uXXXX.
func scriptURL(text string) (string, bool) {
	p := &jsParser{s: text}
	p.space()
	p.directive()
	path := p.path()
	p.space()
	var url string
	var ok bool
	object, method, _ := cutLast(path)
	switch {
	case scriptTargets[path]:
		if !p.eat('=') || p.peek('=') || p.peek('>') {
			return "", false
		}
		p.space()
		url, ok = p.expression()
	case scriptLocations[object] && (method == "replace" || method == "assign"):
		if !p.eat('(') {
			return "", false
		}
		p.space()
		url, ok = p.expression()
		p.space()
		ok = ok && p.eat(')')
	}
	if !ok {
		return "", false
	}
	p.space()
	p.eat(';')
	p.space()
	if p.i != len(p.s) {
		return "", false
	}
	return url, true
}

// jsParser reads the few JavaScript tokens of scriptURL.
type jsParser struct {
	s string
	i int
}

func (p *jsParser) space() {
	for p.i < len(p.s) && isJSSpace(p.s[p.i]) {
		p.i++
	}
}

func (p *jsParser) peek(c byte) bool { return p.i < len(p.s) && p.s[p.i] == c }

func (p *jsParser) eat(c byte) bool {
	if p.peek(c) {
		p.i++
		return true
	}
	return false
}

// directive skips a "use strict" directive, ended by a semicolon or a line
// terminator, and the whitespace that follows it.
func (p *jsParser) directive() {
	if !strings.HasPrefix(p.s[p.i:], `"use strict"`) && !strings.HasPrefix(p.s[p.i:], `'use strict'`) {
		return
	}
	start := p.i
	p.i += len(`"use strict"`)
	space := p.i
	p.space()
	if !p.eat(';') && strings.IndexAny(p.s[space:p.i], "\n\r") < 0 {
		p.i = start
		return
	}
	p.space()
}

// path reads identifiers joined by dots, without whitespace.
func (p *jsParser) path() string {
	start := p.i
	for {
		first := p.i
		for p.i < len(p.s) && (isIdentifierStart(p.s[p.i]) || p.i > first && isDigit(p.s[p.i])) {
			p.i++
		}
		if p.i == first {
			if p.i > start {
				p.i-- // the dot before no identifier is not part of the path
			}
			return p.s[start:p.i]
		}
		if !p.eat('.') {
			return p.s[start:p.i]
		}
	}
}

// expression reads a literal followed by its suffixes.
func (p *jsParser) expression() (string, bool) {
	url, ok := p.literal()
	if !ok {
		return "", false
	}
	for {
		before := p.i
		p.space()
		if !p.eat('+') {
			p.i = before
			return url, true
		}
		p.space()
		wrapped := p.eat('(')
		if wrapped {
			p.space()
		}
		object, property, _ := cutLast(p.path())
		if !scriptSuffixes[object] || property != "search" && property != "hash" {
			return "", false
		}
		if wrapped {
			p.space()
			if !p.eat('|') || !p.eat('|') {
				return "", false
			}
			p.space()
			if fallback, ok := p.literal(); !ok || fallback != "" {
				return "", false
			}
			p.space()
			if !p.eat(')') {
				return "", false
			}
		}
	}
}

func (p *jsParser) literal() (string, bool) {
	if !p.peek('"') && !p.peek('\'') {
		return "", false
	}
	quote := p.s[p.i]
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c == quote:
			p.i++
			return b.String(), true
		case c == '\\':
			n, ok := jsEscape(&b, p.s[p.i:], true)
			if !ok {
				return "", false
			}
			p.i += n
		case c == '\n' || c == '\r':
			return "", false
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	return "", false
}

// jsEscape writes the value of the escape sequence at the start of s, a
// backslash and what follows, and returns its length. Strict allows only
// \/, \\, \', \" and \uXXXX; otherwise the escapes of JavaScript strings are
// accepted, except legacy octal ones, and a line continuation writes
// nothing.
func jsEscape(b *strings.Builder, s string, strict bool) (int, bool) {
	if len(s) < 2 {
		return 0, false
	}
	c := s[1]
	if c == 'u' {
		r, n, ok := unicodeEscape(s, strict)
		if ok {
			b.WriteRune(r)
		}
		return n, ok
	}
	if strict {
		if strings.IndexByte(`/\'"`, c) < 0 {
			return 0, false
		}
		b.WriteByte(c)
		return 2, true
	}
	switch c {
	case 'n':
		b.WriteByte('\n')
	case 't':
		b.WriteByte('\t')
	case 'r':
		b.WriteByte('\r')
	case 'b':
		b.WriteByte('\b')
	case 'f':
		b.WriteByte('\f')
	case 'v':
		b.WriteByte('\v')
	case '\n':
	case '\r':
		if len(s) > 2 && s[2] == '\n' {
			return 3, true
		}
	case '0':
		if len(s) > 2 && isDigit(s[2]) {
			return 0, false
		}
		b.WriteByte(0)
	case 'x':
		if len(s) < 4 || !isHex(s[2]) || !isHex(s[3]) {
			return 0, false
		}
		b.WriteRune(rune(hexValue(s[2:4])))
		return 4, true
	default:
		if isDigit(c) {
			return 0, false
		}
		b.WriteByte(c)
	}
	return 2, true
}

// unicodeEscape decodes the \uXXXX at the start of s, joining a surrogate
// pair written as two escapes, or, unless strict, a \u{X} escape. A lone
// surrogate is refused.
func unicodeEscape(s string, strict bool) (rune, int, bool) {
	if !strict && len(s) > 3 && s[2] == '{' {
		end := strings.IndexByte(s, '}')
		if end < 4 || end > 9 {
			return 0, 0, false
		}
		for i := 3; i < end; i++ {
			if !isHex(s[i]) {
				return 0, 0, false
			}
		}
		r := rune(hexValue(s[3:end]))
		if r > utf8.MaxRune || utf16.IsSurrogate(r) {
			return 0, 0, false
		}
		return r, end + 1, true
	}
	hex4 := func(s string) (rune, bool) {
		if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
			return 0, false
		}
		for i := 2; i < 6; i++ {
			if !isHex(s[i]) {
				return 0, false
			}
		}
		return rune(hexValue(s[2:6])), true
	}
	r, ok := hex4(s)
	if !ok {
		return 0, 0, false
	}
	if !utf16.IsSurrogate(r) {
		return r, 6, true
	}
	low, ok := hex4(s[6:])
	if r > 0xdbff || !ok {
		return 0, 0, false
	}
	if joined := utf16.DecodeRune(r, low); joined != utf8.RuneError {
		return joined, 12, true
	}
	return 0, 0, false
}

func hexValue(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i] | 0x20 // lowercase letters, keep digits
		if c <= '9' {
			n = n*16 + int(c-'0')
		} else {
			n = n*16 + int(c-'a'+10)
		}
	}
	return n
}

// locationTargets are the expressions whose assignment, or the assignment of
// whose href, sends the browser to a new URL, and whose replace and assign
// do.
var locationTargets = map[string]bool{
	"location": true, "window.location": true, "document.location": true, "self.location": true,
	"top.location": true, "parent.location": true, "window.top.location": true,
}

// maxJSDepth bounds the nesting of template literals.
const maxJSDepth = 16

// jsClassifier lexes a script and classifies it as it goes. It keeps only the
// previous token, the start of the member path that token ends and up to two
// distinct literals, so its work and memory grow linearly with the script.
// It fails on what it cannot lex safely: a slash outside a comment, a string
// or a template literal (a division or a regular expression), a backslash or
// a byte that is not ASCII outside them, an unterminated string, comment or
// template literal, an invalid escape, and template literals nested deeper
// than maxJSDepth. It stops as soon as the script mentions location and has
// two distinct literals: the script is then inconsistent, whatever follows.
type jsClassifier struct {
	s string
	i int

	mentions, navigates, stopped bool
	literal                      string // the first literal that counts
	literals                     int    // the distinct literals that count, at most 2

	newline bool // a line terminator since the previous token
	pending bool // the script starts with "use strict", a directive unless the next token continues it

	last jsToken    // the previous token
	path memberPath // the member path that last ends, when last is an identifier
}

type jsTokenKind int

const (
	jsNone jsTokenKind = iota
	jsPunctuator
	jsIdentifier
	jsValue // a string, a template literal or a number
)

type jsToken struct {
	kind jsTokenKind
	text string // the punctuator
}

func (t jsToken) is(punctuator string) bool { return t.kind == jsPunctuator && t.text == punctuator }

// memberPath is identifiers joined by dots, such as window.location.href. It
// keeps only its first segments: no location target is longer.
type memberPath struct {
	segments [4]string
	n        int  // the segments seen
	root     bool // not a property of another expression, as in f().location
}

func (m *memberPath) String() string {
	if m.n > len(m.segments) {
		return ""
	}
	return strings.Join(m.segments[:m.n], ".")
}

// jsOperators are the punctuators of JavaScript that start with an operator
// character, longest first, for maximal munch. A slash is missing: it starts
// a comment, a division or a regular expression, which a lexer cannot tell
// apart without parsing.
var jsOperators = []string{
	">>>=",
	"===", "!==", "**=", "<<=", ">>=", ">>>", "&&=", "||=", "??=", "...",
	"=>", "==", "!=", "<=", ">=", "&&", "||", "??", "?.", "++", "--", "+=", "-=", "*=", "%=", "&=", "|=", "^=", "**", "<<", ">>",
}

// settle decides, at the token after a leading "use strict", whether it was a
// directive: directive tells whether that token ends the statement.
func (c *jsClassifier) settle(directive bool) {
	if c.pending {
		c.pending = false
		if !directive {
			c.record("use strict")
		}
	}
	c.newline = false
}

func (c *jsClassifier) identifier(name string) {
	c.settle(c.newline)
	c.mentions = c.mentions || name == "location"
	if c.last.is(".") && c.path.n > 0 {
		if c.path.n < len(c.path.segments) {
			c.path.segments[c.path.n] = name
		}
		c.path.n++
	} else {
		c.path = memberPath{n: 1, root: !c.last.is(".") && !c.last.is("?.")}
		c.path.segments[0] = name
	}
	c.last = jsToken{kind: jsIdentifier}
}

func (c *jsClassifier) punctuator(p string) {
	c.settle(p == ";" || c.newline && (p == "{" || p == "}"))
	if c.last.kind == jsIdentifier && c.path.root {
		path := c.path.String()
		object, method, _ := cutLast(path)
		switch {
		case p == "=" && (locationTargets[path] || method == "href" && locationTargets[object]),
			p == "(" && (method == "replace" || method == "assign") && locationTargets[object]:
			c.navigates = true
		}
	}
	if p != "." {
		c.path = memberPath{}
	}
	c.last = jsToken{jsPunctuator, p}
}

// value records a string literal, or a static part of a template literal,
// which follows a line terminator as statement only when it is a string.
func (c *jsClassifier) value(v string, statement bool) {
	c.settle(statement)
	c.record(v)
	c.other()
	c.stopped = c.mentions && c.literals > 1
}

func (c *jsClassifier) record(v string) {
	c.mentions = c.mentions || v == "location"
	if strings.Trim(v, "#?&") != "" {
		switch {
		case c.literals == 0:
			c.literal, c.literals = v, 1
		case c.literals == 1 && v != c.literal:
			c.literals = 2
		}
	}
}

// other records a token that ends any member path.
func (c *jsClassifier) other() {
	c.newline = false
	c.path, c.last = memberPath{}, jsToken{kind: jsValue}
}

// tokens lexes up to the end of the script or, in a template substitution,
// up to the brace that closes it.
func (c *jsClassifier) tokens(depth int, substitution bool) bool {
	braces := 0
	for c.i < len(c.s) && !c.stopped {
		b := c.s[c.i]
		switch {
		case isJSSpace(b):
			c.newline = c.newline || b == '\n' || b == '\r'
			c.i++
		case strings.HasPrefix(c.s[c.i:], "//"):
			end := strings.IndexAny(c.s[c.i:], "\n\r")
			if end < 0 {
				end = len(c.s) - c.i
			}
			// A line terminator that is not ASCII would end the comment.
			for _, ch := range []byte(c.s[c.i : c.i+end]) {
				if ch >= utf8.RuneSelf {
					return false
				}
			}
			c.i += end
		case strings.HasPrefix(c.s[c.i:], "/*"):
			end := strings.Index(c.s[c.i+2:], "*/")
			if end < 0 {
				return false
			}
			c.newline = c.newline || strings.ContainsAny(c.s[c.i+2:c.i+2+end], "\n\r")
			c.i += end + 4
		case strings.HasPrefix(c.s[c.i:], "-->"):
			return false
		case b == '\'' || b == '"':
			if !c.string(b, depth == 0) {
				return false
			}
		case b == '`':
			if depth >= maxJSDepth || !c.template(depth+1) {
				return false
			}
		case isIdentifierStart(b):
			start := c.i
			for c.i < len(c.s) && (isIdentifierStart(c.s[c.i]) || isDigit(c.s[c.i])) {
				c.i++
			}
			c.identifier(c.s[start:c.i])
		case isDigit(b):
			for c.i < len(c.s) && (isIdentifierStart(c.s[c.i]) || isDigit(c.s[c.i]) || c.s[c.i] == '.') {
				c.i++
			}
			c.settle(c.newline)
			c.other()
		case b == '{':
			braces++
			c.i++
			c.punctuator("{")
		case b == '}':
			c.i++
			if substitution && braces == 0 {
				c.settle(false)
				c.other()
				return true
			}
			if braces--; braces < 0 {
				return false
			}
			c.punctuator("}")
		case strings.IndexByte("()[];,:.?=<>+-*%&|^!~@#", b) >= 0:
			operator := c.s[c.i : c.i+1]
			for _, o := range jsOperators {
				if strings.HasPrefix(c.s[c.i:], o) {
					operator = o
					break
				}
			}
			c.i += len(operator)
			c.punctuator(operator)
		default:
			// A backslash here escapes an identifier, which can be location.
			c.mentions = c.mentions || b == '\\'
			return false
		}
	}
	return c.stopped || !substitution
}

// string lexes a string literal that starts at c.i with quote. At the start
// of a script, top, a "use strict" without escapes may be a directive.
func (c *jsClassifier) string(quote byte, top bool) bool {
	var b strings.Builder
	start := c.i
	for c.i++; c.i < len(c.s); {
		switch ch := c.s[c.i]; {
		case ch == quote:
			c.i++
			if raw := c.s[start:c.i]; top && c.last.kind == jsNone && (raw == `"use strict"` || raw == `'use strict'`) {
				c.pending = true
				c.other()
				return true
			}
			c.value(b.String(), c.newline)
			return true
		case ch == '\\':
			n, ok := jsEscape(&b, c.s[c.i:], false)
			if !ok {
				return false
			}
			c.i += n
		case ch == '\n' || ch == '\r':
			return false
		default:
			b.WriteByte(ch)
			c.i++
		}
	}
	return false
}

// template lexes a template literal that starts at c.i: each static part is
// a value, and each substitution is lexed as code.
func (c *jsClassifier) template(depth int) bool {
	var b strings.Builder
	for c.i++; c.i < len(c.s) && !c.stopped; {
		switch ch := c.s[c.i]; {
		case ch == '`':
			c.i++
			c.value(b.String(), false)
			return true
		case ch == '\\':
			n, ok := jsEscape(&b, c.s[c.i:], false)
			if !ok {
				return false
			}
			c.i += n
		case strings.HasPrefix(c.s[c.i:], "${"):
			c.value(b.String(), false)
			b.Reset()
			c.i += 2
			if !c.tokens(depth, true) {
				return false
			}
		default:
			b.WriteByte(ch)
			c.i++
		}
	}
	return c.stopped
}

func cutLast(path string) (before, after string, found bool) {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[:i], path[i+1:], true
	}
	return "", path, false
}

func isJSSpace(c byte) bool { return strings.IndexByte(" \t\n\r\v\f", c) >= 0 }
func isIdentifierStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$'
}
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
