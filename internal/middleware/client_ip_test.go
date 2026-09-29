package middleware

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func s63Req(remote, xfp string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	if xfp != "" {
		r.Header.Set("X-Forwarded-Proto", xfp)
	}
	return r
}

var s63Loopback = mustCIDR("127.0.0.0/8")

func TestProxyTrustDirectTLSIsHTTPS(t *testing.T) {
	r := s63Req("203.0.113.9:5555", "http") // even a contradicting header
	r.TLS = &tls.ConnectionState{}
	if !IsHTTPS(r, nil) {
		t.Fatal("direct TLS must be HTTPS regardless of proxy config")
	}
}

func TestProxyTrustTrustedProxyHTTPSForwardIsHTTPS(t *testing.T) {
	r := s63Req("127.0.0.5:8080", "https")
	if !IsHTTPS(r, s63Loopback) {
		t.Fatal("trusted peer forwarding https must be HTTPS")
	}
	// case-insensitive single token
	r = s63Req("127.0.0.5:8080", "HTTPS")
	if !IsHTTPS(r, s63Loopback) {
		t.Fatal("case-insensitive https token must be accepted")
	}
	// surrounding whitespace is trimmed
	r = s63Req("127.0.0.5:8080", "  https  ")
	if !IsHTTPS(r, s63Loopback) {
		t.Fatal("trimmed https token must be accepted")
	}
}

func TestProxyTrustUntrustedPeerCannotSpoofHTTPS(t *testing.T) {
	r := s63Req("203.0.113.9:5555", "https")
	if IsHTTPS(r, s63Loopback) {
		t.Fatal("untrusted peer spoofing X-Forwarded-Proto must NOT be HTTPS")
	}
}

func TestProxyTrustTrustedProxyHTTPIsNotHTTPS(t *testing.T) {
	r := s63Req("127.0.0.5:8080", "http")
	if IsHTTPS(r, s63Loopback) {
		t.Fatal("forwarded http is not HTTPS")
	}
}

func TestProxyTrustMalformedForwardedProtoFailsClosed(t *testing.T) {
	for _, xfp := range []string{"https,https", "https, http", "https http", "", "hxxps", "https://"} {
		r := s63Req("127.0.0.5:8080", xfp)
		if IsHTTPS(r, s63Loopback) {
			t.Fatalf("malformed/multi X-Forwarded-Proto %q must fail closed", xfp)
		}
	}
}

// Actual multiple header VALUES (Header.Get would hide them): the
// invariant is EXACTLY ONE header value — duplicates fail closed too.
func TestProxyTrustMultipleForwardedProtoHeaderValuesFailClosed(t *testing.T) {
	// https + http
	r := s63Req("127.0.0.5:8080", "")
	r.Header.Add("X-Forwarded-Proto", "https")
	r.Header.Add("X-Forwarded-Proto", "http")
	if IsHTTPS(r, s63Loopback) {
		t.Fatal("two X-Forwarded-Proto values (https/http) must fail closed")
	}

	// duplicate https + https — consistency is not the rule; exactly
	// one value is.
	r2 := s63Req("127.0.0.5:8080", "")
	r2.Header.Add("X-Forwarded-Proto", "https")
	r2.Header.Add("X-Forwarded-Proto", "https")
	if IsHTTPS(r2, s63Loopback) {
		t.Fatal("duplicate X-Forwarded-Proto https values must fail closed")
	}

	// zero values
	r3 := s63Req("127.0.0.5:8080", "")
	if IsHTTPS(r3, s63Loopback) {
		t.Fatal("zero X-Forwarded-Proto values must not be HTTPS")
	}
}

func TestProxyTrustMalformedRemoteAddrCannotBecomeTrusted(t *testing.T) {
	for _, remote := range []string{"not-an-addr", "999.999.999.999:1", ""} {
		r := s63Req(remote, "https")
		if IsHTTPS(r, s63Loopback) {
			t.Fatalf("malformed RemoteAddr %q must not become trusted", remote)
		}
	}
}

func mustCIDR(s string) []*net.IPNet {
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return []*net.IPNet{cidr}
}

// s17Req builds a request with a remote addr and any number of
// X-Forwarded-For header lines.
func s17Req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for _, line := range xff {
		r.Header.Add("X-Forwarded-For", line)
	}
	return r
}

// TestGetClientIP — table-driven coverage of the rightmost-untrusted
// X-Forwarded-For evaluation (P1-2).
func TestGetClientIP(t *testing.T) {
	trusted := mustCIDR("127.0.0.0/8")
	cases := []struct {
		name   string
		remote string
		xff    []string
		cidrs  []*net.IPNet
		want   string
	}{
		{
			name:   "no trusted proxies: RemoteAddr wins, XFF ignored",
			remote: "203.0.113.9:5555",
			xff:    []string{"1.2.3.4"},
			cidrs:  nil,
			want:   "203.0.113.9",
		},
		{
			name:   "untrusted peer with XFF: RemoteAddr wins",
			remote: "203.0.113.9:5555",
			xff:    []string{"1.2.3.4"},
			cidrs:  trusted,
			want:   "203.0.113.9",
		},
		{
			// the appended chain: spoofed leftmost, real client last
			name:   "trusted peer: forged leftmost, real client rightmost",
			remote: "127.0.0.5:8080",
			xff:    []string{"9.9.9.9, 198.51.100.7"},
			cidrs:  trusted,
			want:   "198.51.100.7",
		},
		{
			name:   "single appended client",
			remote: "127.0.0.5:8080",
			xff:    []string{"198.51.100.7"},
			cidrs:  trusted,
			want:   "198.51.100.7",
		},
		{
			// Header.Get would only see the first line
			name:   "multi-line XFF flattened, rightmost untrusted wins",
			remote: "127.0.0.5:8080",
			xff:    []string{"9.9.9.9", "198.51.100.7, 127.0.0.9"},
			cidrs:  trusted,
			want:   "198.51.100.7",
		},
		{
			name:   "IPv4-mapped entry normalized to IPv4",
			remote: "127.0.0.5:8080",
			xff:    []string{"::ffff:198.51.100.7"},
			cidrs:  trusted,
			want:   "198.51.100.7",
		},
		{
			// the invalid entry sits LEFT of the answer; the rightmost
			// walk reaches the valid untrusted entry first
			name:   "invalid entry left of the answer is never reached",
			remote: "127.0.0.5:8080",
			xff:    []string{"not-an-ip, 198.51.100.7"},
			cidrs:  trusted,
			want:   "198.51.100.7",
		},
		{
			// the invalid entry sits RIGHT of the answer: the chain is
			// broken where a trusted entry should be — fall back to the
			// direct peer instead of trusting anything further left
			name:   "invalid entry breaks the chain: direct peer returned",
			remote: "127.0.0.5:8080",
			xff:    []string{"198.51.100.7, not-an-ip"},
			cidrs:  trusted,
			want:   "127.0.0.5",
		},
		{
			name:   "all entries trusted: direct peer returned",
			remote: "127.0.0.5:8080",
			xff:    []string{"127.0.0.9, 127.0.0.10"},
			cidrs:  trusted,
			want:   "127.0.0.5",
		},
		{
			name:   "no XFF from trusted peer: peer returned",
			remote: "127.0.0.5:8080",
			cidrs:  trusted,
			want:   "127.0.0.5",
		},
		{
			name:   "forged CF-Connecting-IP ignored",
			remote: "203.0.113.9:5555",
			xff:    []string{},
			cidrs:  trusted,
			want:   "203.0.113.9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := s17Req(tc.remote, tc.xff...)
			r.Header.Set("CF-Connecting-IP", "6.6.6.6") // always present: must never win
			if got := GetClientIP(r, tc.cidrs); got != tc.want {
				t.Fatalf("GetClientIP = %q, want %q", got, tc.want)
			}
		})
	}
}
