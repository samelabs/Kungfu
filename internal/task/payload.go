package task

// Payload utilities shared by contract validation (WO-2a) and
// submission intake (WO-4): RFC 6901 pointer walks, credential
// scanning, draft 2020-12 schema validation with a per-(task, version)
// compiled-schema cache, and the canonical payload hash used for
// idempotency.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"kungfu.md/internal/security"
)

// JSONPointerStrings returns the RFC 6901 pointer of every string
// value in the raw JSON document (pointers start with "/"; the root
// itself is ""). Malformed JSON yields nil.
func JSONPointerStrings(raw []byte) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var out []string
	var walk func(node any, pointer string)
	walk = func(node any, pointer string) {
		switch v := node.(type) {
		case map[string]any:
			for k, item := range v {
				walk(item, joinPointer(pointer, escapeJSONPointerToken(k)))
			}
		case []any:
			for i, item := range v {
				walk(item, joinPointer(pointer, fmt.Sprintf("%d", i)))
			}
		case string:
			out = append(out, pointer)
		}
	}
	walk(doc, "")
	return out
}

// ScanCredentials returns the pointers of every credential-shaped
// string in the raw JSON document (RFC 6901, "/"-prefixed, root "").
// Malformed JSON yields nil.
func ScanCredentials(raw []byte) []string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	var out []string
	var walk func(node any, pointer string)
	walk = func(node any, pointer string) {
		switch v := node.(type) {
		case map[string]any:
			// keys too: a credential as a property name is just as leaked
			for k, item := range v {
				child := joinPointer(pointer, escapeJSONPointerToken(k))
				if security.ContainsCredential(k) {
					out = append(out, child)
				}
				walk(item, child)
			}
		case []any:
			for i, item := range v {
				walk(item, joinPointer(pointer, fmt.Sprintf("%d", i)))
			}
		case string:
			if security.ContainsCredential(v) {
				out = append(out, pointer)
			}
		}
	}
	walk(doc, "")
	return out
}

func joinPointer(pointer, token string) string {
	if pointer == "" {
		return "/" + token
	}
	return pointer + "/" + token
}

// PointerError is one payload validation finding at an RFC 6901
// pointer ("/"-prefixed; the root document itself is "").
type PointerError struct {
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

func (e PointerError) Error() string {
	if e.Pointer == "" {
		return e.Message
	}
	return e.Pointer + ": " + e.Message
}

// compiledSchemaCache caches compiled output.schemas by the hex
// sha256 of the schema bytes — the hot SubmitWork path never
// recompiles, and an edited schema naturally gets a new key.
var compiledSchemaCache sync.Map // hex(sha256(schema)) -> *jsonschema.Schema

// ValidatePayloadForTask validates payload against schema (draft
// 2020-12), caching the compiled schema by its content hash.
func ValidatePayloadForTask(schema, payload []byte) []PointerError {
	key := fmt.Sprintf("%x", sha256.Sum256(schema))
	cached, ok := compiledSchemaCache.Load(key)
	var sch *jsonschema.Schema
	if ok {
		sch = cached.(*jsonschema.Schema)
	} else {
		sch = compilePayloadSchema(schema)
		if sch != nil {
			compiledSchemaCache.Store(key, sch)
		}
	}
	if sch == nil {
		return []PointerError{{Pointer: "", Message: "task schema is not a compilable JSON Schema"}}
	}
	return validatePayloadWith(sch, payload)
}

// ValidatePayload validates payload against schema without any cache
// (used where the schema is transient, e.g. contract acceptance).
func ValidatePayload(schema, payload []byte) []PointerError {
	sch := compilePayloadSchema(schema)
	if sch == nil {
		return []PointerError{{Pointer: "", Message: "schema is not a compilable JSON Schema"}}
	}
	return validatePayloadWith(sch, payload)
}

// compilePayloadSchema compiles a payload schema; nil means it does
// not compile (the caller surfaces that as an error at the root).
func compilePayloadSchema(schema []byte) *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("payload:schema", doc); err != nil {
		return nil
	}
	sch, err := compiler.Compile("payload:schema")
	if err != nil {
		return nil
	}
	return sch
}

func validatePayloadWith(sch *jsonschema.Schema, payload []byte) []PointerError {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return []PointerError{{Pointer: "", Message: "payload is not valid JSON: " + err.Error()}}
	}
	verr := sch.Validate(instance)
	if verr == nil {
		return nil
	}
	valErr, ok := verr.(*jsonschema.ValidationError)
	if !ok {
		return []PointerError{{Pointer: "", Message: verr.Error()}}
	}
	// One PointerError per LEAF violation of the cause tree, at its
	// instance location; duplicated locations collapse to the first.
	printer := message.NewPrinter(language.English)
	var out []PointerError
	seen := map[string]bool{}
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			ptr := tokensToPointer(e.InstanceLocation)
			if !seen[ptr] {
				seen[ptr] = true
				out = append(out, PointerError{Pointer: ptr, Message: e.ErrorKind.LocalizedString(printer)})
			}
			return
		}
		for _, cause := range e.Causes {
			walk(cause)
		}
	}
	walk(valErr)
	return out
}

// tokensToPointer joins instance-location tokens into an RFC 6901
// pointer ("/"-prefixed; no tokens means the root document itself).
func tokensToPointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	for _, tok := range tokens {
		b.WriteByte('/')
		b.WriteString(escapeJSONPointerToken(tok))
	}
	return b.String()
}

// PayloadHash is the idempotency identity of a payload: the payload
// decoded (numbers preserved exactly) and re-encoded as canonical JSON
// (object keys sorted), hashed with SHA-256 as lowercase hex. Field
// order and whitespace differences hash identically.
func PayloadHash(payload []byte) string {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:])
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
