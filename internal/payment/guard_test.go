package payment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPaymentDoesNotMutateCreditsTables is a source guard: the payment domain
// must never write tb_bots.balance or insert into tb_transactions — the only
// grant path is credits.Record (which locks, updates, and inserts inside the
// payment transaction).
func TestPaymentDoesNotMutateCreditsTables(t *testing.T) {
	root := "."
	banned := []string{
		"UPDATE tb_bots",
		"INSERT INTO tb_transactions",
	}
	files, _ := filepath.Glob(filepath.Join(root, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		src := string(data)
		for _, b := range banned {
			if strings.Contains(src, b) {
				t.Fatalf("%s contains %s — payment must not touch credits tables", f, b)
			}
		}
	}
}

// TestPaymentDoesNotImportForbiddenDomains guards the boundary: payment may
// not import Storage/Task/Store-style domains (nor the HTTP server), and
// only reaches credits via the credits.Record call in CompletePayment.
func TestPaymentDoesNotImportForbiddenDomains(t *testing.T) {
	bannedImports := []string{
		"kungfu.md/internal/service",
		"kungfu.md/internal/server",
		"kungfu.md/internal/delivery",
		"kungfu.md/internal/storage",
		"kungfu.md/internal/task",
		"kungfu.md/internal/store",
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
				t.Fatalf("%s imports %s — payment must stay decoupled", f, imp)
			}
		}
	}
}
