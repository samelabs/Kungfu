package admin

// 012 Finance Admin architecture guards.
//
// Locked :
//   • Admin finance handlers must not import repository / credits /
//     payment (server → admin → repository pipeline).
//   • The Finance admin domain must not import payment / credits
//     (no second policy, no mutation authority reuse).
//   • Finance repository SQL must be read-only: no INSERT / UPDATE /
//     DELETE / SET balance anywhere in finance_admin.go.
//   • Finance code must never call credits.Record /
//     credits.RecordAuthoritativeReversal.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func finRoot(t *testing.T) string {
	t.Helper()
	return repoRoot(t)
}

func finProductionFiles(t *testing.T, rel string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(filepath.Join(finRoot(t), rel), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

// Finance admin domain + handlers: forbidden imports.
func TestFinanceAdminFinanceDependencyDirection(t *testing.T) {
	forbidden := map[string][]string{
		"internal/server/handler_admin_finance.go": {
			"kungfu.md/internal/repository",
			"kungfu.md/internal/credits",
			"kungfu.md/internal/payment",
		},
		"internal/admin/finance.go": {
			"kungfu.md/internal/payment",
			"kungfu.md/internal/credits",
		},
	}
	for rel, bans := range forbidden {
		src, err := os.ReadFile(filepath.Join(finRoot(t), rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, ban := range bans {
			if strings.Contains(string(src), "\""+ban+"\"") {
				t.Errorf("%s must not import %s", rel, ban)
			}
		}
	}
}

// Finance repository source must contain no write statements.
func TestFinanceAdminFinanceRepositoryReadOnlySQL(t *testing.T) {
	path := filepath.Join(finRoot(t), "internal", "repository", "finance_admin.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		v := strings.ToUpper(bl.Value)
		for _, banned := range []string{
			"INSERT INTO", "UPDATE ", "DELETE FROM", "SET BALANCE",
			"GRANT ", "REVOKE ",
		} {
			if strings.Contains(v, banned) {
				t.Errorf("finance_admin.go contains write SQL %q in %s", banned, bl.Value)
			}
		}
		return true
	})
}

// Finance admin domain + handlers must never call the Credits
// mutation entry points.
func TestFinanceAdminFinanceNeverCallsCreditsMutation(t *testing.T) {
	for _, rel := range []string{
		"internal/admin/finance.go",
		"internal/server/handler_admin_finance.go",
		"internal/repository/finance_admin.go",
	} {
		src, err := os.ReadFile(filepath.Join(finRoot(t), rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, call := range []string{"credits.Record", "RecordAuthoritativeReversal"} {
			if strings.Contains(string(src), call) {
				t.Errorf("%s references %s — Finance Admin must never mutate Credits", rel, call)
			}
		}
	}
}
