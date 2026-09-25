package task

// Table-driven coverage of every §3 constraint and consistency rule
// 1–4: one violation case per constraint row, plus complete valid
// contracts (sync, async, minimal-defaults, at-the-limits).

import (
	"fmt"
	"strings"
	"testing"
)

func i64(v int64) *int64 { return &v }

// validSyncContract is a complete, valid §3 contract (sync mode).
func validSyncContract() Contract {
	return Contract{
		Title:     "Summarize a page",
		Objective: "A 3-bullet summary of the given page, for a newsletter.",
		Inputs:    "A public URL fetched by the executor.",
		Output: Output{
			Description: "One JSON object with the bullets.",
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"url": {"type": "string"},
					"bullets": {"type": "array", "items": {"type": "string"}, "minItems": 3, "maxItems": 3}
				},
				"required": ["url", "bullets"]
			}`),
		},
		Acceptance: Acceptance{
			Mode: ModeSync,
			Criteria: []Criterion{
				{ID: "C1", Kind: KindRule, Description: "bullets are exactly three sentences"},
				{ID: "C2", Kind: KindSchema, Description: "payload matches output.schema"},
			},
		},
		Boundaries: []string{"no credential material", "no opinion pieces"},
		Examples: []Example{
			{Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`), Accepted: true},
			{Payload: []byte(`{"url":"https://example.com/b","bullets":["only one"]}`),
				Accepted: false, Criteria: []string{"C1"}, Note: "too few bullets"},
		},
		HarnessRefs: []string{"abc123def456"},
		Price:       5,
		Limits:      Limits{MaxAcceptedPerAgent: i64(3), MaxRejectedPerAgent: i64(5)},
		Claim:       ClaimConfig{Required: true, TTL: i64(1800), MaxDuration: i64(7200)},
		Receiver:    Receiver{URL: "https://example.com/hook"},
	}
}

func TestValidateContractValidSync(t *testing.T) {
	if errs := ValidateContract(validSyncContract()); len(errs) != 0 {
		t.Fatalf("valid sync contract rejected: %v", errs)
	}
}

func TestValidateContractValidAsync(t *testing.T) {
	c := validSyncContract()
	c.Acceptance.Mode = ModeAsync
	c.Acceptance.ReviewWindow = i64(3600)
	c.Acceptance.Criteria = append(c.Acceptance.Criteria,
		Criterion{ID: "C3", Kind: KindJudgment, Description: "editorial quality judged by the publisher"})
	c.Receiver = Receiver{} // async without receiver is legal
	if errs := ValidateContract(c); len(errs) != 0 {
		t.Fatalf("valid async contract rejected: %v", errs)
	}
}

// All §3 optional fields absent: everything falls back to 缺省.
func TestValidateContractValidMinimal(t *testing.T) {
	c := validSyncContract()
	c.Boundaries = nil
	c.HarnessRefs = nil
	c.Limits = Limits{}
	c.Claim = ClaimConfig{}
	c.Examples = c.Examples[:1]
	if errs := ValidateContract(c); len(errs) != 0 {
		t.Fatalf("minimal valid contract rejected: %v", errs)
	}
}

// Every bound at its inclusive edge must pass.
func TestValidateContractValidAtLimits(t *testing.T) {
	c := validSyncContract()
	c.Title = strings.Repeat("标", maxTitleLen)
	c.Objective = strings.Repeat("o", maxObjectiveLen)
	c.Inputs = strings.Repeat("i", maxInputsLen)
	c.Output.Description = strings.Repeat("d", maxOutputDescLen)
	c.Acceptance.ReviewWindow = i64(maxReviewWindow)
	c.Acceptance.Criteria = nil
	for i := 0; i < maxCriteria; i++ {
		c.Acceptance.Criteria = append(c.Acceptance.Criteria,
			Criterion{ID: fmt.Sprintf("C%d", i+1), Kind: KindRule, Description: "r"})
	}
	c.Boundaries = make([]string, maxBoundaries)
	for i := range c.Boundaries {
		c.Boundaries[i] = strings.Repeat("b", maxBoundaryLen)
	}
	c.Examples = []Example{ // exactly maxExamples, first accepted
		{Payload: []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`), Accepted: true},
	}
	for i := 1; i < maxExamples; i++ {
		c.Examples = append(c.Examples, Example{
			Payload: []byte(`{"url":"u","bullets":["a","b","c"]}`), Accepted: false, Criteria: []string{"C1"},
		})
	}
	c.HarnessRefs = make([]string, maxHarnessRefs)
	c.Limits = Limits{MaxAcceptedPerAgent: i64(1), MaxRejectedPerAgent: i64(maxMaxRejected)}
	c.Claim = ClaimConfig{TTL: i64(minClaimTTL), MaxDuration: i64(minClaimMaxDur)}
	c.Acceptance.Mode = ModeAsync
	c.Acceptance.ReviewWindow = i64(minReviewWindow)
	c.Receiver = Receiver{}
	if errs := ValidateContract(c); len(errs) != 0 {
		t.Fatalf("at-the-limits contract rejected: %v", errs)
	}
}

func TestWithDefaults(t *testing.T) {
	c := validSyncContract()
	c.Limits = Limits{}
	c.Claim = ClaimConfig{}
	d := c.WithDefaults()
	if v := d.Limits.MaxRejectedPerAgent; v == nil || *v != DefaultMaxRejectedPerAgent {
		t.Fatalf("max_rejected_per_agent default = %v, want %d", v, DefaultMaxRejectedPerAgent)
	}
	if v := d.Claim.TTL; v == nil || *v != DefaultClaimTTLSeconds {
		t.Fatalf("claim.ttl default = %v, want %d", v, DefaultClaimTTLSeconds)
	}
	if v := d.Claim.MaxDuration; v == nil || *v != DefaultClaimMaxDuration {
		t.Fatalf("claim.max_duration default = %v, want %d", v, DefaultClaimMaxDuration)
	}
	if d.Limits.MaxAcceptedPerAgent != nil {
		t.Fatal("max_accepted_per_agent has no numeric default (缺省不限), must stay nil")
	}
	if d.Claim.Required {
		t.Fatal("claim.required default must be false")
	}
	// the original is untouched
	if c.Limits.MaxRejectedPerAgent != nil || c.Claim.TTL != nil || c.Claim.MaxDuration != nil {
		t.Fatal("WithDefaults must not mutate the receiver")
	}
}

// TestValidateContractViolations: one row per §3 constraint. Each case
// mutates the valid contract and expects at least one FieldError whose
// Field matches wantField.
func TestValidateContractViolations(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*Contract)
		wantField string
	}{
		// -- required bounded strings --
		{"title required", func(c *Contract) { c.Title = "" }, "title"},
		{"title max 128 chars", func(c *Contract) { c.Title = strings.Repeat("t", maxTitleLen+1) }, "title"},
		{"objective required", func(c *Contract) { c.Objective = "" }, "objective"},
		{"objective max 2000 chars", func(c *Contract) { c.Objective = strings.Repeat("o", maxObjectiveLen+1) }, "objective"},
		{"inputs required", func(c *Contract) { c.Inputs = "" }, "inputs"},
		{"inputs max 4000 chars", func(c *Contract) { c.Inputs = strings.Repeat("i", maxInputsLen+1) }, "inputs"},
		{"output.description required", func(c *Contract) { c.Output.Description = "" }, "output.description"},
		{"output.description max 2000", func(c *Contract) { c.Output.Description = strings.Repeat("d", maxOutputDescLen+1) }, "output.description"},

		// -- output.schema --
		{"schema required", func(c *Contract) { c.Output.Schema = nil }, "output.schema"},
		{"schema max 32 KB", func(c *Contract) {
			c.Output.Schema = append([]byte(`{"type":"object","description":"`), strings.Repeat("x", maxSchemaBytes)...)
		}, "output.schema"},
		{"schema valid JSON", func(c *Contract) { c.Output.Schema = []byte(`{"type":`) }, "output.schema"},
		{"schema root is an object (not boolean)", func(c *Contract) { c.Output.Schema = []byte(`true`) }, "output.schema"},
		{"schema root type declared", func(c *Contract) {
			c.Output.Schema = []byte(`{"properties":{"url":{"type":"string"}}}`)
		}, "output.schema"},
		{"schema root type object", func(c *Contract) {
			c.Output.Schema = []byte(`{"type":"string"}`)
			c.Examples = []Example{{Payload: []byte(`"x"`), Accepted: true}}
		}, "output.schema"},
		{"schema compiles (bad regex)", func(c *Contract) {
			c.Output.Schema = []byte(`{"type":"object","properties":{"u":{"type":"string","pattern":"["}}}`)
		}, "output.schema"},

		// -- acceptance --
		{"mode required", func(c *Contract) { c.Acceptance.Mode = "" }, "acceptance.mode"},
		{"mode enum", func(c *Contract) { c.Acceptance.Mode = "sometimes" }, "acceptance.mode"},
		{"review_window required for async", func(c *Contract) { c.Acceptance.Mode = ModeAsync }, "acceptance.review_window"},
		{"review_window min 3600", func(c *Contract) {
			c.Acceptance.Mode = ModeAsync
			c.Acceptance.ReviewWindow = i64(minReviewWindow - 1)
		}, "acceptance.review_window"},
		{"review_window max 604800", func(c *Contract) {
			c.Acceptance.Mode = ModeAsync
			c.Acceptance.ReviewWindow = i64(maxReviewWindow + 1)
		}, "acceptance.review_window"},
		{"criteria min 1", func(c *Contract) { c.Acceptance.Criteria = nil }, "acceptance.criteria"},
		{"criteria max 20", func(c *Contract) {
			for i := len(c.Acceptance.Criteria); i <= maxCriteria; i++ {
				c.Acceptance.Criteria = append(c.Acceptance.Criteria,
					Criterion{ID: fmt.Sprintf("C%d", i), Kind: KindRule, Description: "r"})
			}
		}, "acceptance.criteria"},
		{"criteria id regex", func(c *Contract) { c.Acceptance.Criteria[0].ID = "C123" }, "acceptance.criteria[0].id"},
		{"criteria id lowercase", func(c *Contract) { c.Acceptance.Criteria[0].ID = "c1" }, "acceptance.criteria[0].id"},
		{"criteria id unique", func(c *Contract) { c.Acceptance.Criteria[1].ID = c.Acceptance.Criteria[0].ID }, "acceptance.criteria[1].id"},
		{"criteria kind enum", func(c *Contract) { c.Acceptance.Criteria[0].Kind = "manual" }, "acceptance.criteria[0].kind"},
		{"criteria description required", func(c *Contract) { c.Acceptance.Criteria[0].Description = "" }, "acceptance.criteria[0].description"},

		// -- boundaries --
		{"boundaries max 20", func(c *Contract) {
			c.Boundaries = make([]string, maxBoundaries+1)
		}, "boundaries"},
		{"boundary max 500 chars", func(c *Contract) {
			c.Boundaries[0] = strings.Repeat("b", maxBoundaryLen+1)
		}, "boundaries[0]"},

		// -- examples --
		{"examples min 1", func(c *Contract) { c.Examples = nil }, "examples"},
		{"examples max 5", func(c *Contract) {
			for len(c.Examples) <= maxExamples {
				c.Examples = append(c.Examples, Example{
					Payload: []byte(`{"url":"u","bullets":["a","b","c"]}`), Accepted: false, Criteria: []string{"C1"},
				})
			}
		}, "examples"},
		{"example payload required", func(c *Contract) { c.Examples[0].Payload = nil }, "examples[0].payload"},
		{"example payload valid JSON", func(c *Contract) { c.Examples[0].Payload = []byte(`{"url":`) }, "examples[0].payload"},
		{"at least one accepted example", func(c *Contract) {
			c.Examples[0].Accepted = false
			c.Examples[0].Criteria = []string{"C1"}
		}, "examples"},
		{"rejected example cites criteria", func(c *Contract) { c.Examples[1].Criteria = nil }, "examples[1].criteria"},
		{"example criteria declared", func(c *Contract) { c.Examples[1].Criteria = []string{"C9"} }, "examples[1].criteria[0]"},

		// -- consistency 1: accepted example satisfies schema --
		{"accepted example passes schema", func(c *Contract) {
			c.Examples[0].Payload = []byte(`{"url":"https://example.com/a"}`)
		}, "examples[0].payload"},

		// -- harness_refs --
		{"harness_refs max 10", func(c *Contract) {
			c.HarnessRefs = make([]string, maxHarnessRefs+1)
		}, "harness_refs"},

		// -- price --
		{"price positive", func(c *Contract) { c.Price = 0 }, "price"},
		{"price positive (negative)", func(c *Contract) { c.Price = -1 }, "price"},

		// -- limits --
		{"max_accepted_per_agent positive", func(c *Contract) {
			c.Limits.MaxAcceptedPerAgent = i64(0)
		}, "limits.max_accepted_per_agent"},
		{"max_rejected_per_agent min 1", func(c *Contract) {
			c.Limits.MaxRejectedPerAgent = i64(minMaxRejected - 1)
		}, "limits.max_rejected_per_agent"},
		{"max_rejected_per_agent max 50", func(c *Contract) {
			c.Limits.MaxRejectedPerAgent = i64(maxMaxRejected + 1)
		}, "limits.max_rejected_per_agent"},

		// -- claim --
		{"claim.ttl min 300", func(c *Contract) { c.Claim.TTL = i64(minClaimTTL - 1) }, "claim.ttl"},
		{"claim.ttl max 7200", func(c *Contract) { c.Claim.TTL = i64(maxClaimTTL + 1) }, "claim.ttl"},
		{"claim.max_duration min 600", func(c *Contract) { c.Claim.MaxDuration = i64(minClaimMaxDur - 1) }, "claim.max_duration"},
		{"claim.max_duration max 86400", func(c *Contract) { c.Claim.MaxDuration = i64(maxClaimMaxDur + 1) }, "claim.max_duration"},
		{"claim.max_duration >= effective ttl", func(c *Contract) {
			c.Claim.TTL = i64(1800)
			c.Claim.MaxDuration = i64(1700)
		}, "claim.max_duration"},
		{"claim.max_duration >= defaulted ttl", func(c *Contract) {
			c.Claim.TTL = nil // effective ttl = 缺省 1800
			c.Claim.MaxDuration = i64(1700)
		}, "claim.max_duration"},

		// -- receiver --
		{"receiver.url required for sync", func(c *Contract) { c.Receiver = Receiver{} }, "receiver.url"},
		{"receiver.url https only", func(c *Contract) { c.Receiver.URL = "http://example.com/hook" }, "receiver.url"},
		{"receiver.url has host", func(c *Contract) { c.Receiver.URL = "https:///hook" }, "receiver.url"},

		// -- consistency 2: sync without judgment --
		{"sync has no judgment criteria", func(c *Contract) {
			c.Acceptance.Criteria[1].Kind = KindJudgment
		}, "acceptance.criteria[1].kind"},

		// -- consistency 4: credential-shaped strings --
		{"credential in objective", func(c *Contract) {
			c.Objective = "do not leak " + fakeCredential()
		}, "objective"},
		{"credential in boundary", func(c *Contract) {
			c.Boundaries[0] = "never send " + fakeCredential()
		}, "boundaries[0]"},
		{"credential in example payload (nested)", func(c *Contract) {
			c.Examples[0].Payload = []byte(`{"url":"https://example.com/a","bullets":["s1","s2","` + fakeCredential() + `"]}`)
		}, "examples[0].payload"},
		{"credential in schema string", func(c *Contract) {
			c.Output.Schema = []byte(`{"type":"object","properties":{"url":{"type":"string","description":"never ` + fakeCredential() + `"}},"required":["url"]}`)
		}, "output.schema"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validSyncContract()
			tc.mutate(&c)
			errs := ValidateContract(c)
			for _, e := range errs {
				// exact field, or a JSON Pointer hit nested under it
				if e.Field == tc.wantField || strings.HasPrefix(e.Field, tc.wantField+"/") {
					return
				}
			}
			t.Fatalf("want a FieldError at %q (or nested under it), got %v", tc.wantField, errs)
		})
	}
}

// fakeCredential builds a kf_live_-shaped string (the credential shape
// internal/security detects).
func fakeCredential() string {
	return "kf_live_" + strings.Repeat("ab", 32)
}
