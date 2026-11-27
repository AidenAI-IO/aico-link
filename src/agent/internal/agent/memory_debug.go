package agent

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const memoryDebugListMaxItems = 500

// MemoryDebugItem is a read-only inventory row for the Memory debug WebUI.
// It intentionally omits on-disk paths.
type MemoryDebugItem struct {
	ID          string   `json:"id"`
	Type        string   `json:"type,omitempty"`
	Status      string   `json:"status,omitempty"`
	Title       string   `json:"title,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Content     string   `json:"content,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Entities    []string `json:"entities,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
	Sensitive   bool     `json:"sensitive,omitempty"`
	MemoryScope string   `json:"memory_scope,omitempty"`
	Source      string   `json:"source,omitempty"`
	SessionID   string   `json:"session_id,omitempty"`
	AppName     string   `json:"app_name,omitempty"`
}

func (s *LongTermMemoryStore) ListForDebug(ctx context.Context, needle string, limit int) ([]MemoryDebugItem, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if s == nil || s.rootDir == "" {
		return nil, nil
	}
	limit = clampMemoryDebugLimit(limit)
	index, err := s.loadIndexLocked(ctx)
	if err != nil {
		return nil, err
	}
	needle = strings.ToLower(strings.TrimSpace(needle))
	now := time.Now().UTC()
	items := make([]MemoryDebugItem, 0, len(index.Memories))
	for _, entry := range index.Memories {
		if entry.Status != "active" {
			continue
		}
		parsed, path, expired, err := s.resolveActiveEntry(entry, now)
		if expired || err != nil {
			_ = path
			continue
		}
		item := MemoryDebugItem{
			ID:          entry.ID,
			Type:        entry.Type,
			Status:      entry.Status,
			Title:       parsed.Title,
			Summary:     entry.Summary,
			Content:     parsed.Content,
			Tags:        append([]string(nil), entry.Tags...),
			Entities:    append([]string(nil), entry.Entities...),
			CreatedAt:   parsed.Item.CreatedAt,
			ExpiresAt:   firstNonEmpty(parsed.Item.ExpiresAt, entry.ExpiresAt),
			Sensitive:   entry.Sensitive,
			MemoryScope: "",
		}
		if !memoryDebugItemMatches(item, needle) {
			continue
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ID > items[j].ID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (s *DeviceMemoryStore) ListForDebug(ctx context.Context, needle string, limit int) ([]MemoryDebugItem, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if s == nil || s.rootDir == "" {
		return nil, nil
	}
	limit = clampMemoryDebugLimit(limit)
	raw, err := s.readAll()
	if err != nil {
		return nil, err
	}
	needle = strings.ToLower(strings.TrimSpace(needle))
	now := time.Now().UTC()
	items := make([]MemoryDebugItem, 0, len(raw))
	for _, entry := range raw {
		if entry.Status == deviceMemoryStatusDeleted {
			continue
		}
		if memoryExpiresAtPassed(entry.ExpiresAt, now) {
			continue
		}
		item := MemoryDebugItem{
			ID:          entry.ID,
			Type:        entry.Type,
			Status:      string(entry.Status),
			Title:       entry.Title,
			Summary:     entry.Summary,
			Content:     entry.Content,
			Tags:        append([]string(nil), entry.Tags...),
			Entities:    append([]string(nil), entry.Entities...),
			CreatedAt:   entry.UpdatedAt,
			ExpiresAt:   entry.ExpiresAt,
			MemoryScope: "device",
			Source:      "device",
			AppName:     entry.AppName,
		}
		if !memoryDebugItemMatches(item, needle) {
			continue
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ID > items[j].ID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func listSessionChunksForDebug(sessionFolder, needle string, limit int) ([]MemoryDebugItem, error) {
	if strings.TrimSpace(sessionFolder) == "" {
		return nil, nil
	}
	limit = clampMemoryDebugLimit(limit)
	indexPath := filepath.Join(sessionFolder, "chunks", "index.yaml")
	index, err := loadChunkIndexFromPath(indexPath)
	if err != nil {
		return nil, err
	}
	needle = strings.ToLower(strings.TrimSpace(needle))
	items := make([]MemoryDebugItem, 0, len(index.Chunks))
	for _, chunk := range index.Chunks {
		if chunk.Status != "" && chunk.Status != "active" {
			continue
		}
		item := MemoryDebugItem{
			ID:          chunk.ID,
			Type:        "session_chunk",
			Status:      chunk.Status,
			Title:       chunk.Summary,
			Summary:     chunk.Summary,
			Content:     chunk.Summary,
			Tags:        append([]string(nil), chunk.Tags...),
			Entities:    append([]string(nil), chunk.Entities...),
			CreatedAt:   chunk.CreatedAt,
			MemoryScope: "session",
			Source:      "session_" + chunk.SessionID,
			SessionID:   chunk.SessionID,
		}
		if !memoryDebugItemMatches(item, needle) {
			continue
		}
		items = append(items, item)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].CreatedAt != items[j].CreatedAt {
			return items[i].CreatedAt > items[j].CreatedAt
		}
		return items[i].ID > items[j].ID
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func clampMemoryDebugLimit(limit int) int {
	if limit <= 0 {
		return memoryDebugListMaxItems
	}
	if limit > memoryDebugListMaxItems {
		return memoryDebugListMaxItems
	}
	return limit
}

func memoryDebugItemMatches(item MemoryDebugItem, needle string) bool {
	if needle == "" {
		return true
	}
	haystacks := []string{
		item.ID,
		item.Type,
		item.Status,
		item.Title,
		item.Summary,
		item.Content,
		item.MemoryScope,
		item.Source,
		item.SessionID,
		item.AppName,
	}
	haystacks = append(haystacks, item.Tags...)
	haystacks = append(haystacks, item.Entities...)
	for _, value := range haystacks {
		if strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	return false
}
