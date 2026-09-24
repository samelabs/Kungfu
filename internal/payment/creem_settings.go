package payment

// Creem runtime settings come from the database row managed in the
// platform admin (013), not from environment variables. The payment
// domain only READS the row: it opens the sealed secrets with the
// server's SecretBox and resolves the fixed packages.

import (
	"context"
	"encoding/json"
	"log"

	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

// CreemSettings is the decrypted, ready-to-use provider configuration.
type CreemSettings struct {
	// CheckoutEnabled gates NEW purchases only. Webhooks for existing
	// payments (completion, refunds, disputes) are processed whenever
	// the settings are readable, so switching checkout off never drops
	// a reversal.
	CheckoutEnabled bool
	APIKey          string
	WebhookSecret   string
	Mode            string // "test" | "prod"
	SuccessURL      string
	Packages        map[string]CreemPackageSpec
}

// CreemAPIBase maps a mode to the official API host.
func CreemAPIBase(mode string) string {
	if mode == "prod" {
		return "https://api.creem.io"
	}
	return "https://test-api.creem.io"
}

// LoadCreemSettings returns the stored settings, or nil when payments
// are not configured or the secrets cannot be opened (missing or
// rotated SETTINGS_ENC_KEY). nil means "payments unavailable" — every
// caller already fails closed on that.
func LoadCreemSettings(ctx context.Context, q pg.Querier, box *security.SecretBox) (*CreemSettings, error) {
	row, err := repository.GetCreemSettings(ctx, q)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	apiKey, errK := box.Open(row.APIKeyEnc, repository.CreemAPIKeyPurpose)
	secret, errS := box.Open(row.WebhookSecretEnc, repository.CreemWebhookSecretPurpose)
	if errK != nil || errS != nil {
		log.Printf("creem: payments enabled in settings but secrets cannot be opened (check SETTINGS_ENC_KEY); payments unavailable")
		return nil, nil
	}
	var pkgs []CreemPackageSpec
	if err := json.Unmarshal(row.PackagesJSON, &pkgs); err != nil || len(pkgs) == 0 {
		log.Printf("creem: stored packages unreadable or empty; payments unavailable")
		return nil, nil
	}
	out := &CreemSettings{
		CheckoutEnabled: row.Enabled,
		APIKey:          apiKey,
		WebhookSecret:   secret,
		Mode:            row.Mode,
		SuccessURL:      row.SuccessURL,
		Packages:        make(map[string]CreemPackageSpec, len(pkgs)),
	}
	for _, p := range pkgs {
		out.Packages[p.Code] = p
	}
	return out, nil
}
