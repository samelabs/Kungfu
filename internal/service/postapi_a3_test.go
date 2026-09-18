package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
)

// A3 regression tests: PostAPI structural validation single source of truth.
// Shared classifier + both translations + Owner integration paths, run
// against the local dev PostgreSQL via KF_TEST_DATABASE_URL.

// -- shared validator --

func TestClassifyPostAPI(t *testing.T) {
	cases := []struct {
		name    string
		postapi string
		want    postAPIClass
	}{
		{"empty", "", postAPIClassEmpty},
		{"exactly 2048 bytes of valid url is ok", "http://e.com/" + strings.Repeat("a", 2048-len("http://e.com/")), ""},
		{"2049 bytes", "http://e.com/" + strings.Repeat("a", 2049-len("http://e.com/")), postAPIClassTooLong},
		{"malformed no host", "http:///missing-host", postAPIClassInvalidURL},
		{"relative path", "/relative/path", postAPIClassInvalidURL},
		{"not a url", "not-a-url", postAPIClassInvalidURL},
		{"ftp scheme", "ftp://example.com", postAPIClassInvalidScheme},
		{"valid http", "http://example.com/hook", ""},
		{"valid https", "https://example.com/hook", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := classifyPostAPI(tc.postapi, 2048)
			if got != tc.want {
				t.Fatalf("classifyPostAPI(%q) = %q, want %q", tc.postapi, got, tc.want)
			}
		})
	}
}

// -- TaskCheck translation: external semantics unchanged --

func TestValidatePostapiTaskCheckSemantics(t *testing.T) {
	cases := []struct {
		name     string
		postapi  string
		wantHTTP int
		wantCode string
	}{
		{"empty", "", 503, "TASK_NOT_CONFIGURED"},
		{"too long", "http://e.com/" + strings.Repeat("a", 2049-len("http://e.com/")), 500, "TASK_CONFIG_INVALID"},
		{"invalid url", "http:///missing-host", 500, "TASK_CONFIG_INVALID"},
		{"invalid scheme", "ftp://example.com", 500, "TASK_CONFIG_INVALID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := ValidatePostapi(tc.postapi, 2048)
			if e == nil {
				t.Fatal("want error, got nil")
			}
			if e.Rule.HTTPCode != tc.wantHTTP || e.Rule.Code != tc.wantCode {
				t.Fatalf("got %d %s, want %d %s", e.Rule.HTTPCode, e.Rule.Code, tc.wantHTTP, tc.wantCode)
			}
			ae := e.ToAppError()
			if ae.HTTPCode != tc.wantHTTP || ae.Code != tc.wantCode {
				t.Fatalf("AppError got %d %s, want %d %s", ae.HTTPCode, ae.Code, tc.wantHTTP, tc.wantCode)
			}
		})
	}
	if e := ValidatePostapi("https://example.com/hook", 2048); e != nil {
		t.Fatalf("valid https rejected: %v", e)
	}
}

// -- Owner translation: stable 400 INVALID_POSTAPI contract --

func TestValidatePostapiFieldOwnerContract(t *testing.T) {
	cases := []struct {
		postapi  string
		wantCode string
	}{
		{"not-a-url", "INVALID_POSTAPI"},
		{"/relative/path", "INVALID_POSTAPI"},
		{"ftp://example.com", "INVALID_POSTAPI"},
		{"http:///missing-host", "INVALID_POSTAPI"},
		{"", "MISSING_FIELD"},
	}
	for _, tc := range cases {
		err := validatePostapiField(tc.postapi)
		ae, ok := errors.IsAppError(err)
		if !ok {
			t.Fatalf("validatePostapiField(%q): want AppError, got %v", tc.postapi, err)
		}
		if ae.HTTPCode != 400 || ae.Code != tc.wantCode {
			t.Fatalf("validatePostapiField(%q) = %d %s, want 400 %s", tc.postapi, ae.HTTPCode, ae.Code, tc.wantCode)
		}
	}
	if err := validatePostapiField("http://example.com/hook"); err != nil {
		t.Fatalf("valid http rejected: %v", err)
	}
	if err := validatePostapiField("https://example.com/hook"); err != nil {
		t.Fatalf("valid https rejected: %v", err)
	}
}

// -- Owner integration (local PG) --

func a3TestPool(t *testing.T) *pg.Pool {
	t.Helper()
	url := strings.TrimSpace(os.Getenv("KF_TEST_DATABASE_URL"))
	if url == "" {
		t.Skip("KF_TEST_DATABASE_URL not set")
	}
	pool, err := pg.NewPool(url)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func a3TestBot(t *testing.T, pool *pg.Pool, balance float64) int64 {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var botID int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO tb_bots (bot_name, api_key_hash, api_key_last4, password_hash, status, balance)
		 VALUES ($1, $2, $3, 'x', 'active', $4) RETURNING id`,
		"a3own_"+suffix, s61SeedKeyHash("kf_live_"+strings.ReplaceAll(suffix, ".", "")), s61SeedLast4("kf_live_"+strings.ReplaceAll(suffix, ".", "")), balance,
	).Scan(&botID)
	if err != nil {
		t.Fatalf("seed bot: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM tb_task_logs WHERE task_code IN (SELECT code FROM tb_tasks WHERE bot_id = $1)`, botID)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_logs WHERE target_type = 'task' AND target_id IN (SELECT code FROM tb_tasks WHERE bot_id = $1)`, botID)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_logs WHERE bot_id = $1`, botID)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_tasks WHERE bot_id = $1`, botID)
		_, _ = pool.Exec(ctx, `DELETE FROM tb_bots WHERE id = $1`, botID)
	})
	return botID
}

func a3Balance(t *testing.T, pool *pg.Pool, botID int64) float64 {
	t.Helper()
	var b float64
	if err := pool.QueryRow(context.Background(),
		`SELECT balance::float8 FROM tb_bots WHERE id = $1`, botID).Scan(&b); err != nil {
		t.Fatalf("balance: %v", err)
	}
	return b
}

func a3TaskCount(t *testing.T, pool *pg.Pool, botID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM tb_tasks WHERE bot_id = $1`, botID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func a3CreateInput(postapi string) *CreateTaskInput {
	return &CreateTaskInput{
		Title:        "A3 Test Task",
		Requirements: "requirements body",
		PostAPI:      postapi,
		Budget:       1000,
		Price:        1,
	}
}

func wantInvalidPostapi400(t *testing.T, err error) {
	t.Helper()
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("want AppError, got %v", err)
	}
	if ae.HTTPCode != 400 || ae.Code != "INVALID_POSTAPI" {
		t.Fatalf("want 400 INVALID_POSTAPI, got %d %s", ae.HTTPCode, ae.Code)
	}
}

// TestOwnerCreateInvalidPostAPIRejected: invalid URL/scheme fails BEFORE
// InsertTask and budget lock — no task row, balance unchanged.
func TestOwnerCreateInvalidPostAPIRejected(t *testing.T) {
	pool := a3TestPool(t)
	botID := a3TestBot(t, pool, 5000)
	ctx := context.Background()

	for _, postapi := range []string{"not-a-url", "/relative/path", "ftp://example.com", "http:///missing-host"} {
		if n := a3TaskCount(t, pool, botID); n != 0 {
			t.Fatalf("pre-existing task rows: %d", n)
		}
		_, err := CreateTask(ctx, pool, botID, &OwnerTaskConfig{}, a3CreateInput(postapi))
		wantInvalidPostapi400(t, err)
		if n := a3TaskCount(t, pool, botID); n != 0 {
			t.Fatalf("task created with invalid postapi %q", postapi)
		}
		if b := a3Balance(t, pool, botID); b != 5000 {
			t.Fatalf("balance changed on rejected create (%q): %v", postapi, b)
		}
	}
}

// TestOwnerCreateInvalidSchemeRejected: explicit scheme check.
func TestOwnerCreateInvalidSchemeRejected(t *testing.T) {
	pool := a3TestPool(t)
	botID := a3TestBot(t, pool, 5000)
	_, err := CreateTask(context.Background(), pool, botID, &OwnerTaskConfig{}, a3CreateInput("ftp://example.com"))
	wantInvalidPostapi400(t, err)
}

// TestOwnerCreateValidPostAPIAccepted: valid http/https pass.
func TestOwnerCreateValidPostAPIAccepted(t *testing.T) {
	pool := a3TestPool(t)
	botID := a3TestBot(t, pool, 5000)
	ctx := context.Background()

	for _, postapi := range []string{"http://example.com/hook", "https://example.com/hook"} {
		res, err := CreateTask(ctx, pool, botID, &OwnerTaskConfig{}, a3CreateInput(postapi))
		if err != nil {
			t.Fatalf("valid postapi %q rejected: %v", postapi, err)
		}
		if res == nil || res["task"] == nil {
			t.Fatalf("no task in result for %q", postapi)
		}
	}
	if n := a3TaskCount(t, pool, botID); n != 2 {
		t.Fatalf("task count = %d, want 2", n)
	}
}

// TestOwnerEditClosedTaskInvalidPostAPIRejected: editing a closed task to an
// invalid URL fails with 400 and the DB row keeps the original postapi.
func TestOwnerEditClosedTaskInvalidPostAPIRejected(t *testing.T) {
	pool := a3TestPool(t)
	botID := a3TestBot(t, pool, 5000)
	ctx := context.Background()

	created, err := CreateTask(ctx, pool, botID, &OwnerTaskConfig{}, a3CreateInput("https://example.com/hook"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := created["task"].(map[string]interface{})["code"].(string)

	// Move the task to closed (insert as pending, then close via repository-level SQL
	// is forbidden; use SetTaskStatus close which requires pending/closed transition).
	if _, err := SetTaskStatus(ctx, pool, botID, code, "closed"); err != nil {
		t.Fatalf("close: %v", err)
	}

	newAPI := "not-a-url"
	_, err = UpdateTaskBasics(ctx, pool, botID, code, &OwnerTaskConfig{}, &UpdateTaskBasicsInput{PostAPI: &newAPI})
	wantInvalidPostapi400(t, err)

	var current *string
	if err := pool.QueryRow(ctx,
		`SELECT postapi FROM tb_tasks WHERE code = $1`, code).Scan(&current); err != nil {
		t.Fatalf("read postapi: %v", err)
	}
	if current == nil || *current != "https://example.com/hook" {
		t.Fatalf("postapi changed after rejected edit: %v", current)
	}
}

// TestOwnerOpenLegacyInvalidPostAPITaskRejected: a task whose DB row already
// contains a structurally invalid postapi (legacy data) cannot be opened;
// status stays unchanged.
func TestOwnerOpenLegacyInvalidPostAPITaskRejected(t *testing.T) {
	pool := a3TestPool(t)
	botID := a3TestBot(t, pool, 5000)
	ctx := context.Background()

	created, err := CreateTask(ctx, pool, botID, &OwnerTaskConfig{}, a3CreateInput("https://example.com/hook"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	code := created["task"].(map[string]interface{})["code"].(string)

	// Simulate legacy bad data: bypass service validation, write directly.
	if _, err := pool.Exec(ctx,
		`UPDATE tb_tasks SET postapi = 'not-a-url' WHERE code = $1`, code); err != nil {
		t.Fatalf("seed legacy invalid postapi: %v", err)
	}

	_, err = SetTaskStatus(ctx, pool, botID, code, "open")
	wantInvalidPostapi400(t, err)

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM tb_tasks WHERE code = $1`, code).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status changed after rejected open: %q", status)
	}
}
