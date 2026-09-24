package model

type Transaction struct {
	ID           int64   `db:"id" json:"id"`
	BotID        int64   `db:"bot_id" json:"bot_id"`
	Type         string  `db:"type" json:"type"`
	Amount       int64   `db:"amount" json:"amount"`
	BalanceAfter int64   `db:"balance_after" json:"balance_after"`
	RefType      *string `db:"ref_type" json:"ref_type,omitempty"`
	RefID        *string `db:"ref_id" json:"ref_id,omitempty"`
	CreatedAt    string  `db:"created_at" json:"created_at"`
}
