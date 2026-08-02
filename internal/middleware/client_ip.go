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

// ParseTrustedCIDRs converts a list of CIDR strings into net.IPNet.
// Invalid entries are silently skipped.
func ParseTrustedCIDRs(cidrs []string) []*net.IPNet {
	var result []*net.IPNet
	for _, s := range cidrs {
		// If no /prefix, treat as /32 (IPv4) or /128 (IPv6)
		if !strings.Contains(s, "/") {
			ip := net.ParseIP(s)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				s += "/32"
			} else {
				s += "/128"
			}
		}
		_, cidr, err := net.ParseCIDR(s)
		if err != nil {
			continue
		}
		result = append(result, cidr)
	}
	return result
}

// GetUserAgent extracts the User-Agent header.
func GetUserAgent(r *http.Request) string {
	return r.Header.Get("User-Agent")
}
