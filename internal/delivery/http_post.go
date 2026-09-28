package delivery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"
)

// PostResult holds the result of a POST request.
type PostResult struct {
	Success      bool
	Sent         bool    // true once the request headers were written to the wire (httptrace WroteHeaders)
	ResponseCode *int    // nil if no response received (network error)
	ResponseBody *string // nil if no response body
	ErrorCode    string  // empty if success
	ErrorMessage string  // empty if success
}

// PostJSON sends a JSON POST request to a URL.
// Contract:
//   - Content-Type: application/json
//   - Content-Length header set explicitly
//   - 10 second total timeout
//   - 5 second connect timeout
//   - Does NOT follow redirects (returns the raw response)

// errorConfig controls the error codes/messages for different failure types:
//   - networkCode / networkMessage / networkMessagePrefix: for transport errors
//   - rejectedCode / rejectedMessage: for non-2xx HTTP responses
type ErrorConfig struct {
	NetworkCode          string
	NetworkMessage       string
	NetworkMessagePrefix string
	RejectedCode         string
	RejectedMessage      string
}

// PostAPI outbound timeouts — this package is the single owner of these
// values (10s total request, 5s connect).
const (
	// postAPIRequestTimeout bounds a whole outbound PostAPI request.
	postAPIRequestTimeout = 10 * time.Second
	// postAPIConnectTimeout bounds establishing the TCP connection.
	postAPIConnectTimeout = 5 * time.Second
)

// sharedClient is built by ssrf.go's init() on the hardened dial
// authority (validate-then-dial, single DNS resolution). Redirects
// forbidden; 10s total / 5s connect timeouts unchanged.
var sharedClient *http.Client

// requestTimeoutOverride, when positive, bounds each PostJSON request
// INSTEAD of the 10-second production budget. Tests only — production
// never sets it; the §11 value stays the authority.
var requestTimeoutOverride time.Duration

// SetRequestTimeoutForTest overrides the per-request delivery timeout
// for the whole process (tests only; zero restores the §11 10s).
func SetRequestTimeoutForTest(d time.Duration) { requestTimeoutOverride = d }

// maxResponseBytes is the response-body read cap (spec §7.1: 64 KB).
// Transport reads at most this + 1 byte — bounded I/O.
const maxResponseBytes = 65535

// PostJSON sends a POST request with a JSON body and arbitrary
// headers, classifying failures via errCfg. It is the single
// transport path shared by every outbound PostAPI call (SSRF dial
// authority, 10s/5s timeouts, bounded response read); callers add
// only their headers.
//
// Contract:
//   - Content-Type: application/json (always set here)
//   - Content-Length header set explicitly
//   - 10 second total timeout, 5 second connect timeout
//   - Does NOT follow redirects (returns the raw response)
//
// The caller's context governs the request lifecycle: cancellation or
// deadline of ctx terminates the transport work via
// http.NewRequestWithContext. The 10s client timeout remains a lower
// safety net, NOT the primary budget.
func PostJSON(ctx context.Context, url string, body []byte, headers map[string]string, errCfg ErrorConfig) PostResult {
	if requestTimeoutOverride > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeoutOverride)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return PostResult{
			Success:      false,
			ErrorCode:    errCfg.NetworkCode,
			ErrorMessage: errCfg.NetworkMessagePrefix + err.Error(),
		}
	}

	// sent latches true the first time the request HEADERS are written
	// to the wire (httptrace WroteHeaders) and never goes back — the
	// single transport fact behind IsDefinitiveNotDelivered. Latching at
	// the headers (not after the full body) classifies a request that
	// broke off mid-body as UNCERTAIN per §7.2: the receiver may already
	// have acted on it. WroteHeaders runs on the transport's write
	// goroutine, hence the atomic.
	var sent atomic.Bool
	traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaders: func() {
			sent.Store(true)
		},
	})
	req = req.WithContext(traceCtx)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := sharedClient.Do(req)
	if err != nil {
		// Network error (curl error equivalent)
		var respCode *int
		// If we got a response, extract the code (Go gives us this in some error cases)
		if resp != nil && resp.StatusCode > 0 {
			code := resp.StatusCode
			respCode = &code
			resp.Body.Close()
		}
		return PostResult{
			Success:      false,
			Sent:         sent.Load(),
			ResponseCode: respCode,
			ErrorCode:    errCfg.NetworkCode,
			ErrorMessage: errCfg.NetworkMessagePrefix + err.Error(),
		}
	}
	defer resp.Body.Close()

	_, respBody := readBounded(resp.Body)
	respCode := resp.StatusCode

	// Check for non-2xx (rejected)
	if respCode < 200 || respCode >= 300 {
		return PostResult{
			Success:      false,
			ResponseCode: &respCode,
			ResponseBody: respBody,
			ErrorCode:    errCfg.RejectedCode,
			ErrorMessage: errCfg.RejectedMessage,
		}
	}

	return PostResult{
		Success:      true,
		ResponseCode: &respCode,
		ResponseBody: respBody,
		ErrorCode:    "",
		ErrorMessage: "",
	}
}

// IsDefinitiveNotDelivered classifies a failed PostResult as PROVABLY
// not reaching the receiver: no response code and the request headers
// were never written (SSRF refusal, DNS failure, connect refused,
// connect timeout, TLS handshake or certificate failure — everything
// httptrace never reported as WroteHeaders). Once the headers left
// the wire, a later timeout, disconnect or partial body write is
// UNCERTAIN by §7.2.
func IsDefinitiveNotDelivered(result PostResult) bool {
	if result.Success || result.ResponseCode != nil {
		return false // not in scope: had a usable HTTP response
	}
	return !result.Sent
}

// readBounded performs the bounded body read shared by PostJSON and
// PostJSON (maxResponseBytes cap, one byte over-detect).
func readBounded(body io.ReadCloser) ([]byte, *string) {
	maxBody := maxResponseBytes
	respBodyBytes, _ := io.ReadAll(io.LimitReader(body, int64(maxBody)+1))
	if len(respBodyBytes) > maxBody {
		respBodyBytes = respBodyBytes[:maxBody]
	}
	if len(respBodyBytes) > 0 {
		s := string(respBodyBytes)
		return respBodyBytes, &s
	}
	empty := ""
	return respBodyBytes, &empty
}

// AgentSubmitErrorConfig returns the error config for agent task submissions.
func AgentSubmitErrorConfig() ErrorConfig {
	return ErrorConfig{
		NetworkCode:          "POSTAPI_NETWORK_ERROR",
		NetworkMessage:       "Task postapi request failed",
		NetworkMessagePrefix: "Task postapi request failed: ",
		RejectedCode:         "POSTAPI_REJECTED",
		RejectedMessage:      "Task postapi returned a non-success status",
	}
}

// TestTaskErrorConfig returns the error config for the open-time test delivery.
func TestTaskErrorConfig() ErrorConfig {
	return ErrorConfig{
		NetworkCode:          "TESTTASK_NETWORK_ERROR",
		NetworkMessage:       "Task test postapi request failed",
		NetworkMessagePrefix: "Task test postapi request failed: ",
		RejectedCode:         "TESTTASK_POST_REJECTED",
		RejectedMessage:      "Task test postapi returned a non-success status",
	}
}
