package delivery

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// PostResult holds the result of a POST request.
type PostResult struct {
	Success      bool
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

// HTTPClient is a shared client with proper timeouts and no redirect following.
var sharedClient *http.Client

func init() {
	sharedClient = &http.Client{
		Timeout: postAPIRequestTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: postAPIConnectTimeout,
			}).DialContext,
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
		},
		// Do NOT follow redirects — return the raw 3xx response to the caller.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// maxExistingResponseBytes is the largest byte budget any existing
// consumer stores or displays (the 65535-byte TestTask DB log column).
// Transport reads at most this + 1 byte — bounded I/O, existing
// truncation contracts unchanged.
const maxExistingResponseBytes = 65535

// PostJSON sends a POST request with a JSON body, classifying failures via errCfg.
// The caller's context governs the request lifecycle: cancellation or
// deadline of ctx terminates the transport work via
// http.NewRequestWithContext. The 10s client timeout remains a lower
// safety net, NOT the primary budget.
func PostJSON(ctx context.Context, url string, body []byte, errCfg ErrorConfig) PostResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return PostResult{
			Success:      false,
			ErrorCode:    errCfg.NetworkCode,
			ErrorMessage: errCfg.NetworkMessagePrefix + err.Error(),
		}
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

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
			ResponseCode: respCode,
			ErrorCode:    errCfg.NetworkCode,
			ErrorMessage: errCfg.NetworkMessagePrefix + err.Error(),
		}
	}
	defer resp.Body.Close()

	// Bounded read: the largest downstream consumer of a response body is
	// the TestTask DB log column (65535 bytes); reading one byte past that
	// cap detects "over-long" while never buffering an arbitrarily large
	// remote response (100MB response != 100MB memory). A bounded read
	// error (e.g. ErrUnexpectedEOF on a truncated stream) still yields
	// whatever was read; HTTP 2xx/non-2xx semantics are unchanged and the
	// existing upper-layer truncation contracts (16000/65535/4000) stay
	// authoritative for display/logging.
	maxBody := maxExistingResponseBytes
	respBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	if len(respBodyBytes) > maxBody {
		respBodyBytes = respBodyBytes[:maxBody]
	}
	var respBody *string
	if len(respBodyBytes) > 0 {
		s := string(respBodyBytes)
		respBody = &s
	} else {
		empty := ""
		respBody = &empty
	}

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

// BuildPayload attaches task_code to the submission payload.
func BuildPayload(taskCode string, payload map[string]interface{}) map[string]interface{} {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	payload["task_code"] = taskCode
	return payload
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
