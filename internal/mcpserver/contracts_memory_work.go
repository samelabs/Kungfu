package mcpserver

// Typed MCP output contracts. These DTOs describe TRANSPORT output
// only: each is an exact projection of facts already returned by the
// existing services — nothing invented, nothing dropped. When a
// REQUIRED fact is missing or has the wrong type in a service result,
// the projection fails safely with INTERNAL_ERROR instead of
// manufacturing a misleading zero value. Optional/nullable facts
// (e.g. *string description) are preserved as they are.

import (
	"fmt"
)

// ---- memory DTOs ----

// MemoryItem carries the exact list-service facts (no checksum — the
// list service does not provide it).
type MemoryItem struct {
	Code        string   `json:"code"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Description *string  `json:"description"`
	Visibility  string   `json:"visibility"`
	Bytes       int64    `json:"bytes"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// MemoryListOutput projects service.ListKungfusForBot including its
// meta block (total/returned/offset/has_more).
type MemoryListOutput struct {
	Items    []MemoryItem `json:"kungfus"`
	Total    int          `json:"total"`
	Returned int          `json:"returned"`
	Offset   int          `json:"offset"`
	HasMore  bool         `json:"has_more"`
}

// MemoryGetOutput projects service.GetKungfuForBot (detail, has
// content+checksum).
type MemoryGetOutput struct {
	Code        string   `json:"code"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Description *string  `json:"description"`
	Content     string   `json:"content"`
	Checksum    string   `json:"checksum"`
	Visibility  string   `json:"visibility"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// MemoryPutOutput projects service.Push (already a typed service result).
type MemoryPutOutput struct {
	Code       string `json:"code"`
	Title      string `json:"title"`
	Action     string `json:"action"`
	Checksum   string `json:"checksum"`
	Visibility string `json:"visibility"`
}

// MemoryVisibilityOutput projects service.Share / service.Unshare.
type MemoryVisibilityOutput struct {
	Code       string `json:"code"`
	Visibility string `json:"visibility"`
	Message    string `json:"message"`
}

// MemoryDeleteOutput projects the REAL service.Delete result. There is
// no invented "deleted" flag — successful tool completion IS the
// success fact.
type MemoryDeleteOutput struct {
	Code    string `json:"code"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

// ---- safe local extraction helpers ----
// req* helpers: required facts. Missing/wrong type => error (caller
// maps to INTERNAL_ERROR). opt* helpers: optional/nullable facts,
// preserved legitimately.

func reqString(m map[string]interface{}, k string) (string, error) {
	v, ok := m[k]
	if !ok {
		return "", fmt.Errorf("projection: missing required field %q", k)
	}
	switch t := v.(type) {
	case string:
		return t, nil
	case *string:
		if t == nil {
			return "", fmt.Errorf("projection: required field %q is nil", k)
		}
		return *t, nil
	}
	return "", fmt.Errorf("projection: field %q has wrong type %T", k, v)
}

func optStringPtr(m map[string]interface{}, k string) *string {
	switch t := m[k].(type) {
	case nil:
		return nil
	case string:
		return &t
	case *string:
		return t
	}
	return nil
}

func reqStrings(m map[string]interface{}, k string) ([]string, error) {
	switch t := m[k].(type) {
	case []string:
		return t, nil
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("projection: field %q contains non-string", k)
			}
			out = append(out, s)
		}
		return out, nil
	}
	if _, exists := m[k]; !exists {
		return nil, fmt.Errorf("projection: missing required field %q", k)
	}
	return nil, fmt.Errorf("projection: field %q has wrong type %T", k, m[k])
}

// reqInt64 extracts a required CREDIT-bearing integer fact from a
// service result map. The authoritative service values are typed Go
// int64 (task price/budget, billing reward/balance — populated from
func reqInt(m map[string]interface{}, k string) (int, error) {
	switch t := m[k].(type) {
	case int:
		return t, nil
	case int64:
		return int(t), nil
	case float64:
		return int(t), nil
	}
	if _, exists := m[k]; !exists {
		return 0, fmt.Errorf("projection: missing required field %q", k)
	}
	return 0, fmt.Errorf("projection: field %q has wrong type %T", k, m[k])
}

// ---- projections: service result -> typed MCP output ----

func projectMemoryList(result map[string]interface{}) (MemoryListOutput, error) {
	out := MemoryListOutput{Items: []MemoryItem{}}
	projectItem := func(m map[string]interface{}) (MemoryItem, error) {
		code, err := reqString(m, "code")
		if err != nil {
			return MemoryItem{}, err
		}
		title, err := reqString(m, "title")
		if err != nil {
			return MemoryItem{}, err
		}
		tags, err := reqStrings(m, "tags")
		if err != nil {
			return MemoryItem{}, err
		}
		visibility, err := reqString(m, "visibility")
		if err != nil {
			return MemoryItem{}, err
		}
		createdAt, err := reqString(m, "created_at")
		if err != nil {
			return MemoryItem{}, err
		}
		updatedAt, err := reqString(m, "updated_at")
		if err != nil {
			return MemoryItem{}, err
		}
		bytesVal, err := reqInt(m, "bytes")
		if err != nil {
			return MemoryItem{}, err
		}
		return MemoryItem{
			Code:        code,
			Title:       title,
			Tags:        tags,
			Description: optStringPtr(m, "description"),
			Visibility:  visibility,
			Bytes:       int64(bytesVal),
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
		}, nil
	}
	appendRaw := func(e interface{}) error {
		m, ok := e.(map[string]interface{})
		if !ok {
			return fmt.Errorf("projection: list item is %T, want map", e)
		}
		item, err := projectItem(m)
		if err != nil {
			return err
		}
		out.Items = append(out.Items, item)
		return nil
	}
	switch raw := result["kungfus"].(type) {
	case []interface{}:
		for _, e := range raw {
			if err := appendRaw(e); err != nil {
				return MemoryListOutput{}, err
			}
		}
	case []map[string]interface{}:
		for _, e := range raw {
			if err := appendRaw(map[string]interface{}(e)); err != nil {
				return MemoryListOutput{}, err
			}
		}
	default:
		return MemoryListOutput{}, fmt.Errorf("projection: kungfus list has wrong type %T", result["kungfus"])
	}
	meta, ok := result["meta"].(map[string]interface{})
	if !ok {
		return MemoryListOutput{}, fmt.Errorf("projection: missing meta block")
	}
	var err error
	if out.Total, err = reqInt(meta, "total"); err != nil {
		return MemoryListOutput{}, err
	}
	if out.Returned, err = reqInt(meta, "returned"); err != nil {
		return MemoryListOutput{}, err
	}
	if out.Offset, err = reqInt(meta, "offset"); err != nil {
		return MemoryListOutput{}, err
	}
	hasMore, ok := meta["has_more"].(bool)
	if !ok {
		return MemoryListOutput{}, fmt.Errorf("projection: meta.has_more missing or wrong type")
	}
	out.HasMore = hasMore
	return out, nil
}

func projectMemoryGet(result map[string]interface{}) (MemoryGetOutput, error) {
	var out MemoryGetOutput
	var err error
	if out.Code, err = reqString(result, "code"); err != nil {
		return out, err
	}
	if out.Title, err = reqString(result, "title"); err != nil {
		return out, err
	}
	if out.Tags, err = reqStrings(result, "tags"); err != nil {
		return out, err
	}
	if out.Content, err = reqString(result, "content"); err != nil {
		return out, err
	}
	if out.Checksum, err = reqString(result, "checksum"); err != nil {
		return out, err
	}
	if out.Visibility, err = reqString(result, "visibility"); err != nil {
		return out, err
	}
	if out.CreatedAt, err = reqString(result, "created_at"); err != nil {
		return out, err
	}
	if out.UpdatedAt, err = reqString(result, "updated_at"); err != nil {
		return out, err
	}
	out.Description = optStringPtr(result, "description")
	return out, nil
}

func projectVisibility(result map[string]interface{}) (MemoryVisibilityOutput, error) {
	var out MemoryVisibilityOutput
	var err error
	if out.Code, err = reqString(result, "code"); err != nil {
		return out, err
	}
	if out.Visibility, err = reqString(result, "visibility"); err != nil {
		return out, err
	}
	if out.Message, err = reqString(result, "message"); err != nil {
		return out, err
	}
	return out, nil
}

func projectDelete(result map[string]interface{}) (MemoryDeleteOutput, error) {
	var out MemoryDeleteOutput
	var err error
	if out.Code, err = reqString(result, "code"); err != nil {
		return out, err
	}
	if out.Title, err = reqString(result, "title"); err != nil {
		return out, err
	}
	if out.Message, err = reqString(result, "message"); err != nil {
		return out, err
	}
	return out, nil
}

func mapProjErr(err error) error {
	return &ToolError{Code: "INTERNAL_ERROR", Message: "An internal error occurred"}
}
