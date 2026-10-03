package server

// Factual contract regression: every public surface that describes the
// task budget lock must state the REAL authority — the full task budget
// is locked at TASK CREATION time (inside the CreateTask transaction,
// before the task row exists), and remains locked while the task is
// pending. Opening/publishing the task is NOT the moment the budget
// gets locked.
//
// The invariant spans surfaces: if Credits says creation-time lock and
// Terms drifts back to publish/open-time (or vice versa) the site is
// lying to users on one of them. This test locks BOTH sides, in all
// five locales, at the i18n source AND against the rendered /credits
// and /terms HTML.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kungfu.md/internal/i18n"
)

// Per-locale markers that must appear in the budget-lock wording.
// Each marker asserts the locked-at-creation fact in that locale's own wording.
var budgetLockCreationMarkers = map[string][]string{
	"en": {"Creating a task locks its budget from the Owner balance"},
	"zh": {"创建任务时从 Owner 余额锁定其预算"},
	"ja": {"タスクを作成すると Owner 残高から予算がロックされます"},
	"ko": {"작업을 만들면 Owner 잔액에서 예산이 잠깁니다"},
	"es": {"Crear una tarea bloquea su presupuesto del saldo del Owner"},
}

// Phrases that would re-introduce the WRONG authority (lock at
// publish/open time) on the budget-lock sentences. Locale-specific
// publish/open phrasing used by the retired copy.
var budgetLockWrongAuthority = map[string][]string{
	"en": {"publishing locks", "opening locks the task budget", "publishing the task locks"},
	"zh": {"发布任务时会从 Owner 余额锁定", "任务开启时锁定"},
	"ja": {"タスク公開時に", "予算がロックされます。タスクの Post API"}, // 公開時 lock claim
	"ko": {"작업 게시 시", "예산이 잠깁니다. 작업의 Post API"},
	"es": {"Publicar una tarea bloquea el presupuesto", "al abrir la tarea se bloquea"},
}

// Per-locale markers that must appear in the credits-page budget
// wording (credits.use_budget, the "What credits are for" section).
var creditsBudgetCreationMarkers = map[string][]string{
	"en": {"locked from the balance when a task is created or funded"},
	"zh": {"创建或追加任务时从余额锁定积分"},
	"ja": {"タスクの作成・追加時に残高からロックされ"},
	"ko": {"작업을 만들거나 예산을 추가할 때 잔액에서 잠기며"},
	"es": {"los créditos se bloquean del saldo al crear o financiar una tarea"},
}

func TestTermsBudgetLockAuthorityAllLocales(t *testing.T) {
	for _, locale := range []string{"en", "zh", "ja", "ko", "es"} {
		t.Run(locale, func(t *testing.T) {
			s := i18n.T(locale, "terms.s2_b")
			if s == "" || s == "terms.s2_b" {
				t.Fatalf("terms.s2_b missing for %s", locale)
			}
			for _, want := range budgetLockCreationMarkers[locale] {
				if !strings.Contains(s, want) {
					t.Fatalf("%s terms.s2_b missing creation-lock marker %q — the budget-lock authority may have drifted back to publish/open-time. Actual: %s", locale, want, s)
				}
			}
			for _, banned := range budgetLockWrongAuthority[locale] {
				if strings.Contains(s, banned) {
					t.Fatalf("%s terms.s2_b contains banned publish/open-time lock phrasing %q — real authority is creation-time lock. Actual: %s", locale, banned, s)
				}
			}
		})
	}
}

func TestCreditsBudgetLockMatchesTermsAuthority(t *testing.T) {
	for _, locale := range []string{"en", "zh", "ja", "ko", "es"} {
		t.Run(locale, func(t *testing.T) {
			c := i18n.T(locale, "credits.use_budget")
			if c == "" || c == "credits.use_budget" {
				t.Fatalf("credits.use_budget missing for %s", locale)
			}
			// Credits must state creation-time lock too (WO-28: the fact
			// moved into the "What credits are for" section wording).
			for _, want := range creditsBudgetCreationMarkers[locale] {
				if !strings.Contains(c, want) {
					t.Fatalf("%s credits.use_budget lost creation-time lock marker %q: %s", locale, want, c)
				}
			}
			// And must NOT carry publish/open-time lock claims.
			for _, banned := range budgetLockWrongAuthority[locale] {
				if strings.Contains(c, banned) {
					t.Fatalf("%s credits note contains banned publish/open-time lock phrasing %q: %s", locale, banned, c)
				}
			}
		})
	}
}

// Rendered-HTML check: the /terms and /credits pages must actually
// emit the creation-time wording (not just carry it in the source).
func TestTermsCreditsRenderBudgetLockAuthority(t *testing.T) {
	s := storeTestServer(t)
	router := s.buildRouter()

	// /terms renders terms.s2_b (full creation-lock sentence);
	// /credits renders the "What credits are for" section (use_budget).
	// Both must state creation-time lock; neither may carry
	// publish/open-time lock claims.
	cases := []struct {
		path        string
		wantMarkers []string
	}{
		{"/terms", budgetLockCreationMarkers["en"]},
		{"/credits", creditsBudgetCreationMarkers["en"]},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", tc.path, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range tc.wantMarkers {
			if !strings.Contains(body, want) {
				t.Fatalf("%s rendered HTML missing creation-lock marker %q (en default locale)", tc.path, want)
			}
		}
		for _, banned := range budgetLockWrongAuthority["en"] {
			if strings.Contains(body, banned) {
				t.Fatalf("%s rendered HTML contains banned publish/open-time phrasing %q", tc.path, banned)
			}
		}
	}
}
