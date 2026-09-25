package server

// The single strict inbound request-body reader.
//
// readBoundedRequestBody enforces the caller's cap on the REAL read
// path (chunked/unknown-length bodies included — Content-Length is
// never the authority). Exactly maxBytes is legal; one byte more is a
// hard error; any underlying read error propagates. It performs no
// JSON decoding and no business error mapping — the endpoint keeps
// its existing read-failure contract.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// errRequestBodyTooLarge marks a body strictly larger than the cap.
var errRequestBodyTooLarge = errors.New("request body exceeds limit")

// readBoundedRequestBody reads at most maxBytes bytes from r.Body and
// returns them in full. It fails closed when the body is larger than
// maxBytes (maxBytes+1 read => error) or when the underlying read
// errors — never returning a silently truncated prefix as if it were
// the complete body.
func readBoundedRequestBody(r *http.Request, maxBytes int64) ([]byte, error) {
	// MaxBytesReader makes the real read path enforce the cap and
	// surfaces overflow as "http: request body too large"; reading
	// cap+1 through it distinguishes ==cap from >cap without a
	// bespoke counting reader.
	bounded := http.MaxBytesReader(nil, r.Body, maxBytes)
	// Read one extra byte's worth of budget so an exactly-max body
	// reads clean while a larger one trips the reader.
	probe := io.LimitReader(bounded, maxBytes+1)
	data, err := io.ReadAll(probe)
	if err != nil {
		// MaxBytesReader errors on overflow; underlying I/O errors
		// propagate as-is. Both are fail-closed.
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errRequestBodyTooLarge
	}
	return data, nil
}

// parseCredits extracts a whole-integer Credit value from a decoded
// JSON field. EXACT integer parsing only — no Credit value ever passes
// through float64 (which silently corrupts integers above 2^53).
// json.Number (UseNumber bodies) and canonical integer strings parse
// via strconv-grade exact integer semantics; fractional presentations
// ("1000.5", "0.0001", "9007199254740993.0") are REJECTED — never
// rounded, never float-converted. Returns (value, present, ok).
func parseCredits(v interface{}) (int64, bool, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false, true
	case json.Number:
		// Lossless path (body parsed with UseNumber): the exact source
		// text. Int64() rejects fractions and >int64 range alike.
		if n, err := t.Int64(); err == nil {
			return n, true, true
		}
		return 0, true, false
	case string:
		// The ONE canonical parser (shared with jsonCredits): no
		// trim, no float, no coercion. The browser may trim user
		// input once as UX normalization before sending; non-canonical
		// wire input is rejected here, never silently repaired.
		if n, err := parseCanonicalEconInt(t); err == nil {
			return n, true, true
		}
		return 0, true, false
	default:
		return 0, true, false
	}
}
