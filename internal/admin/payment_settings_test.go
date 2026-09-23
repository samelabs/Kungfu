package admin

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

func settingsBox(t *testing.T, hexByte string) *security.SecretBox {
	t.Helper()
	key, err := security.ParseSecretBoxKey(strings.Repeat(hexByte, 32))
	if err != nil {
		t.Fatal(err)
	}
	b, err := security.NewSecretBox(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func principalFor(t *testing.T, db *pg.Pool, username, password string) *Principal {
	t.Helper()
	res := loginForTest(t, db, username, password)
	p, err := ResolveSession(context.Background(), db, res.RawToken)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return p
}

func validCreemInput() CreemSettingsInput {
	return CreemSettingsInput{
		Enabled:       true,
		Mode:          "test",
		SuccessURL:    "https://kungfu.md/owner/credits?payment=success",
		Packages:      []CreemPackage{{Code: "starter", ProductID: "prod_a", Credits: 1000}},
		APIKey:        "creem_test_SECRETKEY9876",
		WebhookSecret: "whsec_SECRETHOOK5432",
	}
}

func appCode(err error) string {
	if ae, ok := errors.IsAppError(err); ok {
		return ae.Code
	}
	return ""
}

func TestCreemSettingsSaveSealsSecretsAndAuditsWithoutThem(t *testing.T) {
	db := createPrivateDB(t)
	ctx := context.Background()
	seedAdminWithRole(t, db, "payroot", "password-123")
	p := principalFor(t, db, "payroot", "password-123")
	box := settingsBox(t, "a1")

	if err := SaveCreemSettings(ctx, db, p, box, validCreemInput()); err != nil {
		t.Fatalf("save: %v", err)
	}
	row, err := repository.GetCreemSettings(ctx, db)
	if err != nil || row == nil {
		t.Fatalf("row: %v %v", row, err)
	}
	if bytes.Contains(row.APIKeyEnc, []byte("SECRETKEY")) || bytes.Contains(row.WebhookSecretEnc, []byte("SECRETHOOK")) {
		t.Fatal("secrets stored in plaintext")
	}
	var auditBlob string
	if err := db.QueryRow(ctx, `SELECT COALESCE(before_json::text,'') || COALESCE(after_json::text,'') || COALESCE(metadata_json::text,'')
		FROM tb_admin_audit_logs WHERE action = 'settings.payment.update' ORDER BY id DESC LIMIT 1`).Scan(&auditBlob); err != nil {
		t.Fatalf("audit row: %v", err)
	}
	if strings.Contains(auditBlob, "SECRET") {
		t.Fatalf("audit leaks a secret: %s", auditBlob)
	}
	if !strings.Contains(auditBlob, `"api_key_changed": true`) && !strings.Contains(auditBlob, `"api_key_changed":true`) {
		t.Fatalf("audit lacks change flag: %s", auditBlob)
	}

	v, err := GetCreemSettings(ctx, db, p, box)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Configured || !v.Enabled || !v.SecretsReadable || v.APIKeyMasked != "••••9876" || v.WebhookSecretMasked != "••••5432" {
		t.Fatalf("view = %+v", v)
	}

	// Empty secrets on a later save keep the stored ones.
	in := validCreemInput()
	in.APIKey, in.WebhookSecret, in.Enabled = "", "", false
	if err := SaveCreemSettings(ctx, db, p, box, in); err != nil {
		t.Fatalf("second save: %v", err)
	}
	v, _ = GetCreemSettings(ctx, db, p, box)
	if v.Enabled || v.APIKeyMasked != "••••9876" {
		t.Fatalf("kept-secret save view = %+v", v)
	}

	// A different key cannot read them and must refuse a keep-secret save.
	other := settingsBox(t, "b2")
	v, _ = GetCreemSettings(ctx, db, p, other)
	if v.SecretsReadable {
		t.Fatal("secrets readable under the wrong key")
	}
	if err := SaveCreemSettings(ctx, db, p, other, in); appCode(err) != "INVALID_SETTINGS" {
		t.Fatalf("wrong-key keep save err = %v", err)
	}
}

func TestCreemSettingsGuards(t *testing.T) {
	db := createPrivateDB(t)
	ctx := context.Background()
	seedAdminWithRole(t, db, "payroot2", "password-123")
	root := principalFor(t, db, "payroot2", "password-123")
	box := settingsBox(t, "c3")

	// No encryption key: refuse to store secrets.
	if err := SaveCreemSettings(ctx, db, root, nil, validCreemInput()); appCode(err) != "ENCRYPTION_KEY_MISSING" {
		t.Fatalf("nil box err = %v", err)
	}
	// First save needs both secrets.
	in := validCreemInput()
	in.WebhookSecret = ""
	if err := SaveCreemSettings(ctx, db, root, box, in); appCode(err) != "INVALID_SETTINGS" {
		t.Fatalf("missing secret err = %v", err)
	}
	// Permission.
	plain := seedAdmin(t, db, "password-123")
	np := principalFor(t, db, plain.Username, "password-123")
	if err := SaveCreemSettings(ctx, db, np, box, validCreemInput()); appCode(err) != "FORBIDDEN" {
		t.Fatalf("no-permission save err = %v", err)
	}
	if _, err := GetCreemSettings(ctx, db, np, box); appCode(err) != "FORBIDDEN" {
		t.Fatalf("no-permission read err = %v", err)
	}
}

func TestValidateCreemSettings(t *testing.T) {
	cases := map[string]func(*CreemSettingsInput){
		"mode":           func(in *CreemSettingsInput) { in.Mode = "live" },
		"url":            func(in *CreemSettingsInput) { in.SuccessURL = "kungfu.md/x" },
		"no packages":    func(in *CreemSettingsInput) { in.Packages = nil },
		"bad code":       func(in *CreemSettingsInput) { in.Packages[0].Code = "Has Space" },
		"no product":     func(in *CreemSettingsInput) { in.Packages[0].ProductID = " " },
		"zero credits":   func(in *CreemSettingsInput) { in.Packages[0].Credits = 0 },
		"dup code":       func(in *CreemSettingsInput) { in.Packages = append(in.Packages, CreemPackage{"starter", "prod_b", 5}) },
		"shared product": func(in *CreemSettingsInput) { in.Packages = append(in.Packages, CreemPackage{"pro", "prod_a", 5}) },
	}
	for name, mut := range cases {
		in := validCreemInput()
		mut(&in)
		if err := ValidateCreemSettings(&in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := validCreemInput()
	if err := ValidateCreemSettings(&ok); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
}
