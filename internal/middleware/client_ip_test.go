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

func TestS63DirectTLSIsHTTPS(t *testing.T) {
	r := s63Req("203.0.113.9:5555", "http") // even a contradicting header
	r.TLS = &tls.ConnectionState{}
	if !IsHTTPS(r, nil) {
		t.Fatal("direct TLS must be HTTPS regardless of proxy config")
	}
}

func TestS63TrustedProxyHTTPSForwardIsHTTPS(t *testing.T) {
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

func TestS63UntrustedPeerCannotSpoofHTTPS(t *testing.T) {
	r := s63Req("203.0.113.9:5555", "https")
	if IsHTTPS(r, s63Loopback) {
		t.Fatal("untrusted peer spoofing X-Forwarded-Proto must NOT be HTTPS")
	}
}

func TestS63TrustedProxyHTTPIsNotHTTPS(t *testing.T) {
	r := s63Req("127.0.0.5:8080", "http")
	if IsHTTPS(r, s63Loopback) {
		t.Fatal("forwarded http is not HTTPS")
	}
}

func TestS63MalformedForwardedProtoFailsClosed(t *testing.T) {
	for _, xfp := range []string{"https,https", "https, http", "https http", "", "hxxps", "https://"} {
		r := s63Req("127.0.0.5:8080", xfp)
		if IsHTTPS(r, s63Loopback) {
			t.Fatalf("malformed/multi X-Forwarded-Proto %q must fail closed", xfp)
		}
	}
}

func TestS63MalformedRemoteAddrCannotBecomeTrusted(t *testing.T) {
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
