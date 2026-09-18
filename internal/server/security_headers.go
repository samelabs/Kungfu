package server

import (
	"net/http"
)

// securityHeadersMiddleware is the ONE global baseline security-header
// authority (S6.5). It sets exactly four headers before downstream
// handlers execute, so normal responses, errors, static assets, and
// recovered failures all inherit them. MIME and cache-policy
// ownership stay with the existing content-type/static helpers — this
// middleware never touches Content-Type or Cache-Control.
//
// Deliberately NOT set (deferred to dedicated tickets):
//   - Content-Security-Policy — inline script/style surfaces exist in
//     current templates; a strict CSP needs a front-end decision.
//   - Strict-Transport-Security — no production TLS-termination /
//     hostname durability contract yet.
//   - X-XSS-Protection / COOP / COEP / CORP / CORS — out of scope.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
