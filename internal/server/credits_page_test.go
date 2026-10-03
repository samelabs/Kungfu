package server

// /credits page closure tests (WO-28): the page renders the real
// economic mechanisms in the shared legal layout, links to no
// signed-in-only screen except the top-up button, never the retired
// placeholder copy, and never a fictional purchase flow.

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

	// Legal document layout: same shell as /terms and /privacy.
	for _, want := range []string{`class="legal-body"`, `class="legal-section"`, `class="site-footer"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("legal layout element %s missing", want)
		}
	}

	// The only signed-in-only link is the top-up button.
	if !strings.Contains(body, "/owner/credits") {
		t.Fatal("top-up entry point /owner/credits missing")
	}
	for _, banned := range []string{"/owner/rewards", "/owner/logs"} {
		if strings.Contains(body, banned) {
			t.Fatalf("signed-in-only entry point %s still linked from the public credits page", banned)
		}
	}

	// Real ledger facts explained on the page. The internal ledger
	// identifiers stay off the public page (they read as engineering
	// artifacts); the test locks the mechanism SEMANTICS instead:
	// where credits come from (signup, top-ups, accepted work), what
	// they are for (task budgets locked at create/fund, unused budget
	// refundable at close; reward redemptions), one shared balance.
	for _, want := range []string{
		"the task price is booked to the account",
		"locked from the balance when a task is created or funded",
		"unused budget is refundable once the task is closed",
		"Reward redemptions",
		"Owner and agent access use the same account balance",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("mechanism semantics %q not explained", want)
		}
	}
	// And the identifiers themselves must NOT leak to the public page.
	for _, banned := range []string{"earn_task", "spend_redemption", "lock_task", "refund_task", "grant_signup"} {
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
