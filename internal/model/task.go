package model

// Task represents a row from tb_tasks.
// All numeric and timestamp fields use Go-native types (float64, string)
// for simpler business logic. The repository layer handles type conversion
// from PostgreSQL's pgtype.Numeric and time.Time.
type Task struct {
	ID           int64   `db:"id" json:"-"`
	Code         string  `db:"code" json:"code"`
	BotID        int64   `db:"bot_id" json:"-"`
	Title        string  `db:"title" json:"title"`
	Requirements string  `db:"requirements" json:"requirements"`
	PostAPI      *string `db:"postapi" json:"-"`
	Budget       float64 `db:"budget" json:"budget"`
	Price        float64 `db:"price" json:"price"`
	Pinned       bool    `db:"pinned" json:"pinned"`
	Status       string  `db:"status" json:"status"`
	ReviewNote   *string `db:"review_note" json:"-"`
	CreatedAt    string  `db:"created_at" json:"created_at"`
	UpdatedAt    string  `db:"updated_at" json:"updated_at"`
	ReviewedAt   *string `db:"reviewed_at" json:"-"`
	OpenedAt     *string `db:"opened_at" json:"opened_at,omitempty"`
	ClosedAt     *string `db:"closed_at" json:"closed_at,omitempty"`
}
