package task

// Verdict — spec §6.1. The receiver (or publisher) produces this
// shape; the platform parses it with ParseVerdict, which enforces the
// §6.1 rules against the contract's declared criteria. Source is set
// by the platform per §6.2 (receiver / publisher / timeout) and never
// taken from the body.

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// §6.1 bounds.
const (
	maxVerdictReason      = 500
	maxVerdictAnnotations = 50
	maxAnnotationMessage  = 300
)

// Annotation is one §6.1 annotations[] entry.
type Annotation struct {
	Pointer   string `json:"pointer"`
	Criterion string `json:"criterion"`
	Message   string `json:"message"`
}

// Verdict is the §6.1 verdict. Stored verbatim on the submission once
// recorded; Retryable is always explicit after parsing (缺省 true).
type Verdict struct {
	Accepted    bool         `json:"accepted"`
	Criteria    []string     `json:"criteria,omitempty"`
	Reason      string       `json:"reason,omitempty"`
	Retryable   bool         `json:"retryable"`
	Annotations []Annotation `json:"annotations,omitempty"`
	Source      string       `json:"source,omitempty"`
}

// ParseVerdict decodes and validates a verdict body (§6.1) against the
// contract's declared criteria:
//   - accepted = true: criteria and reason may be omitted
//   - accepted = false: criteria must cite ≥1 declared id and reason
//     must be 1–500 characters; retryable defaults to true
//   - annotations: 0–50; pointer is an RFC 6901 pointer ("/"-prefixed
//     or "" for the root), criterion declared, message ≤300 characters
func ParseVerdict(body []byte, criteria []Criterion) (Verdict, error) {
	declared := map[string]bool{}
	for _, c := range criteria {
		declared[c.ID] = true
	}

	var raw struct {
		Accepted    *bool        `json:"accepted"`
		Criteria    []string     `json:"criteria"`
		Reason      string       `json:"reason"`
		Retryable   *bool        `json:"retryable"`
		Annotations []Annotation `json:"annotations"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Verdict{}, fmt.Errorf("verdict must be a JSON object: %w", err)
	}
	if raw.Accepted == nil {
		return Verdict{}, fmt.Errorf("accepted is required")
	}

	v := Verdict{
		Accepted:    *raw.Accepted,
		Criteria:    raw.Criteria,
		Reason:      raw.Reason,
		Retryable:   true, // §6.1 缺省
		Annotations: raw.Annotations,
	}
	if raw.Retryable != nil {
		v.Retryable = *raw.Retryable
	}

	if len(v.Annotations) > maxVerdictAnnotations {
		return Verdict{}, fmt.Errorf("annotations must be at most %d, got %d",
			maxVerdictAnnotations, len(v.Annotations))
	}
	for i, a := range v.Annotations {
		if a.Pointer != "" && len(a.Pointer) > 0 && a.Pointer[0] != '/' {
			return Verdict{}, fmt.Errorf("annotations[%d].pointer must be an RFC 6901 pointer", i)
		}
		if !declared[a.Criterion] {
			return Verdict{}, fmt.Errorf("annotations[%d].criterion %q is not declared", i, a.Criterion)
		}
		if n := utf8.RuneCountInString(a.Message); n > maxAnnotationMessage {
			return Verdict{}, fmt.Errorf("annotations[%d].message must be at most %d characters, got %d",
				i, maxAnnotationMessage, n)
		}
	}

	if !v.Accepted {
		if len(v.Criteria) == 0 {
			return Verdict{}, fmt.Errorf("a rejected verdict must cite at least one criterion")
		}
		for _, id := range v.Criteria {
			if !declared[id] {
				return Verdict{}, fmt.Errorf("criterion %q is not declared", id)
			}
		}
		if n := utf8.RuneCountInString(v.Reason); n < 1 || n > maxVerdictReason {
			return Verdict{}, fmt.Errorf("reason must be 1–%d characters, got %d", maxVerdictReason, n)
		}
	}
	return v, nil
}
