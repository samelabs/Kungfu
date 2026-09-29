package middleware

import (
	"net"
	"net/http"
	"strings"
)

// GetClientIP extracts the client IP, only trusting forwarded headers
// when the direct connection comes from a trusted proxy.
// If trustedCIDRs is empty, RemoteAddr is always used.
//
// Under a trusted direct peer, X-Forwarded-For is evaluated
// rightmost-untrusted: appended chains ($proxy_add_x_forwarded_for)
// put the address each proxy saw at the END of the list, so the
// leftmost entry is fully client-controlled and must not be trusted.
// The list is walked from the right, skipping entries inside trusted
// CIDRs; the first valid entry outside every trusted range is the
// client. An INVALID entry stops the walk and falls back to the
// direct peer — the chain is provably broken there, and skipping it
// would keep moving the walk toward the client-controlled left end.
// Entries are normalized via net.ParseIP(...).String() first
// (IPv4-mapped "::ffff:1.2.3.4" collapses to "1.2.3.4") so equivalent
// spellings cannot split rate-limit buckets. When no untrusted entry
// is found, the direct peer's address is returned.
//
// CF-Connecting-IP is deliberately not honored: the reference
// deployment fronts the server with plain nginx, and that header is
// client-forgeable and passed through transparently.
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

	// Connection is from a trusted proxy — walk every X-Forwarded-For
	// entry (all header lines, wire order) from the right, skipping
	// further trusted hops; the first valid untrusted address is the
	// client. An entry that does not parse as an IP breaks the chain
	// exactly there: fall back to the direct peer instead of trusting
	// anything further left (client-controlled territory).
	entries := xffEntries(r)
	for i := len(entries) - 1; i >= 0; i-- {
		ip := net.ParseIP(entries[i])
		if ip == nil {
			return remoteIP
		}
		normalized := ip.String()
		if isTrustedProxy(normalized, trustedCIDRs) {
			continue
		}
		return normalized
	}
	return remoteIP
}

// xffEntries flattens every X-Forwarded-For header line into one
// ordered entry list (Header.Get would hide additional lines).
func xffEntries(r *http.Request) []string {
	var entries []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		for _, entry := range strings.Split(line, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				entries = append(entries, entry)
			}
		}
	}
	return entries
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
