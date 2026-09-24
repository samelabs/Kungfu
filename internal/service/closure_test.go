package service

import (
	"context"
	"strings"
	"testing"

	stderrors "errors"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"kungfu.md/internal/auth"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
)

// -- fakes --

// credsRow / credsRows serve the tb_bots credentials SELECT.
type fakeRow struct {
	bot *botCreds
	err error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.bot == nil {
		return pgx.ErrNoRows
	}
	// dest: id (int32), bot_name, password_hash, status — mirrors FindActiveBotCredentialsByName
	if d, ok := dest[0].(*int32); ok {
		*d = int32(r.bot.id)
	}
	if d, ok := dest[1].(*string); ok {
		*d = r.bot.name
	}
	if d, ok := dest[2].(*string); ok {
		*d = r.bot.hash
	}
	if d, ok := dest[3].(*string); ok {
		*d = r.bot.status
	}
	return nil
}

// Query is not exercised by these code paths (all reads use QueryRow).

type botCreds struct {
	id     int64
	name   string
	hash   string
	status string
}

// fakeQuerier routes SQL to canned behavior:
//   - credentials SELECT on tb_bots -> map lookup
//   - log INSERT into tb_logs       -> recorded, always succeeds
//   - everything else               -> failSQL error (write failure simulation)
type fakeQuerier struct {
	bots    map[string]*botCreds
	logs    []logCall
	failSQL map[string]error // substring -> error (forces repository failure)
}

type logCall struct {
	botID   *int64
	action  string
	success bool
}

func (f *fakeQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FROM tb_bots") {
		name, _ := args[0].(string)
		if err, ok := f.failSQL["tb_bots"]; ok {
			return &fakeRow{err: err}
		}
		return &fakeRow{bot: f.bots[name]}
	}
	if err, ok := f.failSQL["query"]; ok {
		return &fakeRow{err: err}
	}
	return &fakeRow{}
}

func (f *fakeQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, stderrors.New("fakeQuerier: Query not supported")
}

func (f *fakeQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO tb_logs") {
		lc := logCall{success: true}
		if id, ok := args[0].(*int64); ok {
			lc.botID = id
		} else if id, ok := args[0].(int64); ok {
			lc.botID = &id
		}
		lc.action, _ = args[1].(string)
		if s, ok := args[7].(bool); ok {
			lc.success = s
		}
		f.logs = append(f.logs, lc)
		return pgconn.CommandTag{}, nil
	}
	if err, ok := f.failSQL["exec"]; ok {
		return pgconn.CommandTag{}, err
	}
	return pgconn.CommandTag{}, nil
}

func appErrOf(t *testing.T, err error) *errors.AppError {
	t.Helper()
	ae, ok := errors.IsAppError(err)
	if !ok {
		t.Fatalf("expected *errors.AppError, got %T: %v", err, err)
	}
	return ae
}

// -- OwnerLogin --

func TestOwnerLoginSuccess(t *testing.T) {
	hash, _ := auth.HashPassword("correct-horse")
	fq := &fakeQuerier{bots: map[string]*botCreds{
		"alice01": {id: 7, name: "alice01", hash: hash, status: "active"},
	}}
	res, err := OwnerLogin(context.Background(), fq, "alice01", "correct-horse")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if res.BotID != 7 || res.BotName != "alice01" || res.Status != "active" {
		t.Fatalf("unexpected identity: %+v", res)
	}
	// audit: owner_login logged with success=true
	found := false
	for _, l := range fq.logs {
		if l.action == "owner_login" {
			found = true
			if l.botID == nil || *l.botID != 7 || !l.success {
				t.Fatalf("bad owner_login log: %+v", l)
			}
		}
	}
	if !found {
		t.Fatal("expected owner_login audit entry")
	}
}

func TestOwnerLoginUnknownUserInvalidCredentials(t *testing.T) {
	fq := &fakeQuerier{bots: map[string]*botCreds{}}
	_, err := OwnerLogin(context.Background(), fq, "ghost12", "whatever12")
	ae := appErrOf(t, err)
	if ae.Code != "INVALID_CREDENTIALS" || ae.HTTPCode != 401 {
		t.Fatalf("expected 401 INVALID_CREDENTIALS, got %d %s", ae.HTTPCode, ae.Code)
	}
	// no audit entry on failed login
	if len(fq.logs) != 0 {
		t.Fatalf("expected no logs, got %+v", fq.logs)
	}
}

func TestOwnerLoginWrongPassword(t *testing.T) {
	hash, _ := auth.HashPassword("correct-horse")
	fq := &fakeQuerier{bots: map[string]*botCreds{
		"alice01": {id: 7, name: "alice01", hash: hash, status: "active"},
	}}
	_, err := OwnerLogin(context.Background(), fq, "alice01", "wrong-pass")
	ae := appErrOf(t, err)
	if ae.Code != "INVALID_CREDENTIALS" || ae.HTTPCode != 401 {
		t.Fatalf("expected 401 INVALID_CREDENTIALS, got %d %s", ae.HTTPCode, ae.Code)
	}
}

func TestOwnerLoginTrimsName(t *testing.T) {
	hash, _ := auth.HashPassword("correct-horse")
	fq := &fakeQuerier{bots: map[string]*botCreds{
		"alice01": {id: 7, name: "alice01", hash: hash, status: "active"},
	}}
	res, err := OwnerLogin(context.Background(), fq, "  alice01\t", "correct-horse")
	if err != nil {
		t.Fatalf("expected success with padded name, got %v", err)
	}
	if res.BotName != "alice01" {
		t.Fatalf("expected bot_name alice01, got %s", res.BotName)
	}
}

func TestOwnerLoginInvalidName(t *testing.T) {
	fq := &fakeQuerier{bots: map[string]*botCreds{}}
	_, err := OwnerLogin(context.Background(), fq, "ab", "whatever12")
	ae := appErrOf(t, err)
	if ae.Code != "INVALID_NAME" || ae.HTTPCode != 400 {
		t.Fatalf("expected 400 INVALID_NAME, got %d %s", ae.HTTPCode, ae.Code)
	}
}

func TestOwnerLoginRejectsAPIKeyContent(t *testing.T) {
	fq := &fakeQuerier{bots: map[string]*botCreds{}}
	_, err := OwnerLogin(context.Background(), fq, "alice01", "kf_live_"+strings.Repeat("a", 64))
	ae := appErrOf(t, err)
	if ae.HTTPCode != 400 {
		t.Fatalf("expected 400, got %d %s", ae.HTTPCode, ae.Code)
	}
	if ae.Code != "INVALID_PASSWORD" && ae.Code != "SENSITIVE_CONTENT" {
		t.Fatalf("expected INVALID_PASSWORD or SENSITIVE_CONTENT, got %s", ae.Code)
	}
}

func TestOwnerLoginDBErrorIsInternal(t *testing.T) {
	fq := &fakeQuerier{bots: map[string]*botCreds{}, failSQL: map[string]error{
		"tb_bots": context.DeadlineExceeded,
	}}
	_, err := OwnerLogin(context.Background(), fq, "alice01", "whatever12")
	ae := appErrOf(t, err)
	if ae.Code != "INTERNAL_ERROR" || ae.HTTPCode != 500 {
		t.Fatalf("expected 500 INTERNAL_ERROR, got %d %s", ae.HTTPCode, ae.Code)
	}
}

// -- OwnerLogout --

func TestOwnerLogoutWritesAuditLog(t *testing.T) {
	fq := &fakeQuerier{}
	OwnerLogout(context.Background(), fq, 42)
	if len(fq.logs) != 1 {
		t.Fatalf("expected exactly 1 log, got %d", len(fq.logs))
	}
	l := fq.logs[0]
	if l.action != "owner_logout" || l.botID == nil || *l.botID != 42 || !l.success {
		t.Fatalf("bad owner_logout log: %+v", l)
	}
}

func TestOwnerLogoutNoSessionNoLog(t *testing.T) {
	fq := &fakeQuerier{}
	OwnerLogout(context.Background(), fq, 0)
	if len(fq.logs) != 0 {
		t.Fatalf("expected no logs without session, got %+v", fq.logs)
	}
}

// -- Kungfu write failure closure --
// The fakeQuerier cannot serve FindOwnedActiveKungfuByCode scans, so the write
// paths are exercised by forcing repository failures through failSQL and using
// kungfuRowQuerier below for happy-path/visibility flows.

// kungfuFakeQuerier serves tb_kungfus SELECTs with a single owned kungfu.
type kungfuFakeQuerier struct {
	fakeQuerier
	k       *model.Kungfu
	failGet bool
}

func (f *kungfuFakeQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "FROM tb_kungfus") {
		if f.failGet {
			return &fakeRow{err: context.DeadlineExceeded}
		}
		if strings.Contains(sql, "bot_id = $2") {
			code, _ := args[0].(string)
			botID, _ := args[1].(int64)
			if f.k != nil && f.k.Code == code && f.k.BotID == botID && f.k.Status == "active" {
				return &kungfuRow{k: f.k}
			}
			return &fakeRow{} // no rows
		}
		// visibility check for "exists at all"
		if f.k != nil && f.k.Code == args[0] && f.k.Status == "active" {
			return &kungfuRow{k: f.k}
		}
		return &fakeRow{}
	}
	return f.fakeQuerier.QueryRow(ctx, sql, args...)
}

// kungfuRow scans a full kungfu row in the column order of the repository SQL.
type kungfuRow struct{ k *model.Kungfu }

func (r *kungfuRow) Scan(dest ...any) error {
	k := r.k
	setI64 := func(i int, v int64) {
		if d, ok := dest[i].(*int64); ok {
			*d = v
		}
	}
	setS := func(i int, v string) {
		if d, ok := dest[i].(*string); ok {
			*d = v
		}
	}
	setI64(0, k.ID)
	setS(1, k.Code)
	if d, ok := dest[2].(*int32); ok {
		*d = int32(k.BotID)
	}
	setS(3, k.Title)
	setS(4, k.TagsJSON)
	if len(dest) > 5 {
		if k.Description != nil {
			setS(5, *k.Description)
		}
		setS(6, k.Content)
		setS(7, k.Checksum)
		setS(8, k.Visibility)
		setS(9, k.Status)
	}
	return nil
}

func ownedKungfu(visibility string) *model.Kungfu {
	return &model.Kungfu{ID: 100, Code: "aaaabbbbcccc", BotID: 7, Title: "t",
		TagsJSON: `["x"]`, Content: "c", Checksum: "cs", Visibility: visibility, Status: "active"}
}

func TestShareWriteFailureReturnsError(t *testing.T) {
	fq := &kungfuFakeQuerier{k: ownedKungfu("private")}
	fq.failSQL = map[string]error{"exec": context.DeadlineExceeded}
	_, err := Share(context.Background(), fq, 7, "aaaabbbbcccc")
	ae := appErrOf(t, err)
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("expected 500 INTERNAL_ERROR on visibility write failure, got %d %s", ae.HTTPCode, ae.Code)
	}
}

func TestUnshareWriteFailureReturnsError(t *testing.T) {
	fq := &kungfuFakeQuerier{k: ownedKungfu("public")}
	fq.failSQL = map[string]error{"exec": context.DeadlineExceeded}
	_, err := Unshare(context.Background(), fq, 7, "aaaabbbbcccc")
	ae := appErrOf(t, err)
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("expected 500 INTERNAL_ERROR on visibility write failure, got %d %s", ae.HTTPCode, ae.Code)
	}
}

func TestDeleteWriteFailureReturnsError(t *testing.T) {
	fq := &kungfuFakeQuerier{k: ownedKungfu("private")}
	fq.failSQL = map[string]error{"exec": context.DeadlineExceeded}
	_, err := Delete(context.Background(), fq, 7, "aaaabbbbcccc")
	ae := appErrOf(t, err)
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("expected 500 INTERNAL_ERROR on soft-delete failure, got %d %s", ae.HTTPCode, ae.Code)
	}
}

func TestShareLookupFailureIsInternal(t *testing.T) {
	fq := &kungfuFakeQuerier{k: ownedKungfu("private"), failGet: true}
	_, err := Share(context.Background(), fq, 7, "aaaabbbbcccc")
	ae := appErrOf(t, err)
	if ae.HTTPCode != 500 || ae.Code != "INTERNAL_ERROR" {
		t.Fatalf("expected 500 INTERNAL_ERROR on lookup failure, got %d %s", ae.HTTPCode, ae.Code)
	}
}

func TestShareSuccessStillWorks(t *testing.T) {
	fq := &kungfuFakeQuerier{k: ownedKungfu("private")}
	res, err := Share(context.Background(), fq, 7, "aaaabbbbcccc")
	if err != nil {
		t.Fatalf("expected share success, got %v", err)
	}
	if res["visibility"] != "public" {
		t.Fatalf("expected public, got %v", res["visibility"])
	}
	// log write succeeded, one share entry
	found := false
	for _, l := range fq.logs {
		if l.action == "share" && l.success {
			found = true
		}
	}
	if !found {
		t.Fatal("expected share audit entry")
	}
}

func TestShareIdempotentAlreadyPublic(t *testing.T) {
	fq := &kungfuFakeQuerier{k: ownedKungfu("public")}
	res, err := Share(context.Background(), fq, 7, "aaaabbbbcccc")
	if err != nil {
		t.Fatalf("expected idempotent success, got %v", err)
	}
	if res["message"] != "Already public. Share this code with other agents." {
		t.Fatalf("unexpected message: %v", res["message"])
	}
}
