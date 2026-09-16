package admin

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"kungfu.md/internal/pg"
)

// Admin integration tests run against a PRIVATE throwaway database
// created from the full migration chain (001→006) — proving the chain
// applies from zero — so the bootstrap gate (COUNT(tb_admins)==0) is
// deterministic and parallel packages sharing the CI database cannot
// interfere.

func mustTestDatabaseURL(t *testing.T) string {
	t.Helper()
	u := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if u == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	return u
}

// applyMigrations applies the whole chain in filename order.
func applyMigrations(ctx context.Context, dbPool *pg.Pool) error {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := dbPool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// createPrivateDB creates a fresh database from the migration chain.
func createPrivateDB(t *testing.T) *pg.Pool {
	t.Helper()
	baseURL := mustTestDatabaseURL(t)
	ctx := context.Background()

	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse test db url: %v", err)
	}
	adminURL := *parsed
	adminURL.Path = "/postgres"

	maint, err := pg.NewPool(adminURL.String())
	if err != nil {
		t.Skipf("maintenance connection unavailable: %v", err)
	}
	dbName := fmt.Sprintf("kf_admin_test_%d", time.Now().UnixNano())
	if _, err := maint.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Skipf("create private test db: %v", err)
	}
	maint.Close()

	privateURL := *parsed
	privateURL.Path = "/" + dbName
	dbPool, err := pg.NewPool(privateURL.String())
	if err != nil {
		t.Fatalf("connect private db: %v", err)
	}
	if err := applyMigrations(ctx, dbPool); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	t.Cleanup(func() {
		dbPool.Close()
		m2, err := pg.NewPool(adminURL.String())
		if err != nil {
			return
		}
		defer m2.Close()
		_, _ = m2.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
	})
	return dbPool
}

// sharedPool is the package-wide private DB (one migration run for the
// whole package). Bootstrap tests use their own fresh DB instead.
var (
	sharedPoolOnce sync.Once
	sharedPool     *pg.Pool
)

func adminTestPool(t *testing.T) *pg.Pool {
	t.Helper()
	sharedPoolOnce.Do(func() {
		// createPrivateDB registers cleanup on ITS test (t0), which
		// runs for the whole package lifetime via t.Cleanup — safe.
	})
	if sharedPool == nil {
		sharedPool = createPrivateDBOneshot(t)
	}
	return sharedPool
}

func createPrivateDBOneshot(t *testing.T) *pg.Pool {
	return createPrivateDB(t)
}

// seedAdmin creates an admin directly (no bootstrap gate) and returns
// it. Unique username per call.
func seedAdmin(t *testing.T, dbPool *pg.Pool, password string) *struct {
	ID       int64
	Username string
} {
	t.Helper()
	hash, err := hashForTest(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	username := "t_" + fmt.Sprintf("%d", time.Now().UnixNano())
	var id int64
	err = dbPool.QueryRow(context.Background(), `
		INSERT INTO tb_admins (username, display_name, password_hash)
		VALUES ($1, 'Test Admin', $2) RETURNING id`, username, hash).Scan(&id)
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = dbPool.Exec(ctx, `DELETE FROM tb_admin_user_roles WHERE admin_id = $1`, id)
		_, _ = dbPool.Exec(ctx, `DELETE FROM tb_admin_sessions WHERE admin_id = $1`, id)
		_, _ = dbPool.Exec(ctx, `DELETE FROM tb_admins WHERE id = $1`, id)
	})
	return &struct {
		ID       int64
		Username string
	}{ID: id, Username: username}
}
