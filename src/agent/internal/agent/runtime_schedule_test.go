package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"aiden-agent/internal/agent/contextmanager"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestScheduleRuntimeResumesPersistedSessionAndCompaction(t *testing.T) {
	originalNow := promptNow
	now := time.Date(2026, time.October, 22, 12, 0, 0, 0, time.UTC)
	promptNow = func() time.Time { return now }
	t.Cleanup(func() { promptNow = originalNow })
	config := Config{ConfigDir: t.TempDir(), Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}
	makeRuntime := func(model *interruptTestModel) *Runtime {
		r := NewRuntimeWithDeps(config, &testModelResolver{model: model}, NewMemoryManager(""), &ToolSet{tools: map[string]langtools.Tool{}}, NewSkillIndex())
		return r
	}
	r := makeRuntime(&interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
		return contentResponse("I will check the blue shipment."), nil
	}})
	if _, err := r.Run(context.Background(), RunRequest{Input: "The shipment is blue; check it later."}); err != nil {
		t.Fatal(err)
	}
	sessionID := r.contextManager.GetSessionID()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	// A persisted schedule can be the first run after restarting on a new day.
	now = now.Add(24 * time.Hour)

	calls := 0
	r = makeRuntime(&interruptTestModel{generate: func(_ context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
		calls++
		if !runtimeModelCallContains(input, "shipment is blue") {
			t.Error("scheduled execution lost original conversation")
		}
		if !runtimeModelCallContains(input, "Current date: 2026-10-23") {
			t.Error("resumed conversation did not receive the current date")
		}
		return contentResponse("Checked the blue shipment."), nil
	}})
	defer r.Close()
	req := RunRequest{Input: "Continue the scheduled shipment check.", WaitForIdle: true, ResumeSessionID: sessionID}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := r.contextManager.GetSessionID(); got != sessionID {
		t.Fatalf("restarted run changed session: %s", got)
	}
	child, err := contextmanager.NewContextManagerRevisionFromMessageList(r.contextManager, r.contextManager.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.contextManager.Activate(child); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := r.contextManager.GetSessionID(); got != child.GetSessionID() {
		t.Fatal("scheduled run reverted compaction")
	}

	fresh, err := contextmanager.NewContextManagerCandidate(r.contextManager.GetSessionFolder(), "new conversation")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.contextManager.Activate(fresh); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "session was cleared or replaced") {
		t.Fatalf("cleared history error=%v", err)
	}
	if calls != 2 {
		t.Fatalf("wrong-session task reached model: %d calls", calls)
	}
}

func TestScheduleRuntimeWaitsWithoutPreemptingActiveRun(t *testing.T) {
	r := &Runtime{}
	unlock, err := r.lockRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	active, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	r.activeCancel = stop
	hookCalled := false
	r.RegisterPreemptHook(func() { hookCalled = true })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = r.Run(ctx, RunRequest{Input: "scheduled", WaitForIdle: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if active.Err() != nil || hookCalled {
		t.Fatal("waiting schedule interrupted active work or playback")
	}
}
