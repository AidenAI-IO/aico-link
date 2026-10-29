package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

type interruptTestModel struct {
	scriptedModel
	generate func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error)
}

func (m *interruptTestModel) GenerateContent(ctx context.Context, input []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	return m.generate(ctx, input)
}

func newInterruptTestLoop(t *testing.T, model *interruptTestModel, tools ...langtools.Tool) *AgentLoop {
	t.Helper()
	manager, err := freshNewContextManager("system", "task", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewAgentLoop(model, RoleProfile{Tools: tools}, 10, nil, nil, manager)
}

func requireInterruptNotices(t *testing.T, manager *contextmanager.ContextManager, reasons ...string) {
	t.Helper()
	// Reload from disk: an in-memory notice alone does not satisfy recovery.
	reloaded, err := contextmanager.LoadContextManagerFromSessionID(manager.GetSessionFolder(), manager.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	var notices []string
	for _, message := range reloaded.CloneMessageList() {
		if message.Role == messages.MessageRoleNotice && strings.HasPrefix(message.Content, "Interrupt [") {
			notices = append(notices, message.Content)
		}
	}
	if len(notices) != len(reasons) {
		t.Fatalf("interrupt notices = %q, want reasons %q", notices, reasons)
	}
	for i, reason := range reasons {
		if !strings.HasPrefix(notices[i], "Interrupt ["+reason+"]:") {
			t.Fatalf("notice = %q, want %q", notices[i], reason)
		}
	}
	if err := validateStrictToolMessageSequence(messages.ConvertMessageList(reloaded.CloneMessageList())); err != nil {
		t.Fatalf("notice broke tool-call/result ordering: %v", err)
	}
}

func TestInterruptNoticeTerminalFailures(t *testing.T) {
	for _, scenario := range []string{"canceled", "deadline_exceeded", "model_error", "execution_error", "preempted"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			failure := errors.New("provider request failed")
			model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
				switch scenario {
				case "canceled":
					cancel(context.Canceled)
					return nil, ctx.Err()
				case "preempted":
					cancel(errRunPreempted)
					return nil, ctx.Err()
				case "deadline_exceeded":
					return nil, context.DeadlineExceeded
				default:
					return nil, failure
				}
			}}
			loop := newInterruptTestLoop(t, model)
			if scenario == "execution_error" {
				loop.ContextBudgetGuard = func(context.Context, *contextmanager.ContextManager, llms.CallOptions) (*contextmanager.ContextManager, bool, error) {
					return nil, false, errors.New("context cannot fit after pruning")
				}
			}
			if _, err := loop.Run(ctx, "task"); err == nil {
				t.Fatal("expected a failed run")
			}
			requireInterruptNotices(t, loop.contextManager, scenario)
		})
	}
}

func TestInterruptNoticeCancellationWinsOverSteerAndLateSuccess(t *testing.T) {
	for _, withTool := range []bool{false, true} {
		t.Run(map[bool]string{false: "model", true: "tool"}[withTool], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
				if withTool {
					return toolCallResponse("call_1", "late_tool", `{}`), nil
				}
				cancel()
				return contentResponse("late success must not win"), nil
			}}
			tool := &interruptTestTool{name: "late_tool", call: func(context.Context) (string, error) {
				cancel()
				return "side effect already happened", nil
			}}
			loop := newInterruptTestLoop(t, model, tool)
			loop.TerminationPolicy = NewTerminationPolicy(TerminationPolicyConfig{Enabled: terminationPolicyBoolPtr(false)})
			loop.SteerProvider = func(ctx context.Context) (RunSteerMessage, bool) {
				if ctx.Err() != nil {
					t.Fatal("consumed steer after cancellation")
				}
				return RunSteerMessage{}, false
			}
			if _, err := loop.Run(ctx, "task"); err != context.Canceled {
				t.Fatalf("error = %v, want original context.Canceled", err)
			}
			requireInterruptNotices(t, loop.contextManager, "canceled")
		})
	}
}

type interruptTestTool struct {
	name string
	call func(context.Context) (string, error)
}

func (t *interruptTestTool) Name() string                                       { return t.name }
func (t *interruptTestTool) Description() string                                { return "test tool" }
func (t *interruptTestTool) Call(ctx context.Context, _ string) (string, error) { return t.call(ctx) }

func TestInterruptNoticeSteerDuringModelAndTool(t *testing.T) {
	for _, withTool := range []bool{false, true} {
		for _, withdrawn := range []bool{false, true} {
			t.Run(map[bool]string{false: "model", true: "tool"}[withTool]+map[bool]string{false: "_updated", true: "_withdrawn"}[withdrawn], func(t *testing.T) {
				interrupt := make(chan struct{})
				rearmed := make(chan struct{})
				interrupted := false
				consumed := false
				calls := 0
				model := &interruptTestModel{generate: func(ctx context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
					calls++
					if calls > 1 {
						if !runtimeModelCallContains(input, "Interrupt [") {
							t.Fatal("resumed model call did not receive interruption notice")
						}
						return contentResponse("continued"), nil
					}
					if withTool {
						return toolCallResponse("call_1", "slow", `{}`), nil
					}
					interrupted = true
					close(interrupt)
					<-ctx.Done()
					return nil, ctx.Err()
				}}
				tool := &interruptTestTool{name: "slow", call: func(ctx context.Context) (string, error) {
					interrupted = true
					close(interrupt)
					<-ctx.Done()
					return "", ctx.Err()
				}}
				loop := newInterruptTestLoop(t, model, tool)
				loop.SteerInterrupt = func() <-chan struct{} {
					if interrupted {
						return rearmed
					}
					return interrupt
				}
				loop.SteerProvider = func(context.Context) (RunSteerMessage, bool) {
					if !interrupted || withdrawn || consumed {
						return RunSteerMessage{}, false
					}
					consumed = true
					return RunSteerMessage{ID: "steer_1", Content: "updated goal"}, true
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if result, err := loop.Run(ctx, "task"); err != nil || result != "continued" {
					t.Fatalf("run = %q, %v", result, err)
				}
				reason := "steer"
				if withdrawn {
					reason = "steer_resumed"
				}
				requireInterruptNotices(t, loop.contextManager, reason)
			})
		}
	}
}

func TestInterruptNoticeNormalToolErrorAndIntentionalPause(t *testing.T) {
	for _, name := range []string{"normal", "tool_error", "request_user_action", "wait_for_wakeup"} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
				calls++
				if calls == 1 && name != "normal" {
					return toolCallResponse("call_1", name, `{}`), nil
				}
				return contentResponse("done"), nil
			}}
			tool := &interruptTestTool{name: name, call: func(context.Context) (string, error) {
				if name == "tool_error" {
					return "", errors.New("recoverable tool error")
				}
				return "waiting for user", nil
			}}
			loop := newInterruptTestLoop(t, model, tool)
			if _, err := loop.Run(context.Background(), "task"); err != nil {
				t.Fatal(err)
			}
			requireInterruptNotices(t, loop.contextManager)
			if isRunPausingTool(name) {
				notices := collectMessagesByRole(loop.contextManager.CloneMessageList(), messages.MessageRoleNotice)
				if len(notices) != 1 || !strings.HasPrefix(notices[0].Content, "Pause [") {
					t.Fatalf("pause notices = %#v", notices)
				}
				if calls != 1 {
					t.Fatalf("model continued after pause: %d calls", calls)
				}
			}
		})
	}
}

func TestInterruptJournalRecoveryFollowsCompactionAndIsIdempotent(t *testing.T) {
	manager, err := freshNewContextManager("system", "task", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := startRunNotices(func() *contextmanager.ContextManager { return manager }, "run_crash")
	if err != nil {
		t.Fatal(err)
	}
	// Crash after compaction has activated a new revision, with an unmatched
	// tool call in the persisted tail. Recovery must repair it before Notice.
	manager, err = contextmanager.NewContextManagerRevisionFromMessageList(manager, append(manager.CloneMessageList(), messages.Message{
		Role: messages.MessageRoleToolCall, ToolCalls: []messages.ToolCall{{ID: "pending", Name: "tap", Arguments: `{}`}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := contextmanager.SwitchSession(manager.GetSessionFolder(), manager.GetSessionID()); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverPendingBackendRunManager(manager.GetSessionFolder(), nil); err != nil {
		t.Fatal(err)
	}
	requireInterruptNotices(t, manager, "agent_restart")
	// Simulate a second crash between appending the notice and removing journal.
	if err := savePendingBackendRun(manager.GetSessionFolder(), n.journal); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverPendingBackendRunManager(manager.GetSessionFolder(), nil); err != nil {
		t.Fatal(err)
	}
	requireInterruptNotices(t, manager, "agent_restart")
}

func TestInterruptJournalFallsBackWhenIntermediateCompactionIsCorrupt(t *testing.T) {
	root, err := freshNewContextManager("system", "task", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startRunNotices(func() *contextmanager.ContextManager { return root }, "run_corrupt_ancestor"); err != nil {
		t.Fatal(err)
	}
	intermediate, err := contextmanager.NewContextManagerRevisionFromMessageList(root, root.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	current, err := contextmanager.NewContextManagerRevisionFromMessageList(intermediate, intermediate.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	if err := contextmanager.SwitchSession(current.GetSessionFolder(), current.GetSessionID()); err != nil {
		t.Fatal(err)
	}
	intermediatePath := filepath.Join(intermediate.GetSessionFolder(), intermediate.GetSessionID()+".jsonl")
	if err := os.WriteFile(intermediatePath, []byte("not valid json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverPendingBackendRunManager(current.GetSessionFolder(), current)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.GetSessionID() != root.GetSessionID() {
		t.Fatalf("recovered session = %q, want pending session %q", recovered.GetSessionID(), root.GetSessionID())
	}
	if recovered != current {
		t.Fatal("fallback replaced the long-lived context manager")
	}
	reloaded, err := contextmanager.LoadContextManagerFromCurrentSession(root.GetSessionFolder())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GetSessionID() != recovered.GetSessionID() {
		t.Fatal("fallback did not persist the recovered active session")
	}
	requireInterruptNotices(t, recovered, "agent_restart")
	requireInterruptNotices(t, reloaded, "agent_restart")
	if !runtimeModelCallContains(messages.ConvertMessageList(recovered.CloneMessageList()), "Interrupt [agent_restart]:") {
		t.Fatal("active recovered context is missing restart notice")
	}
	if _, err := os.Stat(filepath.Join(root.GetSessionFolder(), pendingBackendRunFile)); !os.IsNotExist(err) {
		t.Fatalf("pending journal remains after fallback: %v", err)
	}
}

func TestInterruptJournalDoesNotContaminateNewConversation(t *testing.T) {
	original, err := freshNewContextManager("system", "task", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startRunNotices(func() *contextmanager.ContextManager { return original }, "run_old"); err != nil {
		t.Fatal(err)
	}
	current, err := contextmanager.NewContextManager(original.GetSessionFolder(), "new system")
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverPendingBackendRunManager(current.GetSessionFolder(), current)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != current {
		t.Fatal("recovery replaced the active conversation with the interrupted conversation")
	}
	requireInterruptNotices(t, current)
	requireInterruptNotices(t, original, "agent_restart")
	if got := contextmanager.CurrentSessionID(current.GetSessionFolder()); got != current.GetSessionID() {
		t.Fatalf("current session changed to %q", got)
	}
}

func TestInterruptJournalRetriesFailedNoticePersistence(t *testing.T) {
	manager, err := freshNewContextManager("system", "task", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := startRunNotices(func() *contextmanager.ContextManager { return manager }, "run_canceled")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(manager.GetSessionFolder(), manager.GetSessionID()+".jsonl")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.finish(ctx, ctx.Err()); err == nil {
		t.Fatal("notice persistence unexpectedly succeeded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverPendingBackendRunManager(manager.GetSessionFolder(), manager); err != nil {
		t.Fatal(err)
	}
	requireInterruptNotices(t, manager, "canceled")
}

func TestInterruptJournalCompletedRunIgnoresLaterCancellation(t *testing.T) {
	manager, err := freshNewContextManager("system", "task", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := startRunNotices(func() *contextmanager.ContextManager { return manager }, "run_done")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := n.finish(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := n.finish(ctx, ctx.Err()); err != nil {
		t.Fatal(err)
	}
	// An unlink failure after persisting Completed also must be harmless.
	if err := savePendingBackendRun(manager.GetSessionFolder(), n.journal); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverPendingBackendRunManager(manager.GetSessionFolder(), manager); err != nil {
		t.Fatal(err)
	}
	requireInterruptNotices(t, manager)
}

func TestInterruptJournalPanicIsRecordedAndRethrown(t *testing.T) {
	model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) { panic("boom") }}
	loop := newInterruptTestLoop(t, model)
	func() {
		defer func() {
			if got := recover(); got != "boom" {
				t.Fatalf("panic = %v", got)
			}
		}()
		_, _ = loop.Run(context.Background(), "task")
	}()
	requireInterruptNotices(t, loop.contextManager, "panic")
}

func TestInterruptJournalClearedHistoryIsNotRestored(t *testing.T) {
	manager, err := freshNewContextManager("system", "task", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startRunNotices(func() *contextmanager.ContextManager { return manager }, "run_cleared"); err != nil {
		t.Fatal(err)
	}
	if err := contextmanager.ClearAllSessions(manager.GetSessionFolder()); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverPendingBackendRunManager(manager.GetSessionFolder(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(manager.GetSessionFolder(), pendingBackendRunFile)); !os.IsNotExist(err) {
		t.Fatalf("cleared journal still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.GetSessionFolder(), manager.GetSessionID()+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("cleared history was recreated: %v", err)
	}
}

func TestRuntimeInterruptNoticeSurvivesRestartBeforeNextModelCall(t *testing.T) {
	cfg := withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "test"})
	model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
		return contentResponse("done"), nil
	}}
	newRuntime := func() *Runtime {
		runtime := NewRuntimeWithDeps(cfg, &testModelResolver{model: model}, NewMemoryManager(""), &ToolSet{tools: map[string]langtools.Tool{}}, NewSkillIndex())
		t.Cleanup(func() { _ = runtime.Close() })
		return runtime
	}
	first := newRuntime()
	if _, err := first.Run(context.Background(), RunRequest{Input: "first task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := startRunNotices(func() *contextmanager.ContextManager { return first.contextManager }, "run_abrupt_exit"); err != nil {
		t.Fatal(err)
	}
	if err := first.contextManager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "unfinished work"}); err != nil {
		t.Fatal(err)
	}
	model.generate = func(_ context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
		if !runtimeModelCallContains(input, "Interrupt [agent_restart]:") {
			t.Fatal("next model call is missing restart notice")
		}
		return contentResponse("recovered"), nil
	}
	restarted := newRuntime()
	restarted.stateManager.SetState("recovered_marker", "present")
	if _, err := restarted.Run(context.Background(), RunRequest{Input: "next task"}); err != nil {
		t.Fatal(err)
	}
	requireInterruptNotices(t, restarted.contextManager, "agent_restart")
	list := restarted.contextManager.CloneMessageList()
	noticeIndex, inputIndex := -1, -1
	stateSeen := false
	for i, message := range list {
		if message.Role == messages.MessageRoleNotice && strings.Contains(message.Content, "agent_restart") {
			noticeIndex = i
		}
		if message.Role == messages.MessageRoleState && strings.Contains(message.Content, "recovered_marker: present") {
			stateSeen = true
		}
		if message.Role == messages.MessageRoleUser && message.Content == "next task" {
			inputIndex = i
		}
	}
	if noticeIndex < 0 || inputIndex <= noticeIndex {
		t.Fatalf("notice/input order = %d/%d", noticeIndex, inputIndex)
	}
	if !stateSeen {
		t.Fatal("recovered context manager did not apply runtime state hook")
	}
}

func TestRuntimeInterruptRecoveryKeepsRotatedConversation(t *testing.T) {
	cfg := withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "test"})
	model := &interruptTestModel{generate: func(_ context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
		if runtimeModelCallContains(input, "old conversation task") || runtimeModelCallContains(input, "Interrupt [agent_restart]:") {
			t.Fatal("recovery injected the old conversation into the next model request")
		}
		if !runtimeModelCallContains(input, "new conversation task") {
			t.Fatal("next model request is missing the new input")
		}
		return contentResponse("done"), nil
	}}
	runtime := NewRuntimeWithDeps(cfg, &testModelResolver{model: model}, NewMemoryManager(""), &ToolSet{tools: map[string]langtools.Tool{}}, NewSkillIndex())
	defer runtime.Close()
	folder := agentpath.ContextManagerSessionFolder(cfg.ConfigDir)
	original, err := freshNewContextManager(runtime.getSystemPrompt(), "old conversation task", nil, folder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := startRunNotices(func() *contextmanager.ContextManager { return original }, "run_old_conversation"); err != nil {
		t.Fatal(err)
	}
	current, err := contextmanager.NewContextManager(folder, runtime.getSystemPrompt())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), RunRequest{Input: "new conversation task"}); err != nil {
		t.Fatal(err)
	}
	if runtime.contextManager.GetSessionID() != current.GetSessionID() {
		t.Fatal("runtime resumed the old conversation after rotation")
	}
	requireInterruptNotices(t, current)
	requireInterruptNotices(t, original, "agent_restart")
	oldContext, err := contextmanager.LoadContextManagerFromSessionID(folder, original.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	if runtimeModelCallContains(messages.ConvertMessageList(oldContext.CloneMessageList()), "new conversation task") {
		t.Fatal("new input was persisted in the old conversation")
	}
}
