package errors

import "fmt"

// AppError is the canonical application error.
// It carries an HTTP status code, a machine-readable error code,
// a human-readable message, optional details, and an optional cause.
type AppError struct {
	HTTPCode int
	Code     string
	Message  string
	Details  map[string]interface{}
	Err      error // wrapped underlying error
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s (caused by: %v)", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *AppError) Unwrap() error {
	return e.Err
}

// New creates a new AppError.
func New(httpCode int, code, message string) *AppError {
	return &AppError{
		HTTPCode: httpCode,
		Code:     code,
		Message:  message,
		Details:  nil,
	}
}

// NewWithDetails creates a new AppError with details.
func NewWithDetails(httpCode int, code, message string, details map[string]interface{}) *AppError {
	return &AppError{
		HTTPCode: httpCode,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}

// Wrap creates a new AppError wrapping an underlying error.
func Wrap(httpCode int, code, message string, err error) *AppError {
	return &AppError{
		HTTPCode: httpCode,
		Code:     code,
		Message:  message,
		Details:  nil,
		Err:      err,
	}
}

// RateLimitError is returned when a client exceeds its rate limit. It embeds
// the retry window metadata (retry_after, limit, window).
type RateLimitError struct {
	*AppError
	RetryAfter int
	Limit      int
	Window     int
}

func NewRateLimitError(retryAfter, limit, window int) *RateLimitError {
	return &RateLimitError{
		AppError: &AppError{
			HTTPCode: 429,
			Code:     "RATE_LIMIT",
			Message:  "Too many requests",
			Details: map[string]interface{}{
				"retry_after": retryAfter,
				"limit":       limit,
				"window":      window,
			},
		},
		RetryAfter: retryAfter,
		Limit:      limit,
		Window:     window,
	}
}

// IsAppError checks if an error is an *AppError and returns it.
func IsAppError(err error) (*AppError, bool) {
	if ae, ok := err.(*AppError); ok {
		return ae, true
	}
	return nil, false
}

// IsRateLimitError checks if an error is a *RateLimitError and returns it.
func IsRateLimitError(err error) (*RateLimitError, bool) {
	if rle, ok := err.(*RateLimitError); ok {
		return rle, true
	}
	return nil, false
}
