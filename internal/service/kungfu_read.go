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

// GetKungfuRevisionForBot serves memory_get(code, revision) — the
// versioned read (kungfu.md §5/§9):
//   - the author reads ANY existing revision of their memory, from
//     the current row or memory_revisions, valid or withdrawn
//     (作者恒可读自己的任何版本);
//   - a non-author never receives a pinned revision: only the
//     current version of a valid PUBLIC memory is readable by
//     others, so an explicit revision is a permission error.
func GetKungfuRevisionForBot(ctx context.Context, pool *pg.Pool, botID int64, code string, revision int64) (map[string]interface{}, error) {
	k, err := repository.FindKungfuByCodeAnyStatus(ctx, pool, code)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving Kungfu")
	}
	if k == nil {
		return nil, errors.New(404, "NOT_FOUND", "Kungfu not found")
	}

	if k.BotID != botID {
		// Non-author: a valid memory still never serves a specific
		// revision to others; a withdrawn one is not readable at all
		// (no existence leak — same answer the current-version path
		// gives).
		if k.Status != "active" {
			return nil, errors.New(404, "NOT_FOUND", "Kungfu not found")
		}
		return nil, errors.New(403, "NOT_OWNER", "Only the creator can read a specific revision")
	}

	// Author: the requested revision is the current one → the row
	// itself; otherwise the archived snapshot must exist.
	if revision == k.Revision {
		logOperation(ctx, pool, &botID, "get", strPtr("kungfu"), &code,
			map[string]interface{}{"title": k.Title, "owner": true, "revision": revision}, true)
		return kungfuDetailFromModel(k), nil
	}

	snapshot, err := repository.FindKungfuRevision(ctx, pool, k.ID, revision)
	if err != nil {
		return nil, errors.New(500, "INTERNAL_ERROR", "Error retrieving Kungfu")
	}
	if snapshot == nil {
		return nil, errors.New(404, "NOT_FOUND", "Kungfu not found")
	}

	logOperation(ctx, pool, &botID, "get", strPtr("kungfu"), &code,
		map[string]interface{}{"title": snapshot.Title, "owner": true, "revision": revision}, true)

	return kungfuDetailFromRevision(k, snapshot), nil
}

// -- Kungfu model → presenter maps --

func kungfuListItemFromRepo(k *repository.KungfuListItem) map[string]interface{} {
	return map[string]interface{}{
		"code":        k.Code,
		"title":       k.Title,
		"tags":        parseJSONTags(k.TagsJSON),
		"description": k.Description,
		"visibility":  k.Visibility,
		"revision":    k.Revision,
		"origin":      k.Origin,
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
		"revision":    k.Revision,
		"created_at":  k.CreatedAt,
		"updated_at":  k.UpdatedAt,
	}
}

// kungfuDetailFromRevision projects one archived version: the
// snapshot's content fields and write time, with the memory-level
// facts (code, visibility, creation time) from the current row.
func kungfuDetailFromRevision(k *model.Kungfu, r *model.KungfuRevision) map[string]interface{} {
	return map[string]interface{}{
		"code":        k.Code,
		"title":       r.Title,
		"tags":        parseJSONTags(r.TagsJSON),
		"description": r.Description,
		"content":     r.Content,
		"checksum":    r.Checksum,
		"visibility":  k.Visibility,
		"revision":    r.Revision,
		"created_at":  k.CreatedAt,
		"updated_at":  r.UpdatedAt,
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
