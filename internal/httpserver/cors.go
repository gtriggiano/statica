package httpserver

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

var preflightHeaders = map[string]bool{
	"range": true, "if-match": true, "if-none-match": true,
	"if-modified-since": true, "if-unmodified-since": true, "if-range": true,
}

func canonicalOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid CORS origin")
	}
	host, err := normalizeAuthority(u.Host)
	if err != nil {
		return "", fmt.Errorf("invalid CORS origin")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	port := u.Port()
	if port != "" && !(u.Scheme == "http" && port == "80" || u.Scheme == "https" && port == "443") {
		host += ":" + port
	}
	return u.Scheme + "://" + host, nil
}

type corsPolicy struct {
	enabled bool
	origins map[string]bool
}

// apply adds grants for ordinary requests, or handles a complete preflight.
// Disallowed ordinary origins remain public requests without a CORS grant.
func (p corsPolicy) apply(h http.Header, r *http.Request) (status int, reason string) {
	if !p.enabled {
		return 0, ""
	}
	mergeVary(h, "Origin")
	values := headerValues(r.Header, "Origin")
	allowed := false
	origin := ""
	if len(values) == 1 {
		origin = values[0]
		canonical, err := canonicalOrigin(origin)
		allowed = err == nil && p.origins[canonical]
	}
	if r.Method != http.MethodOptions {
		if allowed {
			h.Set("Access-Control-Allow-Origin", origin)
		}
		return 0, ""
	}
	mergeVary(h, "Access-Control-Request-Method", "Access-Control-Request-Headers")
	methods := headerValues(r.Header, "Access-Control-Request-Method")
	if !allowed || len(methods) != 1 || (methods[0] != http.MethodGet && methods[0] != http.MethodHead) {
		return http.StatusForbidden, "cors_preflight_denied"
	}
	var names []string
	seen := map[string]bool{}
	for _, value := range headerValues(r.Header, "Access-Control-Request-Headers") {
		for _, name := range strings.Split(value, ",") {
			name = strings.ToLower(strings.Trim(name, " \t"))
			if !preflightHeaders[name] {
				return http.StatusForbidden, "cors_preflight_denied"
			}
			if !seen[name] {
				names = append(names, http.CanonicalHeaderKey(name))
				seen[name] = true
			}
		}
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", "GET, HEAD")
	if len(names) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(names, ", "))
	}
	return http.StatusNoContent, "cors_preflight"
}

func mergeVary(h http.Header, names ...string) {
	for _, name := range names {
		found := false
		for _, value := range h.Values("Vary") {
			for _, item := range strings.Split(value, ",") {
				if strings.EqualFold(strings.TrimSpace(item), name) || strings.TrimSpace(item) == "*" {
					found = true
				}
			}
		}
		if !found {
			h.Add("Vary", name)
		}
	}
}
