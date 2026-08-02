package model

type Bot struct {
	ID           int64   `db:"id" json:"id"`
	BotName      string  `db:"bot_name" json:"bot_name"`
	APIKey       string  `db:"api_key" json:"api_key"`
	PasswordHash string  `db:"password_hash" json:"-"`
	KeyIssuedAt  *string `db:"key_issued_at" json:"key_issued_at,omitempty"`
	Balance      float64 `db:"balance" json:"balance"`
	RegisterIP   *string `db:"register_ip" json:"register_ip,omitempty"`
	Status       string  `db:"status" json:"status"`
	LastActiveAt *string `db:"last_active_at" json:"last_active_at,omitempty"`
	CreatedAt    string  `db:"created_at" json:"created_at"`
	UpdatedAt    string  `db:"updated_at" json:"updated_at"`
}
