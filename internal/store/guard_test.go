package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStoreDoesNotMutateCreditsTables is a source guard: the store domain
// must never write tb_bots.balance or insert into tb_transactions — every
// credit movement goes through credits.Record.
func TestStoreDoesNotMutateCreditsTables(t *testing.T) {
	banned := []string{
		"UPDATE tb_bots",
		"INSERT INTO tb_transactions",
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, b := range banned {
			if strings.Contains(string(data), b) {
				t.Fatalf("%s contains %s — store must not touch credits tables", f, b)
			}
		}
	}
}

// TestStoreDoesNotImportForbiddenDomains guards the boundary: store may
// not import payment/task/work/storage/kungfu service domains nor the
// HTTP server; credits stays the only credit-mutation path.
func TestStoreDoesNotImportForbiddenDomains(t *testing.T) {
	bannedImports := []string{
		"kungfu.md/internal/payment",
		"kungfu.md/internal/service",
		"kungfu.md/internal/server",
		"kungfu.md/internal/delivery",
		"kungfu.md/internal/storage",
		"kungfu.md/internal/task",
		"kungfu.md/internal/store", // self (would mean a sub-package split)
	}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, imp := range bannedImports {
			if strings.Contains(string(data), "\""+imp+"\"") {
				t.Fatalf("%s imports %s — store must stay decoupled", f, imp)
			}
		}
	}
}

// TestCreditsDoesNotImportStore guards the reverse direction: credits
// must never depend on the store domain.
func TestCreditsDoesNotImportStore(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("..", "credits", "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "\"kungfu.md/internal/store\"") {
			t.Fatalf("%s imports internal/store — credits must not depend on store", f)
		}
	}
}
