package server

// wire.go — Lossless economic integer wire contract (browser boundary).
//
// Credits and fiat minor units are authoritative int64. JavaScript
// Number cannot represent the full int64 range (2^53+1 corrupts to
// 2^53), so every economic integer that reaches the OWNER/ADMIN
// BROWSER is serialized as a canonical base-10 decimal STRING:
//
//	"0", "66", "9007199254740993", "-66" (fields where the business
//	itself allows negatives, e.g. a reversal-driven balance)
//
// Canonical form: optional leading '-', then digits with no leading
// zero (except "0" itself). No exponent, no decimal point, no '+'.
//
// The MCP tool contracts keep numeric JSON — this
// is a browser-boundary contract, not a global API change. Ordinary
// non-economic integers (pagination, ids, statuses) are untouched.
//
// The browser must preserve/display/send these strings verbatim; all
// business validation stays with the server authority.
//
// The publisher console's task tools follow the MCP numeric envelope
// instead: task amounts (price, budget, fund) stay JSON numbers on the
// wire. This is exact because task.MaxAmount caps every task money
// field at 2^53-1 — inside the integer range an IEEE-754 double (and
// thus JS Number) represents losslessly.

import (
	"errors"
	"strconv"
)

// econString converts an authoritative economic int64 into the
// canonical decimal string for browser-facing responses.
func econString(v int64) string {
	return strconv.FormatInt(v, 10)
}

// parseCanonicalEconInt is the ONE canonical decimal integer string
// parser for the server package — no trim, no float fallback, no
// leading zeros, no exponent, no '+', no '-0'. Both parseCredits
// (owner boundaries) and jsonCredits (admin boundaries) reuse it, so
// there are not two string-integer rules. Syntax/range only: sign
// BUSINESS rules stay with the domain authority.
//
// Canonical form: optional '-', then "0" or [1-9][0-9]*.
func parseCanonicalEconInt(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	neg := s[0] == '-'
	body := s
	if neg {
		body = s[1:]
	}
	if body == "" {
		return 0, errors.New("bare sign")
	}
	// no leading zero unless the whole body is "0"; "-0" rejected
	if len(body) > 1 && body[0] == '0' {
		return 0, errors.New("leading zero")
	}
	if body == "0" && neg {
		return 0, errors.New("negative zero")
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return 0, errors.New("non-digit")
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errors.New("out of int64 range")
	}
	return n, nil
}

// ownerOverviewWire maps the AccountOverview service result to the
// browser contract. Service internals stay int64; only the wire DTO
// stringifies economic fields.
func ownerOverviewWire(result map[string]interface{}) map[string]interface{} {
	if result == nil {
		return nil
	}
	out := make(map[string]interface{}, len(result)+2)
	for k, v := range result {
		out[k] = v
	}
	if bal, ok := out["balance"].(int64); ok {
		out["balance"] = econString(bal)
	}
	return out
}

// isEconomicKey reports whether a JSON key carries a Credit (or fiat
// minor-unit) integer in Owner/Admin browser payloads. Keys not listed
// here are ordinary integers and stay numeric.
func isEconomicKey(k string) bool {
	switch k {
	case "budget", "price", "amount", "balance_after", "balance",
		"reward", "credits", "credits_price", "credits_cost",
		"amount_minor", "transaction_amount_minor", "amount_paid_minor",
		"refunded_amount_minor":
		return true
	}
	return false
}

// wireNestedEconMap recursively maps a service result: nested maps and
// item lists pass through, and economic-int keys stringify via econString.
func wireNestedEconMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]interface{}:
			out[k] = wireNestedEconMap(t)
		case []map[string]interface{}:
			items := make([]interface{}, 0, len(t))
			for i := range t {
				items = append(items, wireNestedEconMap(t[i]))
			}
			out[k] = items
		case []interface{}:
			items := make([]interface{}, 0, len(t))
			for _, it := range t {
				if im, ok := it.(map[string]interface{}); ok {
					items = append(items, wireNestedEconMap(im))
				} else {
					items = append(items, it)
				}
			}
			out[k] = items
		case int64:
			if isEconomicKey(k) {
				out[k] = econString(t)
			} else {
				out[k] = t
			}
		default:
			out[k] = v
		}
	}
	return out
}

// ownerLogsWire maps the owner-logs service result: stringify the
// per-item economic integers and the balance summary.
func ownerLogsWire(result map[string]interface{}) map[string]interface{} {
	if result == nil {
		return nil
	}
	out := make(map[string]interface{}, len(result))
	for k, v := range result {
		switch t := v.(type) {
		case []map[string]interface{}:
			items := make([]interface{}, 0, len(t))
			for i := range t {
				items = append(items, wireNestedEconMap(t[i]))
			}
			out[k] = items
		case []interface{}:
			items := make([]interface{}, 0, len(t))
			for _, it := range t {
				if im, ok := it.(map[string]interface{}); ok {
					items = append(items, wireNestedEconMap(im))
				} else {
					items = append(items, it)
				}
			}
			out[k] = items
		case int64:
			if isEconomicKey(k) {
				out[k] = econString(t)
			} else {
				out[k] = t
			}
		default:
			out[k] = v
		}
	}
	return out
}
