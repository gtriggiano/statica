package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/gtriggiano/statica/internal/site"
)

// RequestMetadata is an immutable request snapshot. Object is present only when
// the validated request reaches the content handler. Host is empty before host
// admission. IPs and request IDs must never be used as metric labels.
type RequestMetadata struct {
	AdmittedAt                                 time.Time
	Host                                       string
	Target                                     site.Target
	Object                                     *site.Object
	ClientIP, PeerIP                           netip.Addr
	ClientIPSource, RequestID, RequestIDSource string
	AttributionReason, RequestIDReason         string
}

type metadataKey struct{}

// MetadataFromContext returns a copy; callers cannot mutate the context snapshot.
func MetadataFromContext(ctx context.Context) (RequestMetadata, bool) {
	m, ok := ctx.Value(metadataKey{}).(RequestMetadata)
	if m.Object != nil {
		object := *m.Object
		m.Object = &object
	}
	return m, ok
}

type identityPolicy struct {
	trusted                       []netip.Prefix
	clientHeader, requestIDHeader string
	maxHops                       int
}

func (p identityPolicy) trusts(ip netip.Addr) bool {
	for _, prefix := range p.trusted {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

func headerValues(h http.Header, name string) []string {
	var values []string
	for key, v := range h {
		if strings.EqualFold(key, name) {
			values = append(values, v...)
		}
	}
	return values
}

func parseIP(text string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(text)
	return ip.Unmap(), err == nil && ip.Zone() == ""
}

func peerIP(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return netip.Addr{}
	}
	ip, ok := parseIP(host)
	if !ok {
		return netip.Addr{}
	}
	return ip
}

func (p identityPolicy) resolve(r *http.Request) RequestMetadata {
	m := RequestMetadata{PeerIP: peerIP(r.RemoteAddr), ClientIPSource: "peer", RequestIDSource: "generated"}
	m.ClientIP = m.PeerIP
	if !m.PeerIP.IsValid() {
		m.AttributionReason = "invalid_peer"
	}
	if p.trusts(m.PeerIP) {
		m.ClientIP, m.ClientIPSource, m.AttributionReason = p.forwardedIP(r.Header, m.PeerIP)
		values := headerValues(r.Header, p.requestIDHeader)
		if len(values) == 1 && validRequestID(values[0]) {
			m.RequestID, m.RequestIDSource = values[0], "proxy"
		} else if len(values) == 0 {
			m.RequestIDReason = "missing_request_id"
		} else {
			m.RequestIDReason = "invalid_request_id"
		}
	}
	if m.RequestID == "" {
		var bytes [16]byte
		_, _ = rand.Read(bytes[:])
		m.RequestID = hex.EncodeToString(bytes[:])
	}
	return m
}

func validRequestID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for i := range id {
		c := id[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", rune(c))) {
			return false
		}
	}
	return true
}

func (p identityPolicy) forwardedIP(h http.Header, peer netip.Addr) (netip.Addr, string, string) {
	fallback := func(reason string) (netip.Addr, string, string) { return peer, "peer", reason }
	values := headerValues(h, p.clientHeader)
	if len(values) == 0 {
		return fallback("missing_client_ip_header")
	}
	if strings.EqualFold(p.clientHeader, "X-Real-IP") {
		if len(values) != 1 {
			return fallback("invalid_real_ip")
		}
		ip, ok := parseIP(strings.Trim(values[0], " \t"))
		if !ok {
			return fallback("invalid_real_ip")
		}
		return ip, "x_real_ip", ""
	}
	// Bound bytes before splitting, as well as address count, so whitespace and
	// empty fields cannot make a nominally short chain consume unbounded work.
	bytes := 0
	for _, value := range values {
		bytes += len(value)
		if bytes > p.maxHops*64 {
			return fallback("forwarded_chain_too_long")
		}
	}
	addresses := make([]netip.Addr, 0, p.maxHops)
	for _, value := range values {
		for {
			part, rest, more := strings.Cut(value, ",")
			if len(addresses) == p.maxHops {
				return fallback("forwarded_chain_too_long")
			}
			ip, ok := parseIP(strings.Trim(part, " \t"))
			if !ok {
				return fallback("invalid_forwarded_chain")
			}
			addresses = append(addresses, ip)
			if !more {
				break
			}
			value = rest
		}
	}
	for i := len(addresses) - 1; i >= 0; i-- {
		if !p.trusts(addresses[i]) || i == 0 {
			return addresses[i], "x_forwarded_for", ""
		}
	}
	return fallback("invalid_forwarded_chain")
}
