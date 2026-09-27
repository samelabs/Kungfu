package task

// Contract — spec §3. Field names are the verbatim JSON names of the
// spec table. output.schema and examples[].payload stay raw JSON
// (json.RawMessage); money and duration fields are int64. Optional
// numeric/bool fields are pointers so an explicit invalid value stays
// distinguishable from "absent" (the spec's 缺省); WithDefaults
// materializes the spec defaults.
//
// ValidateContract is a pure function: no IO, no database. Ownership
// of harness_refs (发布者本人所有) and receiver reachability are
// service-layer checks and live outside this file.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"kungfu.md/internal/security"
)

// §3 spec defaults (缺省).
const (
	DefaultMaxRejectedPerAgent = 5
	DefaultClaimTTLSeconds     = 1800
	DefaultClaimMaxDuration    = 7200
	// The minimal-task defaults (WO-11): a contract with only title,
	// objective and price becomes an async, 3-day-review judgment task
	// over a result string.
	DefaultReviewWindowSeconds = 259200 // 3 days
	DefaultOutputDescription   = "A JSON object with a non-empty string field result."
	DefaultCriterionID         = "C1"
	DefaultCriterionDesc       = "The result fulfils the objective."
)

// DefaultOutputSchema is the §3 缺省 output.schema: one required,
// non-empty string field `result`.
const DefaultOutputSchema = `{"type":"object","required":["result"],"properties":{"result":{"type":"string","minLength":1}}}`

// §3 bounds.
const (
	maxTitleLen      = 128
	maxObjectiveLen  = 2000
	maxInputsLen     = 4000
	maxOutputDescLen = 2000
	maxSchemaBytes   = 32 * 1024
	minReviewWindow  = 3600
	maxReviewWindow  = 604800
	maxCriteria      = 20
	maxBoundaries    = 20
	maxBoundaryLen   = 500
	maxExamples      = 5
	maxHarnessRefs   = 10
	minMaxRejected   = 1
	maxMaxRejected   = 50
	minClaimTTL      = 300
	maxClaimTTL      = 7200
	minClaimMaxDur   = 600
	maxClaimMaxDur   = 86400
)

// Acceptance modes (§3 acceptance.mode).
const (
	ModeSync  = "sync"
	ModeAsync = "async"
)

// Criterion kinds (§3 acceptance.criteria[].kind).
const (
	KindSchema   = "schema"
	KindRule     = "rule"
	KindJudgment = "judgment"
)

// criterionIDPattern is §3: id 匹配 ^C[0-9]{1,2}$ 且唯一.
var criterionIDPattern = regexp.MustCompile(`^C[0-9]{1,2}$`)

// FieldError is one contract violation at a JSON path into the
// contract (e.g. acceptance.criteria[1].id). ValidateContract returns
// all of them at once.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e FieldError) Error() string {
	return e.Field + ": " + e.Message
}

// Contract is the §3 task contract.
type Contract struct {
	Title       string      `json:"title"`
	Objective   string      `json:"objective"`
	Inputs      string      `json:"inputs"`
	Output      Output      `json:"output"`
	Acceptance  Acceptance  `json:"acceptance"`
	Boundaries  []string    `json:"boundaries,omitempty"`
	Examples    []Example   `json:"examples"`
	HarnessRefs []string    `json:"harness_refs,omitempty"`
	Price       int64       `json:"price"`
	Limits      Limits      `json:"limits,omitempty"`
	Claim       ClaimConfig `json:"claim,omitempty"`
	Receiver    Receiver    `json:"receiver,omitempty"`
}

// Output is the §3 output section.
type Output struct {
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

// Acceptance is the §3 acceptance section.
type Acceptance struct {
	Mode         string      `json:"mode"`
	ReviewWindow *int64      `json:"review_window,omitempty"`
	Criteria     []Criterion `json:"criteria"`
}

// Criterion is one §3 acceptance.criteria[] entry.
type Criterion struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// Example is one §3 examples[] entry. Payload stays raw JSON.
type Example struct {
	Payload  json.RawMessage `json:"payload"`
	Accepted bool            `json:"accepted"`
	Criteria []string        `json:"criteria,omitempty"`
	Note     string          `json:"note,omitempty"`
}

// Limits is the §3 limits section. nil MaxAcceptedPerAgent means
// 缺省不限; nil MaxRejectedPerAgent means 缺省 5 (filled by
// WithDefaults).
type Limits struct {
	MaxAcceptedPerAgent *int64 `json:"max_accepted_per_agent,omitempty"`
	MaxRejectedPerAgent *int64 `json:"max_rejected_per_agent,omitempty"`
}

// ClaimConfig is the §3 claim section.
type ClaimConfig struct {
	Required    bool   `json:"required,omitempty"`
	TTL         *int64 `json:"ttl,omitempty"`
	MaxDuration *int64 `json:"max_duration,omitempty"`
}

// Receiver is the §3 receiver section (sync 必填; 公网可达性 is a
// service-layer check, not a format check).
type Receiver struct {
	URL string `json:"url,omitempty"`
}

// WithDefaults returns a copy with the §3 缺省 filled in. The minimal
// required contract is title + objective + price; everything else
// materializes here:
//
//	inputs → "", output.description/schema → the result-string pair,
//	acceptance.mode → async, review_window → 259 200,
//	criteria → the default judgment criterion (only when mode was not
//	explicitly sync — a sync task must declare its own criteria),
//	limits.max_rejected_per_agent → 5, claim.required → false,
//	claim.ttl → 1800, claim.max_duration → 7200.
//
// limits.max_accepted_per_agent has no numeric default (缺省不限)
// and stays nil.
func (c Contract) WithDefaults() Contract {
	out := c
	if out.Output.Description == "" {
		out.Output.Description = DefaultOutputDescription
	}
	if len(out.Output.Schema) == 0 {
		out.Output.Schema = json.RawMessage(DefaultOutputSchema)
	}
	if out.Acceptance.Mode == "" {
		out.Acceptance.Mode = ModeAsync
	}
	if out.Acceptance.Mode == ModeAsync && out.Acceptance.ReviewWindow == nil {
		w := int64(DefaultReviewWindowSeconds)
		out.Acceptance.ReviewWindow = &w
	}
	if len(out.Acceptance.Criteria) == 0 && c.Acceptance.Mode != ModeSync {
		out.Acceptance.Criteria = []Criterion{{
			ID: DefaultCriterionID, Kind: KindJudgment, Description: DefaultCriterionDesc,
		}}
	}
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

// ValidateContract checks every §3 table constraint and consistency
// rule 1–4. It returns ALL violations; an empty slice means the
// contract is valid. Field paths are JSON paths into the contract;
// hits inside examples[].payload or output.schema append an RFC 6901
// JSON Pointer.
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
	requireBounded("objective", c.Objective, maxObjectiveLen)
	// inputs, output.description and output.schema are optional (§3
	// 缺省; WithDefaults has already filled them) — bounds only.
	if n := utf8.RuneCountInString(c.Inputs); n > maxInputsLen {
		add("inputs", "must be at most %d characters, got %d", maxInputsLen, n)
	}
	requireBounded("output.description", c.Output.Description, maxOutputDescLen)

	// -- output.schema: required, size, valid JSON, root object,
	//    compiles under draft 2020-12 --
	schema := compileContractSchema(c.Output.Schema, add)

	// -- acceptance --
	switch c.Acceptance.Mode {
	case ModeSync, ModeAsync:
	case "":
		add("acceptance.mode", "required")
	default:
		add("acceptance.mode", "must be sync or async, got %q", c.Acceptance.Mode)
	}
	if c.Acceptance.Mode == ModeAsync && c.Acceptance.ReviewWindow == nil {
		add("acceptance.review_window", "required when acceptance.mode is async")
	}
	if w := c.Acceptance.ReviewWindow; w != nil && (*w < minReviewWindow || *w > maxReviewWindow) {
		add("acceptance.review_window", "must be between %d and %d seconds, got %d",
			minReviewWindow, maxReviewWindow, *w)
	}

	// criteria: 1–20, id format + uniqueness, kind enum, description.
	declared := map[string]bool{}
	if n := len(c.Acceptance.Criteria); n < 1 || n > maxCriteria {
		add("acceptance.criteria", "must contain 1 to %d entries, got %d", maxCriteria, n)
	}
	for i, cr := range c.Acceptance.Criteria {
		field := fmt.Sprintf("acceptance.criteria[%d]", i)
		if cr.ID == "" {
			add(field+".id", "required")
		} else if !criterionIDPattern.MatchString(cr.ID) {
			add(field+".id", "must match ^C[0-9]{1,2}$, got %q", cr.ID)
		} else if declared[cr.ID] {
			add(field+".id", "duplicate criterion id %q", cr.ID)
		} else {
			declared[cr.ID] = true
		}
		switch cr.Kind {
		case KindSchema, KindRule, KindJudgment:
		case "":
			add(field+".kind", "required")
		default:
			add(field+".kind", "must be schema, rule or judgment, got %q", cr.Kind)
		}
		if cr.Description == "" {
			add(field+".description", "required")
		}
	}

	// -- boundaries: 0–20, each ≤500 --
	if n := len(c.Boundaries); n > maxBoundaries {
		add("boundaries", "must contain at most %d entries, got %d", maxBoundaries, n)
	}
	for i, b := range c.Boundaries {
		if n := utf8.RuneCountInString(b); n > maxBoundaryLen {
			add(fmt.Sprintf("boundaries[%d]", i), "must be at most %d characters, got %d",
				maxBoundaryLen, n)
		}
	}

	// -- examples: 0–5 (optional, §3 缺省 []); when present, payload
	//    valid JSON, criteria declared, accepted=false must cite
	//    violated criteria, ≥1 accepted --
	if n := len(c.Examples); n > maxExamples {
		add("examples", "must contain at most %d entries, got %d", maxExamples, n)
	}
	anyAccepted := false
	for i, ex := range c.Examples {
		field := fmt.Sprintf("examples[%d]", i)
		if len(ex.Payload) == 0 {
			add(field+".payload", "required")
		} else if !json.Valid(ex.Payload) {
			add(field+".payload", "must be valid JSON")
		}
		for j, ref := range ex.Criteria {
			if !declared[ref] {
				add(fmt.Sprintf("%s.criteria[%d]", field, j),
					"references undeclared criterion %q", ref)
			}
		}
		if ex.Accepted {
			anyAccepted = true
		} else if len(ex.Criteria) == 0 {
			add(field+".criteria", "required when accepted is false (所违反的 criteria)")
		}
		// Consistency 1: accepted=true examples must pass output.schema.
		if ex.Accepted && schema != nil && json.Valid(ex.Payload) {
			if instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(ex.Payload)); err != nil {
				add(field+".payload", "must be valid JSON: %v", err)
			} else if err := schema.Validate(instance); err != nil {
				add(field+".payload", "accepted example must satisfy output.schema: %v", err)
			}
		}
	}
	if len(c.Examples) > 0 && !anyAccepted {
		add("examples", "at least one example must have accepted = true")
	}

	// -- harness_refs: 0–10 (ownership is a service-layer check) --
	if n := len(c.HarnessRefs); n > maxHarnessRefs {
		add("harness_refs", "must contain at most %d entries, got %d", maxHarnessRefs, n)
	}

	// -- price: 正整数 --
	if c.Price <= 0 {
		add("price", "must be a positive integer, got %d", c.Price)
	}

	// -- limits --
	if v := c.Limits.MaxAcceptedPerAgent; v != nil && *v < 1 {
		add("limits.max_accepted_per_agent", "must be a positive integer, got %d", *v)
	}
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

	// -- receiver.url: sync 必填; whenever present, https + host --
	if c.Receiver.URL == "" {
		if c.Acceptance.Mode == ModeSync {
			add("receiver.url", "required when acceptance.mode is sync")
		}
	} else if u, err := url.Parse(c.Receiver.URL); err != nil || u.Scheme != "https" || u.Host == "" {
		add("receiver.url", "must be an https URL with a host, got %q", c.Receiver.URL)
	}

	// -- consistency 2: sync tasks carry no judgment criteria --
	if c.Acceptance.Mode == ModeSync {
		for i, cr := range c.Acceptance.Criteria {
			if cr.Kind == KindJudgment {
				add(fmt.Sprintf("acceptance.criteria[%d].kind", i),
					"sync tasks must not use judgment criteria (use async)")
			}
		}
	}

	// -- consistency 4: no credential-shaped strings anywhere --
	scanCredentialStrings(c, add)

	// A schema that failed to compile already produced its own errors;
	// example schema-validation was skipped in that case (nil schema).
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
// string of the contract and to every string inside examples[].payload
// and output.schema (consistency 4), via the shared ScanCredentials
// walk; hits inside raw JSON carry their RFC 6901 pointer.
func scanCredentialStrings(c Contract, add func(field, format string, args ...any)) {
	for _, s := range []struct {
		field string
		value string
	}{
		{"title", c.Title},
		{"objective", c.Objective},
		{"inputs", c.Inputs},
		{"output.description", c.Output.Description},
		{"receiver.url", c.Receiver.URL},
	} {
		if s.value != "" && security.ContainsAPIKey(s.value) {
			add(s.field, "must not contain credential-shaped strings")
		}
	}
	for i, b := range c.Boundaries {
		if security.ContainsAPIKey(b) {
			add(fmt.Sprintf("boundaries[%d]", i), "must not contain credential-shaped strings")
		}
	}
	for i, cr := range c.Acceptance.Criteria {
		if security.ContainsAPIKey(cr.Description) {
			add(fmt.Sprintf("acceptance.criteria[%d].description", i),
				"must not contain credential-shaped strings")
		}
	}
	for i, ex := range c.Examples {
		if security.ContainsAPIKey(ex.Note) {
			add(fmt.Sprintf("examples[%d].note", i), "must not contain credential-shaped strings")
		}
		field := fmt.Sprintf("examples[%d].payload", i)
		for _, ptr := range ScanCredentials(ex.Payload) {
			add(field+ptr, "must not contain credential-shaped strings")
		}
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
