package service

import (
	"context"
	"encoding/json"

	"kungfu.md/internal/errors"
	"kungfu.md/internal/model"
	"kungfu.md/internal/pg"
	"kungfu.md/internal/repository"
)

// MinOpenBudget matches PHP TaskUtils::MIN_OPEN_BUDGET
const MinOpenBudget = 1000.0

// -- KungfuReadService (maps to PHP services/KungfuReadService.php) --

func ListKungfusForBot(ctx context.Context, pool *pg.Pool, botID int64, botBalance float64, limit, offset int) (map[string]interface{}, error) {
	total, _ := repository.CountActiveKungfusByBotID(ctx, pool, botID)
	rows, err := repository.ListActiveKungfusByBotID(ctx, pool, botID, limit, offset)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error listing kungfus")
	}

	items := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		items = append(items, kungfuListItemFromRepo(&row))
	}

	logOperation(ctx, pool, &botID, "kungfus_list", nil, nil,
		map[string]interface{}{"returned": len(items)}, true)

	return map[string]interface{}{
		"kungfus": items,
		"balance": botBalance,
		"meta": map[string]interface{}{
			"total":    total,
			"returned": len(items),
			"offset":   offset,
			"has_more": (offset + len(items)) < int(total),
		},
	}, nil
}

func GetKungfuForBot(ctx context.Context, pool *pg.Pool, botID int64, botBalance float64, code string) (map[string]interface{}, error) {
	k, err := repository.FindActiveKungfuByCode(ctx, pool, code)
	if err != nil || k == nil {
		return nil, errors.New(404, "NOT_FOUND", "Kungfu not found")
	}

	isOwner := k.BotID == botID

	if !isOwner && k.Visibility != "public" {
		return nil, errors.New(403, "PRIVATE_KUNGFU", "This kungfu is private")
	}

	balance := botBalance
	if !isOwner {
		newBalance, err := Record(ctx, pool, nil, botID, "spend_get", AmountGet, strPtr("kungfu"), &code)
		if err != nil {
			return nil, errors.New(402, "INSUFFICIENT_CREDITS", "Need 1 credit to retrieve. Complete platform tasks to earn credits.")
		}
		balance = newBalance
	}

	logOperation(ctx, pool, &botID, "get", strPtr("kungfu"), &code,
		map[string]interface{}{"title": k.Title, "owner": isOwner}, true)

	return kungfuDetailFromModel(k, balance), nil
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

func kungfuDetailFromModel(k *model.Kungfu, balance float64) map[string]interface{} {
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
		"balance":     balance,
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
