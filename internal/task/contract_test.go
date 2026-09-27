package task

// Table-driven coverage of every §3 constraint: one violation case per
// constraint row, plus complete valid contracts (full, minimal,
// at-the-limits).

import (
	"strings"
	"testing"
)

func i64(v int64) *int64 { return &v }

// validContract is a complete, valid §3 contract.
func validContract() Contract {
	return Contract{
		Title:        "Summarize a page",
		Requirements: "Fetch the given page and return exactly three summary bullets for a newsletter.",
		HarnessRefs:  []string{"abc123def456"},
		Output: Output{
			Schema: []byte(`{
				"type": "object",
				"properties": {
					"url": {"type": "string"},
					"bullets": {"type": "array", "items": {"type": "string"}, "minItems": 3, "maxItems": 3}
				},
				"required": ["url", "bullets"]
			}`),
		},
		Receiver: Receiver{URL: "https://example.com/hook"},
		Sample:   []byte(`{"url":"https://example.com/a","bullets":["s1","s2","s3"]}`),
		Price:    5,
		Limits:   Limits{MaxRejectedPerAgent: i64(5)},
		Claim:    ClaimConfig{Required: true, TTL: i64(1800), MaxDuration: i64(7200)},
	}
}

func TestValidateContractValid(t *testing.T) {
	if errs := ValidateContract(validContract()); len(errs) != 0 {
		t.Fatalf("valid contract rejected: %v", errs)
	}
}

// All optional fields absent: no schema, no harness, 缺省 limits/claim.
func TestValidateContractValidMinimal(t *testing.T) {
	c := Contract{
		Title:        "t",
		Requirements: "r",
		Receiver:     Receiver{URL: "https://example.com/hook"},
		Sample:       []byte(`{}`),
		Price:        1,
	}
	if errs := ValidateContract(c.WithDefaults()); len(errs) != 0 {
		t.Fatalf("minimal valid contract rejected: %v", errs)
	}
}

// Every bound at its inclusive edge must pass.
func TestValidateContractValidAtLimits(t *testing.T) {
	c := validContract()
	c.Title = strings.Repeat("标", maxTitleLen)
	c.Requirements = strings.Repeat("r", maxRequirementsLen)
	c.HarnessRefs = make([]string, maxHarnessRefs)
	c.Limits = Limits{MaxRejectedPerAgent: i64(maxMaxRejected)}
	c.Claim = ClaimConfig{TTL: i64(minClaimTTL), MaxDuration: i64(minClaimMaxDur)}
	if errs := ValidateContract(c); len(errs) != 0 {
		t.Fatalf("at-the-limits contract rejected: %v", errs)
	}
}

func TestWithDefaults(t *testing.T) {
	c := validContract()
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
	if d.Claim.Required {
		t.Fatal("claim.required default must be false")
	}
	// the original is untouched
	if c.Limits.MaxRejectedPerAgent != nil || c.Claim.TTL != nil {
		t.Fatal("WithDefaults mutated its receiver")
	}
}

func TestValidateContractViolations(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*Contract)
		wantField string
	}{
		// -- required bounded strings --
		{"title required", func(c *Contract) { c.Title = "" }, "title"},
		{"title max 128 chars", func(c *Contract) { c.Title = strings.Repeat("t", maxTitleLen+1) }, "title"},
		{"requirements required", func(c *Contract) { c.Requirements = "" }, "requirements"},
		{"requirements max 20000 chars", func(c *Contract) {
			c.Requirements = strings.Repeat("r", maxRequirementsLen+1)
		}, "requirements"},

		// -- output.schema (optional; checked when given) --
		{"schema max 32 KB", func(c *Contract) {
			c.Output.Schema = append([]byte(`{"type":"object","description":"`), strings.Repeat("x", maxSchemaBytes)...)
		}, "output.schema"},
		{"schema valid JSON", func(c *Contract) { c.Output.Schema = []byte(`{"type":`) }, "output.schema"},
		{"schema root is an object (not boolean)", func(c *Contract) { c.Output.Schema = []byte(`true`) }, "output.schema"},
		{"schema root type declared", func(c *Contract) {
			c.Output.Schema = []byte(`{"properties":{"url":{"type":"string"}}}`)
		}, "output.schema"},
		{"schema root type object", func(c *Contract) { c.Output.Schema = []byte(`{"type":"string"}`) }, "output.schema"},
		{"schema compiles (bad regex)", func(c *Contract) {
			c.Output.Schema = []byte(`{"type":"object","properties":{"u":{"type":"string","pattern":"["}}}`)
		}, "output.schema"},

		// -- receiver --
		{"receiver.url required", func(c *Contract) { c.Receiver = Receiver{} }, "receiver.url"},
		{"receiver.url https only", func(c *Contract) { c.Receiver.URL = "http://example.com/hook" }, "receiver.url"},
		{"receiver.url has host", func(c *Contract) { c.Receiver.URL = "https:///hook" }, "receiver.url"},

		// -- sample --
		{"sample required", func(c *Contract) { c.Sample = nil }, "sample"},
		{"sample is an object", func(c *Contract) { c.Sample = []byte(`["x"]`) }, "sample"},
		{"sample satisfies schema", func(c *Contract) { c.Sample = []byte(`{"url":"https://example.com/a"}`) }, "sample"},

		// -- harness_refs --
		{"harness_refs max 10", func(c *Contract) { c.HarnessRefs = make([]string, maxHarnessRefs+1) }, "harness_refs"},

		// -- price --
		{"price positive", func(c *Contract) { c.Price = 0 }, "price"},
		{"price positive (negative)", func(c *Contract) { c.Price = -1 }, "price"},

		// -- limits --
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

		// -- credential-shaped strings --
		{"credential in requirements", func(c *Contract) {
			c.Requirements = "do not leak " + fakeCredential()
		}, "requirements"},
		{"credential in sample (nested)", func(c *Contract) {
			c.Sample = []byte(`{"url":"https://example.com/a","bullets":["s1","s2","` + fakeCredential() + `"]}`)
		}, "sample"},
		{"credential in schema string", func(c *Contract) {
			c.Output.Schema = []byte(`{"type":"object","properties":{"url":{"type":"string","description":"never ` + fakeCredential() + `"}},"required":["url"]}`)
			c.Sample = []byte(`{"url":"u"}`)
		}, "output.schema"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validContract()
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
