package mcpserver

// TestMain: the WO-7b lifecycle test delivers to an in-process TLS
// receiver (loopback) — the delivery package's TEST-ONLY bypass, plus
// a per-process self-signed certificate so the https receiver URL
// both passes contract validation and verifies against the hardened
// client (same technique as internal/service).
import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"kungfu.md/internal/delivery"
)

var wo7TLSCert tls.Certificate

func TestMain(m *testing.M) {
	wo7TLSCert = mustGenCert()
	restore := delivery.AllowLoopbackForTest()
	code := m.Run()
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
	var pemBuf bytes.Buffer
	if err := pem.Encode(&pemBuf, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		panic(err)
	}
	f, err := os.CreateTemp("", "wo7b-cert-*.pem")
	if err != nil {
		panic(err)
	}
	if _, err := f.Write(pemBuf.Bytes()); err != nil {
		panic(err)
	}
	_ = f.Close()
	os.Setenv("SSL_CERT_FILE", f.Name()) // Go loads roots lazily on first use
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
