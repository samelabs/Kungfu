package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
	"kungfu.md/internal/security"
)

// KungfuPushInput holds validated kungfu push payload.
type KungfuPushInput struct {
	Code        string
	Title       string
	Tags        []string
	Description string
	Content     string
	Checksum    string
}

// KungfuPushResult is the return value of Push.
type KungfuPushResult struct {
	Code       string  `json:"code"`
	Title      string  `json:"title"`
	Action     string  `json:"action"`
	Checksum   string  `json:"checksum"`
	Visibility string  `json:"visibility"`
	Balance    float64 `json:"balance"`
}

// Push creates or updates a kungfu.
func Push(ctx context.Context, pool *pg.Pool, botID int64, input map[string]interface{},
	maxTitleLen, maxTags, maxTagLen, maxDescLen, maxContentSize int) (*KungfuPushResult, error) {

	payload, err := validateKungfuPayload(input, maxTitleLen, maxTags, maxTagLen, maxDescLen, maxContentSize)
	if err != nil {
		return nil, err
	}

	if payload.Code != "" {
		// Update existing
		existing, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, botID, payload.Code)
		if err != nil || existing == nil {
			// Check if it exists but owned by someone else
			any, _ := repository.FindActiveKungfuByCode(ctx, pool, payload.Code)
			if any == nil {
				return nil, errors.New(404, "NOT_FOUND", "Kungfu not found")
			}
			return nil, errors.New(403, "NOT_OWNER", "Only the creator can update this Kungfu")
		}

		tagsJSONBytes, _ := json.Marshal(payload.Tags)
		repository.UpdateKungfuContentByID(ctx, pool, existing.ID,
			payload.Title, string(tagsJSONBytes), payload.Description, payload.Content, payload.Checksum)

		balance := GetBalance(ctx, pool, botID)

		logOperation(ctx, pool, &botID, "push", strPtr("kungfu"), &existing.Code,
			map[string]interface{}{"title": payload.Title, "action": "updated"}, true)

		return &KungfuPushResult{
			Code: existing.Code, Title: payload.Title, Action: "updated",
			Checksum: payload.Checksum, Visibility: existing.Visibility, Balance: balance,
		}, nil
	}

	// Create new (charge 1 credit)
	tx, txErr := pool.TxBegin(ctx)
	if txErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during publishing")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	balance, recErr := Record(ctx, pool, tx, botID, "spend_push", AmountPush, strPtr("kungfu"), nil)
	if recErr != nil {
		return nil, errors.New(402, "INSUFFICIENT_CREDITS", "Need 1 credit to publish kungfu. Complete platform tasks to earn credits.")
	}

	code, codeErr := repository.GenerateUniqueKungfuCode(ctx, tx)
	if codeErr != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during publishing")
	}

	tagsJSONBytes, _ := json.Marshal(payload.Tags)
	repository.InsertNewKungfu(ctx, tx, code, botID,
		payload.Title, string(tagsJSONBytes), payload.Description, payload.Content, payload.Checksum)

	if err := tx.Commit(ctx); err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error occurred during publishing")
	}

	logOperation(ctx, pool, &botID, "push", strPtr("kungfu"), &code,
		map[string]interface{}{"title": payload.Title, "action": "created"}, true)

	return &KungfuPushResult{
		Code: code, Title: payload.Title, Action: "created",
		Checksum: payload.Checksum, Visibility: "private", Balance: balance,
	}, nil
}

// Share makes a kungfu public.
func Share(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]interface{}, error) {
	k, err := requireOwnedKungfu(ctx, pool, botID, code, "Only the creator can change sharing status")
	if err != nil {
		return nil, err
	}

	if k.Visibility == "public" {
		return map[string]interface{}{
			"code": code, "visibility": "public",
			"message": "Already public. Share this code with other agents.",
		}, nil
	}

	repository.UpdateKungfuVisibilityByID(ctx, pool, k.ID, "public")
	logOperation(ctx, pool, &botID, "share", strPtr("kungfu"), &code,
		map[string]interface{}{"title": k.Title}, true)

	return map[string]interface{}{
		"code": code, "visibility": "public",
		"message": "Now public. Share this code with other agents.",
	}, nil
}

// Unshare makes a kungfu private.
func Unshare(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]interface{}, error) {
	k, err := requireOwnedKungfu(ctx, pool, botID, code, "Only the creator can change sharing status")
	if err != nil {
		return nil, err
	}

	if k.Visibility == "private" {
		return map[string]interface{}{
			"code": code, "visibility": "private",
			"message": "Already private",
		}, nil
	}

	repository.UpdateKungfuVisibilityByID(ctx, pool, k.ID, "private")
	logOperation(ctx, pool, &botID, "unshare", strPtr("kungfu"), &code,
		map[string]interface{}{"title": k.Title}, true)

	return map[string]interface{}{
		"code": code, "visibility": "private",
		"message": "Now private. Only you can access.",
	}, nil
}

// Delete soft-deletes a kungfu.
func Delete(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]interface{}, error) {
	k, err := requireOwnedKungfu(ctx, pool, botID, code, "Only the creator can delete this Kungfu")
	if err != nil {
		return nil, err
	}

	repository.SoftDeleteKungfuByID(ctx, pool, k.ID)
	logOperation(ctx, pool, &botID, "delete", strPtr("kungfu"), &code,
		map[string]interface{}{"title": k.Title}, true)

	return map[string]interface{}{
		"code": code, "title": k.Title,
		"message": "Deleted (bots that already acquired can still use normally)",
	}, nil
}

// requireOwnedKungfu finds an owned kungfu or returns appropriate errors.
func requireOwnedKungfu(ctx context.Context, pool *pg.Pool, botID int64, code, ownerError string) (*model.Kungfu, error) {
	k, err := repository.FindOwnedActiveKungfuByCode(ctx, pool, botID, code)
	if err == nil && k != nil {
		return k, nil
	}
	// Check if it exists but owned by someone else
	any, _ := repository.FindActiveKungfuByCode(ctx, pool, code)
	if any == nil {
		return nil, errors.New(404, "NOT_FOUND", "Kungfu not found")
	}
	return nil, errors.New(403, "NOT_OWNER", ownerError)
}

// validateKungfuPayload validates input for push.
func validateKungfuPayload(input map[string]interface{}, maxTitleLen, maxTags, maxTagLen, maxDescLen, maxContentSize int) (*KungfuPushInput, error) {
	// Check required fields
	for _, field := range []string{"title", "tags", "content"} {
		if _, ok := input[field]; !ok || input[field] == nil {
			return nil, errors.New(400, "MISSING_FIELD", "Missing required field: "+field)
		}
	}

	code := ""
	if v, ok := input["code"]; ok {
		if s, ok := v.(string); ok {
			code = strings.TrimSpace(s)
		}
	}

	title := strings.TrimSpace(getStr(input["title"]))
	if title == "" {
		return nil, errors.New(400, "MISSING_FIELD", "Missing required field: title")
	}

	description := ""
	if v, ok := input["description"]; ok {
		if s, ok := v.(string); ok {
			description = strings.TrimSpace(s)
		}
	}

	content := getStr(input["content"])

	// Validate code format if provided
	if code != "" {
		valid, err := validateCodeFormat(code)
		if err != nil {
			return nil, err
		}
		if !valid {
			return nil, errors.New(400, "INVALID_CODE", "Invalid code format")
		}
		code = strings.ToLower(code)
	}

	// Title length
	if utf8.RuneCountInString(title) > maxTitleLen {
		return nil, errors.New(400, "TITLE_TOO_LONG", "Title maximum 128 characters")
	}

	// Tags validation
	tagsRaw := input["tags"]
	tagsArr, ok := tagsRaw.([]interface{})
	if !ok {
		return nil, errors.New(400, "INVALID_TAGS", "tags must be an array")
	}
	if len(tagsArr) < 1 {
		return nil, errors.New(400, "INVALID_TAGS", "At least one tag is required")
	}
	if len(tagsArr) > maxTags {
		return nil, errors.New(400, "TOO_MANY_TAGS", "Maximum 10 tags")
	}

	tags := make([]string, 0, len(tagsArr))
	for _, tag := range tagsArr {
		s, ok := tag.(string)
		if !ok {
			return nil, errors.New(400, "INVALID_TAGS", "Each tag must be a string")
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, errors.New(400, "INVALID_TAGS", "Tags cannot be empty")
		}
		if utf8.RuneCountInString(s) > maxTagLen {
			return nil, errors.New(400, "TAG_TOO_LONG", "Each tag maximum 32 characters")
		}
		tags = append(tags, s)
	}

	// Description length
	if utf8.RuneCountInString(description) > maxDescLen {
		return nil, errors.New(400, "DESCRIPTION_TOO_LONG", "Description maximum 500 characters")
	}

	// Content size
	if len(content) > maxContentSize {
		return nil, errors.New(400, "CONTENT_TOO_LARGE", "Content exceeds 100KB limit")
	}

	// Content min length
	if utf8.RuneCountInString(strings.TrimSpace(content)) < 50 {
		return nil, errors.New(400, "CONTENT_TOO_SHORT", "Content too short (minimum 50 characters)")
	}

	// Security check
	allFields := map[string]interface{}{
		"code": code, "title": title, "tags": tags,
		"description": description, "content": content,
	}
	if security.ContainsAPIKey(allFields) {
		return nil, errors.New(400, "SENSITIVE_CONTENT", "kungfu payload must not contain API keys")
	}

	// Checksum
	h := sha256.Sum256([]byte(content))
	checksum := hex.EncodeToString(h[:])

	return &KungfuPushInput{
		Code: code, Title: title, Tags: tags,
		Description: description, Content: content, Checksum: checksum,
	}, nil
}

func validateCodeFormat(code string) (bool, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return false, nil
	}
	if len(code) != 12 {
		return false, nil
	}
	for _, c := range code {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false, nil
		}
	}
	return true, nil
}

func getStr(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	// Try JSON marshal for other types
	b, _ := json.Marshal(v)
	return string(b)
}
