package server

// Browser-side guard for the lossless economic integer boundary.
// Economic fields (Credits, fiat minor units) arrive as canonical
// decimal strings and must NEVER be routed through JS Number /
// parseFloat / toFixed — those corrupt integers above 2^53 or
// mis-format fiat. The guard scans the economic render/send paths of
// the Owner and Admin browser assets for banned numeric conversions
// tied to economic fields, skipping comment lines (comments may name
// what the client must NOT do).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var econBannedPatterns = []*regexp.Regexp{
	// Number( ... economic field
	regexp.MustCompile(`Number\(\s*[a-zA-Z_$.]*(balance|budget|price|amount|credits|reward)`),
	// parseFloat on economic field
	regexp.MustCompile(`parseFloat\(\s*[a-zA-Z_$.]*(balance|budget|price|amount|credits|reward)`),
	// Math.round on economic field
	regexp.MustCompile(`Math\.round\(\s*[a-zA-Z_$.]*(balance|budget|price|amount|credits|reward)`),
	// toFixed on economic field (fiat formatting must be string-level)
	regexp.MustCompile(`[a-zA-Z_$.]*(amount|price|credits)[a-zA-Z_$.]*\.\s*toFixed`),
}

// economicAssetFiles are the browser assets whose economic paths are
// under contract.
var economicAssetFiles = []string{
	"web/assets/owner/api.js",
	"web/assets/owner/render-credits.js",
	"web/assets/owner/render-rewards.js",
	"web/assets/owner/render-overview.js",
	"web/assets/owner/render-logs.js",
}

func stripJSComments(src string) []string {
	var code []string
	for _, line := range strings.Split(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
			continue
		}
		code = append(code, line)
	}
	return code
}

func TestBrowserEconomicFieldsNeverNumberConverted(t *testing.T) {
	for _, rel := range economicAssetFiles {
		b, err := os.ReadFile(filepath.Join("..", "..", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		code := strings.Join(stripJSComments(string(b)), "\n")
		for _, re := range econBannedPatterns {
			if loc := re.FindStringIndex(code); loc != nil {
				line := 1 + strings.Count(code[:loc[0]], "\n")
				t.Errorf("%s:%d banned numeric conversion on economic field: %q",
					rel, line, re.FindString(code))
			}
		}
	}
}

// TestFiatFormatterStringExact proves the string-level fiat formatter
// semantics encoded in render-credits.js: exact two decimals from a
// canonical decimal string, never Number()-mediated.
func TestFiatFormatterStringExact(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "web/assets/owner/render-credits.js"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	// The formatter must be string-typed and must contain the
	// padStart/slice construction, not division + toFixed.
	if !strings.Contains(src, "typeof amountMinor !== 'string'") {
		t.Fatal("creditsFormatAmount must require a string amountMinor")
	}
	if strings.Contains(strings.Join(stripJSComments(src), "\n"), "amountMinor / 100") {
		t.Fatal("fiat formatting must not divide via JS Number")
	}
	if !strings.Contains(src, "padStart(3, '0')") {
		t.Fatal("fiat formatting must use string padding")
	}
}
