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

// Actual multiple header VALUES (Header.Get would hide them): the
// invariant is EXACTLY ONE header value — duplicates fail closed too.
func TestS63MultipleForwardedProtoHeaderValuesFailClosed(t *testing.T) {
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
