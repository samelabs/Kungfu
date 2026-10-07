package model

// Thread is one room row (kungfu.md §6.1/§6.5). The key group —
// KeyHash, KeyRole, KeyIssuedAt — is either all set (exactly one
// active key) or all nil (never issued, revoked, or cleared by
// close). Only the sha256 digest is ever stored; the raw key exists
// in one first-successful-response disclosure (L3).
type Thread struct {
	ID          int64   `db:"id" json:"-"`
	Code        string  `db:"code" json:"code"`
	Subject     *string `db:"subject" json:"subject,omitempty"`
	Status      string  `db:"status" json:"status"`
	KeyHash     *string `db:"key_hash" json:"-"`
	KeyRole     *string `db:"key_role" json:"-"`
	KeyIssuedAt *string `db:"key_issued_at" json:"-"`
	NextSeq     int64   `db:"next_seq" json:"-"`
	CreatedAt   string  `db:"created_at" json:"created_at"`
	ClosedAt    *string `db:"closed_at" json:"closed_at,omitempty"`
}

// ThreadMember is one membership row (kungfu.md §6.2): exactly two
// recorded facts — which key admitted the account (nil for the
// creator, who never joins by key) and when — plus the current role.
// A terminated membership is a deleted row; rejoining is a new row.
type ThreadMember struct {
	ThreadID         int64
	AccountID        int64
	Role             string
	JoinedViaKeyHash *string
	JoinedAt         string
}

// ThreadIdempotency is one stored first-success receipt (kungfu.md
// L3): same (account, tool, key) replays ResultSnapshot verbatim;
// the same key with a different RequestHash is a conflict.
type ThreadIdempotency struct {
	AccountID      int64
	Tool           string
	Key            string
	RequestHash    string
	ResultSnapshot string // JSON protocol-fact fields of the first result
	CreatedAt      string
}
