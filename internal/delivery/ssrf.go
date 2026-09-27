package delivery

// ssrf.go — centralized outbound IP policy authority + hardened dial
// seam for PostAPI (SSRF final closure).
//
// Single owner of the outbound IP policy: PostAPI may only reach
// publicly routable destinations. The mechanism is a custom
// DialContext that:
//
//  1. receives hostname:port from the HTTP transport;
//  2. if hostname is an IP literal: normalize (net/netip parse +
//     Unmap for IPv4-mapped IPv6), apply the outbound IP policy,
//     and dial ONLY that literal IP;
//  3. if hostname is a DNS name: resolve it HERE (single resolution),
//     normalize + validate EVERY returned address against the same
//     policy (any forbidden address fails the WHOLE dial — mixed
//     results never fall back to "try the public one"), dedupe, then
//     dial only already-validated literal IPs in resolution order,
//     with fallback to the next validated IP on connect failure and
//     NO re-resolution (DNS-rebinding defense);
//  4. the original request hostname remains the HTTP Host header /
//     HTTPS TLS SNI authority — the URL is never rewritten to an IP
//     (http.Transport derives both from the request URL, and the dial
//     seam only replaces the TCP connect target).
//
// Policy semantics (net/netip throughout, single table):
//   - one centralized special-purpose prefix table mirroring the IANA
//     IPv4/IPv6 Special-Purpose Address Registries, each entry carrying
//     the registry's Globally-Reachable flag (TRUE / FALSE / N-A —
//     N-A and deprecated states are stored FALSE: fail closed);
//   - LONGEST-PREFIX / most-specific match decides: a globally
//     reachable exception more specific than a special-use parent is
//     allowed (e.g. 192.0.0.9/32 inside 192.0.0.0/24), and a
//     non-globally-reachable prefix more specific than the global
//     unicast default is denied (e.g. 2001:db8::/32 inside 2000::/3);
//   - an address matching NO special-purpose prefix is normal global
//     unicast and continues to the dial flow;
//   - IPv4-mapped IPv6 (::ffff:0:0/96) is unmapped before lookup so
//     the v4 table applies; the NAT64 well-known prefix 64:ff9b::/96
//     is resolved to its embedded IPv4 address and evaluated under
//     the v4 rules (same normalization spirit);
//   - static table with source-date annotation; no runtime IANA fetch.
//
// Redirects remain forbidden and the bounded-read contract is
// unchanged — this file only replaces the dial path.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

// ---------------------------------------------------------------------------
// Centralized IP policy table
// ---------------------------------------------------------------------------

// specialPrefix is one entry of the single centralized outbound policy
// table. Entries mirror the IANA IPv4 and IPv6 Special-Purpose Address
// Registries plus the multicast/reserved blocks; globallyReachable is
// the registry "Globally-Reachable" flag. Registry states FALSE, N/A,
// empty and deprecated are ALL stored false — fail closed.
//
// Table data synchronized against:
//
//	IANA IPv4 Special-Purpose Address Registry — Last Updated 2025-10-09
//	IANA IPv6 Special-Purpose Address Registry — Last Updated 2025-10-09
//
// (PM-verified authoritative snapshot at audit time; no runtime IANA
// access.) The table is the ONLY CIDR policy in this package.
type specialPrefix struct {
	prefix            netip.Prefix
	globallyReachable bool
	note              string
}

var specialPurpose = []specialPrefix{
	// ---- IPv4 special-purpose registry (snapshot 2025-10-09) ----
	{netip.MustParsePrefix("0.0.0.0/8"), false, `"this network`},
	{netip.MustParsePrefix("10.0.0.0/8"), false, "private-use"},
	{netip.MustParsePrefix("100.64.0.0/10"), false, "shared address space (CGNAT)"},
	{netip.MustParsePrefix("127.0.0.0/8"), false, "loopback"},
	{netip.MustParsePrefix("169.254.0.0/16"), false, "link-local (incl. 169.254.169.254 metadata class)"},
	{netip.MustParsePrefix("172.16.0.0/12"), false, "private-use"},
	{netip.MustParsePrefix("192.0.0.0/24"), false, "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.0.0/29"), false, "DS field assignments"},
	{netip.MustParsePrefix("192.0.0.8/32"), false, "IPv4 dummy address"},
	{netip.MustParsePrefix("192.0.0.9/32"), true, "Port Control Protocol Anycast — GR=TRUE"},
	{netip.MustParsePrefix("192.0.0.10/32"), true, "Traversal Using Relays around NAT Anycast — GR=TRUE"},
	{netip.MustParsePrefix("192.0.0.170/32"), false, "NAT64/DNS64 Discovery"},
	{netip.MustParsePrefix("192.0.0.171/32"), false, "NAT64/DNS64 Discovery"},
	{netip.MustParsePrefix("192.0.2.0/24"), false, "TEST-NET-1 documentation"},
	{netip.MustParsePrefix("192.31.196.0/24"), true, "AS112 anycast — GR=TRUE"},
	{netip.MustParsePrefix("192.52.193.0/24"), true, "AS112 anycast — GR=TRUE"},
	{netip.MustParsePrefix("192.88.99.0/24"), false, "6to4 Relay Anycast (deprecated)"},
	{netip.MustParsePrefix("192.88.99.2/32"), false, "6to4 Relay Anycast (deprecated)"},
	{netip.MustParsePrefix("192.168.0.0/16"), false, "private-use"},
	{netip.MustParsePrefix("192.175.48.0/24"), true, "AMPRGATE — GR=TRUE"},
	{netip.MustParsePrefix("198.18.0.0/15"), false, "benchmarking"},
	{netip.MustParsePrefix("198.51.100.0/24"), false, "TEST-NET-2 documentation"},
	{netip.MustParsePrefix("203.0.113.0/24"), false, "TEST-NET-3 documentation"},
	{netip.MustParsePrefix("224.0.0.0/4"), false, "multicast"},
	{netip.MustParsePrefix("240.0.0.0/4"), false, "reserved for future use"},
	{netip.MustParsePrefix("255.255.255.255/32"), false, "limited broadcast"},
	// ---- IPv6 special-purpose registry (snapshot 2025-10-09) ----
	{netip.MustParsePrefix("::/128"), false, "unspecified"},
	{netip.MustParsePrefix("::1/128"), false, "loopback"},
	// ::ffff:0:0/96 (IPv4-mapped) is UNMAPPED before lookup; the v4
	// table applies. 64:ff9b::/96 (NAT64 well-known) is resolved to
	// its embedded v4 address before lookup. Neither is a second
	// policy table — both are address normalization steps.
	{netip.MustParsePrefix("64:ff9b:1::/48"), false, "Local-Use NAT64 Translation"},
	{netip.MustParsePrefix("100::/64"), false, "discard-only"},
	{netip.MustParsePrefix("100:0:0:1::/64"), false, "discard-prefix allocation"},
	{netip.MustParsePrefix("2001::/23"), false, "IETF Protocol Assignments — GR=false parent"},
	{netip.MustParsePrefix("2001::/32"), false, "Teredo — N/A fail closed"},
	{netip.MustParsePrefix("2001:1::1/128"), true, "Port Control Protocol Anycast — GR=TRUE"},
	{netip.MustParsePrefix("2001:1::2/128"), true, "Traversal Using Relays around NAT Anycast — GR=TRUE"},
	{netip.MustParsePrefix("2001:1::3/128"), true, "All-DS Anycast — GR=TRUE"},
	{netip.MustParsePrefix("2001:2::/48"), false, "benchmarking"},
	{netip.MustParsePrefix("2001:3::/32"), true, "AMPRGATE — GR=TRUE"},
	{netip.MustParsePrefix("2001:4:112::/48"), true, "RIPE NCC RIS Project — GR=TRUE"},
	{netip.MustParsePrefix("2001:10::/28"), false, "ORCHID (deprecated)"},
	{netip.MustParsePrefix("2001:20::/28"), true, "ORCHIDv2 — GR=TRUE"},
	{netip.MustParsePrefix("2001:30::/28"), true, "New ORCHID Format (extension) — GR=TRUE"},
	{netip.MustParsePrefix("2001:db8::/32"), false, "documentation"},
	{netip.MustParsePrefix("2002::/16"), false, "6to4 — N/A fail closed"},
	{netip.MustParsePrefix("2620:4f:8000::/48"), true, "Direct Delegation AS112 Service — GR=TRUE"},
	{netip.MustParsePrefix("3fff::/20"), false, "documentation (RFC 9637)"},
	{netip.MustParsePrefix("5f00::/16"), false, "SRv6 services"},
	{netip.MustParsePrefix("fc00::/7"), false, "unique-local (ULA)"},
	{netip.MustParsePrefix("fe80::/10"), false, "link-local"},
	{netip.MustParsePrefix("ff00::/8"), false, "multicast"},
}

// allocatedGlobalUnicastV6 is the static, centralized, read-only
// snapshot of the IANA IPv6 Global Unicast Address Space registry
// containing ONLY prefixes with Status = ALLOCATED. It is NOT a
// second policy authority: the single decision function
// policyAllowsAddr consults it AFTER the special-purpose
// longest-prefix lookup (special-purpose always takes precedence —
// e.g. 2001::/23 and 2002::/16 are ALLOCATED here yet denied by
// special-purpose GR=false / N-A).
//
// Registry snapshot: IANA IPv6 Global Unicast Address Space,
// Last Updated 2025-10-10 (PM-verified authoritative state at audit
// time; no runtime IANA access). The COMPLETE set of Status =
// ALLOCATED prefixes as listed by the registry — 36 entries; the
// trailing RESERVED blocks (2d00::/8 … 3fff::/20) are absent BY
// DESIGN: absence means deny.
var allocatedGlobalUnicastV6 = []netip.Prefix{
	// 2001::/3x subtree allocations
	netip.MustParsePrefix("2001::/23"),      // IANA
	netip.MustParsePrefix("2001:200::/23"),  // APNIC
	netip.MustParsePrefix("2001:400::/23"),  // ARIN
	netip.MustParsePrefix("2001:600::/23"),  // RIPE NCC
	netip.MustParsePrefix("2001:800::/22"),  // RIPE NCC
	netip.MustParsePrefix("2001:c00::/23"),  // APNIC
	netip.MustParsePrefix("2001:e00::/23"),  // APNIC
	netip.MustParsePrefix("2001:1200::/23"), // LACNIC
	netip.MustParsePrefix("2001:1400::/22"), // RIPE NCC
	netip.MustParsePrefix("2001:1800::/23"), // ARIN
	netip.MustParsePrefix("2001:1a00::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:1c00::/22"), // RIPE NCC
	netip.MustParsePrefix("2001:2000::/19"), // RIPE NCC
	netip.MustParsePrefix("2001:4000::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:4200::/23"), // AFRINIC
	netip.MustParsePrefix("2001:4400::/23"), // APNIC
	netip.MustParsePrefix("2001:4600::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:4800::/23"), // ARIN
	netip.MustParsePrefix("2001:4a00::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:4c00::/23"), // RIPE NCC
	netip.MustParsePrefix("2001:5000::/20"), // RIPE NCC
	netip.MustParsePrefix("2001:8000::/19"), // APNIC
	netip.MustParsePrefix("2001:a000::/20"), // APNIC
	netip.MustParsePrefix("2001:b000::/20"), // APNIC
	netip.MustParsePrefix("2002::/16"),      // 6to4 (special-purpose precedence)
	netip.MustParsePrefix("2003::/18"),      // RIPE NCC
	// /12 RIR blocks
	netip.MustParsePrefix("2400::/12"), // APNIC
	netip.MustParsePrefix("2410::/12"), // APNIC
	netip.MustParsePrefix("2600::/12"), // ARIN
	netip.MustParsePrefix("2610::/23"), // ARIN
	netip.MustParsePrefix("2620::/23"), // ARIN
	netip.MustParsePrefix("2630::/12"), // ARIN
	netip.MustParsePrefix("2800::/12"), // LACNIC
	netip.MustParsePrefix("2a00::/12"), // RIPE NCC
	netip.MustParsePrefix("2a10::/12"), // RIPE NCC
	netip.MustParsePrefix("2c00::/12"), // AFRINIC
}

// isAllocatedGlobalUnicastV6 reports whether an IPv6 address with NO
// special-purpose match falls inside a prefix the IANA Global Unicast
// registry currently lists as ALLOCATED. RESERVED or unallocated
// space (e.g. 2d00::/12 … 3fff::/12 within 2000::/3) is denied.
func isAllocatedGlobalUnicastV6(addr netip.Addr) bool {
	for _, p := range allocatedGlobalUnicastV6 {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// nat64WellKnown is the RFC 6052 well-known prefix; addresses inside
// it are normalized to the embedded IPv4 address before the policy
// lookup (a NAT64 rendering of a private v4 is refused exactly like
// the private v4 itself).
var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

// ipPolicyError marks a dial refused by the outbound IP policy.
type ipPolicyError struct{ reason string }

func (e *ipPolicyError) Error() string { return "postapi outbound ip policy: " + e.reason }

// isIPPolicyError reports whether err came from the outbound policy.
func isIPPolicyError(err error) bool {
	_, ok := err.(*ipPolicyError)
	return ok
}

// policyAllowsAddr reports whether addr is publicly routable under the
// centralized policy. addr is normalized (Unmap for IPv4-mapped IPv6;
// NAT64 well-known prefix resolved to its embedded v4). The MOST
// SPECIFIC matching special-purpose entry decides; no match means
// normal global unicast (allowed).
func policyAllowsAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if addr.Is6() && nat64WellKnown.Contains(addr) {
		b := addr.As16()
		embedded, ok := netip.AddrFromSlice(b[12:16])
		if !ok {
			return false // unreachable: 4-byte slice always converts
		}
		return policyAllowsAddr(embedded)
	}
	if allowLoopback && (addr.IsLoopback() || addr.IsPrivate()) {
		return true
	}
	var best *specialPrefix
	for i := range specialPurpose {
		if specialPurpose[i].prefix.Contains(addr) {
			if best == nil || specialPurpose[i].prefix.Bits() > best.prefix.Bits() {
				best = &specialPurpose[i]
			}
		}
	}
	if best == nil {
		// No special-purpose match. "Not in the table" is NOT public:
		//   IPv4 — only normal global unicast is allowed (multicast
		//          and reserved are already table-denied; unicast
		//          outside those blocks is normal global unicast);
		//   IPv6 — ONLY prefixes the IANA IPv6 Global Unicast Address
		//          Space registry (snapshot 2025-10-10) lists as
		//          Status = ALLOCATED are allowed; RESERVED or
		//          unallocated space — including inside 2000::/3 —
		//          is denied.
		if addr.Is4() {
			return true
		}
		return isAllocatedGlobalUnicastV6(addr)
	}
	return best.globallyReachable
}

// ipToAddr converts a net.IP from the resolver into a normalized
// netip.Addr (IPv4-mapped forms collapse to 4-byte form).
func ipToAddr(ip net.IP) (netip.Addr, bool) {
	if v4 := ip.To4(); v4 != nil {
		a, ok := netip.AddrFromSlice(v4)
		return a, ok
	}
	a, ok := netip.AddrFromSlice(ip)
	return a, ok
}

// allowLoopback, when true, relaxes the loopback/private denial so
// in-process httptest servers can act as PostAPI receivers. It is
// TEST-ONLY: a source guard (TestSSRFLoopbackBypassNeverInProduction)
// asserts no production (non _test.go) file outside this file calls
// AllowLoopbackForTest, and the production binary links with it
// permanently false. The core SSRF tests in this package NEVER enable
// the bypass — public policy is proven without it.
var allowLoopback bool

// AllowLoopbackForTest relaxes the outbound IP policy for loopback
// receivers. TEST-ONLY: used by in-process httptest PostAPI fakes
// (service/server/mcpserver test mains).
func AllowLoopbackForTest() func() {
	allowLoopback = true
	return func() { allowLoopback = false }
}

// TrustRootsForTest makes the delivery client trust exactly pool for
// TLS — TEST-ONLY, for in-process httptest TLS receivers with a
// self-signed certificate (macOS Go ignores SSL_CERT_FILE). Returns the
// restore function.
func TrustRootsForTest(pool *x509.CertPool) func() {
	prev := sharedClient
	c := newHardenedClient()
	c.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool}
	sharedClient = c
	return func() { sharedClient = prev }
}

// ---------------------------------------------------------------------------
// Seams: resolver + dialer (injectable in tests; production defaults)
// ---------------------------------------------------------------------------

// resolver is the DNS seam (exactly ONE resolution per dial).
var resolver netResolver = defaultNetResolver{}

// netResolver abstracts the DNS lookup used by the hardened dial.
type netResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

type defaultNetResolver struct{}

func (defaultNetResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// dialer is the CONNECT seam: it receives only already-validated
// LITERAL ip:port targets — structurally incapable of re-resolving a
// hostname (no string host parameter exists on the interface).
var dialer netDialer = defaultNetDialer{d: &net.Dialer{Timeout: postAPIConnectTimeout}}

// netDialer abstracts the TCP connect.
type netDialer interface {
	DialContext(ctx context.Context, network string, target netip.AddrPort) (net.Conn, error)
}

type defaultNetDialer struct{ d *net.Dialer }

func (x defaultNetDialer) DialContext(ctx context.Context, network string, target netip.AddrPort) (net.Conn, error) {
	return x.d.DialContext(ctx, network, target.String())
}

// parseDialPort parses a decimal TCP port from a split host:port.
func parseDialPort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(n), nil
}

// dialForbidden reports a policy refusal as a non-retryable dial
// error (http.Transport surfaces it as a request error).
func dialForbidden(reason string) error {
	return &ipPolicyError{reason: reason}
}

// hardenedDialContext is THE outbound transport authority for
// PostAPI: resolve once → validate every address → dial only
// validated literal IPs (with fallback, without re-resolution).
func hardenedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, dialForbidden("bad address " + addr)
	}

	// Only TCP-family dials are expected; refuse anything else.
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, dialForbidden("network " + network + " not allowed")
	}
	port, err := parseDialPort(portStr)
	if err != nil {
		return nil, dialForbidden("bad port in " + addr)
	}

	var targets []netip.Addr
	if literal, perr := netip.ParseAddr(host); perr == nil {
		// IP literal: normalize, policy-check, dial exactly this IP.
		literal = literal.Unmap()
		if !policyAllowsAddr(literal) {
			return nil, dialForbidden("destination " + host + " is not publicly routable")
		}
		targets = []netip.Addr{literal}
	} else {
		// DNS name: resolve ONCE here. EVERY returned address must be
		// allowed — a single forbidden address fails the whole dial
		// (fail closed: no partial filtering, no hostname fallback).
		addrs, rerr := resolver.LookupIPAddr(ctx, host)
		if rerr != nil {
			return nil, fmt.Errorf("postapi dns resolve %s: %w", host, rerr)
		}
		if len(addrs) == 0 {
			return nil, dialForbidden("dns " + host + " resolved to no addresses")
		}
		seen := make(map[netip.Addr]struct{}, len(addrs))
		for _, a := range addrs {
			ip, ok := ipToAddr(a.IP)
			if !ok {
				return nil, dialForbidden("dns " + host + " returned an uninterpretable address")
			}
			ip = ip.Unmap()
			if !policyAllowsAddr(ip) {
				return nil, dialForbidden("dns " + host + " resolves to non-public " + ip.String())
			}
			if _, dup := seen[ip]; !dup {
				seen[ip] = struct{}{}
				targets = append(targets, ip) // resolver order preserved
			}
		}
	}

	// Dial ONLY validated literal IPs, in order, with fallback on
	// connect failure. The hostname is NEVER handed to any resolver
	// or dialer again. Caller ctx / total timeout stay the ceiling.
	var lastErr error
	for _, ip := range targets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, derr := dialer.DialContext(ctx, network, netip.AddrPortFrom(ip, port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
	}
	return nil, lastErr
}

// newHardenedClient builds the PostAPI HTTP client on the hardened
// dial authority. Redirects forbidden; total timeout preserved; the
// HTTP authority (Host header / TLS SNI) still comes from the request
// URL — the dial seam only replaces the TCP connect target.
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
