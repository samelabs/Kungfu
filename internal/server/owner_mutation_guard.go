package server

import (
	"mime"
	"net/http"
)

// ownerMutationJSONGate is the ONE canonical Owner mutation request-
// shape gate (S6.4). Browser Owner mutations must be
// application/json: a cross-site HTML form (urlencoded / multipart /
// text/plain or no content type at all) cannot satisfy it, and a
// cross-origin fetch that DOES set application/json is forced through
// the browser's CORS preflight — which this application does not
// grant credentially. SameSite=Lax stays as defense-in-depth; this
// gate is the explicit request-forgery boundary for same-site sibling
// origins.
//
// The gate validates ONLY the media type. It never reads or consumes
// the request body and never duplicates JSON decoding — existing
// handlers and bounded-body mechanisms remain the body authorities.
func ownerMutationJSONGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if ct == "" {
			unsupportedOwnerMediaType(w)
			return
		}
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			unsupportedOwnerMediaType(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ownerMutation wraps an Owner unsafe-route handler with the JSON gate.
func ownerMutation(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ownerMutationJSONGate(h).ServeHTTP(w, r)
	}
}

func unsupportedOwnerMediaType(w http.ResponseWriter) {
	ErrorResponse(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE",
		"Owner mutations require Content-Type: application/json", nil)
}
