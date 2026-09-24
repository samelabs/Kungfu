package admin

import (
	"context"
	"encoding/json"
	"fmt"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// Audit: the admin audit trail is APPEND-ONLY platform governance
// fact, independent of the bot-scoped tb_logs.
//
// Invariant: for every successful privileged admin mutation, the
// business mutation AND the audit INSERT must share ONE PostgreSQL
// transaction — an audit write failure rolls the mutation back. The
// WithAuditTx helper is the only sanctioned shape: callers run their
// mutation on the tx and this helper writes the audit row on the SAME
// tx before commit.

// AuditEntry describes one governance fact.
type AuditEntry struct {
	Actor      *model.Admin
	Action     string
	TargetType string
	TargetID   string
	Success    bool
	Before     interface{}
	After      interface{}
	Metadata   interface{}
	IPAddress  string
	UserAgent  string
	ErrorCode  string
}

// WithAuditTx runs fn on a fresh transaction; if fn succeeds, the
// audit row is inserted on the SAME tx and the transaction commits.
// If either the mutation or the audit insert fails, everything rolls
// back — "operation succeeded but audit missing" cannot happen.
func WithAuditTx(ctx context.Context, pool *pg.Pool, e *AuditEntry, fn func(ctx context.Context, tx pg.Querier) error) error {
	if e == nil || e.Action == "" {
		return errors.New(500, "INTERNAL_ERROR", "audit entry is required")
	}
	tx, err := pool.TxBegin(ctx)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	defer pg.Rollback(tx)

	if err := fn(ctx, tx); err != nil {
		return err
	}

	// Strict marshaling happens FIRST: an unmarshalable audit fact
	// aborts before any SQL, and WithAuditTx returns the error with
	// the mutation rolled back.
	logRow, err := auditModel(e)
	if err != nil {
		return errors.New(500, "AUDIT_MARSHAL_FAILED",
			"Audit record could not be serialized — mutation rolled back")
	}
	if err := repository.InsertAdminAuditLog(ctx, tx, logRow); err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Failed to write admin audit — mutation rolled back")
	}
	return tx.Commit(ctx)
}

// auditModel converts an AuditEntry to the persistence model. It
// never carries a raw session token (fields are structured facts
// only). Marshaling Before/After/Metadata is STRICT: an unsupported
// value is an error so WithAuditTx rolls the mutation back — an
// audit row must never silently lose its facts.
func auditModel(e *AuditEntry) (*model.AdminAuditLog, error) {
	l := &model.AdminAuditLog{
		Action:        e.Action,
		ActorUsername: "(system)",
		Success:       e.Success,
	}
	if e.Actor != nil {
		id := e.Actor.ID
		l.ActorAdminID = &id
		l.ActorUsername = e.Actor.Username
	}
	if e.TargetType != "" {
		l.TargetType = &e.TargetType
	}
	if e.TargetID != "" {
		l.TargetID = &e.TargetID
	}
	if e.ErrorCode != "" {
		l.ErrorCode = &e.ErrorCode
	}
	if e.IPAddress != "" {
		l.IPAddress = &e.IPAddress
	}
	if e.UserAgent != "" {
		l.UserAgent = &e.UserAgent
	}
	var err error
	if l.BeforeJSON, err = marshalOrNull(e.Before); err != nil {
		return nil, fmt.Errorf("audit before_json: %w", err)
	}
	if l.AfterJSON, err = marshalOrNull(e.After); err != nil {
		return nil, fmt.Errorf("audit after_json: %w", err)
	}
	if l.MetadataJSON, err = marshalOrNull(e.Metadata); err != nil {
		return nil, fmt.Errorf("audit metadata_json: %w", err)
	}
	return l, nil
}

// marshalOrNull marshals structured audit facts; nil stays nil, and
// an unmarshalable value is an ERROR (never silently dropped).
func marshalOrNull(v interface{}) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}
