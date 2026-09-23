package server

// Platform settings — payment provider (Creem).
//
//	GET /api/samelabs/settings/payment   masked view
//	PUT /api/samelabs/settings/payment   full save (CSRF); empty api_key /
//	                                     webhook_secret keep the stored value

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"kungfu.md/internal/admin"
)

func creemSettingsDTO(v *admin.CreemSettingsView) map[string]interface{} {
	pkgs := v.Packages
	if pkgs == nil {
		pkgs = []admin.CreemPackage{}
	}
	var updatedAt interface{}
	if v.UpdatedAt != nil {
		updatedAt = v.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return map[string]interface{}{
		"configured":            v.Configured,
		"encryption_ready":      v.EncryptionReady,
		"secrets_readable":      v.SecretsReadable,
		"enabled":               v.Enabled,
		"mode":                  v.Mode,
		"success_url":           v.SuccessURL,
		"packages":              pkgs,
		"api_key_masked":        v.APIKeyMasked,
		"webhook_secret_masked": v.WebhookSecretMasked,
		"updated_by":            v.UpdatedBy,
		"updated_at":            updatedAt,
	}
}

func (s *Server) handleAdminPaymentSettingsGet(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminAuth(r)
	if err != nil {
		handleAppError(w, err)
		return
	}
	v, err := admin.GetCreemSettings(r.Context(), s.Pool, principal, s.secretBox)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, creemSettingsDTO(v), "")
}

func (s *Server) handleAdminPaymentSettingsPut(w http.ResponseWriter, r *http.Request) {
	principal, err := s.requireAdminMutation(r, "")
	if err != nil {
		handleAppError(w, err)
		return
	}
	var body struct {
		Enabled       bool                 `json:"enabled"`
		Mode          string               `json:"mode"`
		SuccessURL    string               `json:"success_url"`
		Packages      []admin.CreemPackage `json:"packages"`
		APIKey        string               `json:"api_key"`
		WebhookSecret string               `json:"webhook_secret"`
	}
	// Strict typed decode: unknown fields and fractional credits are
	// rejected (int64 never accepts 1000.5), and exactly one JSON value.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		InvalidJSON(w, "Request body too large or unreadable")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || dec.More() {
		InvalidJSON(w, "Request body must be a single valid settings object")
		return
	}
	if err := admin.SaveCreemSettings(r.Context(), s.Pool, principal, s.secretBox, admin.CreemSettingsInput{
		Enabled:       body.Enabled,
		Mode:          body.Mode,
		SuccessURL:    body.SuccessURL,
		Packages:      body.Packages,
		APIKey:        body.APIKey,
		WebhookSecret: body.WebhookSecret,
	}); err != nil {
		handleAppError(w, err)
		return
	}
	s.invalidateCreemSettings()
	v, err := admin.GetCreemSettings(r.Context(), s.Pool, principal, s.secretBox)
	if err != nil {
		handleAppError(w, err)
		return
	}
	SuccessResponse(w, creemSettingsDTO(v), "Payment settings saved")
}
