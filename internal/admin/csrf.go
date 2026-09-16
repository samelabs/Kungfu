package admin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
)

// CSRF foundation: the Admin plane is a cookie-authenticated control
// plane, so every mutating request (POST/PUT/PATCH/DELETE under
// /api/admin/** except the login POST) must present X-CSRF-Token.
//
// The CSRF token is HMAC-SHA256 over a DOMAIN-SEPARATED value derived
// from the CURRENT raw admin session token and the SessionSecret:
//
//	HMAC-SHA256("admin-csrf:"+<raw session token>, SessionSecret)
//
// It is never written to the database; it rotates with the session.

const (
	// CSRFDomainPrefix provides domain separation from every other
	// HMAC use of the session secret.
	CSRFDomainPrefix = "admin-csrf:"

	// CSRFHeaderName is the required request header.
	CSRFHeaderName = "X-CSRF-Token"
)

// CSRFToken derives the CSRF token for a raw admin session token.
func CSRFToken(rawSessionToken, sessionSecret string) string {
	mac := hmac.New(sha256.New, []byte(sessionSecret))
	mac.Write([]byte(CSRFDomainPrefix + rawSessionToken))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// VerifyCSRF checks the request's X-CSRF-Token against the expected
// value in constant time.
func VerifyCSRF(r *http.Request, rawSessionToken, sessionSecret string) bool {
	presented := r.Header.Get(CSRFHeaderName)
	if presented == "" {
		return false
	}
	expected := CSRFToken(rawSessionToken, sessionSecret)
	return hmac.Equal([]byte(presented), []byte(expected))
}
