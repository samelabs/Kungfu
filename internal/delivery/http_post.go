package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"time"
)

// PostResult holds the result of a POST request.
type PostResult struct {
	Success      bool
	Sent         bool    // true once any attempt fully wrote the request (httptrace WroteRequest)
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

// maxExistingResponseBytes is the largest byte budget any existing
// consumer stores or displays (the 65535-byte TestTask DB log column).
// Transport reads at most this + 1 byte — bounded I/O, existing
// truncation contracts unchanged.
const maxExistingResponseBytes = 65535

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

	// sent latches true the first time any attempt writes the full
	// request to the wire (httptrace WroteRequest with Err == nil) and
	// never goes back — the single transport fact behind
	// IsDefinitiveNotDelivered.
	sent := false
	traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				sent = true
			}
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
			Sent:         sent,
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

// PostJSONWithKey sends a POST request with a JSON body and a stable
// receiver idempotency key (Idempotency-Key header). Every retry of the
// same durable submission MUST send the identical header value and body —
// the header carries the submission's opaque stable identity so a
// compliant receiver can deduplicate side effects end-to-end.
//
// It is a thin wrapper over PostJSON (same SSRF dial authority,
// timeouts and bounded read).
func PostJSONWithKey(ctx context.Context, url string, body []byte, idempotencyKey string, errCfg ErrorConfig) PostResult {
	return PostJSON(ctx, url, body, map[string]string{"Idempotency-Key": idempotencyKey}, errCfg)
}

// IsDefinitiveNotDelivered classifies a failed PostResult as PROVABLY
// not reaching the receiver: no response code and the request was
// never fully written (SSRF refusal, DNS failure, connect refused,
// connect timeout, TLS handshake or certificate failure — everything
// httptrace never reported as WroteRequest). Once the request left
// the wire, a later timeout or disconnect is UNCERTAIN by §7.2.
func IsDefinitiveNotDelivered(result PostResult) bool {
	if result.Success || result.ResponseCode != nil {
		return false // not in scope: had a usable HTTP response
	}
	return !result.Sent
}

// BuildSubmissionPayload produces the canonical outbound JSON bytes for a
// submission. The bytes are hashed for the payload identity, persisted as
// the durable payload_body, and replayed byte-identically on every retry.
func BuildSubmissionPayload(taskCode string, payload map[string]interface{}) []byte {
	built := BuildPayload(taskCode, payload)
	b, _ := json.Marshal(built)
	return b
}

// BuildPayload attaches task_code to the submission payload.
func BuildPayload(taskCode string, payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	payload["task_code"] = taskCode
	return payload
}

// readBounded performs the bounded body read shared by PostJSON and
// PostJSONWithKey (maxExistingResponseBytes cap, one byte over-detect).
func readBounded(body io.ReadCloser) ([]byte, *string) {
	maxBody := maxExistingResponseBytes
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

// TestTaskErrorConfig returns the error config for owner test task.
func TestTaskErrorConfig() ErrorConfig {
	return ErrorConfig{
		NetworkCode:          "TESTTASK_NETWORK_ERROR",
		NetworkMessage:       "Task test postapi request failed",
		NetworkMessagePrefix: "Task test postapi request failed: ",
		RejectedCode:         "TESTTASK_POST_REJECTED",
		RejectedMessage:      "Task test postapi returned a non-success status",
	}
}
