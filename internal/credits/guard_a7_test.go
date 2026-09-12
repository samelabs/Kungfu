package credits

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoServiceRecordAliasLayer: internal/service/transaction.go must not
// reappear as a credits alias/compat layer.
func TestNoServiceRecordAliasLayer(t *testing.T) {
	if _, err := os.Stat(filepath.Join("..", "service", "transaction.go")); err == nil {
		t.Fatal("internal/service/transaction.go exists — credits alias layer must not return")
	}
}

// TestCreditsMutationImportsDirect: every production file that calls
// credits.Record/credits.Balance (outside internal/credits) must import
// internal/credits — no indirect alias indirection.
func TestCreditsMutationImportsDirect(t *testing.T) {
	root := filepath.Join("..", "..", "internal")
	var offenders []string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		src := string(data)
		if strings.Contains(src, "credits.Record(") || strings.Contains(src, "credits.Balance(") {
			if !strings.Contains(src, `"kungfu.md/internal/credits"`) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel)
			}
		}
		// No package-level aliases re-exporting the credits primitives
		// (a local `x, err := credits.Balance(...)` call is fine; a top-level
		// `var Record = credits.Record` is the compat layer we banned).
		for _, line := range strings.Split(src, "\n") {
			lt := strings.TrimSpace(line)
			if (strings.HasPrefix(lt, "var ") || strings.Contains(lt, "\tRecord ") || strings.Contains(lt, "\tGetBalance ")) &&
				(strings.HasSuffix(lt, "= credits.Record") || strings.HasSuffix(lt, "= credits.Balance")) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+" (alias assignment: "+lt+")")
			}
		}
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("credits usage without direct import / alias indirection:\n%s", strings.Join(offenders, "\n"))
	}
}

// TestIdentityRepositoryNotBalanceAuthority: identity/auth queries in
// repository/bot.go must not SELECT the balance column.
func TestIdentityRepositoryNotBalanceAuthority(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "repository", "bot.go"))
	if err != nil {
		t.Fatal(err)
	}
	// crude but effective: no SELECT may name the balance column in bot.go
	// (tb_bots identity queries). Credits owns balance reads/writes.
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "select") && strings.Contains(lower, "balance") {
			t.Fatalf("repository/bot.go identity query selects balance: %s", trimmed)
		}
	}
}
