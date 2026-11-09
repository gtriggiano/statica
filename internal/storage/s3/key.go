package s3

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// validKey treats % as a literal byte. URL decoding belongs to the visitor
// boundary; doing it here would corrupt valid configured publication prefixes.
func validKey(key, prefix string) bool {
	if key == "" || len(key) > 1024 || !utf8.ValidString(key) || !strings.HasPrefix(key, prefix) || strings.Contains(key, "\\") {
		return false
	}
	for _, ch := range key {
		if unicode.IsControl(ch) {
			return false
		}
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
