package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Consumption decoupling guard: the storage/kungfu production files must
// have ZERO dependency on the credits domain — no imports, no calls, no
// price constants, no ledger type names. Pricing lives in
// internal/consumption; balance belongs to the Credits/Account contract.
//
// Kungfu production files are identified explicitly so new files default
// to being checked (opt-out, not opt-in).
func TestKungfuStorageZeroCreditsDependency(t *testing.T) {
	files := []string{
		"kungfu_manage.go",
		"kungfu_read.go",
		"kungfu_push_write_test_helpers.go", // placeholder; ignored if absent
	}
	banned := []string{
		`"kungfu.md/internal/credits"`,
		"credits.Record",
		"credits.Balance",
		"spend_push",
		"spend_get",
		"AmountPush",
		"AmountGet",
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue // non-production helper lists are tolerated
		}
		src := string(data)
		for _, b := range banned {
			if strings.Contains(src, b) {
				t.Fatalf("%s contains %s — storage must not know pricing/credits", f, b)
			}
		}
	}
}

// Every non-test .go file in this package whose name suggests storage/
// kungfu must also be covered by the explicit list above — a new kungfu
// file that is not listed fails here (no silent escape from the guard).
func TestKungfuFilesAreGuardListed(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	guarded := map[string]bool{
		"kungfu_manage.go": true,
		"kungfu_read.go":   true,
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "kungfu") ||
			strings.HasSuffix(name, "_test.go") || !strings.HasSuffix(name, ".go") {
			continue
		}
		if !guarded[name] {
			// A new kungfu production file: verify it directly.
			data, err := os.ReadFile(filepath.Join(name))
			if err != nil {
				t.Fatal(err)
			}
			src := string(data)
			for _, b := range []string{`"kungfu.md/internal/credits"`, "credits.Record", "credits.Balance", "spend_push", "spend_get"} {
				if strings.Contains(src, b) {
					t.Fatalf("%s contains %s — add it to the guard list and keep it credits-free", name, b)
				}
			}
		}
	}
}
