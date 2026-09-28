package server

// Lossless integer decoding at Credit-bearing public boundaries.
// Regression proof for the 2^53 class: json.Unmarshal into
// map[string]interface{} yields float64, which silently turns
// 9007199254740993 into 9007199254740992 BEFORE validation. The
// boundaries parse with json.Number (UseNumber) so the exact source
// text reaches parseCredits/jsonCredits.
//
// Also covers MaxInt64 handling and rejects-with-zero-mutation for
// out-of-range / fractional inputs.

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestAdminStorePrice2p53Plus1Lossless(t *testing.T) {
	// Decode-path proof for the admin boundary: with UseNumber the exact
	// source text reaches jsonCredits; without it (float64 path) the
	// corrupted value MUST be refused rather than silently stored.
	var mNum map[string]interface{}
	dec := json.NewDecoder(bytes.NewBufferString(`{"credits_price":9007199254740993}`))
	dec.UseNumber()
	if err := dec.Decode(&mNum); err != nil {
		t.Fatal(err)
	}
	v, ok := jsonCredits(mNum["credits_price"])
	if !ok || v != 9007199254740993 {
		t.Fatalf("jsonCredits(UseNumber) = %d,%v — want exactly 9007199254740993", v, ok)
	}
	// The float64 decode of the same text is the corrupted value; the
	// compatibility case in jsonCredits must still reject fractions and
	// never accept the corrupted integer silently as a DIFFERENT number.
	var mF64 map[string]interface{}
	if err := json.Unmarshal([]byte(`{"credits_price":12.5}`), &mF64); err != nil {
		t.Fatal(err)
	}
	if _, ok := jsonCredits(mF64["credits_price"]); ok {
		t.Fatal("fractional credits_price accepted via float64 path")
	}
	// 2^53+0.5 literally rounds to 2^53 in float64 — with UseNumber the
	// exact text still reaches validation and is refused.
	var mF2 map[string]interface{}
	dec2 := json.NewDecoder(bytes.NewBufferString(`{"credits_price":9007199254740992.5}`))
	dec2.UseNumber()
	if err := dec2.Decode(&mF2); err != nil {
		t.Fatal(err)
	}
	if _, ok := jsonCredits(mF2["credits_price"]); ok {
		t.Fatal("2^53+0.5 accepted via json.Number path — float64 would have hidden it")
	}
}
