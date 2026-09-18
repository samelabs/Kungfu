package server

// R2.2: the single strict inbound request-body reader.
//
// readBoundedRequestBody enforces the caller's cap on the REAL read
// path (chunked/unknown-length bodies included — Content-Length is
// never the authority). Exactly maxBytes is legal; one byte more is a
// hard error; any underlying read error propagates. It performs no
// JSON decoding and no business error mapping — the endpoint keeps
// its existing read-failure contract.

import (
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
