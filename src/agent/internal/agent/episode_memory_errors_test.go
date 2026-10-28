package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

// episodeMemoryFailingModel fails the first failures proposal calls with err,
// then answers from the scripted model.
type episodeMemoryFailingModel struct {
	*episodeMemoryScriptedModel
	err      error
	failures int
	failed   int
}

func (m *episodeMemoryFailingModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	if m.failed < m.failures {
		m.failed++
		return nil, m.err
	}
	return m.episodeMemoryScriptedModel.GenerateContent(ctx, messages, options...)
}

func addEpisodeMemoryTestEpisodes(t *testing.T, plane *FilesystemMemoryPlane, prefix string, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for index := 0; index < count; index++ {
		endedAt := time.Date(2026, 8, 14, 12, 0, index+1, 0, time.UTC)
		episodeID := fmt.Sprintf("%s_%d", prefix, index)
		if _, err := plane.episodes.AddEpisode(context.Background(), TaskEpisode{
			ID: episodeID, Status: "active", StartedAt: endedAt.Add(-time.Second).Format(time.RFC3339Nano),
			EndedAt: endedAt.Format(time.RFC3339Nano), UserGoal: "打开设置",
			Events: []TaskEpisodeEvent{
				{EventID: episodeID + "_call", Type: runEventToolCall, ToolName: "launch_app"},
				{EventID: episodeID + "_result", Type: "tool_result", ToolName: "launch_app", Content: "Settings opened"},
			},
		}); err != nil {
			t.Fatalf("AddEpisode(%s) error = %v", episodeID, err)
		}
		ids = append(ids, episodeID)
	}
	return ids
}

// A malformed answer for a whole batch does not show which Episode caused it,
// so each Episode gets its own retry instead of being discarded with the
// batch.
func TestEpisodeMemoryMalformedBatchRetriesEpisodesIndividually(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`not-json`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	ids := addEpisodeMemoryTestEpisodes(t, plane, "ep_malformed", episodeMemoryBatchLimit)
	processor.now = func() time.Time { return time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC) }
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	for _, id := range ids {
		status := state.Episodes[episodeMemoryStateKey(id, episodeMemoryExtractorVersion)]
		if status.Status != episodeMemoryStatusRetry || status.RetryBatchLimit != 1 {
			t.Fatalf("status[%s] = %#v, want an individual retry", id, status)
		}
		if !strings.Contains(status.LastError, errEpisodeMemoryBatchSplit.Error()) || !strings.Contains(status.LastError, errEpisodeMemoryInvalidResponse.Error()) {
			t.Fatalf("status[%s].LastError = %q, want the split and invalid-JSON classes", id, status.LastError)
		}
	}
}

// A batch split is not any one Episode's failure, so it must not use up the
// attempts of Episodes that already failed for other reasons.
func TestEpisodeMemoryBatchSplitDoesNotUseAnAttempt(t *testing.T) {
	ctx := context.Background()
	plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
	model := &episodeMemoryScriptedModel{responses: []string{`not-json`}}
	processor := newEpisodeMemoryProcessor(plane, model)
	processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if err := processor.Initialize(); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	ids := addEpisodeMemoryTestEpisodes(t, plane, "ep_split", 2)
	now := time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
	processor.now = func() time.Time { return now }
	for _, id := range ids {
		if err := processor.state.SetEpisode(id, episodeMemoryEpisodeStatus{
			Status:           episodeMemoryStatusRetry,
			ExtractorVersion: episodeMemoryExtractorVersion,
			RetryAt:          now.Add(-time.Minute).Format(time.RFC3339Nano),
			AttemptCount:     episodeMemoryMaxAttempts - 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := processor.ProcessBatch(ctx, nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	state, err := processor.state.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	for _, id := range ids {
		status := state.Episodes[episodeMemoryStateKey(id, episodeMemoryExtractorVersion)]
		if status.Status != episodeMemoryStatusRetry || status.RetryBatchLimit != 1 || status.AttemptCount != episodeMemoryMaxAttempts-1 {
			t.Fatalf("status[%s] = %#v, want an individual retry with its attempts unchanged", id, status)
		}
	}
}

// A request that never got an answer says nothing about the Episode, so it is
// retried, and the ledger records the failure class without the provider text.
func TestEpisodeMemoryModelCallFailureIsRetriedWithItsClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		class error
	}{
		{name: "request", err: errors.New(`Post "https://proxy.example/v1": 502 Bad Gateway`), class: errEpisodeMemoryModelCallFailed},
		{name: "timeout", err: fmt.Errorf("send request: %w", context.DeadlineExceeded), class: errEpisodeMemoryModelTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			plane := NewFilesystemMemoryPlane(filepath.Join(t.TempDir(), "memory"), DefaultMemoryExtractionConfig(), nil)
			scripted := &episodeMemoryScriptedModel{responses: []string{
				`{"episode_assessment":{"goal_result":"unknown","reason":"no durable lesson","evidence_refs":[]},"candidates":[]}`,
			}}
			model := &episodeMemoryFailingModel{episodeMemoryScriptedModel: scripted, err: tc.err, failures: 1}
			processor := newEpisodeMemoryProcessor(plane, model)
			processor.state.bootstrapAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
			if err := processor.Initialize(); err != nil {
				t.Fatalf("Initialize() error = %v", err)
			}
			ids := addEpisodeMemoryTestEpisodes(t, plane, "ep_request", 1)
			firstPass := time.Date(2026, 8, 14, 13, 0, 0, 0, time.UTC)
			processor.now = func() time.Time { return firstPass }
			if _, err := processor.ProcessBatch(ctx, nil); err != nil {
				t.Fatalf("ProcessBatch() error = %v", err)
			}
			key := episodeMemoryStateKey(ids[0], episodeMemoryExtractorVersion)
			state, err := processor.state.Snapshot()
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			status := state.Episodes[key]
			if status.Status != episodeMemoryStatusRetry || status.LastError != tc.class.Error() {
				t.Fatalf("status = %#v, want a retry recording %q", status, tc.class)
			}

			processor.now = func() time.Time { return firstPass.Add(episodeMemoryRetryDelay + time.Minute) }
			if _, err := processor.ProcessBatch(ctx, nil); err != nil {
				t.Fatalf("ProcessBatch() second pass error = %v", err)
			}
			if state, err = processor.state.Snapshot(); err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if status := state.Episodes[key]; status.Status != episodeMemoryStatusDone {
				t.Fatalf("status after retry = %#v, want done", status)
			}
		})
	}
}

func TestSafeEpisodeMemoryErrorKeepsOnlyFailureClasses(t *testing.T) {
	providerText := "model said: the user's PIN is 7316"
	cases := []struct {
		err  error
		want string
	}{
		{err: errors.New(providerText), want: errEpisodeMemoryProcessingFailed.Error()},
		{err: fmt.Errorf("%w: %w", errEpisodeMemoryRetentionAudit, errMemoryMergeTruncated), want: errEpisodeMemoryRetentionAudit.Error() + ": " + errMemoryMergeTruncated.Error()},
		{err: fmt.Errorf("%w: parse: %w: %s", errEpisodeMemoryBatchSplit, errEpisodeMemoryInvalidResponse, providerText), want: errEpisodeMemoryBatchSplit.Error() + ": " + errEpisodeMemoryInvalidResponse.Error()},
	}
	for _, tc := range cases {
		got := safeEpisodeMemoryError(tc.err).Error()
		if got != tc.want {
			t.Errorf("safeEpisodeMemoryError(%q) = %q, want %q", tc.err, got, tc.want)
		}
		if strings.Contains(got, "7316") {
			t.Errorf("safeEpisodeMemoryError leaked provider text: %q", got)
		}
	}
}
