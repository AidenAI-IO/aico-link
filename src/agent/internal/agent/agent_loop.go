package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/executor"
	"aiden-agent/internal/agent/messages"
	"aiden-agent/internal/agent/model"
	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/logging"

	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/callbacks"
	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
)

const agentLoopOutputKey = "output"

var errSteerInterruptToolCancel = errors.New("steer interrupt tool cancel")

type ContextBudgetGuard func(context.Context, *contextmanager.ContextManager, llms.CallOptions) (*contextmanager.ContextManager, bool, error)

type iterationOutcome uint8

const (
	iterationContinue iterationOutcome = iota
	iterationDone
	iterationRestartBudget
)

type AgentLoop struct {
	Model                      model.Model
	Profile                    RoleProfile
	SteerRecorder              steerConversationRecorder
	CallbacksHandler           callbacks.Handler
	MaxIterations              int
	Recorder                   *EpisodeRecorder
	SteerInterrupt             func() <-chan struct{}
	SteerProvider              func(context.Context) (RunSteerMessage, bool)
	SteerWaiter                func(context.Context) (RunSteerMessage, bool, error)
	TerminationPolicy          *TerminationPolicy
	DevicePlatform             string
	ScreenState                *screen.ScreenState
	PointerMode                string
	ToolResultObserver         ToolResultObserver
	ToolResultPolicy           ToolResultPolicy
	ContextCompactionTrigger   int
	ContextThresholdCompaction func(context.Context, *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error)
	ContextBudgetGuard         ContextBudgetGuard
	ContextOverflowRecovery    func(context.Context, *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error)
	toolExecutionHookFactory   func() toolExecutionHookHandler
	contextManager             *contextmanager.ContextManager
	notices                    *runNotices
}

func NewAgentLoop(
	model model.Model,
	profile RoleProfile,
	maxIterations int,
	callbacksHandler callbacks.Handler,
	recorder *EpisodeRecorder,
	contextManager *contextmanager.ContextManager,
) *AgentLoop {
	if contextManager == nil {
		logging.Fatalf("agent", "agent_loop", "context manager is nil")
	}
	return &AgentLoop{
		Model:            model,
		Profile:          profile,
		CallbacksHandler: callbacksHandler,
		MaxIterations:    maxIterations,
		Recorder:         recorder,
		ToolResultPolicy: NewToolResultPolicy(),
		contextManager:   contextManager,
	}
}

func (l *AgentLoop) Run(ctx context.Context, input string, options ...chains.ChainCallOption) (answer string, runErr error) {
	if l.notices == nil {
		l.notices = &runNotices{manager: func() *contextmanager.ContextManager { return l.contextManager }}
		defer func() { l.notices = nil }()
		defer l.notices.finishOnReturn(ctx, &runErr)
	}
	agentTools := l.Profile.Tools
	var transforms []executor.OutboundMessageTransform
	if IsAnthropicModel(l.Model.Spec().Provider, l.Model.Spec().Name) {
		frames := newVisualCoordinates(l.ScreenState)
		transforms = append(transforms, frames)
		agentTools = frames.wrap(agentTools)
	}
	llmExecutor := executor.NewLLMExecutor(l.Model, l.contextManager, transforms...)
	toolSpecs := NewToolSpecs(agentTools)
	parser := &FunctionAgent{
		Tools:     agentTools,
		OutputKey: agentLoopOutputKey,
	}
	callOptions := chains.GetLLMCallOptions(options...)
	if handler, ok := l.CallbacksHandler.(streamingReasoningHandler); ok && handler.StreamingReasoningEnabled() {
		callOptions = append(callOptions, llms.WithStreamingReasoningFunc(func(ctx context.Context, reasoningChunk, _ []byte) error {
			handler.HandleStreamingReasoning(ctx, reasoningChunk)
			return nil
		}))
	}
	var toolExecutionHooks toolExecutionHookHandler
	if l.toolExecutionHookFactory != nil {
		toolExecutionHooks = l.toolExecutionHookFactory()
	}
	policy := l.loopGuardPolicy()
	contextOverflowRecoveryUsed := false

restartBudget:
	for {
		for i := 0; i < l.MaxIterations; i++ {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if decision := policy.CheckBeforeIteration(ctx, i+1, l.MaxIterations); decision.Stop {
				answer, done, err := l.stopWithSteerCheck(ctx, llmExecutor, policy, decision)
				if err != nil {
					return "", err
				}
				if done {
					return answer, nil
				}
				continue restartBudget
			}
			answer, outcome, err := l.runIteration(ctx, i+1, callOptions, llmExecutor, parser, toolSpecs, toolExecutionHooks, policy, &contextOverflowRecoveryUsed)
			if err != nil {
				return "", err
			}
			if outcome == iterationDone {
				return answer, nil
			}
			if outcome == iterationRestartBudget {
				continue restartBudget
			}
		}

		answer, done, err := l.stopWithSteerCheck(ctx, llmExecutor, policy, policy.BudgetExhausted("max_iterations budget exhausted"))
		if err != nil {
			return "", err
		}
		if done {
			return answer, nil
		}
	}
}

func (l *AgentLoop) loopGuardPolicy() *TerminationPolicy {
	if l != nil && l.TerminationPolicy != nil {
		return l.TerminationPolicy
	}
	return NewTerminationPolicy(DefaultTerminationPolicyConfig())
}

// stopWithSteerCheck checks for pending steer before terminating, giving steer priority.
// The done result is false when a steer was consumed and the caller should restart
// the iteration budget so the new instruction can be processed.
func (l *AgentLoop) stopWithSteerCheck(ctx context.Context, executor *executor.LLMExecutor, policy *TerminationPolicy, decision TerminationDecision) (string, bool, error) {
	// Check for pending steer before terminating (except for external cancellation)
	if decision.Reason != StopReasonExternal {
		if _, hasPending, err := l.consumeAndPersistSteer(ctx, executor); err != nil {
			return "", false, err
		} else if hasPending {
			policy.ResetForSteer()
			return "", false, nil
		}
	}
	answer, err := l.stopWithDecision(ctx, policy, decision)
	return answer, true, err
}

func (l *AgentLoop) runIteration(ctx context.Context, iteration int, callOptions []llms.CallOption, llmExecutor *executor.LLMExecutor, parser *FunctionAgent, toolSpecs *ToolSpecs, toolExecutionHooks toolExecutionHookHandler, policy *TerminationPolicy, contextOverflowRecoveryUsed *bool) (string, iterationOutcome, error) {
	iterationStartTime := time.Now()
	toolCallsInIteration := 0
	if l.Recorder != nil {
		l.Recorder.RecordEvent(TaskEpisodeEvent{
			Type: runEventIterationStart,
			Ts:   iterationStartTime.Format(time.RFC3339Nano),
			Metadata: map[string]interface{}{
				"iteration": iteration,
			},
		})
		defer func() {
			iterDuration := time.Since(iterationStartTime).Milliseconds()
			l.Recorder.RecordEvent(TaskEpisodeEvent{
				Type:       runEventIterationEnd,
				Ts:         time.Now().Format(time.RFC3339Nano),
				DurationMs: &iterDuration,
				Metadata: map[string]interface{}{
					"iteration":  iteration,
					"tool_calls": toolCallsInIteration,
				},
			})
		}()
	}

	// Problem 4: Check for pending steer before LLM call
	if _, hasPending, err := l.consumeAndPersistSteer(ctx, llmExecutor); err != nil {
		return "", iterationContinue, err
	} else if hasPending {
		policy.ResetForSteer()
		return "", iterationRestartBudget, nil
	}

	turnOptions := append([]llms.CallOption{}, callOptions...)
	turnOptions = append(turnOptions, llms.WithTools(parser.toolsAsLLM()))
	if err := l.guardContextBudgetBeforeLLM(ctx, llmExecutor, turnOptions); err != nil {
		return "", iterationContinue, err
	}
	compacted, compactErr := l.compactContextBeforeLLM(ctx, llmExecutor, turnOptions)
	if compactErr != nil {
		return "", iterationContinue, compactErr
	}
	if compacted {
		// The summary changes the request; validate its budget again before
		// generation instead of assuming that every summary makes it smaller.
		if err := l.guardContextBudgetBeforeLLM(ctx, llmExecutor, turnOptions); err != nil {
			return "", iterationContinue, err
		}
	}

	// Problem 4: Support interrupting LLM call during generation
	llmCtx, llmCancel := context.WithCancelCause(ctx)
	defer llmCancel(nil)
	// Tag the call so the captured prompt becomes an `agent-response` generation
	// in the episode trace.
	llmCtx = withTelemetryRole(llmCtx, telemetryRoleAgent)

	if l.SteerInterrupt != nil {
		interruptCh := l.SteerInterrupt()
		if interruptCh != nil {
			done := make(chan struct{})
			go func() {
				select {
				case <-interruptCh:
					llmCancel(errSteerInterruptToolCancel)
				case <-llmCtx.Done():
				case <-done:
				}
			}()
			defer close(done)
		}
	}
	if handler, ok := l.CallbacksHandler.(streamingReasoningHandler); ok && handler.StreamingReasoningEnabled() {
		handler.ResetStreamingReasoning(ctx)
	}
	contentResp, err := llmExecutor.GenerateContent(contextWithRawHTTPLog(llmCtx), turnOptions...)
	if err != nil {
		l.abortStreamingResponse(ctx)
		if ctx.Err() != nil {
			return "", iterationContinue, ctx.Err()
		}
		if contextOverflowRecoveryUsed != nil && !*contextOverflowRecoveryUsed && isProviderContextExceededError(err) && l.ContextOverflowRecovery != nil {
			*contextOverflowRecoveryUsed = true
			newManager, compacted, recoveryErr := l.ContextOverflowRecovery(ctx, llmExecutor.ContextManager())
			if recoveryErr != nil {
				return "", iterationContinue, fmt.Errorf("compact context after provider context overflow: %w", recoveryErr)
			}
			if compacted {
				if newManager == nil {
					return "", iterationContinue, fmt.Errorf("compact context after provider context overflow: context manager is nil")
				}
				l.contextManager = newManager
				llmExecutor.ReplaceContextManager(newManager)
				logging.Infof("agent", "context", "provider context limit exceeded; compacted context and retrying")
				return "", iterationRestartBudget, nil
			}
		}
		// If LLM was canceled due to interrupt, check for pending steer
		if errors.Is(err, context.Canceled) || errors.Is(err, errSteerInterruptToolCancel) {
			steerInterrupted := errors.Is(context.Cause(llmCtx), errSteerInterruptToolCancel)
			if _, hasPending, persistErr := l.consumeAndPersistSteer(ctx, llmExecutor); persistErr != nil {
				return "", iterationContinue, persistErr
			} else if hasPending {
				policy.ResetForSteer()
				return "", iterationRestartBudget, nil
			}
			// Only wait for out-of-band steer if cancellation was triggered by steer interrupt,
			// not by external cancellation (shutdown, timeout, etc.)
			if steerInterrupted && l.SteerWaiter != nil {
				// Use ctx (not Background) so outer cancellation can abort the wait
				waitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				waitedSteer, hasText, waitErr := l.SteerWaiter(waitCtx)
				cancel()
				if waitErr == nil && hasText {
					if persistErr := l.persistSteer(ctx, llmExecutor, waitedSteer); persistErr != nil {
						return "", iterationContinue, persistErr
					}
					policy.ResetForSteer()
					return "", iterationRestartBudget, nil
				}
			}
			if err := ctx.Err(); err != nil {
				return "", iterationContinue, err
			}
			if steerInterrupted {
				if err := l.appendSteerResumeNotice(); err != nil {
					return "", iterationContinue, err
				}
				// A queued steer may have been canceled after the interrupt fired.
				// Retry the original task with the request's rearmed signal channel.
				return "", iterationRestartBudget, nil
			}
		}
		return "", iterationContinue, err
	}
	if err := ctx.Err(); err != nil {
		l.abortStreamingResponse(ctx)
		return "", iterationContinue, err
	}
	l.finishStreamingResponse(ctx)
	l.emitRoleOutputWithReasoning(ctx, contentResp)
	if answer := l.touchPointerModeMismatchContentFinalAnswer(contentResp); answer != "" {
		if err := l.notices.stop("device_mode_mismatch", "Execution stopped because the touch mode appears incompatible with the connected device."); err != nil {
			return "", iterationContinue, err
		}
		if l.Recorder != nil {
			l.Recorder.RecordDefaultFinish(answer)
		}
		answer, err = l.finishRun(ctx, answer)
		return answer, iterationDone, err
	}

	actions, finish, err := parser.ParseOutput(contentResp)
	if errors.Is(err, agents.ErrUnableToParseOutput) {
		choiceMessage := messages.ConvertChoiceToContextManagerMessage(*contentResp.Choices[0])
		if choiceMessage.Role != messages.MessageRoleToolCall {
			if err := llmExecutor.AppendMessage(choiceMessage); err != nil {
				return "", iterationContinue, err
			}
		}
		decision := policy.RecordParseFailure()
		if decision.Stop {
			return l.finishStopDecision(ctx, policy, decision)
		}
		l.applyLoopGuardDecision(decision)

		// Problem 4: Check for pending steer after parse failure (continue iteration)
		if steer, hasPending, err := l.consumeAndPersistSteer(ctx, llmExecutor); err != nil {
			return "", iterationContinue, err
		} else if hasPending {
			policy.ResetForSteer()
			logging.Warnf("agent", "steer", "LLM parse failed, steer injected (length=%d)", len(steer.Content))
			return "", iterationRestartBudget, nil
		}

		return "", iterationContinue, nil
	}
	if err != nil {
		return "", iterationContinue, err
	}
	if finish != nil {
		// Don't check steer after finish - let this turn complete normally.
		// Steer will be processed as a new turn.
		if err := llmExecutor.AppendMessage(messages.ConvertChoiceToContextManagerMessage(*contentResp.Choices[0])); err != nil {
			return "", iterationContinue, err
		}
		answer, _ := finish.ReturnValues[agentLoopOutputKey].(string)
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return "", iterationContinue, agents.ErrAgentNoReturn
		}
		if l.Recorder != nil {
			l.Recorder.RecordDefaultFinish(answer)
		}
		answer, err = l.finishRun(ctx, answer)
		return answer, iterationDone, err
	}
	if len(actions) == 0 {
		return "", iterationContinue, agents.ErrAgentNoReturn
	}

	// Problem 4: Check for pending steer after LLM returns actions (before tool execution)
	if steer, hasPending := l.checkPendingSteer(ctx); hasPending {
		policy.ResetForSteer()
		if err := l.persistSteer(ctx, llmExecutor, steer); err != nil {
			return "", iterationContinue, err
		}
		logging.Infof("agent", "steer", "LLM action interrupted, steer injected (length=%d)", len(steer.Content))
		return "", iterationRestartBudget, nil
	}

	action := actions[0]
	toolCallMessage := messages.ConvertChoiceToContextManagerMessage(choiceWithOnlyToolCall(*contentResp.Choices[0], action.ToolID))
	var after AfterToolCallHook
	if toolExecutionHooks != nil {
		after = func(ctx context.Context, call ToolCall, result ToolResult) ToolResult {
			result = DefaultAfterToolCall(ctx, call, result)
			return toolExecutionHooks.AfterToolCall(ctx, call, result)
		}
	}
	toolCtx := WithEpisodeRecorder(ctx, l.Recorder)
	toolExecution := l.executeToolCall(toolCtx, ToolCallExecution{
		Specs:  toolSpecs,
		Action: action,
		Before: func(ctx context.Context, call ToolCall) (ToolResult, bool) {
			result, allowed := policy.BeforeToolCall(call.Spec.Name, call.Input)
			if !allowed {
				return result, false
			}
			result, allowed = DefaultBeforeToolCall(ctx, call)
			if !allowed || toolExecutionHooks == nil {
				return result, allowed
			}
			return toolExecutionHooks.BeforeToolCall(ctx, call)
		},
		After:          after,
		Callback:       l.CallbacksHandler,
		ResultObserver: l.ToolResultObserver,
	})
	if l.Recorder != nil {
		l.Recorder.RecordExecution(ToolCallExecutionResult{
			Call:   toolExecution.Call,
			Step:   toolExecution.Step,
			Result: toolExecution.Result,
			Error:  toolExecution.Error,
		})
	}
	toolCallsInIteration++
	prepared := PreparedToolResult{
		Content:  toolExecution.Step.Observation,
		Complete: true,
	}
	isVisualObservation := parser != nil && parser.isVisualObservationTool(toolExecution.Step.Action.Tool)
	if !isVisualObservation {
		resultPolicy := l.ToolResultPolicy
		if resultPolicy == nil {
			resultPolicy = NewToolResultPolicy()
		}
		preparedResult, prepareErr := resultPolicy.Prepare(ctx, ToolResultPrepareInput{
			Call:            toolExecution.Call,
			Result:          toolExecution.Result,
			ActionCompleted: toolExecution.ActionCompleted,
			ContextManager:  llmExecutor.ContextManager(),
			ModelSpec:       l.Model.Spec(),
			CallOptions:     turnOptions,
		})
		if prepareErr != nil {
			logging.Errorf("agent", "tool_result", "preparation failed for %s: %v", toolExecution.Call.Spec.Name, prepareErr)
			prepared = failedPreparedToolResult(toolExecution.Result, toolExecution.ActionCompleted)
		} else {
			prepared = preparedResult
		}
	}
	if l.Recorder != nil && !isVisualObservation {
		l.Recorder.RecordEvent(TaskEpisodeEvent{
			Type:     runEventToolResultContext,
			Role:     "agent",
			ToolName: toolExecution.Call.Spec.Name,
			Metadata: map[string]interface{}{
				"original_bytes":         prepared.OriginalBytes,
				"original_chars":         prepared.OriginalChars,
				"estimated_tokens":       prepared.EstimatedTokens,
				"context_bytes":          prepared.ContextBytes,
				"context_tokens":         prepared.ContextTokens,
				"processing_reason":      prepared.Reason,
				"artifactized":           prepared.ArtifactPath != "",
				"artifact_complete":      prepared.ArtifactComplete,
				"artifact_store_error":   prepared.ArtifactStoreError,
				"processing_duration_ms": prepared.ProcessingDurationMs,
			},
		})
	}
	appendErr := appendToolExecutionMessages(llmExecutor, parser, toolCallMessage, toolExecution.Step, prepared)
	if err := ctx.Err(); err != nil {
		if appendErr != nil {
			return "", iterationContinue, errors.Join(err, appendErr)
		}
		return "", iterationContinue, err
	}
	if toolExecution.Error != nil {
		if appendErr != nil {
			return "", iterationContinue, errors.Join(toolExecution.Error, appendErr)
		}
		// If the tool was canceled by steering, consume an already queued steer or
		// briefly wait for the out-of-band capture to finish.
		if toolExecution.InterruptedBySteer && errors.Is(toolExecution.Error, context.Canceled) {
			steer, hasPending, err := l.consumeAndPersistSteer(ctx, llmExecutor)
			if err != nil {
				return "", iterationContinue, err
			}
			// Only wait for out-of-band steer if cancellation was triggered by steer interrupt.
			// Check if outer ctx is still valid - if it's canceled, this is external cancellation.
			if !hasPending && ctx.Err() == nil && l.SteerWaiter != nil {
				// Use ctx (not Background) so outer cancellation can abort the wait
				waitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
				waitedSteer, hasText, waitErr := l.SteerWaiter(waitCtx)
				cancel()
				if waitErr == nil && hasText {
					if err := l.persistSteer(ctx, llmExecutor, waitedSteer); err != nil {
						return "", iterationContinue, err
					}
					steer = waitedSteer
					hasPending = true
				}
			}
			if err := ctx.Err(); err != nil {
				return "", iterationContinue, err
			}
			if hasPending {
				policy.ResetForSteer()
				logging.Infof("agent", "steer", "tool canceled but pending steer exists (length=%d), restarting iteration budget", len(steer.Content))
				return "", iterationRestartBudget, nil
			}
			if ctx.Err() == nil {
				if err := l.appendSteerResumeNotice(); err != nil {
					return "", iterationContinue, err
				}
				// The steer signal interrupted the tool, but its queued message was
				// canceled before consumption. Retry the original task with the
				// request's rearmed signal channel and a fresh iteration budget.
				return "", iterationRestartBudget, nil
			}
		}

		decision := policy.AfterToolCall(
			toolExecution.Call.Spec.Name,
			toolExecution.Call.Input,
			toolExecution.Error.Error(),
			true,
		)
		if decision.Stop {
			return l.finishStopDecision(ctx, policy, decision)
		}
		l.applyLoopGuardDecision(decision)
		return "", iterationContinue, nil
	}
	if appendErr != nil {
		return "", iterationContinue, appendErr
	}
	if answer := l.touchPointerModeMismatchFinalAnswer(toolExecution.Step); answer != "" {
		if err := l.notices.stop("device_mode_mismatch", "Execution stopped because the touch mode appears incompatible with the connected device."); err != nil {
			return "", iterationContinue, err
		}
		if l.Recorder != nil {
			l.Recorder.RecordDefaultFinish(answer)
		}
		answer, err = l.finishRun(ctx, answer)
		return answer, iterationDone, err
	}

	// Record tool execution in termination policy BEFORE checking steer
	// This ensures all tool calls are tracked for loop detection
	decision := policy.AfterToolCall(
		toolExecution.Call.Spec.Name,
		toolExecution.Call.Input,
		toolExecution.Step.Observation,
		toolExecution.Result.IsError(),
	)

	// Check for pending steer after tool execution (soft interrupt decision point)
	if _, hasPending, err := l.consumeAndPersistSteer(ctx, llmExecutor); err != nil {
		return "", iterationContinue, err
	} else if hasPending {
		policy.ResetForSteer()
		// Restart the budget so the LLM can process the tool result and steer content.
		return "", iterationRestartBudget, nil
	}

	if isRunPausingTool(toolExecution.Call.Action.Tool) && !toolExecution.Result.IsError() {
		if err := l.contextManager.AppendMessage(messages.Message{
			Role:    messages.MessageRoleNotice,
			Content: fmt.Sprintf("Pause [%s]: Execution is intentionally paused. This is not confirmation that the task is complete. Continue when the requested user action or wakeup input arrives.", toolExecution.Call.Action.Tool),
		}); err != nil {
			return "", iterationContinue, err
		}
		answer := runPausingToolFinalAnswer(&toolExecution.Step)
		if l.Recorder != nil {
			l.Recorder.RecordDefaultFinish(answer)
		}
		answer, err = l.finishRun(ctx, answer)
		return answer, iterationDone, err
	}
	if decision.Stop {
		return l.finishStopDecision(ctx, policy, decision)
	}
	l.applyLoopGuardDecision(decision)

	return "", iterationContinue, nil
}

func (l *AgentLoop) guardContextBudgetBeforeLLM(ctx context.Context, llmExecutor *executor.LLMExecutor, options []llms.CallOption) error {
	if l.ContextBudgetGuard == nil {
		return nil
	}
	var resolvedOptions llms.CallOptions
	for _, option := range options {
		if option != nil {
			option(&resolvedOptions)
		}
	}
	newManager, changed, err := l.ContextBudgetGuard(ctx, llmExecutor.ContextManager(), resolvedOptions)
	if err != nil {
		return fmt.Errorf("guard context budget before model request: %w", err)
	}
	if changed {
		if newManager == nil {
			return fmt.Errorf("guard context budget before model request: context manager is nil")
		}
		l.contextManager = newManager
		llmExecutor.ReplaceContextManager(newManager)
	}
	return nil
}

func (l *AgentLoop) compactContextBeforeLLM(ctx context.Context, llmExecutor *executor.LLMExecutor, options []llms.CallOption) (bool, error) {
	if l == nil || llmExecutor == nil || l.ContextCompactionTrigger <= 0 || l.ContextThresholdCompaction == nil {
		return false, nil
	}
	manager := llmExecutor.ContextManager()
	if manager == nil {
		return false, nil
	}
	promptTokens := estimateActivePromptTokens(manager, options)
	if promptTokens <= l.ContextCompactionTrigger {
		return false, nil
	}
	newManager, compacted, err := l.ContextThresholdCompaction(ctx, manager)
	if err != nil {
		return false, fmt.Errorf("compact context at agent-loop threshold: %w", err)
	}
	if !compacted {
		return false, nil
	}
	if newManager == nil {
		return false, fmt.Errorf("compact context at agent-loop threshold: context manager is nil")
	}
	l.contextManager = newManager
	llmExecutor.ReplaceContextManager(newManager)
	logging.Infof("agent", "context", "agent-loop context reached %d tokens (trigger %d); compacted before next model call", promptTokens, l.ContextCompactionTrigger)
	return true, nil
}

func (l *AgentLoop) finishStopDecision(ctx context.Context, policy *TerminationPolicy, decision TerminationDecision) (string, iterationOutcome, error) {
	// Problem 5: When terminating with pending steer, inject steer as new user message
	// instead of wrapping it as final answer. Apply to all termination reasons except
	// external cancellation (where the context is already done and steer is irrelevant).
	if decision.Reason != StopReasonExternal {
		// Note: executor is not available here, but consumeAndPersistSteer handles nil executor
		if _, hasPending, err := l.consumeAndPersistSteer(ctx, nil); err != nil {
			return "", iterationContinue, err
		} else if hasPending {
			policy.ResetForSteer()
			// Don't terminate; restart the iteration budget for the new instruction.
			return "", iterationRestartBudget, nil
		}
	}
	answer, err := l.stopWithDecision(ctx, policy, decision)
	return answer, iterationDone, err
}

func (l *AgentLoop) applyLoopGuardDecision(decision TerminationDecision) {
	if strings.TrimSpace(decision.Notice) == "" {
		return
	}
	if err := l.contextManager.AppendMessage(messages.Message{
		Role:    messages.MessageRoleNotice,
		Content: decision.Notice,
	}); err != nil {
		logging.Warnf("agent", "loop_guard", "failed to append notice message: %v", err)
	}
}

func (l *AgentLoop) stopWithDecision(ctx context.Context, policy *TerminationPolicy, decision TerminationDecision) (string, error) {
	if decision.Reason == StopReasonExternal && ctx != nil {
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	lastTool := ""
	if policy != nil {
		lastTool = policy.lastToolName
	}
	if err := l.notices.stop(string(decision.Reason), "Execution stopped before completion: "+decision.Message+"."); err != nil {
		return "", err
	}
	answer := formatLoopGuardStopMessage(decision, lastTool)
	if l != nil && l.Recorder != nil {
		l.Recorder.RecordEvent(TaskEpisodeEvent{
			Type: runEventLoopGuardStop,
			Ts:   time.Now().Format(time.RFC3339Nano),
			Metadata: map[string]interface{}{
				"reason":  string(decision.Reason),
				"message": decision.Message,
				"tier":    int(decision.Tier),
			},
		})
		l.Recorder.RecordDefaultFinish(answer)
	}
	return l.finishRun(ctx, answer)
}

func (l *AgentLoop) checkPendingSteer(ctx context.Context) (RunSteerMessage, bool) {
	if l == nil || l.SteerProvider == nil || ctx.Err() != nil {
		return RunSteerMessage{}, false
	}
	return l.SteerProvider(ctx)
}

// consumeAndPersistSteer checks for pending steer, and if found, executes the
// three-step persistence pipeline: append to context manager, persist to memory,
// and emit event. Returns the steer message, whether one was found, and any error.
func (l *AgentLoop) consumeAndPersistSteer(
	ctx context.Context,
	executor *executor.LLMExecutor,
) (RunSteerMessage, bool, error) {
	steer, hasPending := l.checkPendingSteer(ctx)
	if !hasPending {
		return RunSteerMessage{}, false, nil
	}
	if err := l.persistSteer(ctx, executor, steer); err != nil {
		return steer, false, err
	}
	return steer, true, nil
}

func (l *AgentLoop) persistSteer(ctx context.Context, executor *executor.LLMExecutor, steer RunSteerMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Normalize once so the model context, the recorded steer, and the emitted
	// event all carry the same text. Whitespace-only input would otherwise reach
	// the model as an empty message while being recorded as the placeholder.
	steer.Content = steerHumanMessageContent(steer)

	// Persist the interrupt notice and new instruction together.
	manager := l.contextManager
	if executor != nil {
		manager = executor.ContextManager()
	}
	if err := manager.AppendMessages([]messages.Message{
		interruptNotice("steer", "A new instruction interrupted the current execution plan. Continue this run using the following user instruction; the previous plan is not confirmed complete."),
		{Role: messages.MessageRoleUser, Content: steer.Content},
	}); err != nil {
		return err
	}

	// Step 2: Track the steer for session event persistence.
	if l.SteerRecorder != nil {
		if err := l.SteerRecorder.RecordSteer(steer); err != nil {
			return err
		}
	}

	// Step 3: Emit event
	if l.CallbacksHandler != nil {
		if handler, ok := l.CallbacksHandler.(interface {
			HandleSteerMessage(context.Context, RunSteerMessage)
		}); ok {
			handler.HandleSteerMessage(ctx, steer)
		}
	}

	return nil
}

func (l *AgentLoop) appendSteerResumeNotice() error {
	return l.contextManager.AppendMessage(interruptNotice("steer_resumed", "The current model request or tool was interrupted, but no replacement instruction was consumed. Continue the original task from its last confirmed state."))
}

func (l *AgentLoop) executeToolCall(ctx context.Context, execution ToolCallExecution) ToolCallExecutionResult {
	interruptCh := (<-chan struct{})(nil)
	if l != nil && l.SteerInterrupt != nil {
		interruptCh = l.SteerInterrupt()
	}
	if interruptCh == nil {
		return executeToolCall(ctx, execution)
	}

	toolCtx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		select {
		case <-interruptCh:
			cancel(errSteerInterruptToolCancel)
		case <-toolCtx.Done():
		case <-done:
		}
	}()
	result := executeToolCall(toolCtx, execution)
	result.InterruptedBySteer = errors.Is(context.Cause(toolCtx), errSteerInterruptToolCancel)
	close(done)
	cancel(nil)
	return result
}

func (l *AgentLoop) finishRun(ctx context.Context, finalAnswer string) (string, error) {
	finalAnswer = strings.TrimSpace(finalAnswer)
	if l.CallbacksHandler != nil {
		l.CallbacksHandler.HandleAgentFinish(ctx, schema.AgentFinish{
			ReturnValues: map[string]any{agentLoopOutputKey: finalAnswer},
			Log:          finalAnswer,
		})
	}
	return finalAnswer, nil
}

func (l *AgentLoop) emitRoleOutputWithReasoning(ctx context.Context, contentResp *llms.ContentResponse) {
	if contentResp == nil || len(contentResp.Choices) == 0 || contentResp.Choices[0] == nil || l.CallbacksHandler == nil {
		return
	}
	choice := contentResp.Choices[0]
	content := strings.TrimSpace(choice.Content)
	reasoning := strings.TrimSpace(choice.ReasoningContent)
	if content == "" && reasoning == "" {
		return
	}
	if handler, ok := l.CallbacksHandler.(roleOutputHandler); ok {
		handler.HandleRoleOutput(ctx, "agent", content, reasoning)
	}
}

type streamingResponseBoundaryHandler interface {
	FinishStreamingResponse(context.Context)
	AbortStreamingResponse(context.Context)
}

func (l *AgentLoop) finishStreamingResponse(ctx context.Context) {
	if l == nil || l.CallbacksHandler == nil {
		return
	}
	if handler, ok := l.CallbacksHandler.(streamingResponseBoundaryHandler); ok {
		handler.FinishStreamingResponse(ctx)
	}
}

func (l *AgentLoop) abortStreamingResponse(ctx context.Context) {
	if l == nil || l.CallbacksHandler == nil {
		return
	}
	if handler, ok := l.CallbacksHandler.(streamingResponseBoundaryHandler); ok {
		handler.AbortStreamingResponse(ctx)
	}
}

type roleOutputHandler interface {
	HandleRoleOutput(ctx context.Context, role, content, reasoning string)
}

type streamingReasoningHandler interface {
	HandleStreamingReasoning(ctx context.Context, chunk []byte)
	ResetStreamingReasoning(ctx context.Context)
	StreamingReasoningEnabled() bool
}

func choiceWithOnlyToolCall(choice llms.ContentChoice, toolID string) llms.ContentChoice {
	if len(choice.ToolCalls) == 0 {
		if choice.FuncCall != nil {
			choice.ToolCalls = []llms.ToolCall{{
				ID:           ensureToolCallID(toolID, 0),
				Type:         "function",
				FunctionCall: choice.FuncCall,
			}}
		}
		return choice
	}
	toolID = strings.TrimSpace(toolID)
	var firstValid *llms.ToolCall
	for i := range choice.ToolCalls {
		call := choice.ToolCalls[i]
		if call.FunctionCall == nil {
			continue
		}
		if firstValid == nil {
			firstValid = &call
		}
		if toolID != "" && strings.TrimSpace(call.ID) == toolID {
			return selectToolCall(choice, call)
		}
	}
	if firstValid == nil {
		choice.ToolCalls = nil
		choice.FuncCall = nil
		return choice
	}
	return selectToolCall(choice, *firstValid)
}

// selectToolCall points the choice at the call this iteration executes. Stateless
// Responses replay drops the calls that stay unexecuted, because a replayed
// function call without its output makes the next request invalid. A native
// Interactions turn keeps every call instead: the API answers with all the calls
// the model decided on and requires a function_result for each of them, so the
// runtime reports the unexecuted calls as such (see
// unexecutedInteractionToolResults) rather than rewriting the provider's steps.
func selectToolCall(choice llms.ContentChoice, call llms.ToolCall) llms.ContentChoice {
	choice.FuncCall = call.FunctionCall
	if steps, ok := choice.GenerationInfo["interactions_steps"].([]json.RawMessage); ok && len(steps) > 0 {
		return choice
	}
	choice.ToolCalls = []llms.ToolCall{call}
	choice.GenerationInfo = responsesGenerationInfoForToolCall(choice.GenerationInfo, call.ID)
	return choice
}

// responsesGenerationInfoForToolCall removes unexecuted parallel function-call
// items from stateless Responses replay. Some compatible providers, including
// DeepSeek, always allow parallel tool calls even when the request asks them
// not to. Aiden executes one call per iteration, so replaying the other calls
// without matching function_call_output items would make the next request
// invalid.
func responsesGenerationInfoForToolCall(info map[string]any, toolID string) map[string]any {
	toolID = strings.TrimSpace(toolID)
	items, ok := info["responses_output_items"].([]json.RawMessage)
	if !ok || len(items) == 0 || toolID == "" {
		return info
	}
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var metadata struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &metadata) != nil || metadata.Type != "function_call" || strings.TrimSpace(metadata.CallID) == toolID {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) == len(items) {
		return info
	}
	cloned := make(map[string]any, len(info))
	for key, value := range info {
		cloned[key] = value
	}
	cloned["responses_output_items"] = filtered
	return cloned
}

func appendToolExecutionMessages(llmExecutor *executor.LLMExecutor, parser *FunctionAgent, toolCall messages.Message, step schema.AgentStep, prepared PreparedToolResult) error {
	if llmExecutor == nil {
		return fmt.Errorf("llm executor is nil")
	}

	toolContent := prepared.Content
	var followups []llms.MessageContent
	if parser != nil && parser.isVisualObservationTool(step.Action.Tool) {
		if content, visualFollowups := parser.observationMessagesForStep(step); len(visualFollowups) > 0 {
			toolContent = content
			followups = visualFollowups
			prepared = PreparedToolResult{Content: content, Complete: true}
		}
	}
	prepared.Content = toolContent

	contextMessages := []messages.Message{toolCall, toolResultMessage(
		step.Action.ToolID,
		step.Action.Tool,
		prepared,
	)}
	contextMessages[1].ToolResults = append(
		contextMessages[1].ToolResults,
		unexecutedInteractionToolResults(toolCall, step.Action.ToolID)...,
	)
	for _, followup := range followups {
		contextMessages = append(contextMessages, visualFollowupMessageFromLLMContent(llmExecutor.ContextManager(), followup))
	}
	if err := llmExecutor.AppendMessages(contextMessages); err != nil {
		return fmt.Errorf("failed to append tool call and result messages: %w", err)
	}
	return nil
}

// unexecutedInteractionToolResult answers a native Interactions function call the
// iteration did not run.
const unexecutedInteractionToolResult = "Not executed: this runtime runs one tool call per turn. Call it again if it is still needed."

// unexecutedInteractionToolResults answers the native Interactions function calls
// the iteration did not execute. Aiden runs one tool call per iteration so that
// every action is gated and observed on its own, while the Interactions API
// returns all the calls the model decided on and requires a function_result for
// each call_id. The remaining calls are reported as unexecuted instead of being
// deleted from the provider's step list, which keeps local StepList replay valid
// and leaves no call unanswered in a stored interaction. Other transports narrow
// the tool-call message to the executed call, so this reports nothing for them.
func unexecutedInteractionToolResults(toolCall messages.Message, executedToolCallID string) []messages.ToolResult {
	if len(toolCall.ToolCalls) < 2 {
		return nil
	}
	executedToolCallID = strings.TrimSpace(executedToolCallID)
	results := make([]messages.ToolResult, 0, len(toolCall.ToolCalls)-1)
	for _, call := range toolCall.ToolCalls {
		id := strings.TrimSpace(call.ID)
		if id == "" || id == executedToolCallID {
			continue
		}
		results = append(results, messages.ToolResult{
			ToolCallID: id,
			Name:       call.Name,
			Content:    unexecutedInteractionToolResult,
		})
	}
	return results
}

func (l *AgentLoop) touchPointerModeMismatchFinalAnswer(step schema.AgentStep) string {
	if !toolNameEqual(step.Action.Tool, "touch_gesture") {
		return ""
	}
	var payload struct {
		ScreenChanged *bool `json:"screen_changed"`
	}
	if err := json.Unmarshal([]byte(step.Observation), &payload); err != nil {
		return ""
	}
	if payload.ScreenChanged == nil || *payload.ScreenChanged {
		return ""
	}

	platform := strings.ToLower(strings.TrimSpace(l.DevicePlatform))
	pointerMode := strings.ToLower(strings.TrimSpace(l.PointerMode))
	return touchPointerModeMismatchGuidance(platform, pointerMode)
}

func (l *AgentLoop) touchPointerModeMismatchContentFinalAnswer(contentResp *llms.ContentResponse) string {
	if contentResp == nil || len(contentResp.Choices) == 0 || contentResp.Choices[0] == nil {
		return ""
	}
	choice := contentResp.Choices[0]
	if !choiceHasToolCall(choice, "touch_gesture") {
		return ""
	}
	content := strings.TrimSpace(choice.Content)
	if content == "" {
		return ""
	}
	lower := strings.ToLower(content)
	if !strings.Contains(lower, "hid.pointer_mode") &&
		!strings.Contains(lower, "device.device_type") &&
		!strings.Contains(lower, "[basic_settings.device].device_type") {
		return ""
	}
	if !strings.Contains(lower, "stop operation here") && !strings.Contains(lower, "touch mode likely does not match") {
		return ""
	}
	platform := strings.ToLower(strings.TrimSpace(l.DevicePlatform))
	pointerMode := strings.ToLower(strings.TrimSpace(l.PointerMode))
	return touchPointerModeMismatchGuidance(platform, pointerMode)
}

func choiceHasToolCall(choice *llms.ContentChoice, toolName string) bool {
	if choice == nil {
		return false
	}
	for _, call := range choice.ToolCalls {
		if call.FunctionCall != nil && toolNameEqual(call.FunctionCall.Name, toolName) {
			return true
		}
	}
	return false
}

func touchPointerModeMismatchGuidance(platform, pointerMode string) string {
	switch {
	case platform == "android" && pointerMode == "absolute":
		return `touch_gesture produced no visible screen change, and the connected platform is Android while [basic_settings.device].device_type derives hid.pointer_mode="absolute". Stop operation here because the touch mode likely does not match the target. Please switch [basic_settings.device].device_type to "Android", restart the agent, and retry.`
	case (platform == "ios" || platform == "ipados") && pointerMode == "touchscreen":
		return `touch_gesture produced no visible screen change, and the connected platform is iOS while [basic_settings.device].device_type derives hid.pointer_mode="touchscreen". Stop operation here because the touch mode likely does not match the target. Please switch [basic_settings.device].device_type to "iOS", restart the agent, and retry.`
	default:
		return ""
	}
}

func toolNameEqual(got, want string) bool {
	return strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want))
}

func isRunPausingTool(name string) bool {
	return toolNameEqual(name, toolWaitForWakeup) || toolNameEqual(name, toolUserActionStep)
}

func runPausingToolFinalAnswer(step *schema.AgentStep) string {
	if step != nil && toolNameEqual(step.Action.Tool, toolUserActionStep) {
		return humanHandoffFinalAnswer(step)
	}
	return waitForWakeupFinalAnswer(step)
}

func humanHandoffFinalAnswer(step *schema.AgentStep) string {
	if step != nil {
		if content := toolContentFromAction(step.Action); content != "" {
			return content
		}
		if message := humanHandoffMessageFromInput(step.Action.ToolInput); message != "" {
			return message
		}
		if message := humanHandoffMessageFromObservation(step.Observation); message != "" {
			return message
		}
	}
	return waitForWakeupFinalAnswer(step)
}

func humanHandoffMessageFromInput(input string) string {
	var req HumanHandoffRequest
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &req); err != nil {
		return ""
	}
	return firstNonEmptyString([]string{req.SuggestedAction, req.Details})
}

func humanHandoffMessageFromObservation(observation string) string {
	var payload struct {
		Message         string `json:"message"`
		SuggestedAction string `json:"suggested_action"`
		Details         string `json:"details"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(observation)), &payload); err != nil {
		return ""
	}
	return firstNonEmptyString([]string{payload.Message, payload.SuggestedAction, payload.Details})
}

func waitForWakeupFinalAnswer(step *schema.AgentStep) string {
	if step != nil {
		if content := toolContentFromAction(step.Action); content != "" {
			return content
		}
		if message := toolObservationMessage(step.Observation); message != "" {
			return message
		}
	}
	return "I will wait for the next wakeup."
}

func toolObservationMessage(observation string) string {
	observation = strings.TrimSpace(observation)
	if observation == "" || !strings.HasPrefix(observation, "{") {
		return ""
	}
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(observation), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Message)
}
