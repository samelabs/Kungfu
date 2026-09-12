package credits

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBalanceMutationOnlyInCreditsDomain is a lightweight source guard: no Go
// file outside internal/credits may write tb_bots.balance or insert into
// tb_transactions. Test files are exempt (they may seed/cleanup data).
func TestBalanceMutationOnlyInCreditsDomain(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	banned := []string{
		"UPDATE tb_bots SET balance",
		"INSERT INTO tb_transactions",
	}
	violations := []string{}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, "credits") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		src := string(data)
		for _, b := range banned {
			if strings.Contains(src, b) {
				violations = append(violations, rel+": "+b)
			}
		}
		return nil
	})
	if len(violations) > 0 {
		t.Fatalf("balance/ledger SQL outside internal/credits:\n%s", strings.Join(violations, "\n"))
	}
}

// TestCreditsDoesNotImportBusinessDomains guards the dependency direction:
// internal/credits must not import task/kungfu/registration/server packages.
func TestCreditsDoesNotImportBusinessDomains(t *testing.T) {
	bannedImports := []string{
		"kungfu.md/internal/service",
		"kungfu.md/internal/server",
		"kungfu.md/internal/repository",
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
				t.Fatalf("%s imports %s — credits must not depend on business domains", f, imp)
			}
		}
	}
}
