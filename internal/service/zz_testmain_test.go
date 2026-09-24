package service

// TestMain enables the delivery package's TEST-ONLY loopback bypass:
// these tests use in-process httptest servers (loopback) as PostAPI
// receivers. The bypass exists only in test binaries — a source guard
// (internal/delivery TestSSRFLoopbackBypassNeverInProduction) asserts
// no production file references it, and the shipped server always
// enforces the public-routable PostAPI policy.
import (
	"os"
	"testing"

	"kungfu.md/internal/delivery"
)

func TestMain(m *testing.M) {
	restore := delivery.AllowLoopbackForTest()
	code := m.Run()
	restore()
	os.Exit(code)
}
