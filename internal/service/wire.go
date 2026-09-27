package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// WireID is the protocol-boundary id type (WO-10 amendment): int64
// inside, a decimal STRING in every JSON output, and either a string
// or an integer accepted on input. A non-numeric string fails at the
// unmarshal site, which every tool handler maps to VALIDATION_FAILED.
type WireID int64

// MarshalJSON renders the id as a JSON string.
func (w WireID) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(w), 10) + `"`), nil
}

// UnmarshalJSON accepts a JSON integer or a decimal string.
func (w *WireID) UnmarshalJSON(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "null" {
		return nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return fmt.Errorf("id string %q is not an integer", s)
		}
		*w = WireID(n)
		return nil
	}
	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("id must be an integer or a numeric string")
	}
	*w = WireID(n)
	return nil
}

// Int64 returns the id as the internal int64.
func (w WireID) Int64() int64 { return int64(w) }

// Int64Ptr returns the id as *int64 (nil when the pointer is nil).
func (w *WireID) Int64Ptr() *int64 {
	if w == nil {
		return nil
	}
	n := int64(*w)
	return &n
}

// WireIDPtr wraps an internal *int64 as *WireID (nil stays nil).
func WireIDPtr(p *int64) *WireID {
	if p == nil {
		return nil
	}
	w := WireID(*p)
	return &w
}
