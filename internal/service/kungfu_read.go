package service

import (
	"context"
	"encoding/json"

	"kungfu.md/internal/consumption"
	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// -- Kungfu read operations --

// ListKungfusForBot lists a bot's kungfu entries. The list is a pure
// storage projection: it carries no balance — balance belongs to the
// Credits/Account contract.
// Accepts pg.Querier (satisfied by *pg.Pool).
func ListKungfusForBot(ctx context.Context, q pg.Querier, botID int64, limit, offset int) (map[string]interface{}, error) {
	total, err := repository.CountActiveKungfusByBotID(ctx, q, botID)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing kungfus")
	}
	rows, err := repository.ListActiveKungfusByBotID(ctx, q, botID, limit, offset)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing kungfus")
	}

	items := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		items = append(items, kungfuListItemFromRepo(&row))
	}

	logOperation(ctx, q, &botID, "kungfus_list", nil, nil,
		map[string]interface{}{"returned": len(items)}, true)

	return map[string]interface{}{
		"kungfus": items,
		"meta": map[string]interface{}{
			"total":    total,
			"returned": len(items),
			"offset":   offset,
			"has_more": (offset + len(items)) < int(total),
		},
	}, nil
}

func GetKungfuForBot(ctx context.Context, pool *pg.Pool, botID int64, code string) (map[string]interface{}, error) {
	k, err := repository.FindActiveKungfuByCode(ctx, pool, code)
	if err != nil || k == nil {
		return nil, errors.New(404, "NOT_FOUND", "Kungfu not found")
	}

	isOwner := k.BotID == botID

	// Private non-owner is rejected BEFORE any consumption happens.
	if !isOwner && k.Visibility != "public" {
		return nil, errors.New(403, "PRIVATE_KUNGFU", "This kungfu is private")
	}

	// Non-owner reads of public kungfus are charged via the consumption
	// layer (amount + ledger type are consumption policy). The owner's
	// own reads are free.
	if !isOwner {
		if err := consumption.Apply(ctx, pool, nil, botID,
			consumption.ActionStorageGetPublic, "kungfu", code); err != nil {
			return nil, err
		}
	}

	logOperation(ctx, pool, &botID, "get", strPtr("kungfu"), &code,
		map[string]interface{}{"title": k.Title, "owner": isOwner}, true)

	return kungfuDetailFromModel(k), nil
}

// -- Kungfu model → presenter maps --

func kungfuListItemFromRepo(k *repository.KungfuListItem) map[string]interface{} {
	return map[string]interface{}{
		"code":        k.Code,
		"title":       k.Title,
		"tags":        parseJSONTags(k.TagsJSON),
		"description": k.Description,
		"visibility":  k.Visibility,
		"created_at":  k.CreatedAt,
		"updated_at":  k.UpdatedAt,
	}
}

func kungfuListItemFromModel(k *model.Kungfu) map[string]interface{} {
	return map[string]interface{}{
		"code":        k.Code,
		"title":       k.Title,
		"tags":        parseJSONTags(k.TagsJSON),
		"description": k.Description,
		"visibility":  k.Visibility,
		"created_at":  k.CreatedAt,
		"updated_at":  k.UpdatedAt,
	}
}

func kungfuDetailFromModel(k *model.Kungfu) map[string]interface{} {
	return map[string]interface{}{
		"code":        k.Code,
		"title":       k.Title,
		"tags":        parseJSONTags(k.TagsJSON),
		"description": k.Description,
		"content":     k.Content,
		"checksum":    k.Checksum,
		"visibility":  k.Visibility,
		"created_at":  k.CreatedAt,
		"updated_at":  k.UpdatedAt,
	}
}

func parseJSONTags(raw string) []string {
	if raw == "" {
		return []string{}
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return []string{}
	}
	if tags == nil {
		return []string{}
	}
	return tags
}
