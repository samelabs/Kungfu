package model

// Task represents a row from tb_tasks.
// Credits fields (budget, price) are whole integer units (int64).
// Timestamp fields use canonical strings; the repository layer handles
// conversion from PostgreSQL time.Time.
type Task struct {
	ID           int64   `db:"id" json:"-"`
	Code         string  `db:"code" json:"code"`
	BotID        int64   `db:"bot_id" json:"-"`
	Title        string  `db:"title" json:"title"`
	Requirements string  `db:"requirements" json:"requirements"`
	PostAPI      *string `db:"postapi" json:"-"`
	Budget       int64   `db:"budget" json:"budget"`
	Price        int64   `db:"price" json:"price"`
	Pinned       bool    `db:"pinned" json:"pinned"`
	Status       string  `db:"status" json:"status"`
	ReviewNote   *string `db:"review_note" json:"-"`
	CreatedAt    string  `db:"created_at" json:"created_at"`
	UpdatedAt    string  `db:"updated_at" json:"updated_at"`
	ReviewedAt   *string `db:"reviewed_at" json:"-"`
	OpenedAt     *string `db:"opened_at" json:"opened_at,omitempty"`
	ClosedAt     *string `db:"closed_at" json:"closed_at,omitempty"`
}
