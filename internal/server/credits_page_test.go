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

	// Real ledger facts explained on the page. The internal ledger
	// identifiers were removed from user-facing copy in the frontend
	// closure round (they read as engineering artifacts on a public
	// page); the test now locks the mechanism SEMANTICS instead:
	// earning via tasks, redeeming in the store, budget lock at task
	// creation (including pending), and refund of the remaining budget
	// at close.
	for _, want := range []string{"booked to your account", "deducted from your balance", "is locked from your account balance", "can return to you"} {
		if !strings.Contains(body, want) {
			t.Fatalf("mechanism semantics %q not explained", want)
		}
	}
	// And the identifiers themselves must NOT leak to the public page.
	for _, banned := range []string{"earn_task", "spend_redemption", "lock_task", "refund_task"} {
		if strings.Contains(body, banned) {
			t.Fatalf("internal ledger identifier %q exposed on public page", banned)
		}
	}

	// No fictional purchase flow.
	for _, banned := range []string{"Buy credits", "Recharge", "Stripe", "credit package", "Credit package"} {
		if strings.Contains(body, banned) {
			t.Fatalf("fictional purchase copy present: %q", banned)
		}
	}
}
