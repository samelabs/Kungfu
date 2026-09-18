package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps pgxpool.Pool providing connection management.
type Pool struct {
	*pgxpool.Pool
}

// NewPool creates a new connection pool from a database URL.
func NewPool(databaseURL string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	// Connection pool settings
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	// Verify connectivity
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Pool{pool}, nil
}

// Close closes all connections in the pool.
func (p *Pool) Close() {
	p.Pool.Close()
}

// NewPoolMaxConns creates a pool with an explicit maximum connection
// count. Tests use it to build a single-connection pool for
// deterministic concurrency/deadlock proofs; production code uses
// NewPool (fixed settings) and is unaffected.
func NewPoolMaxConns(databaseURL string, maxConns int32) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Pool{pool}, nil
}

// TxBegin starts a new transaction.

func (p *Pool) TxBegin(ctx context.Context) (pgx.Tx, error) {
	return p.Pool.Begin(ctx)
}

// InTransaction checks if we're inside a transaction.
// With pgx, this is handled by passing pgx.Tx explicitly to repositories.
// This helper exists for the Transaction service's nesting detection.
type TxRunner struct {
	tx pgx.Tx
}

// IsTx returns true if a transaction runner has an active tx.
func (t *TxRunner) IsTx() bool {
	return t.tx != nil
}

// BeginOrUse starts a new transaction if none is active, or returns the existing one.
// Returns: tx, startedNew (true if caller should commit/rollback), err
func (p *Pool) BeginOrUse(ctx context.Context, existing pgx.Tx) (pgx.Tx, bool, error) {
	if existing != nil {
		return existing, false, nil
	}
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	return tx, true, nil
}

// CommitOrSkip commits only if startedNew is true.
func CommitOrSkip(ctx context.Context, tx pgx.Tx, startedNew bool) error {
	if !startedNew {
		return nil
	}
	return tx.Commit(ctx)
}

// Rollback is the AUTHORITATIVE transaction rollback-cleanup
// primitive (R2.1 repair): every production transaction owner rolls
// back through here. It deliberately does NOT use the request/
// business context — a cancelled request context would make ROLLBACK
// itself a no-op, leaving aborted transactions holding locks until
// connection destruction. The cleanup-only context has a hard 2s
// timeout: ordinary request-cancellation cleanup completes far below
// it; if rollback cannot complete in 2s the connection is unusable
// and pgx destroys it (the last-resort path, not normal cleanup).
//
// The cleanup context exists ONLY for ROLLBACK — never for queries,
// mutations, commits, provider calls, logging, or audit writes.
// Commit keeps the caller's context.
//
// No retry, no goroutine, no framework.
func Rollback(tx pgx.Tx) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return tx.Rollback(cleanupCtx)
}

// RollbackOrSkip rolls back only if startedNew is true, via the
// authoritative cleanup primitive (request-context independent).
func RollbackOrSkip(tx pgx.Tx, startedNew bool) error {
	if !startedNew {
		return nil
	}
	return Rollback(tx)
}

// Querier is the interface satisfied by both *pgxpool.Pool and pgx.Tx.
// All repository methods accept this interface so they work with or without a transaction.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
