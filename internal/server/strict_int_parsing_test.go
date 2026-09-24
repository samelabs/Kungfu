package server

// Final-repair regression proofs:
//  1. strict single-JSON-document semantics for UseNumber bodies
//     (parseJSONBodyRequiredNumbers);
//  2. zero-float64 Credit parsing — exact integer strings only;
//  3. structural ban on strconv.ParseFloat / float64 conversion in the
//     production Credit parsers.
//
// HTTP-level cases run through the real owner task-create endpoint so
// the whole chain (parser → parseCredits → handler) is exercised.

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---- 1. strict single-document semantics -------------------------------

func TestNumbersParserStrictSingleDocument(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantOK  bool
		wantVal int64 // budget value when ok
	}{
		{"single object", `{"budget":1000}`, true, 1000},
		{"object + trailing whitespace", "{\"budget\":1000}\n\t ", true, 1000},
		{"object + second object", `{"budget":1000} {"budget":2000}`, false, 0},
		{"object + trailing scalar", `{"budget":1000} 42`, false, 0},
		{"object + trailing garbage", `{"budget":1000} ,,,`, false, 0},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(tc.body))
		m, err := parseJSONBodyRequiredNumbers(req, true, "Request body must be valid JSON object")
		if tc.wantOK {
			if err != nil {
				t.Errorf("%s: rejected: %v", tc.name, err)
				continue
			}
			v, present, ok := parseCredits(m["budget"])
			if !present || !ok || v != tc.wantVal {
				t.Errorf("%s: budget = %d (present %v ok %v), want %d", tc.name, v, present, ok, tc.wantVal)
			}
		} else if err == nil {
			t.Errorf("%s: accepted, must reject non-single-document body", tc.name)
		}
	}
}

// ---- 2. zero-float Credit string parsing --------------------------------

func TestParseCreditsExactIntegerStringsOnly(t *testing.T) {
	cases := []struct {
		in      interface{}
		want    int64
		present bool
		ok      bool
	}{
		{"1000", 1000, true, true},
		{"9007199254740993", 9007199254740993, true, true},       // 2^53+1 exact
		{"9223372036854775807", 9223372036854775807, true, true}, // MaxInt64 exact parse; rules reject later
		{"9223372036854775808", 0, true, false},                  // 2^63 reject
		{"1000.5", 0, true, false},
		{"0.0001", 0, true, false},
		{"9007199254740993.0", 0, true, false}, // integral presentation still rejected — never float-converted
		{"", 0, true, false},
		{" 42 ", 0, true, false}, // non-canonical: server never trims wire input (browser trims once as UX before sending)
		{nil, 0, false, true},
		{jsonNum(t, "9007199254740993"), 9007199254740993, true, true},
		{jsonNum(t, "1000.5"), 0, true, false},
		{jsonNum(t, "9223372036854775808"), 0, true, false},
		{42.5, 0, true, false}, // float64 input no longer has an accept path
	}
	for _, tc := range cases {
		v, present, ok := parseCredits(tc.in)
		if v != tc.want || present != tc.present || ok != tc.ok {
			t.Errorf("parseCredits(%v) = (%d,%v,%v), want (%d,%v,%v)",
				tc.in, v, present, ok, tc.want, tc.present, tc.ok)
		}
	}
}

func jsonNum(t *testing.T, s string) interface{} {
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"n":`+s+`}`))
	m, err := parseJSONBodyRequiredNumbers(req, true, "bad")
	if err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	return m["n"]
}

// HTTP end-to-end: a canonical integer STRING for budget is accepted
// exactly; an integral-decimal string is rejected without mutation.
func TestOwnerTaskBudgetStringExact(t *testing.T) {
	e := newEconEnv(t, 5000)
	rec, _ := e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"S","requirements":"r","postapi":"https://example.com/h","budget":"1500","price":10,"open_now":false}`)
	if rec.Code != 200 {
		t.Fatalf("canonical integer string budget rejected: %d %s", rec.Code, rec.Body.String())
	}
	var dbBudget int64
	if err := e.s.Pool.QueryRow(ctxBg(),
		`SELECT budget FROM tb_tasks WHERE bot_id=$1 ORDER BY id DESC LIMIT 1`, e.botID).
		Scan(&dbBudget); err != nil || dbBudget != 1500 {
		t.Fatalf("db budget = %d (err %v), want 1500", dbBudget, err)
	}
	before := e.countTasks(t)
	rec, _ = e.ownerPOST(t, "/api/owner/tasks",
		`{"title":"S","requirements":"r","postapi":"https://example.com/h","budget":"1500.0","price":10,"open_now":false}`)
	if rec.Code == 200 {
		t.Fatal("integral-decimal string accepted — must be rejected (never float-converted)")
	}
	if after := e.countTasks(t); after != before {
		t.Fatalf("mutation on rejected string budget: %d -> %d", before, after)
	}
}

// ---- 3. structural ban on float in production Credit parsers ------------

func TestCreditParsersContainNoFloatPath(t *testing.T) {
	files := []string{
		"handlers.go",            // parseCredits
		"handler_admin_store.go", // jsonCredits
	}
	for _, f := range files {
		path := filepath.Join(".", f)
		srcB, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(srcB)
		// isolate the parser functions (balanced-brace extraction)
		for _, fn := range []string{"parseCredits", "jsonCredits"} {
			body := extractFuncBody(t, src, fn)
			if body == "" {
				continue // not in this file
			}
			for _, banned := range []string{"ParseFloat", "float64("} {
				if strings.Contains(body, banned) {
					t.Errorf("%s: %s() contains %q — Credit values must never pass through float64", f, fn, banned)
				}
			}
			if strings.Contains(body, "case float64") {
				t.Errorf("%s: %s() still has a float64 accept case", f, fn)
			}
		}
	}
}

func extractFuncBody(t *testing.T, src, fn string) string {
	t.Helper()
	needle := "func " + fn + "("
	i := strings.Index(src, needle)
	if i < 0 {
		return ""
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
	t.Fatalf("unbalanced braces extracting %s", fn)
	return ""
}

var _ = strconv.Itoa // keep strconv import if cases change
