package redirect

import (
	"net/netip"
	"net/url"
	"strings"
)

// ObjectPolicy turns a destination that an object declares, such as its
// x-amz-website-redirect-location, into a Location. The destination comes
// from the bucket, not from the configuration, so it is validated on every
// use. Query is drop or preserve. Without External, an absolute destination
// must name the request's own host.
type ObjectPolicy struct {
	Query    string
	External bool
}

// Location returns the Location for an object's destination value, or false
// when the value is not an acceptable destination: anything but a local
// absolute path or an https URL, characters outside a URI, and, without
// External, an https URL to another host than host, the request's normalized
// hostname. rawQuery and forceQuery are the request's validated query, added
// with the preserve policy when the destination has none. A destination
// fragment is kept. A Location longer than MaxTargetBytes is a RequestError
// with status 414.
func (p ObjectPolicy) Location(value, host, rawQuery string, forceQuery bool) (string, bool, error) {
	if value == "" || len(value) > MaxTargetBytes || !uriText(value) {
		return "", false, nil
	}
	reference, fragment, hasFragment := strings.Cut(value, "#")
	if strings.Contains(fragment, "#") {
		return "", false, nil
	}
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		u, err := url.Parse(reference)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || !validAuthority(u) {
			return "", false, nil
		}
		target := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if ip, err := netip.ParseAddr(target); err == nil {
			target = ip.String()
		}
		if target == "" || !p.External && target != host {
			return "", false, nil
		}
	}
	if p.Query == "preserve" && !strings.Contains(reference, "?") && (rawQuery != "" || forceQuery) {
		reference += "?" + safeRawQuery(rawQuery)
	}
	location := reference
	if hasFragment {
		location += "#" + fragment
	}
	if len(location) > MaxTargetBytes {
		return "", false, tooLarge()
	}
	return location, true, nil
}

// uriText reports whether s holds only characters allowed in a URI
// reference, with well-formed percent-escapes.
func uriText(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9':
		case strings.IndexByte("-._~:/?#[]@!$&'()*+,;=", c) >= 0:
		case c == '%':
			if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
				return false
			}
			i += 2
		default:
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}
