package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
)

// OwnerSession manages owner authentication via signed cookies.
// This replaces PHP's native session handling with a stateless HMAC-signed cookie.
//
// Cookie format: base64(JSON{bot_id, exp}) + "." + base64(HMAC-SHA256)
// Cookie name: "kf_owner" (doesn't need to match PHP's PHPSESSID since JS uses fetch with credentials:same-origin)

const (
	OwnerCookieName    = "kf_owner"
	OwnerSessionMaxAge = 24 * time.Hour
)

// OwnerSessionData is the payload encoded in the cookie.
type OwnerSessionData struct {
	BotID int64 `json:"bid"`
	Exp   int64 `json:"exp"`
}

// OwnerLookupFunc is a function that finds a bot by ID for owner session.
type OwnerLookupFunc func(ctx context.Context, botID int64) (*model.Bot, error)

// RequireOwnerSession returns the bot from the session, or an error.
// PHP: OwnerSession::require()
func RequireOwnerSession(ctx context.Context, lookupFn OwnerLookupFunc, r *http.Request, secret string) (*model.Bot, error) {
	session := GetOwnerSession(r, secret)
	if session == nil {
		return nil, errors.New(401, "OWNER_LOGIN_REQUIRED", "Owner login required")
	}

	bot, err := lookupFn(ctx, session.BotID)
	if err != nil || bot == nil {
		return nil, errors.New(401, "OWNER_LOGIN_REQUIRED", "Owner login required")
	}

	return bot, nil
}

// SetOwnerSessionCookie sets the authentication cookie on the response.
func SetOwnerSessionCookie(w http.ResponseWriter, botID int64, secret string, isHTTPS bool) {
	data := OwnerSessionData{
		BotID: botID,
		Exp:   time.Now().Add(OwnerSessionMaxAge).Unix(),
	}

	payload, _ := json.Marshal(data)
	encoded := base64.RawURLEncoding.EncodeToString(payload)

	sig := signCookie(encoded, secret)
	value := encoded + "." + sig

	http.SetCookie(w, &http.Cookie{
		Name:     OwnerCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(OwnerSessionMaxAge.Seconds()),
		Secure:   isHTTPS,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearOwnerSessionCookie clears the authentication cookie.
func ClearOwnerSessionCookie(w http.ResponseWriter, isHTTPS bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     OwnerCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Now().Add(-time.Hour),
		Secure:   isHTTPS,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// GetOwnerSession extracts and validates the session from request cookies.
// Returns nil if no valid session exists.
func GetOwnerSession(r *http.Request, secret string) *OwnerSessionData {
	cookie, err := r.Cookie(OwnerCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}

	parts := strings.SplitN(cookie.Value, ".", 2)
	if len(parts) != 2 {
		return nil
	}

	encoded := parts[0]
	sig := parts[1]

	// Verify signature
	expectedSig := signCookie(encoded, secret)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil
	}

	// Decode payload
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}

	var data OwnerSessionData
	if err := json.Unmarshal(payload, &data); err != nil {
		return nil
	}

	// Check expiry
	if time.Now().Unix() > data.Exp {
		return nil
	}

	return &data
}

// signCookie creates an HMAC-SHA256 signature for the cookie value.
func signCookie(encoded, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// IsHTTPS checks if the request is over HTTPS.
func IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	// Check common proxy headers
	if xfProto := r.Header.Get("X-Forwarded-Proto"); xfProto == "https" {
		return true
	}
	return false
}
