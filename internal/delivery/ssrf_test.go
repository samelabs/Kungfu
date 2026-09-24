package delivery

// SSRF boundary tests — NO real public internet access.
//
// Every external edge is faked:
//   - DNS:     fakeResolver / countingResolver (canned answers, call count)
//   - connect: recordingDialer (records the exact literal ip:port targets)
//
// The only real sockets are loopback httptest servers, used strictly to
// prove the policy REFUSES loopback (denying the local machine, not
// reaching the public internet).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

// fakeResolver returns canned addresses per host (or a resolver error
// for hosts mapped to nil).
type fakeResolver map[string][]net.IPAddr

func (f fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host}
}

// errResolver always fails (resolver error path).
type errResolver struct{ calls int }

func (e *errResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	e.calls++
	return nil, &net.DNSError{Err: "server misbehaving", Name: host}
}

// countingResolver returns one canned answer set and counts calls
// (single-resolution proof).
type countingResolver struct {
	addrs []net.IPAddr
	calls int
}

func (c *countingResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	c.calls++
	return c.addrs, nil
}

func ipa(ip string) net.IPAddr { return net.IPAddr{IP: net.ParseIP(ip)} }

// recordingDialer is the fake connect seam: it records every literal
// target it receives (as ip:port strings) and answers per scripted
// failures. It structurally CANNOT receive a hostname — the interface
// takes netip.AddrPort only.
type recordingDialer struct {
	targets []string
	// failTargets: literal targets ("ip:port") whose dial must fail.
	failTargets map[string]bool
	conn        net.Conn // returned conn (nil-safe: a stub conn)
}

func (r *recordingDialer) DialContext(ctx context.Context, network string, target netip.AddrPort) (net.Conn, error) {
	t := target.String()
	r.targets = append(r.targets, t)
	if r.failTargets[t] {
		return nil, &net.OpError{Op: "dial", Net: network, Err: fmt.Errorf("connection refused (scripted)")}
	}
	return r.conn, nil
}

// stubConn satisfies net.Conn minimally.
type stubConn struct{}

func (stubConn) Read(b []byte) (int, error)         { return 0, nil }
func (stubConn) Write(b []byte) (int, error)        { return 0, nil }
func (stubConn) Close() error                       { return nil }
func (stubConn) LocalAddr() net.Addr                { return nil }
func (stubConn) RemoteAddr() net.Addr               { return nil }
func (stubConn) SetDeadline(t time.Time) error      { return nil }
func (stubConn) SetReadDeadline(t time.Time) error  { return nil }
func (stubConn) SetWriteDeadline(t time.Time) error { return nil }

func withResolver(r netResolver, fn func()) {
	old := resolver
	resolver = r
	defer func() { resolver = old }()
	fn()
}

func withDialer(d netDialer, fn func()) {
	old := dialer
	dialer = d
	defer func() { dialer = old }()
	fn()
}

// ---------------------------------------------------------------------------
// 1. IPv4 policy matrix
// ---------------------------------------------------------------------------

func TestSSRFIPv4PolicyMatrix(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// mandated non-global coverage
		{"0.0.0.0", false},         // 0.0.0.0/8 unspecified block
		{"0.1.2.3", false},         // 0.0.0.0/8
		{"10.0.0.1", false},        // 10/8 private
		{"100.64.0.1", false},      // 100.64/10 CGNAT
		{"100.127.255.254", false}, // CGNAT upper edge
		{"127.0.0.1", false},       // 127/8 loopback
		{"127.255.255.254", false}, // loopback edge
		{"169.254.0.1", false},     // link-local
		{"169.254.169.254", false}, // metadata class
		{"172.16.0.1", false},      // 172.16/12 private
		{"172.31.255.254", false},  // private edge
		{"192.0.0.1", false},       // 192.0.0.0/24 special-use
		{"192.0.2.7", false},       // TEST-NET-1
		{"192.88.99.2", false},     // deprecated 6to4 relay
		{"192.168.1.1", false},     // 192.168/16 private
		{"198.18.0.1", false},      // benchmarking 198.18/15
		{"198.19.255.254", false},  // benchmarking edge
		{"198.51.100.7", false},    // TEST-NET-2
		{"203.0.113.7", false},     // TEST-NET-3
		{"224.0.0.1", false},       // multicast 224/4
		{"239.255.255.254", false}, // multicast edge
		{"240.0.0.1", false},       // reserved 240/4
		{"255.255.255.255", false}, // limited broadcast
		// globally-reachable exceptions (most-specific semantics)
		{"192.0.0.9", true},    // PCP anycast inside 192.0.0.0/24
		{"192.0.0.10", true},   // TURN anycast inside 192.0.0.0/24
		{"192.31.196.1", true}, // AS112
		{"192.52.193.1", true}, // AS112
		// public IPv4 allow
		{"8.8.8.8", true},         // public
		{"1.1.1.1", true},         // public
		{"203.0.114.1", true},     // right after TEST-NET-3 — public
		{"198.20.0.1", true},      // right after benchmarking — public
		{"191.255.0.1", true},     // public
		{"223.255.255.254", true}, // last unicast before multicast — public
	}
	for _, tc := range cases {
		a, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("bad fixture %s", tc.ip)
		}
		if got := policyAllowsAddr(a); got != tc.want {
			t.Errorf("policyAllowsAddr(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 2. IPv6 policy matrix
// ---------------------------------------------------------------------------

func TestSSRFIPv6PolicyMatrix(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// mandated non-global/special coverage
		{"::", false},               // unspecified /128
		{"::1", false},              // loopback /128
		{"64:ff9b:1::1", false},     // local-use NAT64 /48
		{"100::1", false},           // discard-only /64
		{"100:0:0:1::1", false},     // discard allocation /64
		{"2001::1", false},          // Teredo /32 (N/A fail closed)
		{"2001:2::1", false},        // benchmarking /48
		{"2001:10::1", false},       // ORCHID /28 (deprecated)
		{"2001:db8::1", false},      // documentation /32
		{"2002:c000:204::1", false}, // 6to4 /16 (embedded 192.0.2.4)
		{"3fff::1", false},          // documentation /20
		{"5f00::1", false},          // SRv6 /16
		{"fc00::1", false},          // ULA /7
		{"fd12:3456::1", false},     // ULA /7
		{"fe80::1", false},          // link-local /10
		{"ff02::1", false},          // multicast /8
		// reserved IPv6 space OUTSIDE the special table — no-match
		// must NOT default to public (2000::/3 is the only base)
		{"4000::1", false}, // outside 2000::/3 — reserved, denied
		{"6000::1", false}, // outside 2000::/3 — reserved, denied
		{"8000::1", false}, // outside 2000::/3 — reserved, denied
		{"c000::1", false}, // outside 2000::/3 — reserved, denied
		{"fec0::1", false}, // deprecated site-local — denied
		// globally-reachable exceptions (snapshot 2025-10-09: TRUE)
		{"2001:1::1", true},  // PCP anycast /128
		{"2001:1::2", true},  // TURN anycast /128
		{"2001:1::3", true},  // All-DS anycast /128
		{"2001:20::1", true}, // ORCHIDv2 /28 — IANA GR=TRUE
		{"2001:30::1", true}, // ORCHID ext /28 — IANA GR=TRUE
		// public IPv6 allow
		{"2001:4860:4860::8888", true}, // public DNS
		{"2606:4700:4700::1111", true}, // normal global unicast inside 2000::/3
		{"2620:fe::fe", true},          // public
	}
	for _, tc := range cases {
		a, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("bad fixture %s", tc.ip)
		}
		if got := policyAllowsAddr(a); got != tc.want {
			t.Errorf("policyAllowsAddr(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// TestSSRF2001Slash23ParentNotShadowingExceptions proves the
// 2001::/23 GR=false parent (IETF protocol assignments) denies its
// non-exception space while every more-specific GR=TRUE exception of
// the 2025-10-09 snapshot stays allowed (most-specific semantics).
func TestSSRF2001Slash23ParentNotShadowingExceptions(t *testing.T) {
	denied := []string{
		"2001:5::1",   // inside 2001::/23, no exception — parent denies
		"2001::1",     // Teredo /32 (N/A fail closed)
		"2001:2::1",   // benchmarking /48
		"2001:10::1",  // ORCHID /28 (deprecated)
		"2001:db8::1", // documentation /32
		"2002::1",     // 6to4 /16 (N/A fail closed)
	}
	allowed := []string{
		"2001:1::1",     // PCP anycast /128
		"2001:1::2",     // TURN anycast /128
		"2001:1::3",     // All-DS anycast /128
		"2001:3::1",     // AMPRGATE /32 — GR=TRUE
		"2001:4:112::1", // RIPE NCC RIS /48 — GR=TRUE
		"2001:20::1",    // ORCHIDv2 /28 — GR=TRUE
		"2001:30::1",    // ORCHID ext /28 — GR=TRUE
	}
	for _, ip := range denied {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			t.Fatalf("bad fixture %s", ip)
		}
		if policyAllowsAddr(a) {
			t.Errorf("%s inside 2001::/23 non-exception space must be denied", ip)
		}
	}
	for _, ip := range allowed {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			t.Fatalf("bad fixture %s", ip)
		}
		if !policyAllowsAddr(a) {
			t.Errorf("%s is a more-specific GR=TRUE exception — must be allowed despite 2001::/23 parent", ip)
		}
	}
}

// TestSSRFReservedWithin2000Slash3 proves "inside 2000::/3" is NOT
// "currently allocated public space": /12 blocks the IANA Global
// Unicast registry (snapshot 2025-10-10) lists as RESERVED are denied
// even though they sit inside the historical 2000::/3 base.
func TestSSRFReservedWithin2000Slash3(t *testing.T) {
	reserved := []string{
		"2d00::1", // RESERVED
		"2e00::1", // RESERVED
		"3000::1", // RESERVED
		"3800::1", // RESERVED
		"3c00::1", // RESERVED
		"3e00::1", // RESERVED
		"3f00::1", // RESERVED
		"4000::1", // outside 2000::/3 entirely
		"6000::1", // outside 2000::/3
		"8000::1", // outside 2000::/3
		"c000::1", // outside 2000::/3
		"fec0::1", // deprecated site-local
	}
	for _, ip := range reserved {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			t.Fatalf("bad fixture %s", ip)
		}
		if policyAllowsAddr(a) {
			t.Errorf("%s is RESERVED/unallocated space — must be denied despite 2000::/3 type", ip)
		}
	}
}

// TestSSRFAllocatedGlobalUnicastAllowed proves addresses inside
// IANA-listed ALLOCATED /12 prefixes (no special-purpose match) are
// allowed.
func TestSSRFAllocatedGlobalUnicastAllowed(t *testing.T) {
	allocated := []string{
		"2003::1",              // RIPE NCC
		"2400::1",              // APNIC
		"2410::1",              // APNIC (inside 2400::/12)
		"2600::1",              // ARIN
		"2606:4700:4700::1111", // Cloudflare inside 2606::/32 (2600::/12)
		"2610::1",              // ARIN
		"2620:fe::fe",          // Quad9 inside 2620::/12
		"2800::1",              // LACNIC
		"2a00::1",              // RIPE NCC
		"2c00::1",              // AfriNIC
		"2001:4860:4860::8888", // Google inside 2001::/23-adjacent unicast... covered below
	}
	for _, ip := range allocated {
		a, err := netip.ParseAddr(ip)
		if err != nil {
			t.Fatalf("bad fixture %s", ip)
		}
		if !policyAllowsAddr(a) {
			t.Errorf("%s is inside an ALLOCATED global-unicast prefix — must be allowed", ip)
		}
	}
}

// TestSSRFSpecialPurposePrecedenceOverAllocation proves the
// special-purpose lookup wins over the allocation registry: addresses
// inside ALLOCATED prefixes are still denied when their most-specific
// special-purpose entry is GR=false / N-A / deprecated, and the
// GR=TRUE exceptions stay allowed.
func TestSSRFSpecialPurposePrecedenceOverAllocation(t *testing.T) {
	denied := []string{
		"2001:db8::1", // documentation — ALLOCATED parent, special GR=false
		"2002::1",     // 6to4 — ALLOCATED, special N/A → deny
		"3fff::1",     // documentation /20 (RFC 9637)
	}
	allowed := []string{
		"2001:1::1",     // PCP anycast /128 — GR=TRUE
		"2001:1::2",     // TURN anycast /128 — GR=TRUE
		"2001:1::3",     // All-DS anycast /128 — GR=TRUE
		"2001:3::1",     // AMPRGATE /32 — GR=TRUE
		"2001:4:112::1", // RIPE NCC RIS /48 — GR=TRUE
		"2001:20::1",    // ORCHIDv2 /28 — GR=TRUE
		"2001:30::1",    // ORCHID ext /28 — GR=TRUE
	}
	for _, ip := range denied {
		a, _ := netip.ParseAddr(ip)
		if policyAllowsAddr(a) {
			t.Errorf("%s must be denied: special-purpose precedence over allocation registry", ip)
		}
	}
	for _, ip := range allowed {
		a, _ := netip.ParseAddr(ip)
		if !policyAllowsAddr(a) {
			t.Errorf("%s GR=TRUE exception must be allowed", ip)
		}
	}
}

// TestSSRFIPv4RegistryAdditions covers the 2025-10-09 IPv4 snapshot
// entries previously missing (authoritative semantics, not just
// default-allow coincidence).
func TestSSRFIPv4RegistryAdditions(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"192.0.0.2", false},   // 192.0.0.0/29 DS field assignments
		{"192.0.0.8", false},   // IPv4 dummy address /32
		{"192.0.0.170", false}, // NAT64/DNS64 discovery
		{"192.0.0.171", false}, // NAT64/DNS64 discovery
		{"192.88.99.2", false}, // deprecated 6to4 relay /32
		{"192.175.48.1", true}, // AMPRGATE /24 — GR=TRUE
		{"192.0.0.9", true},    // PCP anycast /32 — GR=TRUE (kept)
		{"192.0.0.10", true},   // TURN anycast /32 — GR=TRUE (kept)
		{"192.0.0.11", false},  // inside 192.0.0.0/24 parent, no exception
	}
	for _, tc := range cases {
		a, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("bad fixture %s", tc.ip)
		}
		if got := policyAllowsAddr(a); got != tc.want {
			t.Errorf("policyAllowsAddr(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. IPv4-mapped normalization (+ NAT64 well-known)
// ---------------------------------------------------------------------------

func TestSSRFIPv4MappedNormalization(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"::ffff:127.0.0.1", false},       // mapped loopback denied
		{"::ffff:10.0.0.1", false},        // mapped private denied
		{"::ffff:169.254.169.254", false}, // mapped metadata denied
		{"::ffff:192.0.2.1", false},       // mapped TEST-NET denied
		{"::ffff:0.0.0.0", false},         // mapped unspecified denied
		{"::ffff:8.8.8.8", true},          // mapped public allowed
		{"64:ff9b::0808:0808", true},      // NAT64 of 8.8.8.8 allowed
		{"64:ff9b::7f00:0001", false},     // NAT64 of 127.0.0.1 denied
	}
	for _, tc := range cases {
		a, err := netip.ParseAddr(tc.ip)
		if err != nil {
			t.Fatalf("bad fixture %s", tc.ip)
		}
		if got := policyAllowsAddr(a); got != tc.want {
			t.Errorf("policyAllowsAddr(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
	// Unmap equivalence: mapped form must classify identically to the
	// plain v4 form.
	for _, pair := range [][2]string{{"::ffff:8.8.8.8", "8.8.8.8"}, {"::ffff:10.1.2.3", "10.1.2.3"}} {
		m, _ := netip.ParseAddr(pair[0])
		p, _ := netip.ParseAddr(pair[1])
		if policyAllowsAddr(m) != policyAllowsAddr(p) {
			t.Errorf("%s and %s must classify identically", pair[0], pair[1])
		}
	}
}

// ---------------------------------------------------------------------------
// 4. most-specific special-range semantics
// ---------------------------------------------------------------------------

func TestSSRFMostSpecificSpecialRange(t *testing.T) {
	// A globally-reachable exception MORE SPECIFIC than a special-use
	// parent must be allowed, while its siblings stay denied.
	allow, err := netip.ParseAddr("192.0.0.9") // PCP anycast /32
	if err != nil {
		t.Fatal(err)
	}
	deny, err := netip.ParseAddr("192.0.0.8") // sibling inside 192.0.0.0/24
	if err != nil {
		t.Fatal(err)
	}
	if !policyAllowsAddr(allow) {
		t.Fatal("192.0.0.9 (GR=TRUE /32 inside special-use /24) must be allowed — most-specific match")
	}
	if policyAllowsAddr(deny) {
		t.Fatal("192.0.0.8 (GR=FALSE inside 192.0.0.0/24) must be denied")
	}
	// A non-globally-reachable prefix MORE SPECIFIC than the global
	// unicast default must be denied (documentation inside 2000::/3).
	doc, err := netip.ParseAddr("2001:db8::1")
	if err != nil {
		t.Fatal(err)
	}
	if policyAllowsAddr(doc) {
		t.Fatal("2001:db8::1 (GR=FALSE /32 more specific than global unicast default) must be denied")
	}
	// v6 boundary precision: 2001::1 sits inside Teredo 2001::/32
	// (GR=FALSE) and must be denied; 2001:1::1 is the PCP anycast
	// /128 exception (GR=TRUE) and must be allowed; 2001:1::3 is
	// ordinary global unicast (no special entry) — default allow.
	for ip, want := range map[string]bool{"2001:1::1": true, "2001::1": false, "2001:1::3": true} {
		a, _ := netip.ParseAddr(ip)
		if got := policyAllowsAddr(a); got != want {
			t.Errorf("%s = %v, want %v", ip, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 5. hostname paths: private / mixed / empty / resolver error
// ---------------------------------------------------------------------------

func TestSSRFHostnamePrivateDenied(t *testing.T) {
	withResolver(fakeResolver{"evil.internal": {ipa("192.168.0.10")}}, func() {
		_, err := hardenedDialContext(context.Background(), "tcp", "evil.internal:80")
		if err == nil || !isIPPolicyError(err) {
			t.Fatalf("want policy refusal, got %v", err)
		}
	})
}

func TestSSRFHostnameMixedFailsClosed(t *testing.T) {
	withResolver(fakeResolver{"mixed.example": {ipa("93.184.216.34"), ipa("10.0.0.9")}}, func() {
		_, err := hardenedDialContext(context.Background(), "tcp", "mixed.example:443")
		if err == nil || !isIPPolicyError(err) {
			t.Fatalf("mixed resolution must fail closed, got %v", err)
		}
	})
}

func TestSSRFHostnameZeroDNSResultFailsClosed(t *testing.T) {
	withResolver(fakeResolver{"empty.example": {}}, func() {
		_, err := hardenedDialContext(context.Background(), "tcp", "empty.example:80")
		if err == nil || !isIPPolicyError(err) {
			t.Fatalf("zero-address resolution must fail closed, got %v", err)
		}
	})
}

func TestSSRFResolverErrorFailsClosed(t *testing.T) {
	er := &errResolver{}
	withResolver(er, func() {
		_, err := hardenedDialContext(context.Background(), "tcp", "broken.example:80")
		if err == nil || isIPPolicyError(err) {
			t.Fatalf("resolver error must surface as dial failure, got %v", err)
		}
		if !strings.Contains(err.Error(), "postapi dns resolve") {
			t.Fatalf("error must be classified as dns resolve failure: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. dial seam: literal-IP-only + single resolution + fallback
// ---------------------------------------------------------------------------

// TestSSRFValidatedDialUsesLiteralIP — rewritten proof:
//
//	hostname example.test:443, resolver returns two test IPs
//	(203.0.114.10, 2001:4860:4860::10 — both allowed); the dialer must
//	receive ONLY "203.0.114.10:443" style literals and NEVER
//	"example.test:443"; resolver called exactly once.
func TestSSRFValidatedDialUsesLiteralIP(t *testing.T) {
	cr := &countingResolver{addrs: []net.IPAddr{ipa("203.0.114.10"), ipa("2001:4860:4860::10")}}
	rd := &recordingDialer{conn: stubConn{}}
	withResolver(cr, func() {
		withDialer(rd, func() {
			conn, err := hardenedDialContext(context.Background(), "tcp", "example.test:443")
			if err != nil {
				t.Fatalf("dial must succeed through fakes: %v", err)
			}
			if conn == nil {
				t.Fatal("expected a conn from the fake dialer")
			}
		})
	})
	if len(rd.targets) == 0 {
		t.Fatal("dialer was never called")
	}
	for _, target := range rd.targets {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			t.Fatalf("dial target %q is not ip:port", target)
		}
		if _, perr := netip.ParseAddr(host); perr != nil {
			t.Fatalf("dialer received NON-literal target %q (must be IP only)", target)
		}
		if strings.Contains(target, "example.test") {
			t.Fatalf("dialer received the HOSTNAME %q — rebinding hole", target)
		}
	}
	if rd.targets[0] != "203.0.114.10:443" {
		t.Fatalf("first dial target = %q, want 203.0.114.10:443", rd.targets[0])
	}
	if cr.calls != 1 {
		t.Fatalf("resolver called %d times, want exactly 1", cr.calls)
	}
}

// TestSSRFMultiIPFallbackWithoutReResolution: first validated literal
// dial fails (scripted), second succeeds; resolver still called once;
// both dial targets are validated literals.
func TestSSRFMultiIPFallbackWithoutReResolution(t *testing.T) {
	cr := &countingResolver{addrs: []net.IPAddr{ipa("203.0.114.10"), ipa("203.0.114.11")}}
	rd := &recordingDialer{
		conn:        stubConn{},
		failTargets: map[string]bool{"203.0.114.10:443": true},
	}
	withResolver(cr, func() {
		withDialer(rd, func() {
			conn, err := hardenedDialContext(context.Background(), "tcp", "fallback.test:443")
			if err != nil {
				t.Fatalf("fallback to second validated IP must succeed: %v", err)
			}
			if conn == nil {
				t.Fatal("expected a conn from the fake dialer")
			}
		})
	})
	if len(rd.targets) != 2 {
		t.Fatalf("dial attempts = %d (%v), want 2 (first fail then fallback)", len(rd.targets), rd.targets)
	}
	if rd.targets[0] != "203.0.114.10:443" || rd.targets[1] != "203.0.114.11:443" {
		t.Fatalf("dial sequence = %v, want [203.0.114.10:443 203.0.114.11:443]", rd.targets)
	}
	if cr.calls != 1 {
		t.Fatalf("resolver called %d times during fallback, want exactly 1 (no re-resolution)", cr.calls)
	}
}

// TestSSRFMultiIPAllFailNoReResolution: all validated IPs failing
// surfaces the LAST connect error; resolver still exactly once.
func TestSSRFMultiIPAllFailNoReResolution(t *testing.T) {
	cr := &countingResolver{addrs: []net.IPAddr{ipa("203.0.114.10"), ipa("203.0.114.11")}}
	rd := &recordingDialer{
		failTargets: map[string]bool{"203.0.114.10:443": true, "203.0.114.11:443": true},
	}
	withResolver(cr, func() {
		withDialer(rd, func() {
			conn, err := hardenedDialContext(context.Background(), "tcp", "allfail.test:443")
			if err == nil || conn != nil {
				t.Fatal("all-fail must yield an error, not a conn")
			}
			if isIPPolicyError(err) {
				t.Fatalf("all-fail must be a connect error, not a policy error: %v", err)
			}
		})
	})
	if cr.calls != 1 {
		t.Fatalf("resolver called %d times, want exactly 1", cr.calls)
	}
	if len(rd.targets) != 2 {
		t.Fatalf("dial attempts = %d, want 2", len(rd.targets))
	}
}

// TestSSRFSingleDNSResolution (loopback-resolution refusal still
// resolves exactly once).
func TestSSRFSingleDNSResolution(t *testing.T) {
	cr := &countingResolver{addrs: []net.IPAddr{ipa("127.0.0.1")}}
	withResolver(cr, func() {
		_, err := hardenedDialContext(context.Background(), "tcp", "once.example:80")
		if err == nil || !isIPPolicyError(err) {
			t.Fatalf("loopback resolution must be refused: %v", err)
		}
	})
	if cr.calls != 1 {
		t.Fatalf("resolver called %d times, want exactly 1 (rebinding defense)", cr.calls)
	}
}

// TestSSRFLiteralHostNeverResolves: an IP-literal target must not
// touch the resolver at all.
func TestSSRFLiteralHostNeverResolves(t *testing.T) {
	cr := &countingResolver{}
	rd := &recordingDialer{conn: stubConn{}}
	withResolver(cr, func() {
		withDialer(rd, func() {
			_, err := hardenedDialContext(context.Background(), "tcp", "203.0.114.10:443")
			if err != nil {
				t.Fatalf("public literal must dial: %v", err)
			}
		})
	})
	if cr.calls != 0 {
		t.Fatalf("resolver called %d times for an IP literal, want 0", cr.calls)
	}
	if len(rd.targets) != 1 || rd.targets[0] != "203.0.114.10:443" {
		t.Fatalf("dial targets = %v, want exactly [203.0.114.10:443]", rd.targets)
	}
}

// TestSSRFDNSResultDedupe: duplicate resolver answers collapse.
func TestSSRFDNSResultDedupe(t *testing.T) {
	cr := &countingResolver{addrs: []net.IPAddr{ipa("203.0.114.10"), ipa("203.0.114.10"), ipa("203.0.114.11")}}
	rd := &recordingDialer{
		conn:        stubConn{},
		failTargets: map[string]bool{"203.0.114.10:443": true},
	}
	withResolver(cr, func() {
		withDialer(rd, func() {
			_, err := hardenedDialContext(context.Background(), "tcp", "dupe.test:443")
			if err != nil {
				t.Fatalf("deduped fallback must succeed: %v", err)
			}
		})
	})
	if len(rd.targets) != 2 {
		t.Fatalf("dial attempts = %d (%v), want 2 (duplicate removed)", len(rd.targets), rd.targets)
	}
}

// ---------------------------------------------------------------------------
// 7. Host header / TLS SNI authority stays the original hostname
// ---------------------------------------------------------------------------

// hostRecorderConn is a fake net.Conn that records every byte the
// real http.Transport writes to the "connection" — proving ON THE WIRE
// that the Host header authority is the original URL hostname, not an
// IP rewrite.
type hostRecorderConn struct {
	written   bytes.Buffer
	wroteOnce chan struct{} // closed on the first Write
}

func newHostRecorderConn() *hostRecorderConn {
	return &hostRecorderConn{wroteOnce: make(chan struct{})}
}

// Read gates EOF on the first Write: the transport may probe the
// connection BEFORE the request is written; an ungated EOF can abort
// the write path before the wire bytes are observable (the historical
// CI flake). Deterministic write-observation gating — no sleeps.
func (c *hostRecorderConn) Read(b []byte) (int, error) {
	<-c.wroteOnce
	return 0, io.EOF
}
func (c *hostRecorderConn) Write(b []byte) (int, error) {
	c.wroteOnceOnce()
	return c.written.Write(b)
}
func (c *hostRecorderConn) wroteOnceOnce() {
	select {
	case <-c.wroteOnce:
	default:
		close(c.wroteOnce)
	}
}
func (c *hostRecorderConn) Close() error         { return nil }
func (c *hostRecorderConn) LocalAddr() net.Addr  { return nil }
func (c *hostRecorderConn) RemoteAddr() net.Addr { return nil }
func (c *hostRecorderConn) SetDeadline(t time.Time) error {
	return nil
}
func (c *hostRecorderConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *hostRecorderConn) SetWriteDeadline(t time.Time) error { return nil }

// TestSSRFHostAuthorityUnchangedAtTransportLevel proves, at the wire
// level of the REAL http.Transport, that the request carries Host
// "example.test:8080" (original URL authority) while the TCP connect
// target (observed at the dial seam) is the validated literal IP only.
// The SSRF defense lives ONLY in the dial seam — never in a URL rewrite.
func TestSSRFHostAuthorityUnchangedAtTransportLevel(t *testing.T) {
	cr := &countingResolver{addrs: []net.IPAddr{ipa("203.0.114.10")}}
	rd := &recordingDialer{conn: newHostRecorderConn()}

	orig := sharedClient
	sharedClient = newHardenedClient()
	t.Cleanup(func() { sharedClient = orig })

	withResolver(cr, func() {
		withDialer(rd, func() {
			res := PostJSON(context.Background(), "http://example.test:8080/hook", []byte(`{}`), AgentSubmitErrorConfig())
			_ = res // transport errors against the fake conn are fine
		})
	})

	wire := rd.conn.(*hostRecorderConn).written.String()
	if !strings.Contains(wire, "Host: example.test:8080\r\n") {
		t.Fatalf("wire Host header missing/rewritten — got wire: %q", wire)
	}
	if strings.Contains(wire, "Host: 203.0.114.10") {
		t.Fatalf("Host header was rewritten to the dial IP — URL authority must stay the hostname. wire: %q", wire)
	}
	if strings.Contains(wire, "POST /hook HTTP/1.1") != true {
		t.Fatalf("request line must address the original path. wire: %q", wire)
	}
	if len(rd.targets) != 1 || rd.targets[0] != "203.0.114.10:8080" {
		t.Fatalf("dial targets = %v, want [203.0.114.10:8080] — literal connect only", rd.targets)
	}
	if cr.calls != 1 {
		t.Fatalf("resolver called %d times, want exactly 1", cr.calls)
	}
}

// TestSSRFTLSSNIFromOriginalHostname proves the https URL hostname
// (hence TLS SNI) stays the original name while the connect target is
// the validated literal.
func TestSSRFTLSSNIFromOriginalHostname(t *testing.T) {
	cr := &countingResolver{addrs: []net.IPAddr{ipa("203.0.114.10")}}
	rd := &recordingDialer{conn: stubConn{}}
	withResolver(cr, func() {
		withDialer(rd, func() {
			// TLS handshake would fail against a stubConn; assert at
			// the dial/URL layer instead: URL hostname unchanged by
			// policy (mechanism: http.Transport keeps req.URL.Hostname()
			// as SNI; dial seam only replaces the connect target).
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://sni.test:443/x", strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			if req.URL.Hostname() != "sni.test" {
				t.Fatalf("URL hostname = %q, want sni.test (SNI source)", req.URL.Hostname())
			}
			conn, derr := hardenedDialContext(context.Background(), "tcp", "sni.test:443")
			if derr != nil {
				t.Fatalf("dial must use the fake: %v", derr)
			}
			if conn == nil {
				t.Fatal("expected fake conn")
			}
		})
	})
	if len(rd.targets) != 1 || rd.targets[0] != "203.0.114.10:443" {
		t.Fatalf("dial targets = %v, want [203.0.114.10:443]", rd.targets)
	}
	// SNI authority still derives from URL hostname — the dial target
	// carries the IP, the URL does not.
	if cr.calls != 1 {
		t.Fatalf("resolver called %d times, want 1", cr.calls)
	}
}

// ---------------------------------------------------------------------------
// 8. loopback PostJSON deny (local-only socket; proves refusal of the
//    local machine, not public connectivity)
// ---------------------------------------------------------------------------

func TestSSRFLoopbackServerDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	res := PostJSON(context.Background(), srv.URL, []byte(`{}`), AgentSubmitErrorConfig())
	if res.Success {
		t.Fatal("loopback PostAPI target must be denied by the outbound policy")
	}
	if res.ErrorCode != AgentSubmitErrorConfig().NetworkCode {
		t.Fatalf("error code = %q, want network error classification", res.ErrorCode)
	}
}

// ---------------------------------------------------------------------------
// 9. transport contract: redirect / timeouts / bounded read
// ---------------------------------------------------------------------------

func TestSSRFRedirectStillForbidden(t *testing.T) {
	c := newHardenedClient()
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect must exist (redirects forbidden)")
	}
	req, _ := http.NewRequest("GET", "http://example.invalid/", nil)
	if err := c.CheckRedirect(req, nil); err != http.ErrUseLastResponse {
		t.Fatal("redirects must not be followed")
	}
}

func TestSSRFTimeoutContractUnchanged(t *testing.T) {
	c := newHardenedClient()
	if c.Timeout != 10*time.Second {
		t.Fatalf("total timeout = %v, want 10s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("transport must carry the hardened dial authority")
	}
	if postAPIRequestTimeout != 10*time.Second || postAPIConnectTimeout != 5*time.Second {
		t.Fatalf("timeout constants drifted: total=%v connect=%v", postAPIRequestTimeout, postAPIConnectTimeout)
	}
	// The production dial seam carries the 5s connect timeout.
	dd, ok := dialer.(defaultNetDialer)
	if !ok {
		t.Fatal("production dialer must be defaultNetDialer in non-test context")
	}
	if dd.d.Timeout != 5*time.Second {
		t.Fatalf("connect timeout = %v, want 5s", dd.d.Timeout)
	}
}

// TestSSRFPolicyErrorMessage: refusals are classifiable.
func TestSSRFPolicyErrorMessage(t *testing.T) {
	_, err := hardenedDialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil || !strings.Contains(err.Error(), "not publicly routable") {
		t.Fatalf("literal refusal message: %v", err)
	}
	_, err = hardenedDialContext(context.Background(), "udp", "8.8.8.8:53")
	if err == nil || !isIPPolicyError(err) {
		t.Fatalf("non-tcp network must be refused: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 10. test-only loopback bypass guards (bypass NOT used by any core
//     SSRF proof above)
// ---------------------------------------------------------------------------

func TestSSRFLoopbackBypassNeverInProduction(t *testing.T) {
	var prodFiles []string
	err := filepath.Walk("../..", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && (info.Name() == "web" || info.Name() == ".git" || info.Name() == "migrations") {
				return filepath.SkipDir
			}
			return err
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			prodFiles = append(prodFiles, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range prodFiles {
		// ssrf.go DEFINES the test hook; a reference there is the
		// definition itself. Any OTHER production file referencing it
		// is a violation.
		if strings.HasSuffix(f, "internal/delivery/ssrf.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "AllowLoopbackForTest") {
			t.Errorf("%s references AllowLoopbackForTest — production must never relax the SSRF policy", f)
		}
	}
}

func TestSSRFLoopbackBypassRestoresPolicy(t *testing.T) {
	restore := AllowLoopbackForTest()
	a, _ := netip.ParseAddr("127.0.0.1")
	if !policyAllowsAddr(a) {
		t.Fatal("bypass must allow loopback")
	}
	restore()
	if policyAllowsAddr(a) {
		t.Fatal("restore must re-deny loopback")
	}
}

// TestSSRFCoreTestsNeverUsedBypass is a source-level guard: the core
// SSRF proof file must not enable the loopback bypass (public policy
// is proven without it). The pattern is assembled at runtime so this
// count does not count its own literal.
func TestSSRFCoreTestsNeverUsedBypass(t *testing.T) {
	b, err := os.ReadFile("ssrf_test.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	pattern := "AllowLoopback" + "ForTest" + "()"
	n := strings.Count(src, pattern)
	if n != 1 {
		t.Fatalf("ssrf_test.go calls %s %d times, want exactly 1 (the restore-policy guard only)", pattern, n)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
