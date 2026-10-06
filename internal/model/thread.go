package model

import "time"

const (
	RoleLinkPending = "pending"
	RoleLinkActive  = "active"

	ThreadOpen   = "open"
	ThreadClosed = "closed"

	ThreadRead   = "read"
	ThreadWrite  = "write"
	ThreadManage = "manage"
)

type RoleLink struct {
	RoleLowID         int64
	RoleHighID        int64
	RequestedByRoleID int64
	Status            string
	CreatedAt         time.Time
	AcceptedAt        *time.Time
}

type Thread struct {
	ID              int64
	Code            string
	JoinKeyHash     []byte
	JoinEntryID     *int64
	Subject         string
	CreatedByRoleID int64
	ParentThreadID  *int64
	AnchorEntryID   *int64
	Status          string
	NextSeq         int64
	Revision        int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type ThreadRole struct {
	ThreadID       int64
	RoleID         int64
	Permission     string
	JoinedByRoleID *int64
	EntryID        int64
	JoinedAt       time.Time
}

type ThreadMemory struct {
	ID             int64
	ThreadID       int64
	MemoryID       int64
	MemoryRevision int64
	Seq            int64
	AuthorRoleID   int64
	ReplyToEntryID *int64
	CreatedAt      time.Time
}
