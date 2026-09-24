package admin

// Payment provider settings (013). The admin plane is the ONLY writer
// of the Creem settings row. Secrets are sealed with the SETTINGS_ENC_KEY
// SecretBox before they reach the repository, are never returned to the
// UI (masked last-4 only), and never enter the audit trail — audit facts
// record only whether each secret changed.

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

const permPaymentSettings = "settings.payment.manage"

// CreemPackage is one fixed credits package backed by one Creem product.
type CreemPackage struct {
	Code      string `json:"code"`
	ProductID string `json:"product_id"`
	Credits   int64  `json:"credits"`
}

// CreemSettingsInput is a full settings save. Empty APIKey /
// WebhookSecret keep the currently stored value.
type CreemSettingsInput struct {
	Enabled       bool
	Mode          string
	SuccessURL    string
	Packages      []CreemPackage
	APIKey        string
	WebhookSecret string
}

// CreemSettingsView is the masked read model.
type CreemSettingsView struct {
	Configured          bool
	EncryptionReady     bool
	Enabled             bool
	Mode                string
	SuccessURL          string
	Packages            []CreemPackage
	APIKeyMasked        string
	WebhookSecretMasked string
	SecretsReadable     bool
	UpdatedBy           string
	UpdatedAt           *time.Time
}

// GetCreemSettings returns the masked settings view.
func GetCreemSettings(ctx context.Context, pool *pg.Pool, principal *Principal, box *security.SecretBox) (*CreemSettingsView, error) {
	if err := RequirePermission(ctx, pool, principal, permPaymentSettings); err != nil {
		return nil, err
	}
	row, err := repository.GetCreemSettings(ctx, pool)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Database error")
	}
	v := &CreemSettingsView{EncryptionReady: box != nil, Mode: "test"}
	if row == nil {
		return v, nil
	}
	v.Configured = true
	v.Enabled = row.Enabled
	v.Mode = row.Mode
	v.SuccessURL = row.SuccessURL
	v.UpdatedAt = &row.UpdatedAt
	if row.UpdatedByName != nil {
		v.UpdatedBy = *row.UpdatedByName
	}
	if err := json.Unmarshal(row.PackagesJSON, &v.Packages); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Stored packages are unreadable")
	}
	apiKey, errK := box.Open(row.APIKeyEnc, repository.CreemAPIKeyPurpose)
	secret, errS := box.Open(row.WebhookSecretEnc, repository.CreemWebhookSecretPurpose)
	v.SecretsReadable = errK == nil && errS == nil
	v.APIKeyMasked = security.MaskSecret(apiKey)
	v.WebhookSecretMasked = security.MaskSecret(secret)
	return v, nil
}

var packageCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidateCreemSettings enforces the fixed-package contract: known mode,
// absolute http(s) success URL, at least one package, unique codes AND
// unique product ids (one fiat product must never carry two credit
// entitlements), positive whole credits.
func ValidateCreemSettings(in *CreemSettingsInput) error {
	bad := func(msg string) error { return errors.New(400, "INVALID_SETTINGS", msg) }
	if in.Mode != "test" && in.Mode != "prod" {
		return bad("Mode must be test or prod")
	}
	u, err := url.Parse(strings.TrimSpace(in.SuccessURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return bad("Success URL must be an absolute http or https URL")
	}
	if len(in.Packages) == 0 {
		return bad("Define at least one credits package")
	}
	codes := map[string]bool{}
	products := map[string]string{}
	for _, p := range in.Packages {
		if !packageCodeRe.MatchString(p.Code) {
			return bad("Package code " + quote(p.Code) + " must be 1-32 chars of a-z 0-9 _ -")
		}
		if strings.TrimSpace(p.ProductID) == "" {
			return bad("Package " + quote(p.Code) + " needs a Creem product id")
		}
		if p.Credits <= 0 {
			return bad("Package " + quote(p.Code) + " credits must be a positive whole number")
		}
		if codes[p.Code] {
			return bad("Duplicate package code " + quote(p.Code))
		}
		if other, dup := products[p.ProductID]; dup {
			return bad("Product " + p.ProductID + " is used by both " + quote(other) + " and " + quote(p.Code))
		}
		codes[p.Code] = true
		products[p.ProductID] = p.Code
	}
	return nil
}

func quote(s string) string { return `"` + s + `"` }

// SaveCreemSettings validates, seals, stores and audits a settings save
// in ONE transaction.
func SaveCreemSettings(ctx context.Context, pool *pg.Pool, principal *Principal, box *security.SecretBox, in CreemSettingsInput) error {
	if err := RequirePermission(ctx, pool, principal, permPaymentSettings); err != nil {
		return err
	}
	if box == nil {
		return errors.New(409, "ENCRYPTION_KEY_MISSING", "SETTINGS_ENC_KEY is not configured on the server; secrets cannot be stored")
	}
	in.Mode = strings.TrimSpace(in.Mode)
	in.SuccessURL = strings.TrimSpace(in.SuccessURL)
	in.APIKey = strings.TrimSpace(in.APIKey)
	in.WebhookSecret = strings.TrimSpace(in.WebhookSecret)
	for i := range in.Packages {
		in.Packages[i].Code = strings.TrimSpace(in.Packages[i].Code)
		in.Packages[i].ProductID = strings.TrimSpace(in.Packages[i].ProductID)
	}
	if err := ValidateCreemSettings(&in); err != nil {
		return err
	}
	pkgJSON, err := json.Marshal(in.Packages)
	if err != nil {
		return errors.New(500, "INTERNAL_ERROR", "Could not encode packages")
	}

	entry := &AuditEntry{
		Actor:      principal.Admin,
		Action:     "settings.payment.update",
		TargetType: "payment_provider",
		TargetID:   "creem",
		Success:    true,
	}
	return WithAuditTx(ctx, pool, entry, func(ctx context.Context, tx pg.Querier) error {
		cur, err := repository.LockCreemSettings(ctx, tx)
		if err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		next := &repository.CreemSettingsRow{
			Enabled:      in.Enabled,
			Mode:         in.Mode,
			SuccessURL:   in.SuccessURL,
			PackagesJSON: pkgJSON,
			UpdatedBy:    &principal.Admin.ID,
		}
		apiKeyChanged := in.APIKey != ""
		secretChanged := in.WebhookSecret != ""
		if apiKeyChanged {
			if next.APIKeyEnc, err = box.Seal(in.APIKey, repository.CreemAPIKeyPurpose); err != nil {
				return errors.New(500, "INTERNAL_ERROR", "Could not seal API key")
			}
		} else if cur != nil {
			next.APIKeyEnc = cur.APIKeyEnc
		}
		if secretChanged {
			if next.WebhookSecretEnc, err = box.Seal(in.WebhookSecret, repository.CreemWebhookSecretPurpose); err != nil {
				return errors.New(500, "INTERNAL_ERROR", "Could not seal webhook secret")
			}
		} else if cur != nil {
			next.WebhookSecretEnc = cur.WebhookSecretEnc
		}
		if len(next.APIKeyEnc) == 0 || len(next.WebhookSecretEnc) == 0 {
			return errors.New(400, "INVALID_SETTINGS", "API key and webhook secret are required on first save")
		}
		// Kept secrets must still open under the current key, otherwise
		// the saved configuration would be silently unusable.
		if _, err := box.Open(next.APIKeyEnc, repository.CreemAPIKeyPurpose); err != nil {
			return errors.New(400, "INVALID_SETTINGS", "The stored API key cannot be decrypted with the current SETTINGS_ENC_KEY; enter it again")
		}
		if _, err := box.Open(next.WebhookSecretEnc, repository.CreemWebhookSecretPurpose); err != nil {
			return errors.New(400, "INVALID_SETTINGS", "The stored webhook secret cannot be decrypted with the current SETTINGS_ENC_KEY; enter it again")
		}
		if err := repository.UpsertCreemSettings(ctx, tx, next); err != nil {
			return errors.New(500, "INTERNAL_ERROR", "Database error")
		}
		if cur != nil {
			var prevPkgs []CreemPackage
			_ = json.Unmarshal(cur.PackagesJSON, &prevPkgs)
			entry.Before = map[string]interface{}{
				"enabled": cur.Enabled, "mode": cur.Mode, "success_url": cur.SuccessURL, "packages": prevPkgs,
			}
		}
		entry.After = map[string]interface{}{
			"enabled": in.Enabled, "mode": in.Mode, "success_url": in.SuccessURL, "packages": in.Packages,
			"api_key_changed": apiKeyChanged, "webhook_secret_changed": secretChanged,
		}
		return nil
	})
}
