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
	// Revision is the memory's current version (kungfu.md §5):
	// creation is revision 1; every author update archives the
	// previous version in memory_revisions and bumps this by 1.
	Revision int64 `db:"revision" json:"-"`
	// Origin records what created the memory: 'standalone' (the
	// memory tools) or 'thread' (Thread-stage entry payloads; no
	// writer exists yet — lists already exclude it).
	Origin    string `db:"origin" json:"-"`
	CreatedAt string `db:"created_at" json:"created_at"`
	UpdatedAt string `db:"updated_at" json:"updated_at"`
}

// KungfuRevision is one archived prior version of a memory
// (memory_revisions). It carries the old version's full content
// snapshot; memory-level facts (code, visibility, author) live on
// the tb_kungfus row.
type KungfuRevision struct {
	MemoryID    int64
	Revision    int64
	Title       string
	TagsJSON    string
	Description *string
	Content     string
	Checksum    string
	UpdatedAt   string
}
