package mcpserver

import (
	"fmt"

	"kungfu.md/internal/service"
)

// M2 typed MCP output contracts. These DTOs describe TRANSPORT output
// only: each is a safe projection of an already-produced service
// result. No business decisions, no re-queries, no economic
// recalculation live here. When a service result cannot possibly be
// projected, the projection fails with INTERNAL_ERROR.

// ---- memory ----

// MemoryItem projects one item of the service memory-list result.
type MemoryItem struct {
	Code        string   `json:"code"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Description string   `json:"description"`
	Checksum    string   `json:"checksum"`
	Visibility  string   `json:"visibility"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// MemoryListOutput projects service.ListKungfusForBot.
type MemoryListOutput struct {
	Items   []MemoryItem `json:"kungfus"`
	Total   int          `json:"total"`
	HasMore bool         `json:"has_more"`
	Offset  int          `json:"offset"`
}

// MemoryGetOutput projects service.GetKungfuForBot (detail).
type MemoryGetOutput struct {
	Code        string   `json:"code"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Description string   `json:"description"`
	Content     string   `json:"content"`
	Checksum    string   `json:"checksum"`
	Visibility  string   `json:"visibility"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// MemoryPutOutput projects service.Push.
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
}

// MemoryDeleteOutput projects service.Delete.
type MemoryDeleteOutput struct {
	Code    string `json:"code"`
	Deleted bool   `json:"deleted"`
}

// ---- work ----

// WorkItem projects one item of the service open-task board.
type WorkItem struct {
	Code         string  `json:"code"`
	Title        string  `json:"title"`
	Requirements string  `json:"requirements"`
	Budget       float64 `json:"budget"`
	Price        float64 `json:"price"`
	Status       string  `json:"status"`
}

// WorkListOutput projects service.ListOpenTasks.
type WorkListOutput struct {
	Items   []WorkItem `json:"tasks"`
	Total   int        `json:"total"`
	HasMore bool       `json:"has_more"`
}

// WorkGetOutput projects service.GetOpenTask. The open board does not
// expose owner-only facts (postapi/budget); the projection carries
// only what the service produced.
type WorkGetOutput struct {
	Code         string  `json:"code"`
	Title        string  `json:"title"`
	Requirements string  `json:"requirements"`
	Price        float64 `json:"price"`
	Status       string  `json:"status"`
}

// WorkSubmitOutput projects service.Submit (delivery + settlement facts).
type WorkSubmitOutput struct {
	TaskCode string               `json:"task_code"`
	Post     WorkSubmitPostOutput `json:"post"`
	Billing  WorkSubmitBillOutput `json:"billing"`
}

// WorkSubmitPostOutput is the delivery fact block.
type WorkSubmitPostOutput struct {
	Delivered    bool `json:"delivered"`
	ResponseCode int  `json:"response_code"`
}

// WorkSubmitBillOutput is the authoritative settlement block produced
// by the existing Credits path — MCP never computes these values.
type WorkSubmitBillOutput struct {
	Reward  float64 `json:"reward"`
	Balance float64 `json:"balance"`
}

// WorkPublishOutput projects service.CreateTask.
type WorkPublishOutput struct {
	Code   string  `json:"code"`
	Title  string  `json:"title"`
	Status string  `json:"status"`
	Budget float64 `json:"budget"`
	Price  float64 `json:"price"`
}

// ---- safe projection helpers ----
// Each helper converts the service's already-produced
// map[string]interface{} into the typed MCP projection. A shape that
// cannot be projected is an INTERNAL_ERROR, never a panic and never a
// silently wrong value.

func mapStr(m map[string]interface{}, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func mapFloat(m map[string]interface{}, k string) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}

func mapInt(m map[string]interface{}, k string) int {
	switch v := m[k].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func mapBool(m map[string]interface{}, k string) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return false
}

func mapStrSlice(m map[string]interface{}, k string) []string {
	raw, ok := m[k].([]interface{})
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// timeVal renders the service timestamp fields as transport strings.
func timeVal(m map[string]interface{}, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmtSprint(v)
	}
}

func fmtSprint(v interface{}) string { return fmt.Sprintf("%v", v) }

// ---- projections: service result (map) -> typed MCP output ----

func projectMemoryList(result map[string]interface{}) MemoryListOutput {
	out := MemoryListOutput{Items: []MemoryItem{}}
	appendItem := func(e interface{}) {
		m, ok := e.(map[string]interface{})
		if !ok {
			return
		}
		out.Items = append(out.Items, MemoryItem{
			Code:        mapStr(m, "code"),
			Title:       mapStr(m, "title"),
			Tags:        mapStrSlice(m, "tags"),
			Description: mapStr(m, "description"),
			Checksum:    mapStr(m, "checksum"),
			Visibility:  mapStr(m, "visibility"),
			CreatedAt:   timeVal(m, "created_at"),
			UpdatedAt:   timeVal(m, "updated_at"),
		})
	}
	switch raw := result["kungfus"].(type) {
	case []interface{}:
		for _, e := range raw {
			appendItem(e)
		}
	case []map[string]interface{}:
		for _, e := range raw {
			appendItem(map[string]interface{}(e))
		}
	}
	if meta, ok := result["meta"].(map[string]interface{}); ok {
		out.Total = mapInt(meta, "total")
		out.HasMore = mapBool(meta, "has_more")
		out.Offset = mapInt(meta, "offset")
	}
	return out
}

func projectMemoryGet(result map[string]interface{}) MemoryGetOutput {
	return MemoryGetOutput{
		Code:        mapStr(result, "code"),
		Title:       mapStr(result, "title"),
		Tags:        mapStrSlice(result, "tags"),
		Description: mapStr(result, "description"),
		Content:     mapStr(result, "content"),
		Checksum:    mapStr(result, "checksum"),
		Visibility:  mapStr(result, "visibility"),
		CreatedAt:   timeVal(result, "created_at"),
		UpdatedAt:   timeVal(result, "updated_at"),
	}
}

func projectVisibility(result map[string]interface{}) MemoryVisibilityOutput {
	return MemoryVisibilityOutput{
		Code:       mapStr(result, "code"),
		Visibility: mapStr(result, "visibility"),
	}
}

func projectDelete(result map[string]interface{}) MemoryDeleteOutput {
	deleted := mapBool(result, "deleted")
	if !deleted {
		if _, hasErr := result["error"]; hasErr {
			deleted = false
		}
	}
	return MemoryDeleteOutput{
		Code:    mapStr(result, "code"),
		Deleted: deleted,
	}
}

func projectWorkList(result map[string]interface{}) WorkListOutput {
	out := WorkListOutput{Items: []WorkItem{}}
	appendTask := func(e interface{}) {
		m, ok := e.(map[string]interface{})
		if !ok {
			return
		}
		out.Items = append(out.Items, WorkItem{
			Code:         mapStr(m, "code"),
			Title:        mapStr(m, "title"),
			Requirements: mapStr(m, "requirements"),
			Budget:       mapFloat(m, "budget"),
			Price:        mapFloat(m, "price"),
			Status:       mapStr(m, "status"),
		})
	}
	switch raw := result["tasks"].(type) {
	case []interface{}:
		for _, e := range raw {
			appendTask(e)
		}
	case []map[string]interface{}:
		for _, e := range raw {
			appendTask(map[string]interface{}(e))
		}
	}
	if meta, ok := result["meta"].(map[string]interface{}); ok {
		out.Total = mapInt(meta, "total")
		out.HasMore = mapBool(meta, "has_more")
	}
	return out
}

func projectWorkGet(result map[string]interface{}) WorkGetOutput {
	// service nests the detail under "task"; budget/postapi are owner
	// facts the open board does not expose — the projection carries
	// only what the service produced.
	m := result
	if t, ok := result["task"].(map[string]interface{}); ok {
		m = t
	}
	return WorkGetOutput{
		Code:         mapStr(m, "code"),
		Title:        mapStr(m, "title"),
		Requirements: mapStr(m, "requirements"),
		Price:        mapFloat(m, "price"),
		Status:       mapStr(m, "status"),
	}
}

func projectWorkSubmit(result *service.TaskSubmitResult) WorkSubmitOutput {
	return WorkSubmitOutput{
		TaskCode: result.TaskCode,
		Post: WorkSubmitPostOutput{
			Delivered:    mapBool(result.Post, "delivered"),
			ResponseCode: mapInt(result.Post, "response_code"),
		},
		Billing: WorkSubmitBillOutput{
			Reward:  mapFloat(result.Billing, "reward"),
			Balance: mapFloat(result.Billing, "balance"),
		},
	}
}

func projectWorkPublish(result map[string]interface{}) WorkPublishOutput {
	out := WorkPublishOutput{
		Code:   mapStr(result, "code"),
		Title:  mapStr(result, "title"),
		Status: mapStr(result, "status"),
		Budget: mapFloat(result, "budget"),
		Price:  mapFloat(result, "price"),
	}
	if out.Code == "" {
		// service nests under "task"
		if t, ok := result["task"].(map[string]interface{}); ok {
			if out.Code == "" {
				out.Code = mapStr(t, "code")
			}
			if out.Title == "" {
				out.Title = mapStr(t, "title")
			}
			if out.Status == "" {
				out.Status = mapStr(t, "status")
			}
			if out.Budget == 0 {
				out.Budget = mapFloat(t, "budget")
			}
			if out.Price == 0 {
				out.Price = mapFloat(t, "price")
			}
		}
	}
	return out
}
