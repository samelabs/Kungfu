package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// AST-based SQL literal detector (B1.1 guard repair).
//
// The previous guards matched raw source lines, which both missed
// multiline SQL and could false-positive on comments. This detector
// parses each production .go file with go/parser and inspects ONLY
// actual string literals (interpreted "..." and raw `...`),
// unquoting them via strconv.Unquote / raw-content extraction —
// comments and identifiers are invisible to it by construction.

// adminTables are the tb_admin* tables owned by the repository layer.
var adminTables = []string{
	"tb_admins", "tb_admin_sessions", "tb_admin_roles", "tb_admin_permissions",
	"tb_admin_user_roles", "tb_admin_role_permissions", "tb_admin_audit_logs",
}

// sqlStrongKeywords: any ONE of these alone marks SQL context.
var sqlStrongKeywords = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE",
	"ALTER", "CREATE", "DROP", "TRUNCATE",
}

// sqlWeakKeywords: contextual words (FROM/JOIN/INTO/TABLE/LOCK) that
// appear in SQL but also in ordinary prose ("the tb_admins table is
// owned by..."). They mark SQL context only when at least TWO SQL
// words co-occur, which prose essentially never does while real SQL
// always does (FROM x JOIN y, LOCK TABLE x, INSERT INTO x ...).
var sqlWeakKeywords = []string{"FROM", "JOIN", "INTO", "TABLE", "LOCK"}

// hasSQLContext reports whether the literal carries SQL operation
// context: any strong keyword alone, or two or more SQL words total.
func hasSQLContext(lit string) bool {
	up := strings.ToUpper(lit)
	for _, kw := range sqlStrongKeywords {
		if sqlHasWord(up, kw) {
			return true
		}
	}
	total := 0
	for _, kw := range sqlStrongKeywords {
		if sqlHasWord(up, kw) {
			total++
		}
	}
	for _, kw := range sqlWeakKeywords {
		if sqlHasWord(up, kw) {
			total++
		}
	}
	return total >= 2
}

// sqlViolation describes one detected literal.
type sqlViolation struct {
	Path     string
	Position string // file:line
	Literal  string // possibly truncated for the message
	Table    string
}

// containsAdminTable returns the first admin table referenced by the
// literal (case-insensitive, with non-word boundaries respected via
// word-boundary matching).
func containsAdminTable(lit string) string {
	up := strings.ToUpper(lit)
	for _, table := range adminTables {
		if sqlHasWord(up, table) {
			return table
		}
	}
	return ""
}

// sqlHasWord reports whether up (already uppercased) contains word
// (given in ANY case; uppercased here) as a whole word (bounded by
// non-word characters).
func sqlHasWord(up, word string) bool {
	w := strings.ToUpper(word)
	idx := 0
	for {
		i := strings.Index(up[idx:], w)
		if i < 0 {
			return false
		}
		at := idx + i
		end := at + len(w)
		beforeOK := at == 0 || !isWordChar(up[at-1])
		afterOK := end >= len(up) || !isWordChar(up[end])
		if beforeOK && afterOK {
			return true
		}
		idx = at + len(w)
	}
}

func isWordChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// detectAdminSQLInSource parses Go source and returns violations for
// string literals that reference a tb_admin* table in SQL context.
func detectAdminSQLInSource(path, src string) []sqlViolation {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		// A file that does not parse cannot be vetted; surface loudly.
		return []sqlViolation{{Path: path, Position: "parse error", Literal: err.Error()}}
	}
	var out []sqlViolation
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		lit := unquoteBasicLit(bl)
		if lit == "" {
			return true
		}
		table := containsAdminTable(lit)
		if table == "" {
			return true
		}
		if hasSQLContext(lit) {
			pos := fset.Position(bl.Pos())
			shown := lit
			if len(shown) > 120 {
				shown = shown[:120] + "..."
			}
			out = append(out, sqlViolation{
				Path:     path,
				Position: pos.String(),
				Literal:  shown,
				Table:    table,
			})
		}
		return true
	})
	return out
}

// unquoteBasicLit decodes both interpreted ("...") and raw (`...`)
// string literals to their true content.
func unquoteBasicLit(bl *ast.BasicLit) string {
	if len(bl.Value) >= 6 && strings.HasPrefix(bl.Value, "`") && strings.HasSuffix(bl.Value, "`") {
		return bl.Value[1 : len(bl.Value)-1]
	}
	s, err := strconv.Unquote(bl.Value)
	if err != nil {
		// e.g. strings with special escapes we still want to see:
		// fall back to the raw value text
		return bl.Value
	}
	return s
}

// collectProductionGoFiles lists non-test .go files under dir.
func collectProductionGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if strings.HasPrefix(info.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// detectAdminSQLInDirs runs the detector over every production .go
// file under the given directories.
func detectAdminSQLInDirs(t *testing.T, dirs ...string) []sqlViolation {
	t.Helper()
	var violations []sqlViolation
	for _, dir := range dirs {
		for _, path := range collectProductionGoFiles(t, dir) {
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			violations = append(violations, detectAdminSQLInSource(path, string(src))...)
		}
	}
	return violations
}

// Guard: SQL against tb_admin* tables lives ONLY in
// internal/repository/admin.go (outside the scanned dirs) or in
// migrations (not Go). internal/admin, internal/server, and
// cmd/adminctl production sources must contain none.
func TestAdminSQLLivesOnlyInRepository(t *testing.T) {
	root := repoRoot(t)
	violations := detectAdminSQLInDirs(t,
		filepath.Join(root, "internal", "admin"),
		filepath.Join(root, "internal", "server"),
		filepath.Join(root, "cmd", "adminctl"),
	)
	for _, v := range violations {
		t.Errorf("%s: admin-table SQL (%s) must live only in internal/repository/admin.go: %q",
			v.Position, v.Table, v.Literal)
	}
}

// Guard: tb_admin_audit_logs is append-only in PRODUCTION Go source
// everywhere — including internal/repository/admin.go, the sole legal
// owner of admin SQL, which may only ever INSERT/SELECT the audit
// table. UPDATE/DELETE/TRUNCATE against it fail wherever they appear.
// (FK ON DELETE SET NULL is DB-internal behavior driven by the
// migration, not production Go SQL, and does not conflict.)
func TestAdminAuditLogsAppendOnlyInProductionCode(t *testing.T) {
	root := repoRoot(t)

	auditLiterals := func(path, src string) []sqlViolation {
		var out []sqlViolation
		for _, v := range detectAdminSQLInSource(path, src) {
			if v.Table == "tb_admin_audit_logs" {
				out = append(out, v)
			}
		}
		return out
	}

	bannedOps := []string{"UPDATE", "DELETE", "TRUNCATE", "DROP", "ALTER"}
	isBanned := func(lit string) (string, bool) {
		up := strings.ToUpper(lit)
		for _, op := range bannedOps {
			if sqlHasWord(up, op) {
				return op, true
			}
		}
		return "", false
	}

	// every production Go file in the repo (all packages)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, v := range auditLiterals(path, string(src)) {
			if op, bad := isBanned(v.Literal); bad {
				t.Errorf("%s: append-only violation — %s against tb_admin_audit_logs is forbidden in production code: %q",
					v.Position, op, v.Literal)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// -- Negative/positive fixtures for the detector itself --
// Prove the detector catches real shapes (including multiline raw
// strings) and does not fire on benign strings or comments.

func TestSQLLiteralDetectorSyntheticFixtures(t *testing.T) {
	mustDetect := []struct {
		name   string
		source string
		table  string
	}{
		{
			name:   "inline update",
			source: `q := "UPDATE tb_admins SET status='disabled' WHERE id=$1"`,
			table:  "tb_admins",
		},
		{
			name:   "multiline raw update",
			source: "q := `\nUPDATE\n    tb_admins\nSET status='disabled'\nWHERE id = $1\n`",
			table:  "tb_admins",
		},
		{
			name:   "multiline raw select",
			source: "q := `\nSELECT id\nFROM tb_admin_sessions\nWHERE token_hash=$1\n`",
			table:  "tb_admin_sessions",
		},
		{
			name:   "inline delete",
			source: "q := \"DELETE FROM tb_admin_roles WHERE id = $1\"",
			table:  "tb_admin_roles",
		},
		{
			name:   "lock table",
			source: "q := `LOCK TABLE tb_admins IN EXCLUSIVE MODE`",
			table:  "tb_admins",
		},
		{
			name:   "insert into",
			source: "q := `INSERT INTO tb_admin_user_roles (admin_id, role_id) VALUES ($1,$2)`",
			table:  "tb_admin_user_roles",
		},
		{
			name:   "audit table update",
			source: "q := `UPDATE tb_admin_audit_logs SET actor_username='x'`",
			table:  "tb_admin_audit_logs",
		},
		{
			name:   "audit table delete multiline",
			source: "q := `\nDELETE FROM\n  tb_admin_audit_logs\n`",
			table:  "tb_admin_audit_logs",
		},
	}

	for _, tc := range mustDetect {
		t.Run("detect/"+tc.name, func(t *testing.T) {
			src := "package fixtures\n\nfunc f() {\n\tvar q string\n\t_ = q\n\t" + tc.source + "\n}\n"
			vs := detectAdminSQLInSource("fixtures.go", src)
			if len(vs) == 0 {
				t.Fatalf("detector MISSED %s: %s", tc.name, tc.source)
			}
			if vs[0].Table != tc.table {
				t.Fatalf("detector found table %s, want %s", vs[0].Table, tc.table)
			}
		})
	}

	mustNotDetect := []struct {
		name   string
		source string
	}{
		{
			name:   "bare table name string",
			source: `name := "tb_admins"`,
		},
		{
			name:   "table name in prose",
			source: `msg := "the tb_admins table is owned by the repository"`,
		},
		{
			name:   "identifier-looking func name",
			source: `fn := "repository.FindAdminByUsername"`,
		},
		{
			name:   "sql without admin table",
			source: "q := `UPDATE tb_bots SET status='disabled'`",
		},
		{
			name:   "substring not word",
			source: "q := `UPDATE xtb_admins_y SET a=1`",
		},
		{
			name: "comment mentioning sql and table",
			source: "// UPDATE tb_admins SET status='disabled'\n" +
				"q := `SELECT 1`",
		},
	}

	for _, tc := range mustNotDetect {
		t.Run("ignore/"+tc.name, func(t *testing.T) {
			src := "package fixtures\n\nfunc f() {\n\t" + tc.source + "\n\t_ = q\n" +
				"\tvar name, msg, fn string\n\t_, _, _ = name, msg, fn\n}\n"
			vs := detectAdminSQLInSource("fixtures.go", src)
			if len(vs) != 0 {
				t.Fatalf("detector FALSE POSITIVE on %s: %+v", tc.name, vs)
			}
		})
	}
}

// The detector must also flag the append-only violation inside the
// sole legal SQL owner (repository) when it touches the audit table
// destructively.
func TestSQLLiteralDetectorFlagsRepositoryAuditDelete(t *testing.T) {
	src := "package repository\n\nfunc f() {\n\tq := \"DELETE FROM tb_admin_audit_logs\"\n\t_ = q\n}\n"
	vs := detectAdminSQLInSource("admin.go", src)
	if len(vs) != 1 || vs[0].Table != "tb_admin_audit_logs" {
		t.Fatalf("repository-scope detection failed: %+v", vs)
	}
	up := strings.ToUpper(vs[0].Literal)
	if !sqlHasWord(up, "DELETE") {
		t.Fatal("DELETE operation not recognized")
	}
}
