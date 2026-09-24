package model

type TaskLog struct {
	ID           int64   `db:"id" json:"id"`
	TaskCode     string  `db:"task_code" json:"task_code"`
	BotID        *int64  `db:"bot_id" json:"bot_id,omitempty"`
	Action       string  `db:"action" json:"action"`
	PayloadJSON  *string `db:"payload_json" json:"payload_json,omitempty"`
	ResponseCode *int    `db:"response_code" json:"response_code,omitempty"`
	ResponseBody *string `db:"response_body" json:"response_body,omitempty"`
	Success      bool    `db:"success" json:"success"`
	ErrorCode    *string `db:"error_code" json:"error_code,omitempty"`
	ErrorMessage *string `db:"error_message" json:"error_message,omitempty"`
	CreatedAt    string  `db:"created_at" json:"created_at"`
}
