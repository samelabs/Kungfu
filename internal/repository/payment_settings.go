package repository

// Payment provider settings (013). One row per provider; the secrets
// columns hold sealed ciphertext only — sealing and opening happen in
// the callers (internal/security), never here.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"kungfu.md/internal/pg"
)

// Purposes bound into the sealed secret blobs (AEAD additional data):
// the admin writer seals and the payment runtime opens with these.
const (
	CreemAPIKeyPurpose        = "creem.api_key"
	CreemWebhookSecretPurpose = "creem.webhook_secret"
)

// CreemSettingsRow is the stored Creem configuration.
type CreemSettingsRow struct {
	Enabled          bool
	Mode             string
	SuccessURL       string
	PackagesJSON     []byte
	APIKeyEnc        []byte
	WebhookSecretEnc []byte
	UpdatedBy        *int64
	UpdatedByName    *string
	UpdatedAt        time.Time
}

const creemSettingsColumns = `s.enabled, s.mode, s.success_url, s.packages, s.provider_key_enc,
	       s.webhook_secret_enc, s.updated_by, a.username, s.updated_at`

func scanCreemSettings(row pgx.Row) (*CreemSettingsRow, error) {
	var r CreemSettingsRow
	err := row.Scan(&r.Enabled, &r.Mode, &r.SuccessURL, &r.PackagesJSON, &r.APIKeyEnc,
		&r.WebhookSecretEnc, &r.UpdatedBy, &r.UpdatedByName, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetCreemSettings returns the stored row, or nil when never configured.
func GetCreemSettings(ctx context.Context, q pg.Querier) (*CreemSettingsRow, error) {
	return scanCreemSettings(q.QueryRow(ctx, `
		SELECT `+creemSettingsColumns+`
		FROM tb_payment_provider_settings s
		LEFT JOIN tb_admins a ON a.id = s.updated_by
		WHERE s.provider = 'creem'`))
}

// LockCreemSettings reads the row FOR UPDATE inside a transaction so a
// concurrent save cannot interleave between read-modify-write.
func LockCreemSettings(ctx context.Context, q pg.Querier) (*CreemSettingsRow, error) {
	return scanCreemSettings(q.QueryRow(ctx, `
		SELECT `+creemSettingsColumns+`
		FROM tb_payment_provider_settings s
		LEFT JOIN tb_admins a ON a.id = s.updated_by
		WHERE s.provider = 'creem'
		FOR UPDATE OF s`))
}

// UpsertCreemSettings writes the full row.
func UpsertCreemSettings(ctx context.Context, q pg.Querier, r *CreemSettingsRow) error {
	_, err := q.Exec(ctx, `
		INSERT INTO tb_payment_provider_settings
		    (provider, enabled, mode, success_url, packages, provider_key_enc, webhook_secret_enc, updated_by, updated_at)
		VALUES ('creem', $1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (provider) DO UPDATE SET
		    enabled = EXCLUDED.enabled,
		    mode = EXCLUDED.mode,
		    success_url = EXCLUDED.success_url,
		    packages = EXCLUDED.packages,
		    provider_key_enc = EXCLUDED.provider_key_enc,
		    webhook_secret_enc = EXCLUDED.webhook_secret_enc,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = NOW()`,
		r.Enabled, r.Mode, r.SuccessURL, r.PackagesJSON, r.APIKeyEnc, r.WebhookSecretEnc, r.UpdatedBy)
	return err
}
