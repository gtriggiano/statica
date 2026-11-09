package site

import (
	"mime"
	"net/http"
	"strings"
)

// Header presence matters: an absent Accept-Encoding permits any coding while
// an explicitly empty field permits identity alone. Malformed members and
// duplicate members use the lowest quality, so an explicit refusal cannot be
// undone by another occurrence. Qualities are exact thousandths, never floats.
type encodingPreferences struct {
	present bool
	quality map[string]int
}

func headerValue(h http.Header, name string) (string, bool) {
	var values []string
	present := false
	for key, vs := range h {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, vs...)
		}
	}
	return strings.Join(values, ","), present
}

func parseEncodings(h http.Header) encodingPreferences {
	value, present := headerValue(h, "Accept-Encoding")
	return encodingPreferences{present: present, quality: parseQualities(value)}
}

func (p encodingPreferences) q(coding string) int {
	if !p.present {
		return 1000
	}
	if q, ok := p.quality[coding]; ok {
		return q
	}
	if coding == "identity" {
		if q, ok := p.quality["*"]; ok && q == 0 {
			return 0
		}
		return 1000
	}
	if q, ok := p.quality["*"]; ok {
		return q
	}
	return 0
}

func parseQualities(value string) map[string]int {
	result := make(map[string]int)
	for _, member := range strings.Split(value, ",") {
		fields := strings.Split(member, ";")
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		if name == "" {
			continue
		}
		q := 1000
		if len(fields) > 1 {
			if len(fields) != 2 {
				q = 0
			} else {
				key, value, ok := strings.Cut(strings.TrimSpace(fields[1]), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
					q = 0
				} else {
					q = quality(strings.TrimSpace(value))
				}
			}
		}
		if previous, ok := result[name]; !ok || q < previous {
			result[name] = q
		}
	}
	return result
}

func quality(s string) int {
	if s == "0" {
		return 0
	}
	if s == "1" {
		return 1000
	}
	if len(s) < 2 || len(s) > 5 || s[1] != '.' || s[0] != '0' && s[0] != '1' {
		return 0
	}
	q, scale := 0, 100
	for _, c := range s[2:] {
		if c < '0' || c > '9' || s[0] == '1' && c != '0' {
			return 0
		}
		q += int(c-'0') * scale
		scale /= 10
	}
	if s[0] == '1' {
		return 1000
	}
	return q
}

func acceptsHTML(h http.Header) bool {
	value, _ := headerValue(h, "Accept")
	// Accept permits media parameters in addition to q. A quoted comma inside
	// a parameter is part of that member, not the start of another media range.
	found, best := false, 1000
	for _, member := range splitHTTPValues(value, ',') {
		fields := splitHTTPValues(member, ';')
		if !strings.EqualFold(strings.TrimSpace(fields[0]), "text/html") {
			continue
		}
		q := 1000
		_, params, err := mime.ParseMediaType(member)
		if err != nil {
			q = 0
		} else if weight, ok := params["q"]; ok {
			q = quality(weight)
			// HTTP qvalues are unquoted numeric tokens; MIME's parser would otherwise
			// also accept a quoted number as a parameter value.
			for _, field := range fields[1:] {
				key, raw, ok := strings.Cut(strings.TrimSpace(field), "=")
				if ok && strings.EqualFold(strings.TrimSpace(key), "q") && strings.HasPrefix(strings.TrimSpace(raw), "\"") {
					q = 0
				}
			}
		}
		if !found || q < best {
			best = q
		}
		found = true
	}
	return found && best > 0
}

func splitHTTPValues(value string, delimiter byte) []string {
	var result []string
	start := 0
	quoted, escaped := false, false
	for i := 0; i < len(value); i++ {
		if escaped {
			escaped = false
			continue
		}
		if quoted && value[i] == '\\' {
			escaped = true
			continue
		}
		if value[i] == '"' {
			quoted = !quoted
			continue
		}
		if !quoted && value[i] == delimiter {
			result = append(result, value[start:i])
			start = i + 1
		}
	}
	return append(result, value[start:])
}
