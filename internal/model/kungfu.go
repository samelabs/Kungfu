package model

type Kungfu struct {
	ID          int64   `db:"id" json:"-"`
	Code        string  `db:"code" json:"code"`
	BotID       int64   `db:"bot_id" json:"-"`
	Title       string  `db:"title" json:"title"`
	TagsJSON    string  `db:"tags_json" json:"-"`
	Description *string `db:"description" json:"description,omitempty"`
	Content     string  `db:"content" json:"content"`
	Checksum    string  `db:"checksum" json:"checksum"`
	Visibility  string  `db:"visibility" json:"visibility"`
	Status      string  `db:"status" json:"-"`
	CreatedAt   string  `db:"created_at" json:"created_at"`
	UpdatedAt   string  `db:"updated_at" json:"updated_at"`
}
