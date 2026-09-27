package mcpserver

// TestMain: the WO-7b lifecycle test delivers to an in-process TLS
// receiver (loopback) — the delivery package's TEST-ONLY bypass, plus
// a per-process self-signed certificate so the https receiver URL
// both passes contract validation and verifies against the hardened
// client (same technique as internal/service).
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

var wo7TLSCert tls.Certificate

// okReceiverURL is a package-wide receiver that accepts everything
// (HTTP 200 with a message body): the default receiver of test tasks.
var okReceiverURL string

func TestMain(m *testing.M) {
	wo7TLSCert = mustGenCert()
	restore := delivery.AllowLoopbackForTest()
	ok := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"accepted"}`))
	}))
	ok.TLS = &tls.Config{Certificates: []tls.Certificate{wo7TLSCert}}
	ok.StartTLS()
	okReceiverURL = ok.URL
	code := m.Run()
	ok.Close()
	restore()
	os.Exit(code)
}

func mustGenCert() tls.Certificate {
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
