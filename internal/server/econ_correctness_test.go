package server

// Candidate-repair regression proofs (economic / owner correctness):
//  1. task create UI control flow reaches the POST on valid budget;
//  2. /api/key balance on the browser wire is a canonical string;
//  3. the shared canonical economic integer parser table;
//  4. registration returns the committed balance (66) and the genesis
//     ledger has exactly one grant_signup;
//  5. owner logs carry 2^53+1 amount/balance_after as strings.

import (
	"context"
	"strings"
	"testing"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/service"
)

// ---- 1. UI control-flow guard ------------------------------------------

// TestCreateSubmitValidPathReachesPOST proves bindCreateSubmit's VALID
// path flows to the requestJson('/api/owner/tasks', POST) call — the
// old unconditional trailing `return;` made the POST unreachable.
// Method: extract the bindCreateSubmit function body and verify the
// control-flow structure (budget guard is a BLOCK that returns, the
// POST call follows it at the same statement level), plus the request
// call text and the open_now assignment before it.
func TestCreateSubmitValidPathReachesPOST(t *testing.T) {
	src := readOwnerAsset(t, "render-tasks.js")
	body := extractJSFunc(t, src, "bindCreateSubmit")

	// The request call must exist in this function.
	postIdx := strings.Index(body, "requestJson('/api/owner/tasks', {method: 'POST'")
	if postIdx < 0 {
		t.Fatal("bindCreateSubmit must POST /api/owner/tasks")
	}
	// open_now is set before the POST on the valid path.
	openIdx := strings.Index(body, "data.open_now =")
	if openIdx < 0 || openIdx > postIdx {
		t.Fatal("open_now assignment must precede the POST (valid path)")
	}
	// The budget guard must be a braced block with its own return —
	// a single-line `if (...) return toast(); return;` shape is the
	// regression: the second return fires unconditionally. Lock the
	// FIXED shape: guard block containing showToast + return, closed
	// before open_now.
	guardOpen := strings.Index(body, "if (!/^-?(0|[1-9][0-9]*)$/")
	if guardOpen < 0 || guardOpen > openIdx {
		t.Fatal("budget canonical check must precede open_now")
	}
	guardClose := strings.Index(body[guardOpen:], "}")
	if guardClose < 0 || guardOpen+guardClose > openIdx {
		t.Fatal("budget guard block must close before the valid path continues")
	}
	// The regression shape was `return showToast(...); return;` — a
	// STATEMENT-LEVEL unconditional return right after the budget
	// guard, before open_now. Error-path `return;` inside braces
	// (e.g. `if (!json.success) { ...; return; }`) is legitimate.
	// Assert: no return statement appears BETWEEN the guard block
	// close and the open_now assignment.
	between := body[guardOpen+guardClose+1 : openIdx]
	for _, bad := range []string{"return"} {
		if strings.Contains(between, bad) {
			t.Fatalf("statement between budget guard and open_now aborts the valid path: %q", between)
		}
	}
	// Valid path calls requestJson inside try (delivery actually runs).
	if !strings.Contains(body[openIdx:], "requestJson('/api/owner/tasks'") {
		t.Fatal("POST not reachable after open_now assignment")
	}
}

func readOwnerAsset(t *testing.T, name string) string {
	t.Helper()
	return s61Read(t, "web/assets/owner/"+name)
}

func extractJSFunc(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "function "+name+"(")
	if i < 0 {
		t.Fatalf("function %s not found", name)
	}
	depth := 0
	start := -1
	for j := i; j < len(src); j++ {
		switch src[j] {
		case '{':
			if depth == 0 {
				start = j
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start >= 0 {
				return src[start:j]
			}
		}
	}
	t.Fatalf("unbalanced braces in %s", name)
	return ""
}

// ---- 2. /api/key wire ----------------------------------------------------

func TestKeyBalanceWireIsCanonicalString(t *testing.T) {
	e := newEconEnv(t, 9007199254740993) // 2^53+1
	rec := e.ownerGET(t, "/api/key")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	wire := decodeWireJSON(t, rec.Body.String())
	data, _ := wire["data"].(map[string]interface{})
	bal, ok := data["balance"].(string)
	if !ok {
		t.Fatalf("balance must be a canonical decimal string, got %T %v", data["balance"], data["balance"])
	}
	if bal != "9007199254740993" {
		t.Fatalf("balance = %q, want exactly 9007199254740993", bal)
	}
	if strings.Contains(rec.Body.String(), `"balance": 9007199254740993`) {
		t.Fatal("balance serialized as JSON number")
	}
}

// ---- 3. canonical parser table -------------------------------------------

func TestCanonicalEconIntTable(t *testing.T) {
	valid := map[string]int64{
		"0": 0, "1": 1, "66": 66,
		"9007199254740993":    9007199254740993,
		"9223372036854775807": 9223372036854775807,
		"-1":                  -1,
	}
	for in, want := range valid {
		got, err := parseCanonicalEconInt(in)
		if err != nil || got != want {
			t.Errorf("parseCanonicalEconInt(%q) = (%d, %v), want (%d, nil)", in, got, err, want)
		}
	}
	invalid := []string{
		"", " 1", "1 ", "+1", "01", "00", "-0", "1.0", "1e3",
		"0.0001", "9223372036854775808", "-", "--1", "1x", "٠١",
	}
	for _, in := range invalid {
		if got, err := parseCanonicalEconInt(in); err == nil {
			t.Errorf("parseCanonicalEconInt(%q) accepted (%d)", in, got)
		}
	}
	// Both boundary entry points share the ONE parser: identical
	// accept/reject on the table.
	for _, in := range invalid {
		if _, _, ok := parseCredits(in); ok {
			t.Errorf("parseCredits accepted non-canonical %q", in)
		}
		if _, ok := jsonCredits(in); ok {
			t.Errorf("jsonCredits accepted non-canonical %q", in)
		}
	}
	for in, want := range valid {
		if in == "-1" {
			continue // business sign rules are domain authority
		}
		if v, _, ok := parseCredits(in); !ok || v != want {
			t.Errorf("parseCredits(%q) = (%d,%v)", in, v, ok)
		}
		if v, ok := jsonCredits(in); !ok || v != want {
			t.Errorf("jsonCredits(%q) = (%d,%v)", in, v, ok)
		}
	}
}

// ---- 4. registration committed balance -----------------------------------

func TestRegistrationReturnsCommittedBalance(t *testing.T) {
	pool, err := pg.NewPool(testDatabaseURL(t))
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	suffix := nanoEconSuffix()
	name := "regbal_" + suffix
	res, rerr := service.Register(context.Background(), pool, name, "password123", "127.0.0.1")
	if rerr != nil {
		t.Fatalf("register: %v", rerr)
	}
	if res.Balance != 66 {
		t.Fatalf("response balance = %d, want committed 66", res.Balance)
	}
	// DB committed balance equals the response.
	var dbBal int64
	var botID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id, balance FROM tb_bots WHERE bot_name=$1`, name).Scan(&botID, &dbBal); err != nil || dbBal != 66 {
		t.Fatalf("db balance = %d (err %v), want 66", dbBal, err)
	}
	// Genesis ledger: exactly ONE grant_signup row for this bot.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_transactions WHERE bot_id=$1 AND type='grant_signup'`, botID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("grant_signup rows = %d (err %v), want exactly 1", n, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_transactions WHERE bot_id=$1`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_logs WHERE bot_id=$1`, botID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tb_bots WHERE id=$1`, botID)
	})
}

// ---- 5. owner logs exact fields -------------------------------------------

func TestOwnerLogsEconomicFieldsExactStrings(t *testing.T) {
	e := newEconEnv(t, 0)
	// Seed a REAL ledger row at 2^53+1 via the credits authority.
	if _, err := e.s.Pool.Exec(ctxBg(), `
		INSERT INTO tb_transactions (bot_id, type, amount, balance_after, ref_type, ref_id)
		VALUES ($1, 'earn_task', $2, $3, 'task', 'seed0000001')`,
		e.botID, int64(9007199254740993), int64(9007199254740993)); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
	rec := e.ownerGET(t, "/api/owner/logs?type=credits")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	wire := decodeWireJSON(t, rec.Body.String())
	data, _ := wire["data"].(map[string]interface{})
	items, _ := data["items"].([]interface{})
	found := false
	for _, it := range items {
		m, _ := it.(map[string]interface{})
		if m["ref_id"] == "seed0000001" {
			found = true
			amt, aok := m["amount"].(string)
			bal, bok := m["balance_after"].(string)
			if !aok || amt != "9007199254740993" {
				t.Fatalf("amount = %T %v, want canonical string 9007199254740993", m["amount"], m["amount"])
			}
			if !bok || bal != "9007199254740993" {
				t.Fatalf("balance_after = %T %v, want canonical string 9007199254740993", m["balance_after"], m["balance_after"])
			}
		}
	}
	if !found {
		t.Fatal("seeded ledger row not returned by /api/owner/logs")
	}
	if strings.Contains(rec.Body.String(), `"amount": 9007199254740993`) {
		t.Fatal("amount serialized as JSON number")
	}
}
