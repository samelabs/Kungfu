package server

// TestMain enables the delivery package's TEST-ONLY loopback bypass:
// these tests use in-process httptest servers (loopback) as PostAPI
// receivers. The bypass exists only in test binaries — a source guard
// (internal/delivery TestSSRFLoopbackBypassNeverInProduction) asserts
// no production file references it, and the shipped server always
// enforces the public-routable PostAPI policy.
//
// It also starts one https receiver that accepts every delivery (HTTP
// 200, `{"message":"accepted"}`) under a per-process self-signed
// certificate the delivery client trusts: the receiver.url of test
// tasks (same technique as internal/service).
import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"kungfu.md/internal/delivery"
)

// okReceiverURL is the accept-everything test receiver.
var okReceiverURL string

func TestMain(m *testing.M) {
	restore := delivery.AllowLoopbackForTest()
	ok := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"accepted"}`))
	}))
	ok.TLS = &tls.Config{Certificates: []tls.Certificate{mustServerTestCert()}}
	ok.StartTLS()
	okReceiverURL = ok.URL
	code := m.Run()
	ok.Close()
	restore()
	os.Exit(code)
}

func mustServerTestCert() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	delivery.TrustRootsForTest(pool)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
