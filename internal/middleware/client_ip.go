package middleware

import (
	"net"
	"net/http"
	"strings"
)

// GetClientIP extracts the client IP, only trusting forwarded headers
// when the direct connection comes from a trusted proxy.
// If trustedCIDRs is empty, RemoteAddr is always used.
func GetClientIP(r *http.Request, trustedCIDRs []*net.IPNet) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}
	remoteIP = net.ParseIP(remoteIP).String()
	if remoteIP == "<nil>" {
		return "0.0.0.0"
	}

	// If no trusted proxies configured, use RemoteAddr directly
	if len(trustedCIDRs) == 0 {
		return remoteIP
	}

	// Check if the direct connection is from a trusted proxy
	if !isTrustedProxy(remoteIP, trustedCIDRs) {
		return remoteIP
	}

	// Connection is from a trusted proxy — honor X-Forwarded-For
	// Take the leftmost (original client) IP
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		ip := strings.TrimSpace(strings.Split(xff, ",")[0])
		if net.ParseIP(ip) != nil {
			return ip
		}
	}

	// Fall back to CF-Connecting-IP (Cloudflare)
	cfIP := r.Header.Get("CF-Connecting-IP")
	if cfIP != "" {
		ip := strings.TrimSpace(cfIP)
		if net.ParseIP(ip) != nil {
			return ip
		}
	}

	return remoteIP
}

// directPeerIP extracts the parsed direct TCP peer address from
// RemoteAddr, or nil when it cannot be parsed.
func directPeerIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// IsHTTPS is the ONE canonical HTTPS authority for Owner/Admin cookie
// lifecycle decisions:
//
//	A. r.TLS != nil                              -> true (unconditional)
//	B. trusted direct peer + single-token
//	   X-Forwarded-Proto "https" (case-insens.) -> true
//	C. anything else (untrusted peer, malformed
//	   or multi-value forwarded proto)           -> false
//
// It reuses the same direct-peer trust predicate as GetClientIP, so
// an untrusted peer can never make cookies Secure by spoofing
// X-Forwarded-Proto.
func IsHTTPS(r *http.Request, trustedCIDRs []*net.IPNet) bool {
	if r.TLS != nil {
		return true
	}
	ip := directPeerIP(r)
	if ip == nil {
		return false
	}
	if !isTrustedProxy(ip.String(), trustedCIDRs) {
		return false
	}
	// Header.Get returns only the FIRST value — ambiguous multi-header
	// forwarded proto must fail closed, so read the full value slice:
	// zero or multiple header values -> false; exactly one -> trimmed,
	// case-insensitive "https".
	protos := r.Header.Values("X-Forwarded-Proto")
	if len(protos) != 1 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(protos[0]), "https")
}

func isTrustedProxy(ip string, cidrs []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, cidr := range cidrs {
		if cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

// GetUserAgent extracts the User-Agent header.
func GetUserAgent(r *http.Request) string {
	return r.Header.Get("User-Agent")
}
