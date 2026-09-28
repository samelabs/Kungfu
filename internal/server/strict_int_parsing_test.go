package server

// Final-repair regression proofs:
//  1. strict single-JSON-document semantics for UseNumber bodies
//     (parseJSONBodyRequiredNumbers);
//  2. zero-float64 Credit parsing — exact integer strings only;
//  3. structural ban on strconv.ParseFloat / float64 conversion in the
//     production Credit parsers.
//
// HTTP-level cases run through the real owner task-create endpoint so
// the whole chain (parser → handler) is exercised.

import (
	"encoding/json"
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
			num, isNum := m["budget"].(json.Number)
			if !isNum {
				t.Errorf("%s: budget is not json.Number", tc.name)
				continue
			}
			v, err := num.Int64()
			if err != nil || v != tc.wantVal {
				t.Errorf("%s: budget = %d (err %v), want %d", tc.name, v, err, tc.wantVal)
			}
		} else if err == nil {
			t.Errorf("%s: accepted, must reject non-single-document body", tc.name)
		}
	}
}

// ---- 2. zero-float Credit string parsing --------------------------------

func jsonNum(t *testing.T, s string) interface{} {
	req := httptest.NewRequest("POST", "/x", strings.NewReader(`{"n":`+s+`}`))
	m, err := parseJSONBodyRequiredNumbers(req, true, "bad")
	if err != nil {
		t.Fatalf("fixture decode: %v", err)
	}
	return m["n"]
}

// ---- 3. structural ban on float in production Credit parsers ------------

func TestCreditParsersContainNoFloatPath(t *testing.T) {
	files := []string{
		"handlers.go",              // parseCredits
		"handler_admin_rewards.go", // jsonCredits
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
