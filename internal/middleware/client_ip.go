package middleware

import (
	"net"
	"net/http"
	"strings"
)

// GetClientIP extracts the client IP from request headers.
// PHP: Logger::getClientIp()
// Checks in order: CF-Connecting-IP, X-Forwarded-For, X-Forwarded, X-Cluster-Client-IP,
// Forwarded-For, Forwarded, REMOTE_ADDR
func GetClientIP(r *http.Request) string {
	headers := []string{
		"CF-Connecting-IP",
		"X-Forwarded-For",
		"X-Forwarded",
		"X-Cluster-Client-IP",
		"Forwarded-For",
		"Forwarded",
	}

	for _, header := range headers {
		value := r.Header.Get(header)
		if value == "" {
			continue
		}
		// Handle comma-separated IPs (X-Forwarded-For may have multiple)
		ip := strings.TrimSpace(strings.Split(value, ",")[0])
		if net.ParseIP(ip) != nil {
			return ip
		}
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return "0.0.0.0"
	}
	if net.ParseIP(ip) == nil {
		return "0.0.0.0"
	}
	return ip
}

// GetUserAgent extracts the User-Agent header.
func GetUserAgent(r *http.Request) string {
	return r.Header.Get("User-Agent")
}
