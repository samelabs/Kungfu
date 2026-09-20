package delivery

// ssrf.go — hardened outbound dial authority for PostAPI (SSRF defense).
//
// Single owner of the outbound IP policy: PostAPI may only reach
// publicly routable destinations. The mechanism is a custom
// DialContext that:
//
//  1. receives hostname:port from the HTTP transport;
//  2. if hostname is an IP literal: normalize, apply the outbound IP
//     policy, and dial ONLY that literal IP;
//  3. if hostname is a DNS name: resolve it HERE (single resolution),
//     validate EVERY returned address against the same policy (any
//     forbidden address fails the whole dial — mixed results never
//     fall back to "try the next one"), and dial the already-validated
//     literal IP;
//  4. the original request hostname remains the HTTP Host header /
//     HTTPS TLS SNI authority — the URL is never rewritten to an IP.
//
// DNS-rebinding invariant: there is exactly ONE resolution per dial
// and the final connect uses the validated literal IP, so a second
// (rebinding) lookup can never hand the connection to an internal
// address after validation.
//
// Redirects remain forbidden and the bounded-read contract is
// unchanged — this file only replaces the dial path.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// deniedPrefixes is the single centralized CIDR policy table for
// PostAPI outbound traffic. Anything matching is refused; anything
// not matching a special-purpose range is treated as public.
var deniedPrefixes = mustCIDRs(
	// IPv4 loopback + RFC1918 private
	"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	// link-local (incl. 169.254.169.254 metadata class)
	"169.254.0.0/16",
	// shared / CGNAT
	"100.64.0.0/10",
	// unspecified + multicast + reserved
	"0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "255.255.255.255/32",
	// documentation / example ranges (TEST-NET-1/2/3) — clearly not
	// publicly routable in practice; centralized here, not per-file.
	"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24",
	// IPv6 loopback, ULA, link-local, unspecified, multicast
	"::1/128", "fc00::/7", "fe80::/10", "::/128", "ff00::/8",
	// IPv4-mapped IPv6 (::ffff:a.b.c.d) is unwrapped by To4() before
	// the IPv4 rules above, so mapped private/loopback is denied by
	// the same table; ::ffff:0:0/96 public mapping is allowed only
	// when the embedded v4 is public.
)

func mustCIDRs(specs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(specs))
	for _, s := range specs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("ssrf: bad policy CIDR " + s)
		}
		out = append(out, n)
	}
	return out
}

// ipPolicyError marks a dial refused by the outbound IP policy.
type ipPolicyError struct{ reason string }

func (e *ipPolicyError) Error() string { return "postapi outbound ip policy: " + e.reason }

// isIPPolicyError reports whether err came from the outbound policy.
func isIPPolicyError(err error) bool {
	_, ok := err.(*ipPolicyError)
	return ok
}

// policyAllowsIP reports whether ip is publicly routable under the
// centralized policy. IPv4-mapped IPv6 addresses are unwrapped so the
// v4 table applies.
func policyAllowsIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if allowLoopback && (ip.IsLoopback() || ip.IsPrivate()) {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return false
	}
	for _, n := range deniedPrefixes {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// allowLoopback, when true, relaxes the loopback/private denial so
// in-process httptest servers can act as PostAPI receivers. It is
// TEST-ONLY: a source guard (TestSSRFLoopbackBypassNeverInProduction)
// asserts no production (non _test.go) file calls
// AllowLoopbackForTest, and the production binary links with it
// permanently false (nothing in internal/ outside _test.go references
// it).
var allowLoopback bool

// AllowLoopbackForTest relaxes the outbound IP policy for loopback
// receivers. TEST-ONLY: used by in-process httptest PostAPI fakes.
func AllowLoopbackForTest() func() {
	allowLoopback = true
	return func() { allowLoopback = false }
}

// resolver is the DNS seam (injectable in tests; production default).
var resolver netResolver = defaultNetResolver{}

// netResolver abstracts the DNS lookup used by the hardened dial.
type netResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type defaultNetResolver struct{}

func (defaultNetResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// dialForbidden reports a policy refusal as a non-retryable dial
// error (http.Transport surfaces it as a request error).
func dialForbidden(reason string) error {
	return &ipPolicyError{reason: reason}
}

// hardenedDialContext is THE outbound transport authority for
// PostAPI: validate-then-dial with a single DNS resolution.
func hardenedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, dialForbidden("bad address " + addr)
	}

	// Only TCP-family dials are expected; refuse anything else.
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, dialForbidden("network " + network + " not allowed")
	}

	var dialIP net.IP
	if ip := net.ParseIP(host); ip != nil {
		// IP literal: normalize (To4 unwraps IPv4-mapped IPv6), then
		// policy-check; the dial uses exactly this literal IP.
		if v4 := ip.To4(); v4 != nil {
			dialIP = v4
		} else {
			dialIP = ip
		}
		if !policyAllowsIP(dialIP) {
			return nil, dialForbidden("destination " + host + " is not publicly routable")
		}
	} else {
		// DNS name: resolve ONCE here. Every returned address must be
		// allowed; a single forbidden address fails the whole dial
		// (fail closed — no partial policy filtering, no fallback to a
		// second resolution).
		addrs, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("postapi dns resolve %s: %w", host, err)
		}
		if len(addrs) == 0 {
			return nil, dialForbidden("dns " + host + " resolved to no addresses")
		}
		for _, a := range addrs {
			ip := a.IP
			if v4 := ip.To4(); v4 != nil {
				ip = v4
			}
			if !policyAllowsIP(ip) {
				return nil, dialForbidden("dns " + host + " resolves to non-public " + a.IP.String())
			}
		}
		// Dial the FIRST validated literal IP — never the hostname
		// again (rebinding defense).
		ip := addrs[0].IP
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		dialIP = ip
	}

	dialer := &net.Dialer{Timeout: postAPIConnectTimeout}
	return dialer.DialContext(ctx, network, net.JoinHostPort(dialIP.String(), port))
}

// newHardenedClient builds the PostAPI HTTP client on the hardened
// dial authority. Redirects forbidden; total timeout preserved.
func newHardenedClient() *http.Client {
	return &http.Client{
		Timeout: postAPIRequestTimeout,
		Transport: &http.Transport{
			DialContext:     hardenedDialContext,
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func init() {
	// sharedClient (http_post.go) now rides the hardened dial path —
	// the SSRF authority lives in this package, not in callers.
	sharedClient = newHardenedClient()
}

var _ = isIPPolicyError // used by tests
