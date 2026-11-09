package httpserver

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// normalizeAuthority deliberately accepts ASCII only. IDNA conversion belongs
// to configuration loading, never to visitor-controlled authority parsing.
func normalizeAuthority(authority string) (string, error) {
	bad := func() (string, error) { return "", fmt.Errorf("malformed authority") }
	if authority == "" {
		return bad()
	}
	for i := range authority {
		if authority[i] <= 32 || authority[i] >= 127 {
			return bad()
		}
	}
	host := authority
	if strings.HasPrefix(host, "[") {
		end := strings.IndexByte(host, ']')
		if end < 0 {
			return bad()
		}
		tail := host[end+1:]
		if tail != "" && (!strings.HasPrefix(tail, ":") || !validPort(tail[1:])) {
			return bad()
		}
		ip, err := netip.ParseAddr(host[1:end])
		if err != nil || !ip.Is6() || ip.Zone() != "" {
			return bad()
		}
		return ip.String(), nil
	}
	if strings.ContainsAny(host, "[]/\\?#@%") {
		return bad()
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		if !validPort(host[i+1:]) {
			return bad()
		}
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip, err := netip.ParseAddr(host); err == nil && ip.Is4() {
		return ip.String(), nil
	}
	if len(host) == 0 || len(host) > 253 {
		return bad()
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return bad()
		}
		for i := range label {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return bad()
			}
		}
	}
	return host, nil
}

func validPort(port string) bool {
	if len(port) == 0 || len(port) > 5 {
		return false
	}
	for i := range port {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}
