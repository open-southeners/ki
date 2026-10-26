package auth

import (
	"net"
	"net/http"
	"strings"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

// clientIPKey is the per-peer state key under which the client IP is stored.
const clientIPKey = "auth.clientip"

// SetClientIP records the remote address p's connection came from, for audit
// and rate limiting. It must be called before p starts running.
func SetClientIP(p *rpc.Peer, ip string) { p.Set(clientIPKey, ip) }

// ClientIPOf returns the address recorded by SetClientIP, or "" if none.
func ClientIPOf(p *rpc.Peer) string {
	v, _ := p.Get(clientIPKey)
	s, _ := v.(string)
	return s
}

// ClientIP returns the address r came from. With trustForwarded the last
// X-Forwarded-For entry (the one appended by the trusted reverse proxy) is
// used when present and parseable; otherwise, and always when trustForwarded is
// false, the host part of RemoteAddr is returned.
func ClientIP(r *http.Request, trustForwarded bool) string {
	if trustForwarded {
		var last string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					last = part
				}
			}
		}
		if ip := net.ParseIP(last); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
