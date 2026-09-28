package task

// Contract — spec §3. The task contract is what a publisher hands to
// Kungfu: what to do (title, requirements), the execution material
// (harness_refs), what to hand in (optional output.schema), where the
// result goes (receiver.url, tested with sample before opening), and
// the money (price). Field names are the verbatim JSON names of the
// spec table. output.schema and sample stay raw JSON; optional numeric
// fields are pointers so an explicit invalid value stays
// distinguishable from "absent"; WithDefaults materializes the spec
// defaults.
//
// ValidateContract is a pure function: no IO, no database. Ownership
// of harness_refs (发布者本人所有) and receiver reachability are
// service-layer checks and live outside this file.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"kungfu.md/internal/security"
)

// §3 spec defaults (缺省).
const (
	DefaultMaxRejectedPerAgent = 5
	DefaultClaimTTLSeconds     = 1800
	DefaultClaimMaxDuration    = 7200
)

// MaxAmount is the ceiling on every task money field (price, budget,
// fund amount and the accumulated budget_locked). 2^53−1 is the exact
// integer range of an IEEE-754 double: a task amount is therefore
// lossless in JSON and in JavaScript Number all the way to the cap
// (the wire.go browser contract keeps them strings regardless).
const MaxAmount = 1<<53 - 1

// §3 bounds.
const (
	maxTitleLen        = 128
	maxRequirementsLen = 20000
	maxSchemaBytes     = 32 * 1024
	maxSampleBytes     = 512 * 1024
	maxHarnessRefs     = 10
	minMaxRejected     = 1
	maxMaxRejected     = 50
	minClaimTTL        = 300
	maxClaimTTL        = 7200
	minClaimMaxDur     = 600
	maxClaimMaxDur     = 86400
)

// FieldError is one contract violation at a JSON path into the
// contract (e.g. claim.ttl). ValidateContract returns all of them at
// once.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e FieldError) Error() string {
	return e.Field + ": " + e.Message
}

// Contract is the §3 task contract.
type Contract struct {
	Title        string          `json:"title"`
	Requirements string          `json:"requirements"`
	HarnessRefs  []string        `json:"harness_refs,omitempty"`
	Output       Output          `json:"output,omitempty"`
	Receiver     Receiver        `json:"receiver"`
	Sample       json.RawMessage `json:"sample"`
	Price        int64           `json:"price"`
	Limits       Limits          `json:"limits,omitempty"`
	Claim        ClaimConfig     `json:"claim,omitempty"`
}

// Output is the §3 output section: an optional JSON Schema the
// platform checks every payload against before delivery.
type Output struct {
	Schema json.RawMessage `json:"schema,omitempty"`
}

// Limits is the §3 limits section; nil MaxRejectedPerAgent means
// 缺省 5 (filled by WithDefaults).
type Limits struct {
	MaxRejectedPerAgent *int64 `json:"max_rejected_per_agent,omitempty"`
}

// ClaimConfig is the §3 claim section.
type ClaimConfig struct {
	Required    bool   `json:"required,omitempty"`
	TTL         *int64 `json:"ttl,omitempty"`
	MaxDuration *int64 `json:"max_duration,omitempty"`
}

// Receiver is the §3 receiver section (必填; 公网可达性 is proven by
// the open-time test delivery, not by a format check).
type Receiver struct {
	URL string `json:"url"`
}

// WithDefaults returns a copy with the §3 缺省 filled in:
// limits.max_rejected_per_agent → 5, claim.ttl → 1800,
// claim.max_duration → 7200 (claim.required is false when absent).
func (c Contract) WithDefaults() Contract {
	out := c
	if out.Limits.MaxRejectedPerAgent == nil {
		v := int64(DefaultMaxRejectedPerAgent)
		out.Limits.MaxRejectedPerAgent = &v
	}
	if out.Claim.TTL == nil {
		v := int64(DefaultClaimTTLSeconds)
		out.Claim.TTL = &v
	}
	if out.Claim.MaxDuration == nil {
		v := int64(DefaultClaimMaxDuration)
		out.Claim.MaxDuration = &v
	}
	return out
}

// ValidateContract checks every §3 table constraint. It returns ALL
// violations; an empty slice means the contract is valid. Field paths
// are JSON paths into the contract; hits inside sample or
// output.schema append an RFC 6901 JSON Pointer.
func ValidateContract(c Contract) []FieldError {
	var errs []FieldError
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	// -- required bounded strings (字符 counted as runes) --
	requireBounded := func(field, value string, max int) {
		if value == "" {
			add(field, "required")
			return
		}
		if n := utf8.RuneCountInString(value); n > max {
			add(field, "must be at most %d characters, got %d", max, n)
		}
	}
	requireBounded("title", c.Title, maxTitleLen)
	requireBounded("requirements", c.Requirements, maxRequirementsLen)

	// -- output.schema: optional; when given, size, valid JSON, root
	//    object, compiles under draft 2020-12 --
	var schema *jsonschema.Schema
	if len(c.Output.Schema) > 0 && string(c.Output.Schema) != "null" {
		schema = compileContractSchema(c.Output.Schema, add)
	}

	// -- receiver.url: required, https + host --
	if c.Receiver.URL == "" {
		add("receiver.url", "required")
	} else if u, err := url.Parse(c.Receiver.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		add("receiver.url", "must be an https URL with a host, got %q", c.Receiver.URL)
	}

	// -- sample: required JSON object, ≤ the payload limit, satisfies
	//    output.schema when one is given --
	switch {
	case len(c.Sample) == 0 || string(c.Sample) == "null":
		add("sample", "required (the payload the open-time test delivery sends)")
	case len(c.Sample) > maxSampleBytes:
		add("sample", "must be at most %d bytes, got %d", maxSampleBytes, len(c.Sample))
	default:
		var obj map[string]any
		if err := json.Unmarshal(c.Sample, &obj); err != nil {
			add("sample", "must be a JSON object")
		} else if schema != nil {
			if instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(c.Sample)); err != nil {
				add("sample", "must be valid JSON: %v", err)
			} else if err := schema.Validate(instance); err != nil {
				add("sample", "must satisfy output.schema: %v", err)
			}
		}
	}

	// -- harness_refs: 0–10 (ownership is a service-layer check) --
	if n := len(c.HarnessRefs); n > maxHarnessRefs {
		add("harness_refs", "must contain at most %d entries, got %d", maxHarnessRefs, n)
	}

	// -- price: 正整数，≤ MaxAmount --
	if c.Price <= 0 || c.Price > MaxAmount {
		add("price", "must be a positive integer no greater than %d, got %d", MaxAmount, c.Price)
	}

	// -- limits --
	if v := c.Limits.MaxRejectedPerAgent; v != nil && (*v < minMaxRejected || *v > maxMaxRejected) {
		add("limits.max_rejected_per_agent", "must be between %d and %d, got %d",
			minMaxRejected, maxMaxRejected, *v)
	}

	// -- claim (ranges on explicit values; the ttl ≤ max_duration
	//    comparison uses the effective values including 缺省) --
	if v := c.Claim.TTL; v != nil && (*v < minClaimTTL || *v > maxClaimTTL) {
		add("claim.ttl", "must be between %d and %d seconds, got %d",
			minClaimTTL, maxClaimTTL, *v)
	}
	if v := c.Claim.MaxDuration; v != nil && (*v < minClaimMaxDur || *v > maxClaimMaxDur) {
		add("claim.max_duration", "must be between %d and %d seconds, got %d",
			minClaimMaxDur, maxClaimMaxDur, *v)
	}
	ttl, maxDur := int64(DefaultClaimTTLSeconds), int64(DefaultClaimMaxDuration)
	if c.Claim.TTL != nil {
		ttl = *c.Claim.TTL
	}
	if c.Claim.MaxDuration != nil {
		maxDur = *c.Claim.MaxDuration
	}
	if maxDur < ttl {
		add("claim.max_duration", "must be ≥ claim.ttl (%d), got %d", ttl, maxDur)
	}

	// -- no credential-shaped strings anywhere --
	scanCredentialStrings(c, add)
	return errs
}

// compileContractSchema decodes, size-checks and compiles output.schema
// under draft 2020-12, enforcing the object root type. It returns the
// compiled schema (nil on failure) and reports every problem via add.
func compileContractSchema(raw json.RawMessage, add func(field, format string, args ...any)) *jsonschema.Schema {
	if len(raw) == 0 {
		add("output.schema", "required")
		return nil
	}
	if len(raw) > maxSchemaBytes {
		add("output.schema", "must be at most %d bytes, got %d", maxSchemaBytes, len(raw))
		return nil
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		add("output.schema", "must be valid JSON: %v", err)
		return nil
	}
	root, ok := doc.(map[string]any)
	if !ok {
		add("output.schema", "must be a JSON Schema object with root type object")
		return nil
	}
	// §3: 根类型必须为 object — declared explicitly at the root.
	typeObject := false
	switch t := root["type"].(type) {
	case string:
		typeObject = t == "object"
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && s == "object" {
				typeObject = true
			}
		}
	}
	if !typeObject {
		add("output.schema", "root type must be object")
		return nil
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("contract:output.schema", doc); err != nil {
		add("output.schema", "invalid JSON Schema: %v", err)
		return nil
	}
	schema, err := compiler.Compile("contract:output.schema")
	if err != nil {
		add("output.schema", "does not compile as JSON Schema draft 2020-12: %v", err)
		return nil
	}
	return schema
}

// scanCredentialStrings applies the credential-shape detector to every
// string of the contract and to every string inside sample and
// output.schema, via the shared ScanCredentials walk; hits inside raw
// JSON carry their RFC 6901 pointer.
func scanCredentialStrings(c Contract, add func(field, format string, args ...any)) {
	for _, s := range []struct {
		field string
		value string
	}{
		{"title", c.Title},
		{"requirements", c.Requirements},
		{"receiver.url", c.Receiver.URL},
	} {
		if s.value != "" && security.ContainsAPIKey(s.value) {
			add(s.field, "must not contain credential-shaped strings")
		}
	}
	for _, ptr := range ScanCredentials(c.Sample) {
		add("sample"+ptr, "must not contain credential-shaped strings")
	}
	for i, ref := range c.HarnessRefs {
		if security.ContainsAPIKey(ref) {
			add(fmt.Sprintf("harness_refs[%d]", i), "must not contain credential-shaped strings")
		}
	}
	for _, ptr := range ScanCredentials(c.Output.Schema) {
		add("output.schema"+ptr, "must not contain credential-shaped strings")
	}
}

// escapeJSONPointerToken escapes one RFC 6901 reference token (~ → ~0,
// / → ~1).
func escapeJSONPointerToken(tok string) string {
	s := bytes.ReplaceAll([]byte(tok), []byte("~"), []byte("~0"))
	return string(bytes.ReplaceAll(s, []byte("/"), []byte("~1")))
}
