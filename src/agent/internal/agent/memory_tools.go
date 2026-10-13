package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type RecallMemoryTool struct {
	store     *LongTermMemoryStore
	temporary *LongTermMemoryStore
}

type SaveMemoryTool struct {
	store *LongTermMemoryStore
}

type ForgetMemoryTool struct {
	store     *LongTermMemoryStore
	temporary *LongTermMemoryStore
}

type RecallDeviceMemoryTool struct {
	store *DeviceMemoryStore
}

type InspectEpisodeTool struct {
	store *TaskEpisodeStore
}

// RecallSessionChunksTool searches conversation chunks written at compaction
// time across every ContextManager session.
type RecallSessionChunksTool struct {
	store *MultiSessionChunkStore
}

func NewRecallSessionChunksTool(store *MultiSessionChunkStore) *RecallSessionChunksTool {
	return &RecallSessionChunksTool{store: store}
}

func (t *RecallSessionChunksTool) Name() string { return "recall_session_chunks" }

func (t *RecallSessionChunksTool) Description() string {
	return strings.Join([]string{
		"Recall compressed session history chunks from this conversation and prior sessions.",
		"Call this tool whenever the user references or asks about prior conversation content that is not present in your visible context — including denials such as 'we never discussed X'. The visible context is only the recent hot window; older turns are compressed into archived chunks invisible until recalled.",
		"The tool searches all sessions in a single call. If no results match, retry once with broader topic terms or no filters before concluding the history is unavailable.",
		"Prefer chunk_ids when known; otherwise pass tags (topic keywords from the user's question) and use empty tags [] for recent history.",
		"For remembered preferences, rules, procedures, or facts, use recall_memory instead.",
	}, " ")
}

func (t *RecallSessionChunksTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"chunk_ids": stringArrayArgSchema("Specific session chunk ids to retrieve."),
		"tags":      stringArrayArgSchema("Topic keywords matched against chunk metadata and summaries when chunk_ids are not known."),
		"entities":  stringArrayArgSchema("Named entities to search for."),
		"limit":     minIntegerArgSchema("Maximum number of chunks to return.", 1),
	})
}

func (t *RecallSessionChunksTool) Call(ctx context.Context, input string) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("session chunk store is not configured")
	}
	query, err := decodeChunkRecallQuery(input)
	if err != nil {
		return "", fmt.Errorf("decode recall_session_chunks input: %w", err)
	}
	results, err := t.store.RecallChunks(ctx, query)
	if err != nil {
		return "", err
	}

	return encodeToolJSON(map[string]any{"results": results})
}

func NewRecallMemoryTool(store *LongTermMemoryStore) *RecallMemoryTool {
	return &RecallMemoryTool{store: store}
}

func NewRecallMemoryToolWithTemporary(store, temporary *LongTermMemoryStore) *RecallMemoryTool {
	return &RecallMemoryTool{store: store, temporary: temporary}
}

func (t *RecallMemoryTool) Name() string { return "recall_memory" }

func (t *RecallMemoryTool) Description() string {
	return strings.Join([]string{
		"Recall temporary and long-term memories by tags, entities, or types. Leave arrays empty to match all.",
		"Temporary memories are short-lived notification-derived conclusions and are returned with memory_scope=temporary; long-term memories are returned with memory_scope=long_term.",
		"Use for remembered preferences, rules, procedures, facts, profile info, or screen content the user saved earlier with the device button.",
		"Screen content the user saved belongs here even when they phrase it as something recent (\"the tracking number I just saved\"); use types [\"screen_snapshot\"] for those. Only use recall_session_chunks for what was actually said in conversation.",
		"Notification-derived temporary memories are included automatically. Use this tool for remembered notification conclusions; when the exact original notification record is required, use shell to read `/userdata/agent/memory/notifications/events/*.jsonl` without modifying it.",
		"Returns matching memories with id, type, title, content, summary.",
	}, " ")
}

func (t *RecallMemoryTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"tags":     stringArrayArgSchema("Topic or domain keywords such as verification, payment, or expense."),
		"entities": stringArrayArgSchema("Specific named things such as apps, accounts, services, or people."),
		"types":    stringArrayArgSchema("Memory categories: preference (likes/dislikes), rule (must/must-not), procedure (how-to), fact (stable info), profile (user background), screen_snapshot (screen content the user saved with the device button). Leave empty and pass no tags to get the most recently saved screen_snapshot entries first."),
		"limit":    minIntegerArgSchema("Maximum number of memories to return.", 1),
	})
}

func (t *RecallMemoryTool) Call(ctx context.Context, input string) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("long-term memory store is not configured")
	}
	query, err := decodeMemoryQuery(input)
	if err != nil {
		return "", fmt.Errorf("decode recall_memory input: %w", err)
	}
	results := make([]MemoryResult, 0)
	if t.temporary != nil {
		temporary, err := t.temporary.Search(ctx, query)
		if err != nil {
			return "", err
		}
		for i := range temporary {
			temporary[i].MemoryScope = "temporary"
		}
		results = append(results, temporary...)
	}
	longTerm, err := t.store.Search(ctx, query)
	if err != nil {
		return "", err
	}
	for i := range longTerm {
		longTerm[i].MemoryScope = "long_term"
	}
	results = append(results, longTerm...)
	sort.SliceStable(results, func(i, j int) bool {
		si := recallMemoryResultScore(query, results[i])
		sj := recallMemoryResultScore(query, results[j])
		if si != sj {
			return si > sj
		}
		if results[i].MemoryScope != results[j].MemoryScope {
			return results[i].MemoryScope == "temporary"
		}
		if results[i].Priority != results[j].Priority {
			return results[i].Priority > results[j].Priority
		}
		return results[i].ID < results[j].ID
	})
	if query.Limit > 0 && len(results) > query.Limit {
		results = results[:query.Limit]
	}
	recordRecalledMemoryIDs(ctx, results, func(r MemoryResult) string { return r.ID })
	return encodeToolJSON(map[string]any{"results": results})
}

func recallMemoryResultScore(query MemoryQuery, result MemoryResult) int {
	if len(query.Tags) == 0 && len(query.Entities) == 0 && len(query.Types) == 0 {
		if result.MemoryScope == "temporary" {
			return 1
		}
		return 0
	}
	score := scoreMemoryEntry(query, memoryIndexEntry{
		Type: result.Type, Tags: result.Tags, Entities: result.Entities, Summary: result.Summary,
	}, parsedMemoryMarkdown{Title: result.Title, Content: result.Content})
	if score > 0 && result.MemoryScope == "temporary" {
		score++
	}
	return score
}

// recordRecalledMemoryIDs reports the IDs of memories surfaced by a recall tool
// to the active episode recorder. Episode Memory consolidation prioritizes
// those records when checking for updates and conflicts.
func recordRecalledMemoryIDs[T any](ctx context.Context, results []T, id func(T) string) {
	recorder := EpisodeRecorderFromContext(ctx)
	if recorder == nil || len(results) == 0 {
		return
	}
	ids := make([]string, 0, len(results))
	for _, r := range results {
		if v := strings.TrimSpace(id(r)); v != "" {
			ids = append(ids, v)
		}
	}
	recorder.RecordMemoryRecall(ids)
}

func encodeToolJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func NewSaveMemoryTool(store *LongTermMemoryStore) *SaveMemoryTool {
	return &SaveMemoryTool{store: store}
}

func (t *SaveMemoryTool) Name() string { return "save_memory" }

func (t *SaveMemoryTool) Description() string {
	return strings.Join([]string{
		"Save long-term memory for future recall. Mandatory when the user asks to remember/save; also use for observed stable preferences, rules, or procedures.",
		"Do not tell the user something was remembered or saved until this tool returns status=saved or status=ignored as a duplicate.",
		"Use profile for durable user facts (name, nickname, location, timezone, home city, role, background); preference/rule for future defaults and must/must-not behavior such as a default city; fact for stable info that should be recalled but not surfaced in the synthesized user profile.",
		"Returns status=saved with id, or status=ignored when it is a duplicate.",
	}, " ")
}

func (t *SaveMemoryTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"type":     stringEnumArgSchema("Memory category.", "preference", "rule", "procedure", "fact", "profile"),
		"title":    stringArgSchema("Short title for the memory."),
		"content":  stringArgSchema("The stable information to remember."),
		"tags":     stringArrayArgSchema("Topic or domain keywords for future search, e.g. verification, payment, expense. Not time words or vague terms."),
		"entities": stringArrayArgSchema("Specific named things mentioned by the memory, such as apps, accounts, services, or people."),
		"evidence": stringArrayArgSchema("Original user quotes or observations that led to this memory, to help verify relevance later."),
		"priority": rangedIntegerArgSchema("Importance from 1 to 100: 80+ for user-stated rules/preferences, 60+ for inferred patterns, 40+ for observations.", 1, 100),
	}, "type", "content")
}

func (t *SaveMemoryTool) Call(ctx context.Context, input string) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("long-term memory store is not configured")
	}
	req, err := decodeSaveMemoryRequest(input)
	if err != nil {
		return "", fmt.Errorf("decode save_memory input: %w", err)
	}
	if strings.TrimSpace(req.Content) == "" {
		return "", fmt.Errorf("content is required")
	}
	if len(req.Evidence) == 0 {
		req.Evidence = []string{req.Content}
	}
	if req.Priority <= 0 {
		req.Priority = 80
	}
	item := MemoryItem{
		Type:             req.Type,
		Priority:         req.Priority,
		Confidence:       0.9,
		Title:            req.Title,
		Content:          req.Content,
		Tags:             req.Tags,
		Entities:         req.Entities,
		EvidenceExcerpts: req.Evidence,
	}
	result, err := t.store.ApplyMemoryCandidate(ctx, item)
	if err != nil {
		return "", err
	}
	if result.Operation == MemoryOperationIgnore {
		return encodeToolJSON(map[string]string{"status": "ignored", "reason": "duplicate of " + result.ID})
	}
	t.store.RequestProfileRebuild()
	return encodeToolJSON(map[string]string{"status": "saved", "id": result.ID, "operation": string(result.Operation)})
}

func NewForgetMemoryTool(store *LongTermMemoryStore) *ForgetMemoryTool {
	return &ForgetMemoryTool{store: store}
}

func NewForgetMemoryToolWithTemporary(store, temporary *LongTermMemoryStore) *ForgetMemoryTool {
	return &ForgetMemoryTool{store: store, temporary: temporary}
}

func (t *ForgetMemoryTool) Name() string { return "forget_memory" }

func (t *ForgetMemoryTool) Description() string {
	return strings.Join([]string{
		"Forget (delete) a long-term memory.",
		"Use when the user asks to remove a previously saved memory.",
		"First use recall_memory to find the memory ID, then call this tool.",
		"Returns confirmation of deletion.",
	}, " ")
}

func (t *ForgetMemoryTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"id":     stringArgSchema("Memory id to delete."),
		"reason": stringArgSchema("Reason for deleting the memory."),
	}, "id")
}

func (t *ForgetMemoryTool) Call(ctx context.Context, input string) (string, error) {
	if t.store == nil && t.temporary == nil {
		return "", fmt.Errorf("long-term memory store is not configured")
	}
	req, err := decodeForgetMemoryRequest(input)
	if err != nil {
		return "", fmt.Errorf("decode forget_memory input: %w", err)
	}
	if strings.TrimSpace(req.ID) == "" {
		return "", fmt.Errorf("memory id is required")
	}
	if req.Reason == "" {
		req.Reason = "user requested"
	}
	store := t.store
	if strings.HasPrefix(req.ID, "tmp_") && t.temporary != nil {
		store = t.temporary
	}
	if store == nil {
		return "", fmt.Errorf("memory store is not configured for %q", req.ID)
	}
	if err := store.Forget(ctx, req.ID, req.Reason); err != nil {
		return "", err
	}
	return encodeToolJSON(map[string]string{"status": "deleted", "id": req.ID})
}

func NewRecallDeviceMemoryTool(store *DeviceMemoryStore) *RecallDeviceMemoryTool {
	return &RecallDeviceMemoryTool{store: store}
}

func (t *RecallDeviceMemoryTool) Name() string { return "recall_device_memory" }

func (t *RecallDeviceMemoryTool) Description() string {
	return strings.Join([]string{
		"Recall device and UI memory by terms, tags, entities, types, or device id.",
		"Required when a task materially depends on saved device or app profiles, procedures, navigation, failure-prevention lessons, calibration notes, or facts, including natural-language references to previously learned behavior, prior device experience, or an earlier workaround; recall before answering even when the answer appears obvious, but do not call merely because a device or app is mentioned.",
		`Input JSON: {"terms":["微信"],"tags":["登录"],"entities":["微信App"],"types":["procedure","failure"],"device_id":"default","limit":5}`,
	}, " ")
}

func (t *RecallDeviceMemoryTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"terms":     stringArrayArgSchema("Free-text search terms."),
		"tags":      stringArrayArgSchema("Topic tags to match."),
		"entities":  stringArrayArgSchema("Named apps, devices, accounts, or UI concepts to match."),
		"types":     stringArrayArgSchema("Device memory categories such as procedure, navigation, failure, calibration, or fact."),
		"device_id": stringArgSchema("Device id to filter by; omit for default search."),
		"limit":     minIntegerArgSchema("Maximum number of device memories to return.", 1),
	})
}

func (t *RecallDeviceMemoryTool) Call(ctx context.Context, input string) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("device memory store is not configured")
	}
	query, err := decodeDeviceMemoryQuery(input)
	if err != nil {
		return "", fmt.Errorf("decode recall_device_memory input: %w", err)
	}
	if query.Limit <= 0 || query.Limit > 5 {
		query.Limit = 5
	}
	results, err := t.store.Search(ctx, query)
	if err != nil {
		return "", err
	}
	if len(results) == 0 && len(query.Tags) > 0 && len(query.Terms) > 0 {
		// LLMs often emit a useful free-text query together with an overly
		// specific tag filter. Retry with tags as ranking terms while preserving
		// hard entity, type, and device constraints. Entities can identify an app,
		// account, or device boundary, so relaxing them risks cross-scope recall.
		fallback := query
		fallback.Terms = append(append([]string(nil), query.Terms...), query.Tags...)
		fallback.Tags = nil
		results, err = t.store.Search(ctx, fallback)
		if err != nil {
			return "", err
		}
	}
	results = limitDeviceMemoryRecall(results, 4800)
	recordRecalledMemoryIDs(ctx, results, func(h MemoryHit) string { return h.ID })
	return encodeToolJSON(map[string]any{"results": results})
}

func limitDeviceMemoryRecall(results []MemoryHit, charBudget int) []MemoryHit {
	if charBudget <= 0 || len(results) == 0 {
		return nil
	}
	limited := make([]MemoryHit, 0, len(results))
	used := len(`{"results":[]}`)
nextHit:
	for _, hit := range results {
		candidate := hit
		candidate.EvidenceRefs = nil
		candidate.FilePath = ""
		for {
			encoded, err := json.Marshal(candidate)
			if err != nil {
				break
			}
			cost := len(encoded) + 1
			if used+cost <= charBudget {
				limited = append(limited, candidate)
				used += cost
				break
			}
			switch {
			case len(candidate.Content) > 320:
				candidate.Content = truncateForLog(candidate.Content, len(candidate.Content)/2)
			case len(candidate.Summary) > 160:
				candidate.Summary = truncateForLog(candidate.Summary, len(candidate.Summary)/2)
			case len(candidate.Steps) > 1:
				candidate.Steps = candidate.Steps[:len(candidate.Steps)-1]
			default:
				continue nextHit
			}
		}
	}
	return limited
}

func NewInspectEpisodeTool(store *TaskEpisodeStore) *InspectEpisodeTool {
	return &InspectEpisodeTool{store: store}
}

func (t *InspectEpisodeTool) Name() string { return "inspect_episode" }

func (t *InspectEpisodeTool) Description() string {
	return strings.Join([]string{
		"Debug inspect a stored task episode by ID, including metadata and compact event trace.",
		"The runtime writes task episodes automatically after runs; use this only for memory debugging or explicit user requests.",
		`Input JSON: {"id":"ep_..."}`,
	}, " ")
}

func (t *InspectEpisodeTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"id": stringArgSchema("Stored task episode id to inspect."),
	}, "id")
}

func (t *InspectEpisodeTool) Call(ctx context.Context, input string) (string, error) {
	if t.store == nil {
		return "", fmt.Errorf("episode store is not configured")
	}
	req, err := decodeInspectEpisodeRequest(input)
	if err != nil {
		return "", fmt.Errorf("decode inspect_episode input: %w", err)
	}
	if strings.TrimSpace(req.ID) == "" {
		return "", fmt.Errorf("episode id is required")
	}
	episode, err := t.store.Get(ctx, req.ID)
	if err != nil {
		return "", err
	}
	return encodeToolJSON(map[string]any{"episode": episode})
}
