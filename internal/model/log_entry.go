package model

type LogEntry struct {
	ID          int64   `db:"id" json:"id"`
	BotID       *int64  `db:"bot_id" json:"bot_id,omitempty"`
	Action      string  `db:"action" json:"action"`
	TargetType  *string `db:"target_type" json:"target_type,omitempty"`
	TargetID    *string `db:"target_id" json:"target_id,omitempty"`
	IPAddress   *string `db:"ip_address" json:"ip_address,omitempty"`
	UserAgent   *string `db:"user_agent" json:"user_agent,omitempty"`
	RequestData *string `db:"request_data" json:"request_data,omitempty"`
	Success     bool    `db:"success" json:"success"`
	ErrorCode   *string `db:"error_code" json:"error_code,omitempty"`
	ErrorMsg    *string `db:"error_msg" json:"error_msg,omitempty"`
	CreatedAt   string  `db:"created_at" json:"created_at"`
}
