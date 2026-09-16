package model

import (
	"time"
)

// Admin is a platform administrator — a THIRD identity surface,
// permanently separate from bots (Agent/Owner planes). An Admin
// carries NO bot_id; authorization is resolved per-request from
// roles/permissions in the database.
type Admin struct {
	ID                int64
	Username          string
	DisplayName       string
	PasswordHash      string
	Status            string
	AuthVersion       int64
	LastLoginAt       *time.Time
	PasswordChangedAt time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// AdminRole is an RBAC role.
type AdminRole struct {
	ID          int64
	Code        string
	Name        string
	Description *string
	IsSystem    bool
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// AdminSession is a server-side admin session row. The raw token
// NEVER reaches the database — only SHA-256(token) is stored in
// TokenHash. This struct never carries a raw token field by design.
type AdminSession struct {
	ID          int64
	AdminID     int64
	TokenHash   string
	AuthVersion int64
	IPAddress   *string
	UserAgent   *string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

// AdminAuditLog is one append-only platform-governance fact.
// ActorUsername is a historical snapshot: later renames/deletes of
// the admin must not damage the audit trail.
type AdminAuditLog struct {
	ID            int64
	ActorAdminID  *int64
	ActorUsername string
	Action        string
	TargetType    *string
	TargetID      *string
	Success       bool
	BeforeJSON    []byte
	AfterJSON     []byte
	MetadataJSON  []byte
	IPAddress     *string
	UserAgent     *string
	ErrorCode     *string
	CreatedAt     time.Time
}
