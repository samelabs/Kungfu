package service

// TestMain enables the delivery package's TEST-ONLY loopback bypass:
// these tests use in-process httptest servers (loopback) as PostAPI
// receivers. The bypass exists only in test binaries — a source guard
// (internal/delivery TestSSRFLoopbackBypassNeverInProduction) asserts
// no production file references it, and the shipped server always
// enforces the public-routable PostAPI policy.
//
// It also generates a self-signed server certificate for 127.0.0.1 and
// makes the delivery client trust it (delivery.TrustRootsForTest), so httptest TLS
// receivers get https:// URLs that both pass §3 receiver validation and
// verify against the hardened delivery client.
import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"kungfu.md/internal/delivery"
)

var pubTestTLSCert tls.Certificate

func TestMain(m *testing.M) {
	pubTestTLSCert = mustGenerateTestCert()
	restore := delivery.AllowLoopbackForTest()
	code := m.Run()
	restore()
	os.Exit(code)
}

func mustGenerateTestCert() tls.Certificate {
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
	leaf, perr := x509.ParseCertificate(der)
	if perr != nil {
		panic(perr)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	// Trust the test cert in the delivery client explicitly: macOS Go
	// verifies with the system keychain and ignores SSL_CERT_FILE.
	delivery.TrustRootsForTest(pool)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
