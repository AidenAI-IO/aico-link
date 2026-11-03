package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	modelpkg "aico-agent/internal/agent/model"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
)

type episodeMemoryScriptedModel struct {
	mu             sync.Mutex
	responses      []string
	auditResponses []string
	lastResponse   string
	calls          [][]llms.MessageContent
	// stopReason, when set, is reported on batch proposal responses so tests can
	// drive budget-exhaustion handling. Empty by default, so existing scripted
	// responses read as normally-finished.
	stopReason string
	// batchOptions records the call options of each proposal call, so tests can
	// assert the requested output budget.
	batchOptions [][]llms.CallOption
	// auditOptions records retention-gate options separately because audit calls
	// are intentionally excluded from calls and batchOptions.
	auditOptions [][]llms.CallOption
}

type episodeMemoryBlockingModel struct {
	inner   *episodeMemoryScriptedModel
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *episodeMemoryBlockingModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	blocked := false
	m.once.Do(func() {
		blocked = true
		close(m.started)
	})
	if blocked {
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.inner.GenerateContent(ctx, messages, options...)
}

func (m *episodeMemoryBlockingModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	return m.inner.Call(ctx, prompt, options...)
}

func (m *episodeMemoryBlockingModel) CallOptions() []chains.ChainCallOption {
	return m.inner.CallOptions()
}

func (m *episodeMemoryBlockingModel) Spec() modelpkg.ModelSpec {
	return m.inner.Spec()
}

func (m *episodeMemoryScriptedModel) GenerateContent(_ context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if episodeMemoryMessagesContain(messages, "mandatory retention gate") {
		m.auditOptions = append(m.auditOptions, options)
		if len(m.auditResponses) > 0 {
			response := m.auditResponses[0]
			m.auditResponses = m.auditResponses[1:]
			m.lastResponse = response
			return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: response, StopReason: m.stopReason}}}, nil
		}
		if response := defaultEpisodeMemoryRetentionAudit(messages); response != "" {
			m.lastResponse = response
			return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: response, StopReason: m.stopReason}}}, nil
		}
	}
	m.calls = append(m.calls, messages)
	m.batchOptions = append(m.batchOptions, options)
	if len(m.responses) == 0 {
		return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: `{}`}}}, nil
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	response = normalizeEpisodeMemoryScriptedResponse(response)
	if strings.TrimSpace(response) != "" && !strings.Contains(response, `"results"`) {
		if episodeID := episodeMemoryPromptEpisodeID(messages); episodeID != "" {
			response = `{"results":[{"episode_id":` + strconv.Quote(episodeID) + `,"proposal":` + response + `}]}`
		}
	}
	m.lastResponse = response
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: response, StopReason: m.stopReason}}}, nil
}

// setStopReason changes the reported stop reason between calls, so a test can
// truncate one response and let the next one finish normally.
func (m *episodeMemoryScriptedModel) setStopReason(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopReason = reason
}

// batchMaxTokens returns the output budget requested on batch proposal call
// index, or 0 if that call was never made.
func (m *episodeMemoryScriptedModel) batchMaxTokens(index int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return episodeMemoryRecordedMaxTokens(m.batchOptions, index)
}

func (m *episodeMemoryScriptedModel) auditMaxTokens(index int) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return episodeMemoryRecordedMaxTokens(m.auditOptions, index)
}

func episodeMemoryRecordedMaxTokens(options [][]llms.CallOption, index int) int {
	if index < 0 || index >= len(options) {
		return 0
	}
	resolved := llms.CallOptions{}
	for _, option := range options[index] {
		option(&resolved)
	}
	return resolved.MaxTokens
}

func normalizeEpisodeMemoryScriptedResponse(response string) string {
	var value any
	if err := json.Unmarshal([]byte(response), &value); err != nil {
		return response
	}
	var visit func(any)
	visit = func(node any) {
		switch item := node.(type) {
		case map[string]any:
			if _, ok := item["lesson_key"]; ok {
				if _, ok := item["retention"]; !ok {
					item["retention"] = string(episodeMemoryRetentionDurable)
				}
			}
			for _, child := range item {
				visit(child)
			}
		case []any:
			for _, child := range item {
				visit(child)
			}
		}
	}
	visit(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return response
	}
	return string(encoded)
}

func episodeMemoryPromptEpisodeID(messages []llms.MessageContent) string {
	for _, message := range messages {
		for _, part := range message.Parts {
			textPart, ok := part.(llms.TextContent)
			if !ok {
				continue
			}
			const marker = "===== Episode "
			if index := strings.Index(textPart.Text, marker); index >= 0 {
				value := textPart.Text[index+len(marker):]
				if end := strings.Index(value, " ====="); end >= 0 {
					return strings.TrimSpace(value[:end])
				}
			}
		}
	}
	return ""
}

func defaultEpisodeMemoryRetentionAudit(messages []llms.MessageContent) string {
	const marker = "Untrusted candidates:\n"
	for _, message := range messages {
		for _, part := range message.Parts {
			textPart, ok := part.(llms.TextContent)
			if !ok {
				continue
			}
			index := strings.LastIndex(textPart.Text, marker)
			if index < 0 {
				continue
			}
			var candidates []episodeMemoryCandidate
			if err := json.Unmarshal([]byte(strings.TrimSpace(textPart.Text[index+len(marker):])), &candidates); err != nil {
				return ""
			}
			reviews := episodeMemoryRetentionAudit{Reviews: make([]episodeMemoryRetentionReview, 0, len(candidates))}
			for _, candidate := range candidates {
				review := episodeMemoryRetentionReview{
					LessonKey:       candidate.LessonKey,
					Reason:          "the scripted test candidate is reusable and safe for its declared scope",
					SensitiveValues: []string{},
				}
				// Older fixtures predate the explicit retention field. Treat an
				// omitted class as durable in this scripted model so these tests
				// continue to exercise batch persistence rather than the retention
				// schema migration itself.
				if candidate.Retention == "" || candidate.Retention == episodeMemoryRetentionDurable {
					review.Decision = episodeMemoryRetentionDecisionRetain
					review.Retention = episodeMemoryRetentionDurable
					review.Rewrite = &episodeMemoryRetentionRewrite{
						Situation: candidate.Situation, Guidance: candidate.Guidance, ExpectedEffect: candidate.ExpectedEffect,
						Scope: cloneStringMap(candidate.Scope), Tags: append([]string(nil), candidate.Tags...), EvidenceRefs: append([]string(nil), candidate.EvidenceRefs...),
					}
				} else {
					review.Decision = episodeMemoryRetentionDecisionDiscard
					review.Reason = "the scripted test candidate is not durable"
				}
				reviews.Reviews = append(reviews.Reviews, review)
			}
			encoded, err := json.Marshal(reviews)
			if err != nil {
				return ""
			}
			return string(encoded)
		}
	}
	return ""
}

func episodeMemoryMessagesContain(messages []llms.MessageContent, needle string) bool {
	for _, message := range messages {
		for _, part := range message.Parts {
			if textPart, ok := part.(llms.TextContent); ok && strings.Contains(textPart.Text, needle) {
				return true
			}
		}
	}
	return false
}

func (m *episodeMemoryScriptedModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", nil
}

func (m *episodeMemoryScriptedModel) CallOptions() []chains.ChainCallOption { return nil }

func (m *episodeMemoryScriptedModel) Spec() modelpkg.ModelSpec { return modelpkg.ModelSpec{} }

func (m *episodeMemoryScriptedModel) appendResponses(responses ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, responses...)
}

func (m *episodeMemoryScriptedModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *episodeMemoryScriptedModel) firstCallHasImage() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return false
	}
	for _, message := range m.calls[0] {
		for _, part := range message.Parts {
			if _, ok := part.(llms.BinaryContent); ok {
				return true
			}
		}
	}
	return false
}

func (m *episodeMemoryScriptedModel) firstCallText() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return ""
	}
	var parts []string
	for _, message := range m.calls[0] {
		for _, part := range message.Parts {
			if textPart, ok := part.(llms.TextContent); ok {
				parts = append(parts, textPart.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func TestEpisodeMemoryEpisodeDueHonorsLeaseAndRetry(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name     string
		status   episodeMemoryEpisodeStatus
		eligible bool
		due      time.Time
	}{
		{
			name:     "processing lease active",
			status:   episodeMemoryEpisodeStatus{Status: episodeMemoryStatusProcessing, ProcessingStartedAt: now.Add(-episodeMemoryProcessingLease + time.Second).Format(time.RFC3339Nano)},
			eligible: false,
			due:      now.Add(time.Second),
		},
		{
			name:     "processing lease expired",
			status:   episodeMemoryEpisodeStatus{Status: episodeMemoryStatusProcessing, ProcessingStartedAt: now.Add(-episodeMemoryProcessingLease).Format(time.RFC3339Nano)},
			eligible: true,
			due:      now,
		},
		{
			name:     "retry waiting",
			status:   episodeMemoryEpisodeStatus{Status: episodeMemoryStatusRetry, RetryAt: now.Add(time.Second).Format(time.RFC3339Nano)},
			eligible: false,
			due:      now.Add(time.Second),
		},
		{
			name:     "retry due",
			status:   episodeMemoryEpisodeStatus{Status: episodeMemoryStatusRetry, RetryAt: now.Format(time.RFC3339Nano)},
			eligible: true,
			due:      now,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			eligible, due := episodeMemoryEpisodeDue(test.status, now)
			if eligible != test.eligible || !due.Equal(test.due) {
				t.Fatalf("episodeMemoryEpisodeDue() = (%v, %v), want (%v, %v)", eligible, due, test.eligible, test.due)
			}
		})
	}
}

func TestEpisodeMemoryStateKeepsOnlyRecentTerminalEpisodes(t *testing.T) {
	base := time.Now().UTC()
	store := newEpisodeMemoryStateStore(filepath.Join(t.TempDir(), "reflection.yaml"), base.Add(-time.Minute))
	for index := 0; index < episodeMemoryRecentTerminals+6; index++ {
		id := fmt.Sprintf("ep_terminal_%03d", index)
		if err := store.CompleteEpisode(id, base.Add(time.Duration(index)*time.Second), episodeMemoryEpisodeStatus{Status: episodeMemoryStatusDone}); err != nil {
			t.Fatalf("CompleteEpisode(%s) error = %v", id, err)
		}
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(state.Episodes) != episodeMemoryRecentTerminals {
		t.Fatalf("terminal state count = %d, want %d", len(state.Episodes), episodeMemoryRecentTerminals)
	}
	if state.CompletedThroughID != fmt.Sprintf("ep_terminal_%03d", episodeMemoryRecentTerminals+5) {
		t.Fatalf("CompletedThroughID = %q, want last Episode", state.CompletedThroughID)
	}
	if _, found := state.Episodes[episodeMemoryStateKey("ep_terminal_000", episodeMemoryExtractorVersion)]; found {
		t.Fatal("old terminal Episode was not pruned")
	}
}

func TestEpisodeMemoryStatePreservesExplicitZeroConfidence(t *testing.T) {
	store := newEpisodeMemoryStateStore(filepath.Join(t.TempDir(), "episode-memory.yaml"), time.Now().UTC())
	proposal := episodeMemoryProposal{Candidates: []episodeMemoryCandidate{{
		LessonKey:  "explicit_zero",
		Confidence: episodeMemoryConfidencePointer(0),
	}}}
	if err := store.SetEpisode("ep_zero", episodeMemoryEpisodeStatus{
		Status:           episodeMemoryStatusProposed,
		ExtractorVersion: episodeMemoryExtractorVersion,
		Proposal:         &proposal,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	status := state.Episodes[episodeMemoryStateKey("ep_zero", episodeMemoryExtractorVersion)]
	if status.Proposal == nil || len(status.Proposal.Candidates) != 1 {
		t.Fatalf("reloaded proposal=%#v, want one candidate", status.Proposal)
	}
	confidence := status.Proposal.Candidates[0].Confidence
	if confidence == nil || *confidence != 0 {
		t.Fatalf("reloaded confidence=%v, want explicit zero preserved", confidence)
	}
	if _, err := normalizeEpisodeMemoryConfidence(confidence); err == nil {
		t.Fatal("reloaded explicit zero confidence was accepted as omitted")
	}
}

func TestNormalRecallHidesLegacyFailureMemories(t *testing.T) {
	ctx := context.Background()
	store := NewDeviceMemoryStore(t.TempDir())
	for _, item := range []DeviceMemoryItem{
		{ID: "legacy_failure", Type: "failure", Status: "active", Title: "legacy", Content: "legacy failure"},
		{ID: "reflected_failure", Type: "failure", Status: "active", Title: "reflected", Content: "reflected failure", Tags: []string{legacyReflectionFailureTag}},
	} {
		if _, err := store.Upsert(ctx, item); err != nil {
			t.Fatalf("Upsert(%s) error = %v", item.ID, err)
		}
	}
	hits, err := store.Search(ctx, DeviceMemoryQuery{Types: []string{"failure"}, Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "reflected_failure" {
		t.Fatalf("failure recall hits = %#v, want only legacy-consolidation-managed memory", hits)
	}
}

func TestNormalRecallHidesConflictedLegacyFailureMemories(t *testing.T) {
	ctx := context.Background()
	store := NewDeviceMemoryStore(t.TempDir())
	if _, err := store.Upsert(ctx, DeviceMemoryItem{
		ID:      "conflicted_reflection_failure",
		Type:    "failure",
		Status:  "conflicted",
		Title:   "conflicted reflected failure",
		Content: "conflicted reflected failure",
		Tags:    []string{legacyReflectionFailureTag},
	}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}
	hits, err := store.Search(ctx, DeviceMemoryQuery{Types: []string{"conflict"}, Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("conflicted legacy failure recall hits = %#v, want none", hits)
	}
}

func TestLongTermRecallHidesLegacyFailureMemories(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(t.TempDir())
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID:               "legacy_long_term_failure",
		Type:             "failure",
		Status:           "active",
		Priority:         80,
		Confidence:       0.8,
		Title:            "legacy failure",
		Content:          "legacy failure content",
		EvidenceExcerpts: []string{"legacy evidence"},
	}); err != nil {
		t.Fatalf("AddMemory() error = %v", err)
	}
	hits, err := store.Search(ctx, MemoryQuery{Types: []string{"failure"}, Limit: 5})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("legacy long-term failure recall hits = %#v, want none", hits)
	}
}

func TestRuntimeCallbackRecordsSteerInEpisode(t *testing.T) {
	recorder := NewEpisodeRecorder(MemoryRetrieveRequest{Input: "original goal", EpisodeID: "ep_steer"}, MemoryContext{})
	handler := &runtimeCallbackHandler{episode: recorder, episodeID: recorder.ID()}
	handler.HandleSteerMessage(context.Background(), RunSteerMessage{Content: "use the second result instead"})
	episode := recorder.Finish("", nil, context.Canceled, nil, nil)
	if len(episode.Events) != 1 || episode.Events[0].Type != "steer" || episode.Events[0].Content != "use the second result instead" {
		t.Fatalf("steer episode events = %#v", episode.Events)
	}
}

func TestFailureEpisodeDoesNotWriteImmediateFailureMemory(t *testing.T) {
	ctx := context.Background()
	memoryDir := filepath.Join(t.TempDir(), "memory")
	plane := NewFilesystemMemoryPlane(memoryDir, DefaultMemoryExtractionConfig(), nil)
	episode := episodeMemoryTestFailureEpisode("ep_no_direct_memory", "open settings")
	if err := plane.CommitEpisode(ctx, episode); err != nil {
		t.Fatalf("CommitEpisode() error = %v", err)
	}
	deviceItems, err := plane.device.readAll()
	if err != nil {
		t.Fatalf("device readAll() error = %v", err)
	}
	for _, item := range deviceItems {
		if item.Type == "failure" {
			t.Fatalf("failure episode wrote immediate device memory: %#v", item)
		}
	}
	results, err := plane.longTerm.Search(ctx, MemoryQuery{Types: []string{"failure"}, Limit: 10})
	if err != nil {
		t.Fatalf("long-term Search() error = %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("failure episode wrote immediate long-term memory: %#v", results)
	}
}

func episodeMemoryTestFailureEpisode(id, goal string) TaskEpisode {
	now := time.Now().UTC()
	return TaskEpisode{
		ID:        id,
		Status:    "active",
		StartedAt: now.Add(-time.Second).Format(time.RFC3339Nano),
		EndedAt:   now.Format(time.RFC3339Nano),
		UserGoal:  goal,
		Outcome: TaskEpisodeOutcome{
			Success:       false,
			FailureReason: "clicked the wrong result",
		},
		Events: []TaskEpisodeEvent{
			{
				EventID:   id + "_call",
				Type:      runEventToolCall,
				ToolName:  "mouse_click",
				ToolInput: `{"x":100,"y":200}`,
			},
			{
				EventID:     id + "_result",
				Type:        "tool_result",
				ToolName:    "mouse_click",
				Observation: "the wrong item opened",
				IsError:     true,
			},
		},
	}
}
