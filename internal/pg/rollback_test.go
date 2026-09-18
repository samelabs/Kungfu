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

	// and the pool still serves a fresh transaction
	tx2, err := pool.TxBegin(ctxBG)
	if err != nil {
		t.Fatalf("fresh transaction failed after cleanup: %v", err)
	}
	if _, err := tx2.Exec(ctx2ctx(), `SELECT 1`); err != nil {
		_ = Rollback(tx2)
		t.Fatalf("fresh transaction work failed: %v", err)
	}
	if err := tx2.Commit(ctx2ctx()); err != nil {
		t.Fatal(err)
	}
}

func ctx2ctx() context.Context {
	c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = cancel
	return c
}

// ===========================================================================
// Architecture guard (AST): production Go files outside internal/pg
// must not call .Rollback( directly on a transaction — all rollback
// cleanup goes through the authoritative pg.Rollback primitive.
// Tests are exempt.
// ===========================================================================

func TestRollbackOwnedOnlyByPG(t *testing.T) {
	root, err := os.Getwd() // internal/pg
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(filepath.Dir(root)) // repo root

	violations := []string{}
	err = filepath.Walk(filepath.Join(root, "internal"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(path, filepath.Join(root, "internal", "pg")+string(filepath.Separator)) {
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
			// receiver must be an identifier named tx/useTx (a transaction)
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name != "tx" && ident.Name != "useTx" {
				return true // not a transaction variable
			}
			pos := fset.Position(call.Pos())
			violations = append(violations, fmt.Sprintf("%s: direct %s.Rollback(...) — use pg.Rollback", pos, ident.Name))
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
