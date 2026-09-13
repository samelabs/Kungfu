package server

// /credits page closure tests: the page renders the real economic
// mechanisms and live entry points, never the retired placeholder copy,
// and never a fictional purchase flow.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreditsPageRendersRealMechanisms(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/credits", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content-type = %s", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()

	// The retired placeholder copy is gone.
	if strings.Contains(body, "will be listed here") {
		t.Fatal("placeholder copy 'will be listed here' still present")
	}

	// Live entry points.
	for _, want := range []string{"/owner/store", "/owner/tasks", "/owner/logs"} {
		if !strings.Contains(body, want) {
			t.Fatalf("entry point %s missing", want)
		}
	}

	// Real ledger facts explained on the page.
	for _, want := range []string{"earn_task", "spend_redemption", "lock_task", "refund_task"} {
		if !strings.Contains(body, want) {
			t.Fatalf("mechanism %s not explained", want)
		}
	}

	// No fictional purchase flow.
	for _, banned := range []string{"Buy credits", "Recharge", "Stripe", "credit package", "Credit package"} {
		if strings.Contains(body, banned) {
			t.Fatalf("fictional purchase copy present: %q", banned)
		}
	}
}
