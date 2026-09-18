package pg

// R2.1 repair proof: the authoritative Rollback cleanup primitive
// reuses the connection under ordinary request cancellation — a
// MaxConns=1 pool keeps the SAME backend PID across cancel+rollback.

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rollbackTestURL(t *testing.T) string {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	return url
}

func backendPID(t *testing.T, q Querier) uint32 {
	t.Helper()
	var pid uint32
	if err := q.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

func TestRollbackCleanupReusesConnectionAfterCancel(t *testing.T) {
	// exactly ONE connection: any replacement would show as a PID change
	pool, err := NewPoolMaxConns(rollbackTestURL(t), 1)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	defer pool.Close()
	ctxBG := context.Background()

	// PID A on the single connection
	pidA := backendPID(t, pool)

	// a transaction with an uncommitted write (holds locks)
	reqCtx, cancelReq := context.WithCancel(ctxBG)
	tx, err := pool.TxBegin(reqCtx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(reqCtx,
		`CREATE TABLE IF NOT EXISTS r21_rollback_probe (id int)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(reqCtx, `INSERT INTO r21_rollback_probe VALUES (1)`); err != nil {
		t.Fatal(err)
	}

	// the request is cancelled (deadline/cancel occurs)
	cancelReq()

	// the authoritative cleanup primitive — NOT the request ctx
	if err := Rollback(tx); err != nil {
		t.Fatalf("Rollback under cancelled request ctx returned %v — connection destroyed instead of reused", err)
	}

	// transaction facts absent
	var n int
	_ = pool.QueryRow(ctxBG, `SELECT COUNT(*) FROM r21_rollback_probe`).Scan(&n)
	if n != 0 {
		t.Fatalf("uncommitted facts survived: %d rows", n)
	}
	_, _ = pool.Exec(ctxBG, `DROP TABLE IF EXISTS r21_rollback_probe`)

	// the SAME single connection still serves: PID must be unchanged
	pidB := backendPID(t, pool)
	if pidA != pidB {
		t.Fatalf("backend PID changed %d -> %d — cleanup relied on connection destruction, not reuse", pidA, pidB)
	}

	// and the pool still serves a fresh transaction (one properly
	// bounded context owns the whole proof — cancel always deferred)
	freshCtx, freshCancel := context.WithTimeout(ctxBG, 3*time.Second)
	defer freshCancel()
	tx2, err := pool.TxBegin(freshCtx)
	if err != nil {
		t.Fatalf("fresh transaction failed after cleanup: %v", err)
	}
	if _, err := tx2.Exec(freshCtx, `SELECT 1`); err != nil {
		_ = Rollback(tx2)
		t.Fatalf("fresh transaction work failed: %v", err)
	}
	if err := tx2.Commit(freshCtx); err != nil {
		t.Fatal(err)
	}
}

// ===========================================================================
// Architecture guard (AST): production transaction rollback ownership
// lives ONLY in internal/pg. ANY selector call *.Rollback(...) on ANY
// receiver in ANY production .go file outside internal/pg is a
// violation — no receiver-name heuristics. Scans from the repo root
// (cmd/ included); excludes _test.go, internal/pg/**, and .git/**.
// ===========================================================================

func TestRollbackOwnedOnlyByPG(t *testing.T) {
	root, err := os.Getwd() // internal/pg
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(filepath.Dir(root)) // repo root

	pgDir := filepath.Join(root, "internal", "pg")
	violations := []string{}
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir // .git and friends
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(path, pgDir+string(filepath.Separator)) {
			return nil // pg itself owns the primitive
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			violations = append(violations, path+": parse error "+perr.Error())
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Rollback" {
				return true
			}
			// The SANCTIONED form is the package-qualified primitive
			// pg.Rollback(tx) — receiver is the package identifier.
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "pg" {
				return true
			}
			// ANY other receiver (variable, chained call, anything):
			// ownership is structural, not name-based.
			pos := fset.Position(call.Pos())
			violations = append(violations, fmt.Sprintf(
				"%s: direct .Rollback(...) call — production rollback ownership belongs to internal/pg (use pg.Rollback)", pos))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}
